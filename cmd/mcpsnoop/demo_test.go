package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// TestDemoScriptValidJSON guards against a typo in any scripted frame.
func TestDemoScriptValidJSON(t *testing.T) {
	for i, f := range demoScript() {
		if f.raw == "" {
			continue // stderr line, no JSON
		}
		if !json.Valid([]byte(f.raw)) {
			t.Fatalf("frame %d is not valid JSON:\n%s", i, f.raw)
		}
	}
}

// TestDemoScriptIngests checks the scripted session folds into a coherent model:
// one session, a negotiated handshake, and the flaky tool flagged as an error.
func TestDemoScriptIngests(t *testing.T) {
	st := store.New(0)
	for i, f := range demoScript() {
		env := proxy.Envelope{
			SessionID:   "demo",
			ServerLabel: "demo",
			Seq:         uint64(i + 1),
			TS:          time.Now(),
			Direction:   f.dir,
			Transport:   "stdio",
		}
		if f.text != "" {
			env.Text = f.text
		} else {
			env.Raw = []byte(f.raw)
		}
		st.Ingest(env)
	}

	if got := len(st.Sessions()); got != 1 {
		t.Fatalf("want 1 session, got %d", got)
	}
	if _, ok := st.Capabilities("demo"); !ok {
		t.Fatal("expected negotiated capabilities from the handshake")
	}

	var sawFlaky bool
	for _, c := range st.Calls("demo") {
		if c.ID == "6" {
			sawFlaky = true
			if !c.Failed() {
				t.Fatal("flaky call (id 6) should be flagged as a tool error")
			}
		}
	}
	if !sawFlaky {
		t.Fatal("did not find the flaky tool call (id 6) in the session")
	}
}
