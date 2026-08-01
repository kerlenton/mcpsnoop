package tui

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/toolbaseline"
)

// TestObserveAndNudgeSurfacesDrift checks the off-the-delivery-path observation:
// a changed tool definition drifts on the next observation and the UI is nudged,
// the same outcome the worker produces asynchronously.
func TestObserveAndNudgeSurfacesDrift(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search docs"}]}}`))

	m := toolbaseline.New(t.TempDir())
	nudges := 0
	nudge := func() { nudges++ }

	// First observation trusts the definitions: clean, but it still nudges.
	observeAndNudge(m, st, "s1", nudge)
	if d, _ := st.ToolDrift("s1"); d.Count() != 0 || d.BaselineError != "" {
		t.Fatalf("first observation should be clean, got %+v", d)
	}

	// A later tools/list changes the description, so the next observation drifts.
	st.Ingest(env(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	st.Ingest(env(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"search","description":"Search private docs"}]}}`))
	observeAndNudge(m, st, "s1", nudge)

	d, _ := st.ToolDrift("s1")
	if changed := d.Names(store.DriftDescription); len(changed) != 1 || changed[0] != "search" {
		t.Fatalf("drift should surface after the observation, got %+v", d)
	}
	if nudges != 2 {
		t.Fatalf("each observation should nudge the UI, got %d", nudges)
	}
}

// TestObserveAndNudgeSurfacesBaselineError checks a failed observation still
// records a per-session BaselineError and nudges, as the callback did inline.
func TestObserveAndNudgeSurfacesBaselineError(t *testing.T) {
	st := store.New()
	// No server label, so the baseline cannot be keyed and observation errors.
	st.Ingest(proxy.Envelope{SessionID: "s2", ServerLabel: "", Seq: 1, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)})
	st.Ingest(proxy.Envelope{SessionID: "s2", ServerLabel: "", Seq: 2, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search"}]}}`)})

	nudged := false
	observeAndNudge(toolbaseline.New(t.TempDir()), st, "s2", func() { nudged = true })

	if d, ok := st.ToolDrift("s2"); !ok || d.BaselineError == "" {
		t.Fatalf("a baseline error should reach the session, got %+v ok %v", d, ok)
	}
	if !nudged {
		t.Fatal("a baseline error should still nudge the UI")
	}
}

// TestRunReportsAnAlreadyRunningHub. hub.Run returns before it backfills when
// the socket is taken, so throwing the error away left the second instance on an
// empty screen with the footer still claiming to listen and nothing on stderr.
func TestRunReportsAnAlreadyRunningHub(t *testing.T) {
	// A short base, since a unix socket path is capped near 104 bytes and the
	// default temp dir on macOS is already most of that.
	dir, err := os.MkdirTemp("/tmp", "mcs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s.sock")
	held, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer held.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = RunWithHistoryLimit(ctx, socket, dir, 0)
	if err == nil {
		t.Fatal("a second hub on a taken socket must report it rather than show an empty view")
	}
	if !strings.Contains(err.Error(), socket) {
		t.Fatalf("the error should name the socket, got %v", err)
	}
}
