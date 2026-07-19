package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// TestDemoIsolatesStateHome checks the demo never writes into the user's real
// state directory: after it sets up its throwaway home, every paths-derived
// location resolves inside it, so the scripted tools/list baseline lands there
// and the real home is left untouched.
func TestDemoIsolatesStateHome(t *testing.T) {
	real := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", real) // stand in for the user's real home

	dir, err := demoIsolatedHome()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	if got := paths.ToolBaselinesDir(); !strings.HasPrefix(got, dir) {
		t.Fatalf("tool baselines resolve to %q, want inside the demo home %q", got, dir)
	}
	if entries, _ := os.ReadDir(filepath.Join(real, "tool-baselines")); len(entries) != 0 {
		t.Fatalf("the real home must stay untouched, found %d entries under it", len(entries))
	}
}

// TestDemoScriptValidJSON guards against a typo in any scripted frame. Frames
// marked invalid are the deliberate stdout-corruption case and must not parse as
// JSON-RPC. Every other frame must be valid JSON.
func TestDemoScriptValidJSON(t *testing.T) {
	for i, f := range demoScript() {
		if f.raw == "" {
			continue // stderr line, no JSON
		}
		if f.invalid {
			if _, ok := proxy.ParseRPC([]byte(f.raw)); ok {
				t.Fatalf("frame %d is marked invalid but parses as JSON-RPC:\n%s", i, f.raw)
			}
			continue
		}
		if !json.Valid([]byte(f.raw)) {
			t.Fatalf("frame %d is not valid JSON:\n%s", i, f.raw)
		}
	}
}

// TestDemoScriptIngests checks the scripted session folds into a coherent model,
// one session, a negotiated handshake, and the flaky tool flagged as an error.
func TestDemoScriptIngests(t *testing.T) {
	st := store.New()
	var sawInvalid bool
	for i, f := range demoScript() {
		env := demoEnvelope("demo", uint64(i+1), f)
		// Every frame must encode, or the sink would silently drop it before the
		// hub ever sees it.
		if err := json.NewEncoder(io.Discard).Encode(env); err != nil {
			t.Fatalf("demo frame %d failed to encode: %v", i, err)
		}
		if ev := st.Ingest(env); f.invalid && ev.Kind == store.EventInvalid {
			sawInvalid = true
		}
	}

	if got := len(st.Sessions()); got != 1 {
		t.Fatalf("want 1 session, got %d", got)
	}
	if !sawInvalid {
		t.Fatal("the deliberate stdout-corruption frame should be flagged as EventInvalid")
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
