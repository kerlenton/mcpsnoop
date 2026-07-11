package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func env(session string, seq uint64, method string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: session,
		Seq:       seq,
		TS:        time.Now(),
		Direction: proxy.ClientToServer,
		Transport: "stdio",
		Raw:       json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, seq, method)),
	}
}

// writeLog writes envelopes as JSONL to the session log for session.
func writeLog(t *testing.T, dir, session string, envs ...proxy.Envelope) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, session+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range envs {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHubBackfillLiveDedup verifies that file backfill and a live socket stream
// merge for the same session without duplicates, and that a fresh session over
// the socket comes through.
func TestHubBackfillLiveDedup(t *testing.T) {
	sessionsDir := t.TempDir()
	// Short socket path, macOS caps unix socket paths at ~104 bytes.
	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcpsnoop-test-%d.sock", os.Getpid()))
	defer os.Remove(sockPath)

	// History on disk for session s1, seq 1..3.
	writeLog(t, sessionsDir, "s1", env("s1", 1, "initialize"), env("s1", 2, "tools/list"), env("s1", 3, "tools/call"))

	got := make(chan proxy.Envelope, 64)
	h := New(sockPath, sessionsDir, func(e proxy.Envelope) { got <- e })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	// Live shim, re-sends s1 seq 2,3 (overlap → must dedup) and 4 (new), plus a
	// brand-new session s2.
	sink := proxy.NewSocketSink(sockPath, 0)
	defer sink.Close()
	// Give the hub a moment to finish backfill and start accepting.
	time.Sleep(300 * time.Millisecond)
	for _, e := range []proxy.Envelope{
		env("s1", 2, "tools/list"), env("s1", 3, "tools/call"), env("s1", 4, "ping"),
		env("s2", 1, "initialize"), env("s2", 2, "tools/list"),
	} {
		sink.Emit(e)
	}

	// Expect 6 unique envelopes, s1{1,2,3,4} + s2{1,2}.
	maxSeq := map[string]uint64{}
	count := 0
	deadline := time.After(3 * time.Second)
	for count < 6 {
		select {
		case e := <-got:
			count++
			if e.Seq <= maxSeq[e.SessionID] {
				t.Fatalf("duplicate/out-of-order frame: session=%s seq=%d (already saw %d)", e.SessionID, e.Seq, maxSeq[e.SessionID])
			}
			maxSeq[e.SessionID] = e.Seq
		case <-deadline:
			t.Fatalf("timed out: got %d/6 frames, maxSeq=%v", count, maxSeq)
		}
	}
	if maxSeq["s1"] != 4 || maxSeq["s2"] != 2 {
		t.Fatalf("unexpected high-water marks: %v", maxSeq)
	}

	// No extra frames should arrive (e.g. a leaked duplicate).
	select {
	case e := <-got:
		t.Fatalf("unexpected extra frame: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}
