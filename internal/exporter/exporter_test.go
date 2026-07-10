package exporter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func writeEnv(t *testing.T, path string, env proxy.Envelope) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func sampleLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: t0.Add(25 * time.Millisecond),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`),
	})
	return path
}

func TestBuildCorrelatedExport(t *testing.T) {
	st, id, err := LoadFile(sampleLog(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Session.ID != "s1" || out.Session.Requests != 1 || out.Session.Responses != 1 {
		t.Fatalf("bad summary: %+v", out.Session)
	}
	if len(out.Calls) != 1 || out.Calls[0].ToolName != "echo" || out.Calls[0].DurationMS == nil {
		t.Fatalf("bad calls: %+v", out.Calls)
	}
	if len(out.Events) != 2 || out.Events[1].CallIndex == nil || *out.Events[1].CallIndex != 0 {
		t.Fatalf("bad event correlation: %+v", out.Events)
	}
}

func TestBuildIncludesValidationWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warning.jsonl")
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"id":1,"method":"tools/list"}`),
	})
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || out.Events[0].Warning != "missing jsonrpc=2.0" {
		t.Fatalf("warning not exported: %+v", out.Events)
	}
}

func TestWriteFormats(t *testing.T) {
	st, id, err := LoadFile(sampleLog(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []Format{FormatJSON, FormatHTML, FormatText} {
		var buf bytes.Buffer
		if err := Write(&buf, out, Options{Format: format}); err != nil {
			t.Fatalf("%s write failed: %v", format, err)
		}
		got := buf.String()
		if !strings.Contains(got, "echo") {
			t.Fatalf("%s export missing tool name:\n%s", format, got)
		}
	}
}

// TestResolveSessionPath covers every branch of the resolver that both `export`
// and `open` share: a session id, the newest saved log, and an existing path
// outside the sessions directory that must pass through unchanged.
func TestResolveSessionPath(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())

	older := paths.SessionLogPath("older")
	newer := paths.SessionLogPath("newer")
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, old.Add(time.Hour), old.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A session id resolves to its log under the sessions directory.
	if got, err := ResolveSessionPath("older"); err != nil || got != older {
		t.Fatalf("ResolveSessionPath(\"older\") = %q, %v; want %q", got, err, older)
	}

	// No argument resolves to the newest saved log by mtime.
	if got, err := ResolveSessionPath(""); err != nil || got != newer {
		t.Fatalf("ResolveSessionPath(\"\") = %q, %v; want newest %q", got, err, newer)
	}

	// An existing path outside the sessions directory (a --trace-file capture or
	// a teammate's log) passes through unchanged.
	external := filepath.Join(t.TempDir(), "capture.jsonl")
	if err := os.WriteFile(external, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveSessionPath(external); err != nil || got != external {
		t.Fatalf("ResolveSessionPath(%q) = %q, %v; want it unchanged", external, got, err)
	}

	// An unknown session id and a missing path both error.
	if _, err := ResolveSessionPath("no-such-id"); err == nil {
		t.Fatal("ResolveSessionPath(unknown id) should error")
	}
	if _, err := ResolveSessionPath(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("ResolveSessionPath(missing path) should error")
	}
}

// TestResolveSessionPathNoSessions checks that the empty argument errors clearly
// when nothing has been captured yet.
func TestResolveSessionPathNoSessions(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	if _, err := ResolveSessionPath(""); err == nil {
		t.Fatal("ResolveSessionPath(\"\") with no saved sessions should error")
	}
}
