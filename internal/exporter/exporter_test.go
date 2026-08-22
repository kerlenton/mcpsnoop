package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// ingest folds raw JSON-RPC frames into a fresh session, alternating client and
// server direction, so a test can build a store to export without a log file.
func ingest(t *testing.T, st *store.Store, raws ...string) {
	t.Helper()
	now := time.Now()
	for i, raw := range raws {
		dir := proxy.ClientToServer
		if i%2 == 1 {
			dir = proxy.ServerToClient
		}
		st.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: uint64(i + 1),
			TS: now.Add(time.Duration(i) * time.Millisecond), Direction: dir,
			Raw: json.RawMessage(raw),
		})
	}
}

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

func TestBuildExportsMissingFrames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 4, TS: t0.Add(time.Millisecond),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Session.MissingFrames != 2 {
		t.Fatalf("missing_frames = %d, want 2", out.Session.MissingFrames)
	}

	var buf bytes.Buffer
	if err := Write(&buf, out, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Session SessionSummary `json:"session"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Session.MissingFrames != 2 {
		t.Fatalf("json export missing_frames = %d, want 2", payload.Session.MissingFrames)
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

func TestBuildKeepsNullIDCallsDistinct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "null-id.jsonl")
	t0 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "files", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"read_file"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "files", Seq: 2, TS: t0.Add(time.Second),
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"write_file"}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Calls) != 2 || len(out.Events) != 2 {
		t.Fatalf("calls/events = %d/%d, want 2/2", len(out.Calls), len(out.Events))
	}
	for i, want := range []string{"read_file", "write_file"} {
		if out.Events[i].CallIndex == nil || *out.Events[i].CallIndex != i {
			t.Fatalf("event %d call index = %v, want %d", i, out.Events[i].CallIndex, i)
		}
		if out.Calls[i].ToolName != want {
			t.Fatalf("call %d tool = %q, want %q", i, out.Calls[i].ToolName, want)
		}
	}
}

func TestBuildExportsCallCancellationAndLateResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancelled-call.jsonl")
	t0 := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	cancelledAt := t0.Add(250 * time.Millisecond)
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: cancelledAt,
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"moved on"}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Session.Pending != 0 || cancelled.Session.LateResults != 0 || len(cancelled.Calls) != 1 {
		t.Fatalf("cancelled summary/calls = %+v %+v", cancelled.Session, cancelled.Calls)
	}
	call := cancelled.Calls[0]
	if call.State != "cancelled" || call.Status != "call_cancelled" {
		t.Fatalf("cancelled state/status = %q/%q", call.State, call.Status)
	}
	if call.CancelledAt == nil || *call.CancelledAt != cancelledAt || call.CancelReason != "moved on" {
		t.Fatalf("cancelled metadata = %v %q", call.CancelledAt, call.CancelReason)
	}
	if call.EndedAt != nil || call.DurationMS != nil || call.LateResult || len(call.Result) != 0 {
		t.Fatalf("unanswered cancellation exported as %+v", call)
	}

	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 3, TS: t0.Add(time.Second),
		Direction: proxy.ServerToClient,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`),
	})
	late, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	call = late.Calls[0]
	if late.Session.LateResults != 1 || call.Status != "late_result" || !call.LateResult {
		t.Fatalf("late result summary/call = %+v %+v", late.Session, call)
	}
	if call.EndedAt == nil || call.DurationMS == nil || *call.DurationMS != 1000 {
		t.Fatalf("late result timing = ended %v duration %v", call.EndedAt, call.DurationMS)
	}
	if len(late.Events) != 3 || late.Events[2].Observation != "result arrived 750ms after cancellation" {
		t.Fatalf("late result event = %+v", late.Events)
	}

	var text bytes.Buffer
	if err := Write(&text, late, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"late results: 1", "status=late_result duration_ms=1000", "observation=result arrived 750ms after cancellation"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text export missing %q\n%s", want, text.String())
		}
	}

	var otlp bytes.Buffer
	if err := Write(&otlp, late, Options{Format: FormatOTLP}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mcpsnoop.session.late_results", "mcpsnoop.call.cancelled_at", "mcpsnoop.call.cancel_reason", "mcpsnoop.call.late_result", "STATUS_CODE_UNSET"} {
		if !strings.Contains(otlp.String(), want) {
			t.Fatalf("OTLP export missing %q\n%s", want, otlp.String())
		}
	}
}

func TestBuildExportsCancelledTaskWithoutFlaggingAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: t0.Add(time.Millisecond),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"task","taskId":"cancel-9","status":"working"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 3, TS: t0.Add(2 * time.Second),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"cancel-9","status":"cancelled"}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Calls) != 1 {
		t.Fatalf("want 1 call, got %d: %+v", len(out.Calls), out.Calls)
	}
	c := out.Calls[0]
	// The state still reports what happened (the call ended without a result), but
	// the status is its own cancelled verdict ahead of the error branch, and is_error
	// tracks the error axis, which a deliberate cancel is not on.
	if c.Status != "cancelled" {
		t.Fatalf("cancelled task status = %q, want cancelled", c.Status)
	}
	if c.IsError {
		t.Fatal("a cancelled task must not be flagged is_error")
	}
	if c.State != "failed" {
		t.Fatalf("state = %q, want failed", c.State)
	}
	// Without these the exported trace never says a task was cancelled at all.
	if c.TaskID != "cancel-9" || c.TaskStatus != "cancelled" {
		t.Fatalf("export must carry the task outcome, got task_id %q task_status %q", c.TaskID, c.TaskStatus)
	}
}

func TestBuildFlagsToolErrorTaskAsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolerr.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"grep"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: t0.Add(time.Millisecond),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"task","taskId":"toolerr-1","status":"working"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 3, TS: t0.Add(time.Second),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"taskId":"toolerr-1"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 4, TS: t0.Add(2 * time.Second),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"taskId":"toolerr-1","status":"completed","result":{"content":[{"type":"text","text":"boom"}],"isError":true}}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	// A tool error inside a task is on the error axis everywhere, including here.
	call := out.Calls[0]
	if call.ToolName != "grep" {
		t.Fatalf("first call = %+v, want the grep tools/call", call)
	}
	if call.Status != "error" || !call.IsError {
		t.Fatalf("tool error task = status %q is_error %v, want error/true", call.Status, call.IsError)
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
					ParentSpanID      string `json:"parentSpanId"`
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
	if span.ParentSpanID != "" {
		t.Fatalf("span without trace context has parentSpanId %q", span.ParentSpanID)
	}
	var rawPayload struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []map[string]json.RawMessage `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rawPayload); err != nil {
		t.Fatal(err)
	}
	if _, ok := rawPayload.ResourceSpans[0].ScopeSpans[0].Spans[0]["parentSpanId"]; ok {
		t.Fatalf("span without trace context must omit parentSpanId: %s", buf.String())
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

func TestWriteOTLPAcceptsFutureTraceparentVersions(t *testing.T) {
	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
	)
	cases := map[string]string{
		"fixed fields": "01-" + traceID + "-" + parentSpanID + "-01",
		"extension":    "01-" + traceID + "-" + parentSpanID + "-01-vendor-data",
	}
	for name, traceparent := range cases {
		t.Run(name, func(t *testing.T) {
			data := SessionExport{
				Session: SessionSummary{ID: "s1"},
				Calls: []CallExport{{
					ID:        "call-1",
					Method:    "tools/call",
					StartedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
					Params:    json.RawMessage(`{"_meta":{"traceparent":"` + traceparent + `"}}`),
				}},
			}
			spans := writeOTLPTraceFields(t, data)
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}
			if spans[0].TraceID != traceID || spans[0].ParentSpanID != parentSpanID {
				t.Fatalf("future-version trace context = %q/%q, want %q/%q", spans[0].TraceID, spans[0].ParentSpanID, traceID, parentSpanID)
			}
		})
	}
}

func TestWriteOTLPUsesPerCallTraceparent(t *testing.T) {
	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
		traceparent  = "00-" + traceID + "-" + parentSpanID + "-01"
	)
	started := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	data := SessionExport{
		Session: SessionSummary{ID: "s1"},
		Calls: []CallExport{
			{
				ID:        "with-context",
				Method:    "tools/call",
				StartedAt: started,
				Params:    json.RawMessage(`{"_meta":{"traceparent":"` + traceparent + `"}}`),
			},
			{
				ID:        "without-context",
				Method:    "tools/call",
				StartedAt: started,
				Params:    json.RawMessage(`{"name":"lookup"}`),
			},
		},
	}

	spans := writeOTLPTraceFields(t, data)
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	if spans[0].TraceID != traceID || spans[0].ParentSpanID != parentSpanID {
		t.Fatalf("propagated span context = %q/%q, want %q/%q", spans[0].TraceID, spans[0].ParentSpanID, traceID, parentSpanID)
	}
	syntheticTraceID := otlpID(16, "trace", data.Session.ID)
	if spans[1].TraceID != syntheticTraceID || spans[1].ParentSpanID != "" {
		t.Fatalf("context leaked to next span: traceId %q parentSpanId %q, want %q/empty", spans[1].TraceID, spans[1].ParentSpanID, syntheticTraceID)
	}
}

func TestWriteOTLPRejectsInvalidTraceparent(t *testing.T) {
	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
		traceparent  = "00-" + traceID + "-" + parentSpanID + "-01"
	)
	cases := map[string]json.RawMessage{
		"malformed value":     json.RawMessage(`{"_meta":{"traceparent":"not-a-traceparent"}}`),
		"malformed params":    json.RawMessage(`{"_meta":`),
		"meta is not object":  json.RawMessage(`{"_meta":"nope"}`),
		"value is not string": json.RawMessage(`{"_meta":{"traceparent":42}}`),
		"uppercase meta key":  json.RawMessage(`{"_META":{"traceparent":"` + traceparent + `"}}`),
		"uppercase value key": json.RawMessage(`{"_meta":{"TraceParent":"` + traceparent + `"}}`),
		"zero trace id":       json.RawMessage(`{"_meta":{"traceparent":"00-00000000000000000000000000000000-` + parentSpanID + `-01"}}`),
		"zero parent span id": json.RawMessage(`{"_meta":{"traceparent":"00-` + traceID + `-0000000000000000-01"}}`),
		"uppercase trace id":  json.RawMessage(`{"_meta":{"traceparent":"00-4BF92F3577B34DA6A3CE929D0E0E4736-` + parentSpanID + `-01"}}`),
		"uppercase version":   json.RawMessage(`{"_meta":{"traceparent":"0A-` + traceID + `-` + parentSpanID + `-01"}}`),
		"uppercase parent id": json.RawMessage(`{"_meta":{"traceparent":"00-` + traceID + `-00F067AA0BA902B7-01"}}`),
		"uppercase flags":     json.RawMessage(`{"_meta":{"traceparent":"00-` + traceID + `-` + parentSpanID + `-0A"}}`),
		"forbidden version":   json.RawMessage(`{"_meta":{"traceparent":"ff-` + traceID + `-` + parentSpanID + `-01"}}`),
		"invalid flags":       json.RawMessage(`{"_meta":{"traceparent":"00-` + traceID + `-` + parentSpanID + `-0g"}}`),
		"version 00 suffix":   json.RawMessage(`{"_meta":{"traceparent":"00-` + traceID + `-` + parentSpanID + `-01-extra"}}`),
		"version separator":   json.RawMessage(`{"_meta":{"traceparent":"00_` + traceID + `-` + parentSpanID + `-01"}}`),
		"trace id separator":  json.RawMessage(`{"_meta":{"traceparent":"00-` + traceID + `_` + parentSpanID + `-01"}}`),
		"parent id separator": json.RawMessage(`{"_meta":{"traceparent":"00-` + traceID + `-` + parentSpanID + `_01"}}`),
		"extension separator": json.RawMessage(`{"_meta":{"traceparent":"01-` + traceID + `-` + parentSpanID + `-01extra"}}`),
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			data := SessionExport{
				Session: SessionSummary{ID: "s1"},
				Calls: []CallExport{{
					ID:        "call-1",
					Method:    "tools/call",
					StartedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
					Params:    params,
				}},
			}
			spans := writeOTLPTraceFields(t, data)
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}
			wantTraceID := otlpID(16, "trace", data.Session.ID)
			if spans[0].TraceID != wantTraceID || spans[0].ParentSpanID != "" {
				t.Fatalf("invalid traceparent produced context %q/%q, want %q/empty", spans[0].TraceID, spans[0].ParentSpanID, wantTraceID)
			}
		})
	}
}

type otlpTraceFields struct {
	TraceID      string `json:"traceId"`
	ParentSpanID string `json:"parentSpanId"`
	TraceState   string `json:"traceState"`
}

func writeOTLPTraceFields(t *testing.T, data SessionExport) []otlpTraceFields {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, data); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []otlpTraceFields `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("invalid OTLP JSON: %v\n%s", err, buf.String())
	}
	if len(payload.ResourceSpans) != 1 || len(payload.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("unexpected OTLP hierarchy: %s", buf.String())
	}
	return payload.ResourceSpans[0].ScopeSpans[0].Spans
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

// TestExportCarriesContextCost checks the figures reach the export, which is
// what makes them trackable over time and diffable between two captures.
func TestExportCarriesContextCost(t *testing.T) {
	st := store.New()
	ingest(t, st,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search things.","inputSchema":{"type":"object"}}]}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hello"}]}}`,
	)

	out, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary.Definitions == nil {
		t.Fatal("the export should carry the tool list's fixed cost")
	}
	def := out.Summary.Definitions
	if !def.Complete || def.Tools != 1 || def.Bytes == 0 {
		t.Fatalf("definitions = %+v", def)
	}
	if len(def.PerTool) != 1 || def.PerTool[0].Name != "search" || def.PerTool[0].SchemaBytes == 0 {
		t.Fatalf("per-tool breakdown = %+v", def.PerTool)
	}
	if len(out.Summary.Tools) != 1 || out.Summary.Tools[0].ResultBytes == 0 {
		t.Fatalf("result bytes missing from the export: %+v", out.Summary.Tools)
	}
	if out.Summary.Tools[0].MaxResultBytes == 0 {
		t.Fatalf("worst-case result missing from the export: %+v", out.Summary.Tools)
	}
}

// TestExportCarriesTheTransportEvent checks the export end of the new event
// kind. A 401 that produced no envelope at all before this now has to survive
// into a machine-readable artefact, status and challenge included, and the text
// export has to stay one line per frame.
func TestExportCarriesTheTransportEvent(t *testing.T) {
	const challenge = `Bearer resource_metadata="https://auth.example/x"`
	st := store.New()
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: "http",
		Status: 401, AuthChallenge: challenge,
	})

	out, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("expected one exported event, got %d", len(out.Events))
	}
	ev := out.Events[0]
	if ev.Kind != "transport" {
		t.Fatalf("kind = %q, want transport", ev.Kind)
	}
	if ev.Status != 401 || ev.AuthChallenge != challenge {
		t.Fatalf("the status and challenge must reach the export, got %d %q", ev.Status, ev.AuthChallenge)
	}

	var buf bytes.Buffer
	if err := Write(&buf, out, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "transport") {
		t.Fatalf("the text export should name the kind, got %q", buf.String())
	}
}

// TestExportNamesASpecDefinedErrorCode. The export is what a script reads, so
// the name rides alongside the wire error rather than inside it, and a code the
// spec leaves to implementations carries no name at all.
func TestExportNamesASpecDefinedErrorCode(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{-32020, "header mismatch"},
		{-32001, ""},
	} {
		st := store.New()
		ingest(t, st,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":"nope"}}`, tc.code))
		out, err := Build(st, "s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Calls) != 1 {
			t.Fatalf("expected one call, got %d", len(out.Calls))
		}
		if out.Calls[0].ErrorName != tc.want {
			t.Fatalf("code %d exported name %q, want %q", tc.code, out.Calls[0].ErrorName, tc.want)
		}
		if out.Calls[0].Error == nil || out.Calls[0].Error.Code != tc.code {
			t.Fatalf("the wire error object must stay intact, got %+v", out.Calls[0].Error)
		}
	}
}

// TestWriteOTLPCarriesTraceState covers the second half of the carrier. W3C
// expects a participant continuing a trace to pass tracestate along, and an OTLP
// span has a field for it, so dropping it strands whatever vendor state the
// caller was routing or sampling on.
func TestWriteOTLPCarriesTraceState(t *testing.T) {
	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
		traceparent  = "00-" + traceID + "-" + parentSpanID + "-01"
		tracestate   = "vendorname1=opaqueValue1,vendorname2=opaqueValue2"
	)
	data := SessionExport{
		Session: SessionSummary{ID: "s1"},
		Calls: []CallExport{{
			ID:        "call-1",
			Method:    "tools/call",
			StartedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
			Params: json.RawMessage(`{"_meta":{"traceparent":"` + traceparent +
				`","tracestate":"` + tracestate + `"}}`),
		}},
	}

	spans := writeOTLPTraceFields(t, data)
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].TraceState != tracestate {
		t.Fatalf("traceState = %q, want the caller's %q", spans[0].TraceState, tracestate)
	}
	// mcpsnoop observes rather than participates, so it adds no member of its own
	// and the value must arrive byte for byte as the caller sent it.
	if spans[0].TraceID != traceID || spans[0].ParentSpanID != parentSpanID {
		t.Fatalf("trace context = %q/%q, want %q/%q", spans[0].TraceID, spans[0].ParentSpanID, traceID, parentSpanID)
	}
}

// TestWriteOTLPOmitsUnusableTraceState checks the cases where carrying the value
// on would be worse than dropping it, including the one W3C is explicit about:
// tracestate belonging to a traceparent that did not parse describes a trace we
// cannot name, so it goes with it.
func TestWriteOTLPOmitsUnusableTraceState(t *testing.T) {
	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
		traceparent  = "00-" + traceID + "-" + parentSpanID + "-01"
	)
	tooMany := make([]string, 33)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("vendor%d=value", i)
	}

	cases := map[string]json.RawMessage{
		"absent":            json.RawMessage(`{"_meta":{"traceparent":"` + traceparent + `"}}`),
		"empty":             json.RawMessage(`{"_meta":{"traceparent":"` + traceparent + `","tracestate":""}}`),
		"not a string":      json.RawMessage(`{"_meta":{"traceparent":"` + traceparent + `","tracestate":42}}`),
		"member has no key": json.RawMessage(`{"_meta":{"traceparent":"` + traceparent + `","tracestate":"=value"}}`),
		"member is not a pair": json.RawMessage(`{"_meta":{"traceparent":"` + traceparent +
			`","tracestate":"vendorname1=value1,garbage"}}`),
		"past the 32-member limit": json.RawMessage(`{"_meta":{"traceparent":"` + traceparent +
			`","tracestate":"` + strings.Join(tooMany, ",") + `"}}`),
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			data := SessionExport{
				Session: SessionSummary{ID: "s1"},
				Calls: []CallExport{{
					ID:        "call-1",
					Method:    "tools/call",
					StartedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
					Params:    params,
				}},
			}
			spans := writeOTLPTraceFields(t, data)
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}
			if spans[0].TraceState != "" {
				t.Fatalf("traceState = %q, want it dropped", spans[0].TraceState)
			}
			// Dropping the state must not cost the parenting: the traceparent is
			// valid in every one of these cases.
			if spans[0].TraceID != traceID || spans[0].ParentSpanID != parentSpanID {
				t.Fatalf("an unusable tracestate cost the trace context: %q/%q", spans[0].TraceID, spans[0].ParentSpanID)
			}
		})
	}
}

// TestWriteOTLPDiscardsTraceStateWithAnInvalidTraceparent is the rule W3C states
// outright: state without a trustworthy traceparent goes nowhere.
func TestWriteOTLPDiscardsTraceStateWithAnInvalidTraceparent(t *testing.T) {
	data := SessionExport{
		Session: SessionSummary{ID: "s1"},
		Calls: []CallExport{{
			ID:        "call-1",
			Method:    "tools/call",
			StartedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
			Params:    json.RawMessage(`{"_meta":{"traceparent":"nope","tracestate":"vendorname1=value1"}}`),
		}},
	}

	spans := writeOTLPTraceFields(t, data)
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].TraceState != "" || spans[0].ParentSpanID != "" {
		t.Fatalf("nothing should be carried from an invalid traceparent: %q/%q", spans[0].TraceState, spans[0].ParentSpanID)
	}
	if want := otlpID(16, "trace", data.Session.ID); spans[0].TraceID != want {
		t.Fatalf("traceId = %q, want the session-derived %q", spans[0].TraceID, want)
	}
}

// TestWriteOTLPOmitsTraceStateFieldWhenAbsent keeps the payload clean for the
// ordinary session, where no caller propagated anything.
func TestWriteOTLPOmitsTraceStateFieldWhenAbsent(t *testing.T) {
	data := SessionExport{
		Session: SessionSummary{ID: "s1"},
		Calls: []CallExport{{
			ID:        "call-1",
			Method:    "tools/list",
			StartedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		}},
	}
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, data); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []map[string]json.RawMessage `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload.ResourceSpans[0].ScopeSpans[0].Spans[0]["traceState"]; ok {
		t.Fatalf("a span with no propagated state must omit traceState: %s", buf.String())
	}
}

// incompleteSession is a one-call export with a stated number of dropped frames,
// so the artifact tests differ only in the thing under test.
func incompleteSession(missing uint64) SessionExport {
	return SessionExport{
		Session: SessionSummary{ID: "s1", Label: "demo", MissingFrames: missing},
		Calls: []CallExport{{
			ID:        "call-1",
			Method:    "tools/list",
			Direction: proxy.ClientToServer,
			StartedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		}},
	}
}

// TestWriteHARFlagsAnIncompleteCapture. The count already reaches the JSON
// export and the check gate, but a HAR is opened somewhere else entirely, and a
// call whose frames never reached the log is simply absent from it. Absence
// reads as "it never happened", so the file has to say otherwise itself.
func TestWriteHARFlagsAnIncompleteCapture(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHAR(&buf, incompleteSession(3)); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Log struct {
			Comment string `json:"comment"`
			Entries []struct {
				Time float64 `json:"time"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("invalid HAR JSON: %v\n%s", err, buf.String())
	}
	if payload.Log.Comment == "" {
		t.Fatalf("an incomplete capture must say so in log.comment:\n%s", buf.String())
	}
	if !strings.Contains(payload.Log.Comment, "3 frames") {
		t.Fatalf("the comment should name the count, got %q", payload.Log.Comment)
	}
	// The warning must not come at the cost of the entries themselves.
	if len(payload.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(payload.Log.Entries))
	}
}

// TestWriteHARCommentIsSingularForOneFrame keeps the prose readable, since this
// field is rendered to a person rather than parsed.
func TestWriteHARCommentIsSingularForOneFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHAR(&buf, incompleteSession(1)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "1 frame were") && !strings.Contains(buf.String(), "1 frame was") {
		t.Fatalf("expected singular wording for a single dropped frame:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "1 frames") {
		t.Fatalf("plural used for a single frame:\n%s", buf.String())
	}
}

// TestWriteHAROmitsCommentWhenComplete. log.comment is prose for a reader, and a
// remark on every export saying nothing was dropped is noise in a field devtools
// surface as a note.
func TestWriteHAROmitsCommentWhenComplete(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHAR(&buf, incompleteSession(0)); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Log map[string]json.RawMessage `json:"log"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload.Log["comment"]; ok {
		t.Fatalf("a complete capture should carry no comment:\n%s", buf.String())
	}
}

// otlpResourceAttrs pulls the resource attributes out of an OTLP payload.
func otlpResourceAttrs(t *testing.T, data SessionExport) map[string]otlpAnyValue {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, data); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []otlpAttribute `json:"attributes"`
			} `json:"resource"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("invalid OTLP JSON: %v\n%s", err, buf.String())
	}
	if len(payload.ResourceSpans) != 1 {
		t.Fatalf("unexpected OTLP hierarchy: %s", buf.String())
	}
	attrs := map[string]otlpAnyValue{}
	for _, attr := range payload.ResourceSpans[0].Resource.Attributes {
		attrs[attr.Key] = attr.Value
	}
	return attrs
}

// TestWriteOTLPCarriesMissingFrames. A resource attribute rather than a span one,
// because incompleteness is a property of the capture, and resource attributes
// survive the spans of one session landing in several traces.
func TestWriteOTLPCarriesMissingFrames(t *testing.T) {
	attrs := otlpResourceAttrs(t, incompleteSession(3))
	value, ok := attrs["mcpsnoop.session.missing_frames"]
	if !ok {
		t.Fatalf("the dropped-frame count must reach the payload, got keys %v", attrs)
	}
	if value.IntValue == nil {
		t.Fatalf("a count belongs in intValue so a backend can filter on it, got %+v", value)
	}
	// proto3 JSON encodes int64 as a string; a bare number is what a collector
	// rejects after the payload has looked right all the way to the wire.
	if *value.IntValue != "3" {
		t.Fatalf("intValue = %q, want \"3\"", *value.IntValue)
	}
}

// TestWriteOTLPStatesZeroMissingFrames. Absence would be ambiguous between a
// capture that dropped nothing and one exported before the attribute existed,
// and the point of the count is that the span total can be trusted.
func TestWriteOTLPStatesZeroMissingFrames(t *testing.T) {
	attrs := otlpResourceAttrs(t, incompleteSession(0))
	value, ok := attrs["mcpsnoop.session.missing_frames"]
	if !ok {
		t.Fatal("a complete capture should claim zero rather than say nothing")
	}
	if value.IntValue == nil || *value.IntValue != "0" {
		t.Fatalf("intValue = %+v, want \"0\"", value)
	}
}

// TestJSONExportPreservesMarkup. The JSON export re-encodes every captured
// payload, so it rewrote markup the same way the trace file did.
func TestJSONExportPreservesMarkup(t *testing.T) {
	st := store.New()
	ingest(t, st,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"render"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"<b>hi</b> & bye"}]}}`)
	out, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []Format{FormatJSON, FormatHAR} {
		var buf bytes.Buffer
		if err := Write(&buf, out, Options{Format: format}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), `\u003c`) {
			t.Fatalf("the %s export must not rewrite markup:\n%s", format, buf.String())
		}
		if !strings.Contains(buf.String(), "<b>hi</b> & bye") {
			t.Fatalf("the %s export should carry the payload verbatim:\n%s", format, buf.String())
		}
	}
}

// TestParamHeadersSurviveTheJSONLRoundTrip pins the on-disk shape of the
// Mcp-Param capture and of the redaction flag that scopes its checks. open,
// export and check all rebuild a session by decoding the log back into
// proxy.Envelope, so a renamed or dropped json tag loses every captured header
// silently: the store sees none, reports no header verdict, and a check run over
// a bad capture comes back clean. Nothing else in the suite reads a header off
// disk, so a rename ships green without this.
func TestParamHeadersSurviveTheJSONLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	tool := `{"name":"route","inputSchema":{"type":"object","properties":` +
		`{"region":{"type":"string","x-mcp-header":"Region"}}}}`
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}`
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Transport: proxy.TransportHTTP, Direction: proxy.ClientToServer,
		MCPMethod: "tools/list", MCPProtocolVersion: "2026-07-28",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: t0.Add(time.Millisecond),
		Transport: proxy.TransportHTTP, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[` + tool + `]}}`),
	})
	// The header disagrees with the body, so a session reloaded from disk must
	// still carry both the captured header and the verdict drawn from it.
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 3, TS: t0.Add(2 * time.Millisecond),
		Transport: proxy.TransportHTTP, Direction: proxy.ClientToServer,
		MCPMethod: "tools/call", MCPName: "route", MCPProtocolVersion: "2026-07-28",
		MCPParamHeaders: []proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "eu-west1"}},
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` +
			`{"name":"route","arguments":{"region":"us-west1"},` + meta + `}}`),
	})
	// A frame mcpsnoop scrubbed. Its placeholder must still read as a placeholder
	// after a reload, or the redaction guard turns back into a false mismatch.
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 4, TS: t0.Add(3 * time.Millisecond),
		Transport: proxy.TransportHTTP, Direction: proxy.ClientToServer,
		MCPMethod: "tools/call", MCPName: "route", MCPProtocolVersion: "2026-07-28",
		MCPParamHeaders: []proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "[REDACTED]", Redacted: true}},
		Redacted:        true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":` +
			`{"name":"route","arguments":{"region":"us-west1"},` + meta + `}}`),
	})

	// The wire names are part of the log format, not implementation details, so
	// they are asserted off the raw line. A struct-to-struct round trip inside one
	// build would happily agree with itself after a rename.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if want := `"mcp_param_headers":[{"name":"Mcp-Param-Region","value":"eu-west1"}]`; !strings.Contains(lines[2], want) {
		t.Fatalf("on-disk envelope lost its parameter headers:\n%s", lines[2])
	}
	if !strings.Contains(lines[3], `"redacted":true`) {
		t.Fatalf("on-disk envelope lost its redaction flag:\n%s", lines[3])
	}
	// The per-header flag is what the store trusts on the header side, so it has
	// to survive the round trip as its own field rather than be re-guessed from
	// the value.
	if want := `{"name":"Mcp-Param-Region","value":"[REDACTED]","redacted":true}`; !strings.Contains(lines[3], want) {
		t.Fatalf("on-disk header lost its redaction flag:\n%s", lines[3])
	}

	st, sessionID, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	timeline := st.Timeline(sessionID)
	call := timeline[len(timeline)-2]
	if len(call.MCPParamHeaders) != 1 ||
		call.MCPParamHeaders[0].Name != "Mcp-Param-Region" ||
		call.MCPParamHeaders[0].Value != "eu-west1" {
		t.Fatalf("reloaded parameter headers = %+v", call.MCPParamHeaders)
	}
	if !call.RoutingMismatch || !strings.Contains(call.Warning, "Mcp-Param-Region") {
		t.Fatalf("reloaded call lost its header verdict: mismatch=%v warning=%q",
			call.RoutingMismatch, call.Warning)
	}
	scrubbed := timeline[len(timeline)-1]
	if scrubbed.RoutingMismatch || strings.Contains(scrubbed.Warning, "Mcp-Param") {
		t.Fatalf("a reloaded scrubbed frame was reported as a mismatch: warning=%q", scrubbed.Warning)
	}
}

// TestResolveSessionPathSkipsAnEmptyLog. The trace file is created before the
// wrapped server is started, so a launch that fails leaves a zero-byte log which
// is then the newest thing in the directory. A bare `check` or `export` meaning
// the last real capture resolved to it and answered "no envelopes found", which
// in CI reads as a failure caused by an unrelated run.
func TestResolveSessionPathSkipsAnEmptyLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", home)
	dir := paths.SessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real.jsonl")
	writeEnv(t, real, proxy.Envelope{
		SessionID: "real", ServerLabel: "demo", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`),
	})
	failed := filepath.Join(dir, "failedstart.jsonl")
	if err := os.WriteFile(failed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(failed, later, later); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSessionPath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("resolved %q, want the last real capture %q", got, real)
	}
}

// TestLoadFileLinesTracksTrueLineNumbers. Seq is scoped per session and skips
// frames dropped upstream, so a result pointed at Seq would send a reader to the
// wrong frame in exactly the captures worth pointing them at: this log has a
// gap in one session and a second session that restarts numbering at 1.
func TestLoadFileLinesTracksTrueLineNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "two.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	frames := []struct {
		session string
		seq     uint64
	}{
		{"alpha", 1},
		{"alpha", 2},
		{"alpha", 5}, // Seq 3 and 4 were dropped upstream.
		{"beta", 1},
		{"beta", 2},
	}
	for i, frame := range frames {
		writeEnv(t, path, proxy.Envelope{
			SessionID: frame.session, ServerLabel: "demo", Seq: frame.seq,
			TS: t0.Add(time.Duration(i) * time.Millisecond), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		})
	}

	st, first, lines, err := LoadFileLines(path)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || first != "alpha" {
		t.Fatalf("first session = %q, want alpha", first)
	}
	for i, frame := range frames {
		want := i + 1
		got, ok := lines.Line(frame.session, frame.seq)
		if !ok {
			t.Fatalf("no line recorded for %s seq %d", frame.session, frame.seq)
		}
		if got != want {
			t.Fatalf("%s seq %d is on line %d, want %d", frame.session, frame.seq, got, want)
		}
	}
	// The point of the index: after the gap, and in the second session, the line
	// and the Seq are different numbers.
	if line, _ := lines.Line("alpha", 5); line == 5 {
		t.Fatal("alpha seq 5 must not be reported on line 5")
	}
	if line, _ := lines.Line("beta", 1); line == 1 {
		t.Fatal("beta seq 1 must not be reported on line 1")
	}
	if _, ok := lines.Line("alpha", 3); ok {
		t.Fatal("a frame that never reached the log must have no line")
	}
	// A stream loaded without the index answers the same way as a missing frame,
	// so a caller cannot mistake "not tracked" for line zero.
	if _, ok := FrameLines(nil).Line("alpha", 1); ok {
		t.Fatal("an absent index must report no line")
	}
}

// TestLoadFileLinesCountsBlankLines. json.Decoder buffers past the value it
// returns, so a newline count kept alongside Decode would run ahead of it, and
// whitespace between envelopes would shift every line after it.
func TestLoadFileLinesCountsBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "padded.jsonl")
	envelope := func(seq uint64) string {
		b, err := json.Marshal(proxy.Envelope{
			SessionID: "s1", ServerLabel: "demo", Seq: seq,
			TS: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// Envelopes on lines 1, 2, 4 and 5, with line 3 blank.
	body := envelope(1) + "\n" + envelope(2) + "\n\n" + envelope(3) + "\n" + envelope(4) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, lines, err := LoadFileLines(path)
	if err != nil {
		t.Fatal(err)
	}
	for seq, want := range map[uint64]int{1: 1, 2: 2, 3: 4, 4: 5} {
		got, ok := lines.Line("s1", seq)
		if !ok || got != want {
			t.Fatalf("seq %d is on line %d (ok=%v), want %d", seq, got, ok, want)
		}
	}
}

// TestNewlineCounterDropsOffsetsItHasWalkedPast. json.Decoder reads ahead of the
// value it hands back, so the buffer is essentially never empty when lineAt runs
// and a "reuse it once drained" rule never fires. That kept one int64 per line of
// the capture alive for the whole load, on the format that never asked for a line
// index at all.
func TestNewlineCounterDropsOffsetsItHasWalkedPast(t *testing.T) {
	const lines = 20000
	body := strings.Repeat("x\n", lines)
	// A reader that hands over the whole stream at once, which is the worst case:
	// every newline is recorded before the first lineAt call walks past any.
	counter := &newlineCounter{r: strings.NewReader(body)}
	buf := make([]byte, len(body))
	if _, err := io.ReadFull(counter, buf); err != nil {
		t.Fatal(err)
	}

	for line := 1; line <= lines; line++ {
		// Two bytes per line, so a value on line n ends just past offset 2n-1, one
		// short of that line's own newline. Stopping there rather than walking past
		// it is the point: it is the state a forward pass is always in, and the state
		// the old "reuse it once drained" rule never recognised.
		if got := counter.lineAt(int64(2*line - 1)); got != line {
			t.Fatalf("lineAt(%d) = %d, want %d", 2*line-1, got, line)
		}
	}
	if got := len(counter.offsets); got > 1 {
		t.Fatalf("offsets = %d after walking %d lines, want only what is still ahead", got, lines)
	}
}

// TestExportSaysWhatItCaptured checks that an exported document identifies its
// own server. Two proxies pointed at two paths of one host share a default label,
// so without the endpoint the two exports are indistinguishable.
func TestExportSaysWhatItCaptured(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	meta, err := json.Marshal(proxy.SessionMeta{Target: "https://api.example.com/tenant-a/mcp?key=[stripped]"})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New()
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "api.example.com", Seq: 1, TS: base, Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: meta})
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "api.example.com", Seq: 2, TS: base.Add(time.Millisecond),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})

	data, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := data.Session.Endpoint, "https://api.example.com/tenant-a/mcp?key=[stripped]"; got != want {
		t.Fatalf("Session.Endpoint = %q, want %q", got, want)
	}
	if got := data.Session.Transport; got != proxy.TransportHTTP {
		t.Fatalf("Session.Transport = %q, want %q", got, proxy.TransportHTTP)
	}

	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"endpoint": "https://api.example.com/tenant-a/mcp?key=[stripped]"`) {
		t.Fatalf("the JSON export does not name the endpoint:\n%s", buf.String())
	}
}

// TestAStdioExportOmitsTheEndpointKey pins the omitempty rather than assuming it.
// A stdio export that carries an empty endpoint invites a reader to conclude the
// endpoint was unknown rather than inapplicable.
func TestAStdioExportOmitsTheEndpointKey(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "server.js"}})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New()
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: base, Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta})
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: base.Add(time.Millisecond),
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})

	data, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"endpoint"`) {
		t.Fatalf("a stdio export carries an endpoint key:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"transport": "stdio"`) {
		t.Fatalf("the export does not name the transport:\n%s", buf.String())
	}
}

// TestExportSplitsTheTwoKindsOfToolError keeps a consumer from having to guess
// which failure a number describes. Both new fields always add up to errors.
func TestExportSplitsTheTwoKindsOfToolError(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	st := store.New()
	seq := uint64(0)
	call := func(tool, response string) {
		seq++
		id := fmt.Sprintf("%d", seq)
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ClientToServer, Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":%q}}`, id, tool))})
		seq++
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ServerToClient, Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,%s}`, id, response))})
	}
	call("search", `"result":{"content":[],"isError":true}`)
	call("search", `"error":{"code":-32603,"message":"boom"}`)

	data, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Summary.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(data.Summary.Tools))
	}
	tool := data.Summary.Tools[0]
	if tool.Errors != 2 || tool.ProtocolErrors != 1 || tool.ToolErrors != 1 {
		t.Fatalf("errors = %d (%d protocol, %d tool), want 2 (1, 1)", tool.Errors, tool.ProtocolErrors, tool.ToolErrors)
	}

	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"protocol_errors": 1`, `"tool_errors": 1`, `"errors": 2`} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("the export is missing %s:\n%s", want, buf.String())
		}
	}
}

// TestExportReportsRetiredExchanges keeps the parking cap from being silent in a
// document somebody reads without the TUI. A capture where the cap fired may
// report one operation as two calls, and the reader needs that stated.
func TestExportReportsRetiredExchanges(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	st := store.New()
	var seq uint64
	// Enough abandoned exchanges that the store's parking cap has to drop some.
	for i := range 200 {
		seq++
		id := fmt.Sprintf("%d", i)
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ClientToServer, Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"ask"}}`, id))})
		seq++
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ServerToClient, Raw: json.RawMessage(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%q,"result":{"resultType":"input_required","requestState":"s-%d","inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}}`, id, i))})
	}

	data, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if data.Session.RetiredExchanges == 0 {
		t.Fatal("200 abandoned exchanges and the export reports none retired")
	}

	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"retired_exchanges"`) {
		t.Fatalf("the export does not name the retired exchanges:\n%s", buf.String()[:2000])
	}
}

// TestACleanExportOmitsRetiredExchanges pins the omitempty. A zero on every
// ordinary capture would train a reader to ignore the field.
func TestACleanExportOmitsRetiredExchanges(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	st := store.New()
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)})
	data, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"retired_exchanges"`) {
		t.Fatalf("a clean export carries the field:\n%s", buf.String())
	}
}

// writeElicitCapture writes a capture holding one form exchange, one url
// exchange and one nobody ever answered. The submitted value is deliberately
// distinctive so a test can prove it never reaches the ledger.
func writeElicitCapture(t *testing.T, path, submitted string) {
	t.Helper()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	seq := uint64(0)
	env := func(dir proxy.Direction, off time.Duration, raw string) proxy.Envelope {
		seq++
		return proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(off),
			Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)}
	}
	seq++
	envs := []proxy.Envelope{{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta}}
	envs = append(envs,
		env(proxy.ClientToServer, time.Second, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"create_account"}}`),
		env(proxy.ServerToClient, 2*time.Second, `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"profile":{"method":"elicitation/create","params":{"mode":"form","message":"contact information","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}}}}}`),
		env(proxy.ClientToServer, 11*time.Second, fmt.Sprintf(`{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"create_account","inputResponses":{"profile":{"action":"accept","content":{"name":%q}}},"requestState":"st-1"}}`, submitted)),
		env(proxy.ServerToClient, 12*time.Second, `{"jsonrpc":"2.0","id":"2","result":{"content":[]}}`),
		env(proxy.ClientToServer, 20*time.Second, `{"jsonrpc":"2.0","id":"3","method":"tools/call","params":{"name":"sync_calendar"}}`),
		env(proxy.ServerToClient, 21*time.Second, `{"jsonrpc":"2.0","id":"3","result":{"resultType":"input_required","requestState":"st-2","inputRequests":{"auth":{"method":"elicitation/create","params":{"mode":"url","url":"https://mcp.example.com/ui/set_api_key","message":"api key"}}}}}`),
		env(proxy.ClientToServer, 50*time.Second, `{"jsonrpc":"2.0","id":"4","method":"tools/call","params":{"name":"sync_calendar","inputResponses":{"auth":{"action":"accept"}},"requestState":"st-2"}}`),
		env(proxy.ServerToClient, 51*time.Second, `{"jsonrpc":"2.0","id":"4","result":{"content":[]}}`),
		env(proxy.ClientToServer, 60*time.Second, `{"jsonrpc":"2.0","id":"5","method":"tools/call","params":{"name":"abandoned"}}`),
		env(proxy.ServerToClient, 61*time.Second, `{"jsonrpc":"2.0","id":"5","result":{"resultType":"input_required","requestState":"st-3","inputRequests":{"q":{"method":"elicitation/create","params":{"message":"still there","requestedSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}}}}`),
	)
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestExportCarriesTheElicitationLedger covers the surfaces the ledger has to
// reach, and the boundary it must not cross.
func TestExportCarriesTheElicitationLedger(t *testing.T) {
	const submitted = "hunter2-do-not-repeat-me"
	path := filepath.Join(t.TempDir(), "elicit.jsonl")
	writeElicitCapture(t, path, submitted)
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Elicitations) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(data.Elicitations), data.Elicitations)
	}

	byKey := map[string]ElicitationExport{}
	for _, e := range data.Elicitations {
		byKey[e.Key] = e
	}
	form := byKey["profile"]
	if form.Mode != "form" || form.Action != "accept" || form.ElapsedMS == nil || *form.ElapsedMS != 9000 {
		t.Fatalf("form row = %+v", form)
	}
	if len(form.Fields) != 1 || form.Fields[0].Name != "name" || form.Fields[0].Type != "string" {
		t.Fatalf("form fields = %+v", form.Fields)
	}
	if form.CallIndex == nil || data.Calls[*form.CallIndex].ID != form.CallID {
		t.Fatalf("call_index %v does not point at call %q", form.CallIndex, form.CallID)
	}
	url := byKey["auth"]
	if url.URL != "https://mcp.example.com/ui/set_api_key" || url.Host != "mcp.example.com" {
		t.Fatalf("url row = %+v", url)
	}
	pending := byKey["q"]
	if !pending.Pending || pending.Action != "" || pending.AnsweredAt != nil || pending.ElapsedMS != nil {
		t.Fatalf("pending row = %+v", pending)
	}

	for _, format := range []Format{FormatJSON, FormatText, FormatHTML} {
		var buf bytes.Buffer
		if err := Write(&buf, data, Options{Format: format}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		out := buf.String()
		if !strings.Contains(out, "profile") || !strings.Contains(out, "mcp.example.com") {
			t.Fatalf("%s does not carry the ledger:\n%s", format, out[:min(len(out), 1500)])
		}
	}

	// The submitted value belongs to the capture and to nothing else. It rides the
	// raw frames legitimately, so the assertion is about the ledger itself.
	ledger, err := json.Marshal(data.Elicitations)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ledger), submitted) {
		t.Fatalf("a submitted value reached the ledger: %s", ledger)
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, submitted) && !strings.Contains(line, `"name"`) {
			t.Fatalf("the text export repeats a submitted value outside the raw frame: %q", line)
		}
	}
}

// TestExportWithoutElicitationOmitsTheLedger keeps an ordinary export byte
// identical to what it was.
func TestExportWithoutElicitationOmitsTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.jsonl")
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Elicitations) != 0 {
		t.Fatalf("elicitations = %+v, want none", data.Elicitations)
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "elicitations") {
		t.Fatalf("a session with none carries the key anyway:\n%s", buf.String())
	}
	if err := Write(&buf, data, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "elicitations:") {
		t.Fatal("the text export gained an empty section")
	}
}

// TestElicitationTextExportCannotForgeRows keeps a value the wire supplied from
// ending the line it is printed on. A key, a message and a field name are all
// written by the server, and a newline in one made the lines after it read as
// more ledger rows.
func TestElicitationTextExportCannotForgeRows(t *testing.T) {
	data := SessionExport{Elicitations: []ElicitationExport{{
		CallID: "1", Method: "tools/call", ToolName: "login",
		Key:     "creds\nelicitations:",
		Mode:    "form",
		Message: "m\n  FORGED [form] x: accept after 99s",
		Fields:  []ElicitFieldExport{{Name: "pass\nword", Type: "string"}},
		Action:  "decline",
	}}}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "\nFORGED") || strings.Contains(out, "\n  FORGED") {
		t.Fatalf("a forged row reached the output:\n%s", out)
	}
	if !strings.Contains(out, `\n`) {
		t.Fatalf("the control character was dropped rather than quoted, so it is not recoverable:\n%s", out)
	}
	// One question, one headline line, one message line, one detail line.
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "tools/call login") {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("the ledger printed %d headline rows for one question:\n%s", rows, out)
	}
}

// TestElicitationTextExportKeepsTheMessage covers the line the switch used to
// swallow. The message is what the server wrote for a human, so it is on every
// row rather than standing in for the fields when there are none.
func TestElicitationTextExportKeepsTheMessage(t *testing.T) {
	data := SessionExport{Elicitations: []ElicitationExport{{
		CallID: "1", Method: "tools/call", Key: "k", Mode: "form",
		Message: "Enter your admin password to continue",
		Fields:  []ElicitFieldExport{{Name: "password", Type: "string"}},
		Action:  "decline",
	}}}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Enter your admin password to continue") {
		t.Fatalf("the message a human was shown is missing:\n%s", out)
	}
	if !strings.Contains(out, "password string") {
		t.Fatalf("the fields are missing:\n%s", out)
	}
}

// TestElicitationCallIndexIsAbsentRatherThanNegative keeps a sentinel out of a
// document other tools read. jq and Python both resolve calls[-1] to the last
// call, so -1 would silently point a consumer at the wrong one.
func TestElicitationCallIndexIsAbsentRatherThanNegative(t *testing.T) {
	data := SessionExport{Elicitations: []ElicitationExport{{
		CallID: "1", Method: "tools/call", Key: "k", Mode: "form", Pending: true,
	}}}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "-1") {
		t.Fatalf("a negative index reached the document:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "call_index") {
		t.Fatalf("an unresolved index is present rather than absent:\n%s", buf.String())
	}
}

// writeMRTRCapture writes the capture from issue #201: a three hop book_flight
// chain where the server works 1.2s and the user takes 37s, plus one ordinary
// call for contrast.
func writeMRTRCapture(t *testing.T, path string) {
	t.Helper()
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight"}}`},
		{proxy.ServerToClient, 400, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"confirm":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 12400, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","requestState":"st-1","inputResponses":{"confirm":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 12700, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"input_required","requestState":"st-2","inputRequests":{"seat":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 37700, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"book_flight","requestState":"st-2","inputResponses":{"seat":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 38200, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`},
		{proxy.ClientToServer, 39000, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lookup_price"}}`},
		{proxy.ServerToClient, 39200, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`},
	}
	var buf bytes.Buffer
	for i, f := range frames {
		b, err := json.Marshal(proxy.Envelope{SessionID: "demo", ServerLabel: "booking", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f.raw)})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestExportCarriesTheInteractions covers the json shape and the two fields the
// per-call record gains, so a consumer stops reading the whole duration as
// server work.
func TestExportCarriesTheInteractions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mrtr.jsonl")
	writeMRTRCapture(t, path)
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Interactions) != 2 {
		t.Fatalf("interactions = %d, want one per operation: %+v", len(data.Interactions), data.Interactions)
	}
	var book InteractionExport
	for _, in := range data.Interactions {
		if in.ToolName == "book_flight" {
			book = in
		}
	}
	if book.RoundTrips != 3 || book.ServerTimeMS != 1200 || book.ClientTurnaroundMS != 37000 || book.DurationMS != 38200 {
		t.Fatalf("book_flight = %+v", book)
	}
	if book.ServerTimeMS+book.ClientTurnaroundMS != book.DurationMS {
		t.Fatalf("the two shares do not add up: %+v", book)
	}
	if len(book.Hops) != 3 || !book.HopsComplete {
		t.Fatalf("hops = %d complete=%v", len(book.Hops), book.HopsComplete)
	}
	if book.Hops[1].ClientTurnaroundMS != 12000 || book.Hops[1].ServerTimeMS != 300 {
		t.Fatalf("hop 2 = %+v", book.Hops[1])
	}
	if book.CallIndex == nil || data.Calls[*book.CallIndex].ID != book.CallID {
		t.Fatalf("call_index %v does not point at call %q", book.CallIndex, book.CallID)
	}

	for _, c := range data.Calls {
		if c.ToolName != "book_flight" {
			continue
		}
		if c.RoundTrips != 3 || c.ServerTimeMS == nil || *c.ServerTimeMS != 1200 {
			t.Fatalf("the call record does not carry its share: %+v", c)
		}
		if c.DurationMS == nil || *c.DurationMS != 38200 {
			t.Fatalf("the wall clock changed: %+v", c.DurationMS)
		}
	}

	for _, format := range []Format{FormatJSON, FormatText, FormatHTML} {
		var buf bytes.Buffer
		if err := Write(&buf, data, Options{Format: format}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !strings.Contains(buf.String(), "book_flight") {
			t.Fatalf("%s does not name the chained operation", format)
		}
	}
	var text bytes.Buffer
	if err := Write(&text, data, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "3 round trips, 38.2s total, 1.2s server, 37s client") {
		t.Fatalf("the text export does not split the total:\n%s", text.String())
	}
}

// TestHARSplitsWaitFromBlocked stops a viewer drawing a server that was busy for
// the whole interaction. HAR keeps wait for the server and blocked for time the
// entry spent waiting on something else, which here is a person answering.
func TestHARSplitsWaitFromBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mrtr.jsonl")
	writeMRTRCapture(t, path)
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteHAR(&buf, data); err != nil {
		t.Fatal(err)
	}
	var har struct {
		Log struct {
			Entries []struct {
				Time    float64 `json:"time"`
				Request struct {
					URL string `json:"url"`
				} `json:"request"`
				Timings struct {
					Blocked float64 `json:"blocked"`
					Send    float64 `json:"send"`
					Wait    float64 `json:"wait"`
					Receive float64 `json:"receive"`
				} `json:"timings"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(buf.Bytes(), &har); err != nil {
		t.Fatalf("the HAR does not parse: %v", err)
	}
	if len(har.Log.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(har.Log.Entries))
	}
	for _, e := range har.Log.Entries {
		sum := e.Timings.Send + e.Timings.Wait + e.Timings.Receive
		if e.Timings.Blocked > 0 {
			sum += e.Timings.Blocked
		}
		if sum != e.Time {
			t.Fatalf("%s: timings sum to %v against a time of %v", e.Request.URL, sum, e.Time)
		}
		if !strings.Contains(e.Request.URL, "book_flight") {
			continue
		}
		if e.Timings.Wait != 1200 {
			t.Fatalf("wait = %v, want the 1.2s the server held it", e.Timings.Wait)
		}
		if e.Timings.Blocked != 37000 {
			t.Fatalf("blocked = %v, want the 37s the client took", e.Timings.Blocked)
		}
	}
}

// TestAnOrdinaryExportGainsNoInteractionsSection keeps a capture with no chain
// unchanged, since every operation there is already one line.
func TestAnOrdinaryExportGainsNoInteractionsSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.jsonl")
	writeEnv(t, path, proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}`)})
	writeEnv(t, path, proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: time.Now().Add(time.Millisecond),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)})
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatText}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "interactions:") {
		t.Fatalf("a capture with no chain gained a section:\n%s", buf.String())
	}
	if len(data.Interactions) != 1 || data.Interactions[0].RoundTrips != 1 {
		t.Fatalf("an ordinary call is still a one hop interaction: %+v", data.Interactions)
	}
}

// TestHARNeverEmitsAnIllegalBlocked covers the entries HAR 1.2 constrains most.
// An operation with no exportable duration used to report a negative blocked,
// which is not the -1 sentinel and is the one negative the format forbids
// elsewhere.
func TestHARNeverEmitsAnIllegalBlocked(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full.jsonl")
	writeMRTRCapture(t, full)
	// The same capture cut while the user is still answering, which MRTR makes an
	// ordinary outcome rather than damage.
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(body), "\n", 5)
	abandoned := filepath.Join(dir, "abandoned.jsonl")
	if err := os.WriteFile(abandoned, []byte(strings.Join(lines[:4], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{full, abandoned} {
		st, id, err := LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := Build(st, id)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := WriteHAR(&buf, data); err != nil {
			t.Fatal(err)
		}
		var har struct {
			Log struct {
				Entries []struct {
					Time    float64 `json:"time"`
					Timings struct {
						Blocked float64 `json:"blocked"`
						Send    float64 `json:"send"`
						Wait    float64 `json:"wait"`
						Receive float64 `json:"receive"`
					} `json:"timings"`
				} `json:"entries"`
			} `json:"log"`
		}
		if err := json.Unmarshal(buf.Bytes(), &har); err != nil {
			t.Fatal(err)
		}
		for _, e := range har.Log.Entries {
			tm := e.Timings
			if tm.Blocked < 0 && tm.Blocked != -1 {
				t.Fatalf("%s: blocked = %v, and the only negative HAR allows is -1", filepath.Base(path), tm.Blocked)
			}
			for name, v := range map[string]float64{"send": tm.Send, "wait": tm.Wait, "receive": tm.Receive} {
				if v < 0 {
					t.Fatalf("%s: %s = %v, which HAR never allows to be negative", filepath.Base(path), name, v)
				}
			}
			if tm.Wait > e.Time {
				t.Fatalf("%s: wait %v is larger than the entry time %v", filepath.Base(path), tm.Wait, e.Time)
			}
		}
	}
}

// TestHopsCarryWhetherAnAnswerWasReadable keeps an unreadable hop apart from one
// that asked for nothing, which is what a released frame body produces.
func TestHopsCarryWhetherAnAnswerWasReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mrtr.jsonl")
	writeMRTRCapture(t, path)
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatJSON}); err != nil {
		t.Fatal(err)
	}
	// A batch read holds every body, so nothing is unknown and the key stays out.
	if strings.Contains(buf.String(), "asked_unknown") {
		t.Fatalf("a complete capture reports an unreadable hop:\n%s", buf.String()[:600])
	}
	for _, in := range data.Interactions {
		for _, h := range in.Hops {
			if h.AskedUnknown {
				t.Fatalf("hop %s is marked unreadable in a batch read", h.RequestID)
			}
		}
	}
}
