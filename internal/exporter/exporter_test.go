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
	if len(out.Summary.Tools) != 1 || out.Summary.Tools[0].Name != "echo" || out.Summary.Tools[0].P50MS != 25 {
		t.Fatalf("bad tool summary: %+v", out.Summary)
	}
	if len(out.Summary.SlowestCalls) != 1 || out.Summary.SlowestCalls[0].CallIndex != 0 || out.Summary.SlowestCalls[0].DurationMS != 25 {
		t.Fatalf("bad slowest calls: %+v", out.Summary.SlowestCalls)
	}
}

func TestBuildExportsSupersededCallNotAsOk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reuse.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	// Two requests reuse id 1 while the first is still in flight, so the first call
	// is superseded and never answered.
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: t0.Add(5 * time.Millisecond),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(out.Calls))
	}
	sup := out.Calls[0]
	if sup.State != "superseded" || sup.Status != "superseded" {
		t.Fatalf("superseded call = state %q status %q, want both superseded", sup.State, sup.Status)
	}
	if sup.DurationMS != nil {
		t.Fatalf("superseded call must omit duration, got %v ms", *sup.DurationMS)
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

func TestBuildExportsTruncatedFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunc.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	// A response whose observed copy was capped: it must export a truncated marker,
	// not lose the reason its bytes are short now that truncation left the warning.
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ServerToClient, Truncated: true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","result":{}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || !out.Events[0].Truncated {
		t.Fatalf("expected one truncated event, got %+v", out.Events)
	}
	if out.Events[0].Warning != "" {
		t.Fatalf("truncation must not ride the warning field, got %q", out.Events[0].Warning)
	}

	var buf bytes.Buffer
	if err := Write(&buf, out, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "truncated") {
		t.Fatalf("text export should surface the truncation marker\n%s", buf.String())
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
		if format == FormatJSON {
			for _, field := range []string{`"summary"`, `"p50_ms": 25`, `"slowest_calls"`, `"call_index": 0`} {
				if !strings.Contains(got, field) {
					t.Fatalf("json export missing %s:\n%s", field, got)
				}
			}
		}
	}
}

// TestExportFromReaderMatchesFile covers the reader form of ExportFile, the path
// `export -` uses to render a piped log, so stdin exports like a file does.
func TestExportFromReaderMatchesFile(t *testing.T) {
	data, err := os.ReadFile(sampleLog(t))
	if err != nil {
		t.Fatal(err)
	}
	var reader, file bytes.Buffer
	if err := Export(bytes.NewReader(data), "stdin", &reader, Options{Format: FormatText}); err != nil {
		t.Fatalf("export from reader failed: %v", err)
	}
	if err := ExportFile(sampleLog(t), &file, Options{Format: FormatText}); err != nil {
		t.Fatalf("export from file failed: %v", err)
	}
	if reader.String() != file.String() {
		t.Fatalf("reader export differs from file export:\n--- reader ---\n%s\n--- file ---\n%s", reader.String(), file.String())
	}
	if !strings.Contains(reader.String(), "echo") {
		t.Fatalf("reader export missing tool name:\n%s", reader.String())
	}
}

// TestResolveSessionPath covers every branch of the resolver that both `export`
// and `open` share, a session id, the newest saved log, and an existing path
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

func TestWriteOTLP(t *testing.T) {
	st, id, err := LoadFile(sampleLog(t))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatOTLP}); err != nil {
		t.Fatal(err)
	}

	var payload struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID           string `json:"traceId"`
					SpanID            string `json:"spanId"`
					Name              string `json:"name"`
					StartTimeUnixNano string `json:"startTimeUnixNano"`
					EndTimeUnixNano   string `json:"endTimeUnixNano"`
					Status            struct {
						Code string `json:"code"`
					} `json:"status"`
					Attributes []struct {
						Key string `json:"key"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("invalid OTLP JSON: %v\n%s", err, buf.String())
	}
	if len(payload.ResourceSpans) != 1 || len(payload.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("unexpected OTLP hierarchy: %s", buf.String())
	}
	spans := payload.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "tools/call" || len(span.TraceID) != 32 || len(span.SpanID) != 16 || span.StartTimeUnixNano == "" || span.EndTimeUnixNano == "" || span.Status.Code != "STATUS_CODE_OK" {
		t.Fatalf("bad OTLP span: %+v", span)
	}
	keys := make(map[string]bool, len(span.Attributes))
	for _, attr := range span.Attributes {
		keys[attr.Key] = true
	}
	for _, key := range []string{"rpc.system", "rpc.method", "mcpsnoop.call.duration_ms", "mcpsnoop.call.is_error", "mcpsnoop.call.tool_name"} {
		if !keys[key] {
			t.Errorf("OTLP span missing %q", key)
		}
	}
}

func TestDefaultOutputPathExtensions(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	cases := map[Format]string{
		FormatJSON: "s1.json",
		FormatHTML: "s1.html",
		FormatText: "s1.txt",
		FormatOTLP: "s1.otlp.json",
	}
	for format, suffix := range cases {
		if got := DefaultOutputPath("s1", format); !strings.HasSuffix(got, suffix) {
			t.Errorf("DefaultOutputPath(%q) = %q, want suffix %q", format, got, suffix)
		}
	}
}
