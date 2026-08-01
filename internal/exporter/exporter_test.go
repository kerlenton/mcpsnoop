package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
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
