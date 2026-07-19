package tui

import (
	"encoding/json"
	"testing"

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
	if len(d.ChangedDescriptions) != 1 || d.ChangedDescriptions[0] != "search" {
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
