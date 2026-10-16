package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// sessionEnvelopes is a two-frame session named id, a call answered with result.
func sessionEnvelopes(id, result string) []proxy.Envelope {
	return []proxy.Envelope{
		{SessionID: id, ServerLabel: id, Seq: 1, TS: time.Unix(1, 0), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`)},
		{SessionID: id, ServerLabel: id, Seq: 2, TS: time.Unix(2, 0), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,` + result + `}`)},
	}
}

const (
	answered = `"result":{"content":[]}`
	failed   = `"error":{"code":-32603,"message":"boom"}`
)

// TestCheckReadsSeveralCaptures. An agent run with several servers leaves one
// capture per server, and a gate over one of them passes whatever happened in
// the others. Several paths, and a directory of them, are checked together,
// each session on its own line.
func TestCheckReadsSeveralCaptures(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	clean, dirty := filepath.Join(dir, "a-clean.jsonl"), filepath.Join(dir, "b-dirty.jsonl")
	writeCheckLog(t, clean, sessionEnvelopes("clean", answered)...)
	writeCheckLog(t, dirty, sessionEnvelopes("dirty", failed)...)
	// What --trace-file leaves behind when no client ever connected.
	if err := os.WriteFile(filepath.Join(dir, "c-unused.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"two paths":   {clean, dirty},
		"a directory": {dir},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := executeCheck(t, args, "")
			if code != 1 {
				t.Fatalf("exit = %d, want 1 for the failing session; stderr %q", code, stderr)
			}
			for _, want := range []string{"session clean: errors=0", "session dirty: errors=1", "check failed: error"} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("stdout = %q, want %q", stdout, want)
				}
			}
		})
	}

	if code, _, stderr := executeCheck(t, []string{clean, filepath.Join(dir, "c-unused.jsonl")}, ""); code != 2 {
		t.Fatalf("an empty file named outright is still no capture, exit = %d, stderr = %q", code, stderr)
	}
}

// TestCheckSARIFPointsEachFindingAtItsOwnFile. One report covers every file,
// and a finding has to name the file its session came from, or code scanning
// files it against the wrong one.
func TestCheckSARIFPointsEachFindingAtItsOwnFile(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	writeCheckLog(t, "one.jsonl", sessionEnvelopes("one", failed)...)
	writeCheckLog(t, "two.jsonl", sessionEnvelopes("two", failed)...)

	code, stdout, _ := executeCheck(t, []string{"--format", "sarif", "one.jsonl", "two.jsonl"}, "")
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	report := decodeCheckSARIF(t, stdout)
	seen := map[string]string{}
	for _, result := range report.Runs[0].Results {
		if len(result.Locations) != 1 {
			t.Fatalf("result %q has %d locations", result.Message.Text, len(result.Locations))
		}
		uri := result.Locations[0].PhysicalLocation.ArtifactLocation.URI
		switch {
		case strings.Contains(result.Message.Text, "session one"):
			seen["one"] = uri
		case strings.Contains(result.Message.Text, "session two"):
			seen["two"] = uri
		}
	}
	if seen["one"] != "one.jsonl" || seen["two"] != "two.jsonl" {
		t.Fatalf("locations = %v, want each session's own file", seen)
	}
}

func TestCheckRefusesWhatItCannotRead(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	writeCheckLog(t, path, sessionEnvelopes("a", answered)...)
	empty := t.TempDir()
	for name, args := range map[string][]string{
		"stdin beside a file": {"-", path},
		"a directory of none": {empty},
		"a path that is not":  {path, filepath.Join(dir, "missing.jsonl")},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := executeCheck(t, args, ""); code != 2 {
				t.Fatalf("exit = %d, want 2, since nothing was checked", code)
			}
		})
	}
}

// TestHTTPWritesTheTraceFileItIsGiven. The HTTP proxy had no --trace-file, so a
// CI job could not say where its capture would land, and a trace file in a
// directory that did not exist yet was silently not written at all.
func TestHTTPWritesTheTraceFileItIsGiven(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "msh")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("MCPSNOOP_HOME", home)
	trace := filepath.Join(t.TempDir(), "not", "yet", "there.jsonl")

	prev := runHTTPFn
	runHTTPFn = func(_ context.Context, cfg proxy.HTTPConfig) error {
		cfg.Sink.Emit(proxy.Envelope{SessionID: cfg.SessionID, ServerLabel: cfg.Label, Seq: 1, TS: time.Now(),
			Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)})
		return nil
	}
	defer func() { runHTTPFn = prev }()

	if code := execute([]string{"http", "--target", "http://127.0.0.1:1/mcp", "--trace-file", trace}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	data, err := os.ReadFile(trace)
	if err != nil || !strings.Contains(string(data), `"method":"ping"`) {
		t.Fatalf("the trace file holds %q (%v)", data, err)
	}
}
