package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestIngestRoutingHeaderMismatch(t *testing.T) {
	s := New()
	now := time.Now()

	// The Mcp-Method header says tools/list but the body is tools/call. A gateway
	// routes on the header and the server rejects the disagreement, so flag it.
	bad := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPMethod: "tools/list", MCPName: "search",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`),
	}
	ev := s.Ingest(bad)
	if ev.MCPMethod != "tools/list" || ev.MCPName != "search" {
		t.Fatalf("routing headers not captured: %+v", ev)
	}
	if !strings.Contains(ev.Warning, "Mcp-Method") || !strings.Contains(ev.Warning, "disagrees") {
		t.Fatalf("expected a mismatch warning, got %q", ev.Warning)
	}
	if !ev.RoutingMismatch {
		t.Fatalf("mismatch should be flagged structurally, not only in the warning text")
	}

	// A matching header carries no mismatch warning or flag.
	good := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPMethod: "tools/call",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`),
	}
	if g := s.Ingest(good); strings.Contains(g.Warning, "disagrees") || g.RoutingMismatch {
		t.Fatalf("matching header should not warn, got warning %q mismatch %v", g.Warning, g.RoutingMismatch)
	}
}

func TestIngestRoutingHeaderNameMismatch(t *testing.T) {
	s := New()
	now := time.Now()

	// The method agrees but Mcp-Name claims a safe tool while the body calls a
	// different one. This is the tool-shadowing case, so it must be flagged.
	shadow := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPMethod: "tools/call", MCPName: "safe_tool",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dangerous_tool"}}`),
	}
	ev := s.Ingest(shadow)
	if !strings.Contains(ev.Warning, "Mcp-Name") || !strings.Contains(ev.Warning, "disagrees") {
		t.Fatalf("expected an Mcp-Name mismatch warning, got %q", ev.Warning)
	}
	if !ev.RoutingMismatch {
		t.Fatalf("name mismatch should set the structured flag")
	}

	// Mcp-Name matching the body operation is clean, even for a resources/read
	// whose target lives in params.uri rather than params.name.
	ok := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPMethod: "resources/read", MCPName: "file:///a.txt",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"file:///a.txt"}}`),
	}
	if g := s.Ingest(ok); g.RoutingMismatch {
		t.Fatalf("matching uri should not flag a mismatch, got %q", g.Warning)
	}
}

func TestIngestRoutingHeadersInvalidOnBatch(t *testing.T) {
	s := New()
	now := time.Now()

	// A single routing header cannot address N methods, so a batch carrying one is
	// invalid by construction. The first element carries the header (per emitFrames)
	// and must earn one clear warning rather than a fabricated method disagreement.
	first := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPMethod: "tools/list", Batch: true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	}
	ev := s.Ingest(first)
	if !strings.Contains(ev.Warning, "batch") || !ev.RoutingMismatch {
		t.Fatalf("batch element with a routing header should warn about the batch, got %q flag %v", ev.Warning, ev.RoutingMismatch)
	}
	if strings.Contains(ev.Warning, "disagrees") {
		t.Fatalf("batch warning must not fabricate a per-element method disagreement: %q", ev.Warning)
	}

	// Later batch elements carry no header, so they stay clean.
	rest := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", Batch: true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`),
	}
	if g := s.Ingest(rest); g.RoutingMismatch || strings.Contains(g.Warning, "batch") {
		t.Fatalf("headerless batch element should stay clean, got %q flag %v", g.Warning, g.RoutingMismatch)
	}
}

func TestIngestProtocolVersionMismatch(t *testing.T) {
	s := New()
	now := time.Now()

	// The MCP-Protocol-Version header says 2026-07-28 but the version the request
	// repeats in its _meta says otherwise. A gateway routes on the header while the
	// server reads the body, so flag the disagreement.
	// The routing headers are filled in on every frame here so the version
	// disagreement stays the only variable: on 2026-07-28 they are REQUIRED, and a
	// frame omitting them would carry a second, unrelated warning.
	bad := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPProtocolVersion: "2026-07-28", MCPMethod: "tools/call", MCPName: "echo",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}}`),
	}
	ev := s.Ingest(bad)
	if !ev.RoutingMismatch {
		t.Fatal("a protocol-version disagreement should set the structured mismatch flag")
	}
	if !strings.Contains(ev.Warning, "MCP-Protocol-Version") || !strings.Contains(ev.Warning, "disagrees") {
		t.Fatalf("expected a protocol-version mismatch warning, got %q", ev.Warning)
	}

	// Header agreeing with the _meta version is clean.
	good := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPProtocolVersion: "2026-07-28", MCPMethod: "tools/call", MCPName: "echo",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`),
	}
	if g := s.Ingest(good); g.RoutingMismatch || strings.Contains(g.Warning, "disagrees") {
		t.Fatalf("matching version should not warn, got mismatch %v warning %q", g.RoutingMismatch, g.Warning)
	}

	// Header present but no _meta version means nothing to disagree with.
	noMeta := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: now, Direction: proxy.ClientToServer,
		Transport: "http", MCPProtocolVersion: "2026-07-28", MCPMethod: "tools/list",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`),
	}
	if g := s.Ingest(noMeta); g.RoutingMismatch {
		t.Fatal("a header with no _meta version to compare must not flag a mismatch")
	}
}

func req(seq uint64, ts time.Time, dir proxy.Direction, id, method, params string) proxy.Envelope {
	raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q`, id, method)
	if params != "" {
		raw += `,"params":` + params
	}
	raw += "}"
	return proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: ts, Direction: dir, Raw: json.RawMessage(raw)}
}

func resp(seq uint64, ts time.Time, dir proxy.Direction, id, body string) proxy.Envelope {
	raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,%s}`, id, body)
	return proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: ts, Direction: dir, Raw: json.RawMessage(raw)}
}

func TestCancellationSettlesPendingCall(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	s.Ingest(req(1, t0, proxy.ClientToServer, `"job-1"`, "tools/call", `{"name":"slow"}`))
	cancelledAt := t0.Add(250 * time.Millisecond)
	cancel := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: cancelledAt,
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"job-1","reason":"no longer needed"}}`),
	})

	if cancel.Call == nil || cancel.Call.State != Cancelled {
		t.Fatalf("cancelled call = %+v", cancel.Call)
	}
	if cancel.Call.CancelledAt != cancelledAt || cancel.Call.CancelReason != "no longer needed" {
		t.Fatalf("cancellation metadata = %s %q", cancel.Call.CancelledAt, cancel.Call.CancelReason)
	}
	if cancel.Call.LateResult || cancel.Call.Duration() != 0 {
		t.Fatalf("unanswered cancellation has late=%v duration=%s", cancel.Call.LateResult, cancel.Call.Duration())
	}
	if cancel.Warning != "" {
		t.Fatalf("cancellation warning = %q, want none", cancel.Warning)
	}
	header := s.Sessions()[0]
	if header.Pending != 0 || header.Errors != 0 || header.LateResults != 0 {
		t.Fatalf("session counts = pending %d errors %d late %d", header.Pending, header.Errors, header.LateResults)
	}
	if got := s.Timeline("s1")[0].Call.State; got != Cancelled {
		t.Fatalf("request state = %s, want cancelled", got)
	}
}

func TestLateResponseAfterCancellationRemainsAnObservation(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	s.Ingest(req(1, t0, proxy.ClientToServer, "7", "tools/call", `{"name":"slow"}`))
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(250 * time.Millisecond),
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`),
	})
	late := s.Ingest(resp(3, t0.Add(time.Second), proxy.ServerToClient, "7", `"result":{"content":[]}`))

	if late.Call == nil || late.Call.State != Cancelled || !late.Call.LateResult {
		t.Fatalf("late response call = %+v", late.Call)
	}
	if late.Observation != "result arrived 750ms after cancellation" || late.Warning != "" {
		t.Fatalf("late response observation=%q warning=%q", late.Observation, late.Warning)
	}
	if string(late.Call.Result) != `{"content":[]}` || late.Call.Duration() != time.Second {
		t.Fatalf("late result=%s duration=%s", late.Call.Result, late.Call.Duration())
	}
	header := s.Sessions()[0]
	if header.Pending != 0 || header.Errors != 0 || header.LateResults != 1 {
		t.Fatalf("session counts = pending %d errors %d late %d", header.Pending, header.Errors, header.LateResults)
	}
	summary, ok := s.ToolSummary("s1")
	if !ok || len(summary.Tools) != 1 || summary.Tools[0].P50 != time.Second {
		t.Fatalf("tool summary = %+v, ok=%v", summary, ok)
	}

	duplicate := s.Ingest(resp(4, t0.Add(2*time.Second), proxy.ServerToClient, "7", `"result":{"content":["again"]}`))
	if !strings.Contains(duplicate.Warning, "duplicate response") {
		t.Fatalf("duplicate warning = %q", duplicate.Warning)
	}
	if got := s.Sessions()[0].LateResults; got != 1 {
		t.Fatalf("late results = %d, want 1", got)
	}
}

func TestReusedRequestIdKeepsPendingCounterAndTimelineInSync(t *testing.T) {
	s := New()
	t0 := time.Now()
	// Two requests reuse id 1 while the first is still in flight (no response).
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"a"}`))
	s.Ingest(req(2, t0.Add(time.Millisecond), proxy.ClientToServer, "1", "tools/call", `{"name":"b"}`))

	header := s.Sessions()[0]
	events := s.Timeline("s1")
	timelinePending := 0
	for _, ev := range events {
		if ev.Kind == EventRequest && ev.Call != nil && ev.Call.State == Pending {
			timelinePending++
		}
	}
	// The counter and the timeline must tell the same story.
	if header.Pending != timelinePending {
		t.Fatalf("pending disagree: header %d, timeline %d", header.Pending, timelinePending)
	}
	if header.Pending != 1 {
		t.Fatalf("header pending = %d, want 1", header.Pending)
	}
	// The superseded first request is no longer pending, and the reuse is explained
	// on the second request.
	if events[0].Call == nil || events[0].Call.State != Superseded {
		t.Fatalf("first call should be superseded, got %+v", events[0].Call)
	}
	if !strings.Contains(events[1].Warning, "reuses an id already in flight") {
		t.Fatalf("second request should warn about id reuse, got %q", events[1].Warning)
	}
}

func TestNullRequestIDsRemainDistinct(t *testing.T) {
	s := New()
	t0 := time.Now()
	first := s.Ingest(req(1, t0, proxy.ClientToServer, "null", "tools/call", `{"name":"read_file"}`))
	second := s.Ingest(req(2, t0.Add(time.Millisecond), proxy.ClientToServer, "null", "tools/call", `{"name":"write_file"}`))

	want := "request id is null; MCP requires a string or integer id"
	if first.Warning != want || second.Warning != want {
		t.Fatalf("null id warnings = %q and %q, want %q", first.Warning, second.Warning, want)
	}
	if strings.Contains(second.Warning, "reuses an id") {
		t.Fatalf("null ids must not be treated as reusable identifiers: %q", second.Warning)
	}
	response := s.Ingest(resp(3, t0.Add(2*time.Millisecond), proxy.ServerToClient, "null", `"error":{"code":-32700,"message":"parse error"}`))
	if !strings.Contains(response.Warning, "no matching request") {
		t.Fatalf("null response warning = %q, want an unmatched response", response.Warning)
	}
	if got := s.Sessions()[0].Pending; got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
	calls := s.Calls("s1")
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].State != Pending || calls[1].State != Pending {
		t.Fatalf("null id calls should both remain pending: %+v", calls)
	}
	if calls[0].ToolName != "read_file" || calls[1].ToolName != "write_file" {
		t.Fatalf("tool names = %q and %q", calls[0].ToolName, calls[1].ToolName)
	}
}

func TestInvalidRequestIDTypesWarn(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"string", `"abc"`, ""},
		{"integer", "42", ""},
		{"negative integer", "-7", ""},
		// JSON has several spellings for one integer, and JSON Schema's own
		// "integer" matches any number with a zero fractional part. A client whose
		// serializer round-trips an id through a float is conforming, so judging the
		// spelling rather than the value would warn on correct traffic.
		{"integer written as a float", "1.0", ""},
		{"integer written with an exponent", "1e3", ""},
		{"integer past the safe range", "12345678901234567890", ""},
		{"null", "null", "request id is null; MCP requires a string or integer id"},
		{"fraction", "1.5", "request id 1.5 is not an integer; MCP requires a string or integer id"},
		{"boolean", "true", "request id has type boolean; MCP requires a string or integer id"},
		{"array", "[]", "request id has type array; MCP requires a string or integer id"},
		{"object", "{}", "request id has type object; MCP requires a string or integer id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := New()
			ev := s.Ingest(req(1, time.Now(), proxy.ClientToServer, test.id, "ping", ""))
			if ev.Warning != test.want {
				t.Fatalf("warning = %q, want %q", ev.Warning, test.want)
			}
		})
	}
}

func TestNullErrorResponseDoesNotUseRequestIDWarning(t *testing.T) {
	s := New()
	ev := s.Ingest(resp(1, time.Now(), proxy.ServerToClient, "null", `"error":{"code":-32700,"message":"parse error"}`))
	if strings.Contains(ev.Warning, "request id") {
		t.Fatalf("null error response got request warning %q", ev.Warning)
	}
}

func TestSessionReportsSeqGapAsMissingFrames(t *testing.T) {
	now := time.Now()

	gap := New()
	gap.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", ""))
	gap.Ingest(req(2, now, proxy.ClientToServer, "2", "tools/list", ""))
	// A jump from 2 to 5 means seq 3 and 4 were dropped upstream.
	gap.Ingest(req(5, now, proxy.ClientToServer, "3", "tools/list", ""))
	if h := gap.Sessions()[0]; h.MissingFrames != 2 {
		t.Fatalf("missing frames = %d, want 2 for a seq gap of two", h.MissingFrames)
	}

	contiguous := New()
	for seq := uint64(1); seq <= 4; seq++ {
		contiguous.Ingest(req(seq, now, proxy.ClientToServer, fmt.Sprintf("%d", seq), "tools/list", ""))
	}
	if h := contiguous.Sessions()[0]; h.MissingFrames != 0 {
		t.Fatalf("a contiguous session should report zero missing, got %d", h.MissingFrames)
	}
}

func TestIngestTruncatedFrameIsMarkedNotInvalid(t *testing.T) {
	s := New()
	// A body whose observed copy was cut at the cap: the partial bytes do not parse,
	// but it must be marked as truncated, not flagged as an invalid (corrupt) frame.
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(), Direction: proxy.ClientToServer,
		Transport: "http", Truncated: true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"blob":"AAAA`),
	})
	if ev.Kind == EventInvalid {
		t.Fatal("a truncated observation must not be flagged as an invalid frame")
	}
	if !ev.Truncated {
		t.Fatal("a truncated frame should carry the structured truncated flag")
	}
	if ev.Warning != "" {
		// Routing it through Warning would fail a default `check --fail-on warn`.
		t.Fatalf("truncation must not go through the warning field, got %q", ev.Warning)
	}
}

func TestActivityBuckets(t *testing.T) {
	st := New()
	now := time.Now()
	// Frames arrive oldest first, as a real session does. One is well outside the
	// two minute window and must be ignored, one is about a minute old, then two
	// land in the most recent bucket.
	st.Ingest(req(1, now.Add(-10*time.Minute), proxy.ClientToServer, "1", "tools/list", ""))
	st.Ingest(req(2, now.Add(-60*time.Second), proxy.ClientToServer, "2", "tools/list", ""))
	st.Ingest(req(3, now, proxy.ClientToServer, "3", "tools/list", ""))
	st.Ingest(req(4, now, proxy.ClientToServer, "4", "tools/list", ""))

	buckets := st.Activity("s1", 8, 2*time.Minute)
	if len(buckets) != 8 {
		t.Fatalf("want 8 buckets, got %d", len(buckets))
	}
	if buckets[7] != 2 {
		t.Fatalf("most recent bucket = %d, want 2", buckets[7])
	}
	total := 0
	for _, v := range buckets {
		total += v
	}
	if total != 3 {
		t.Fatalf("total in window = %d, want 3 (the 10 minute old frame is excluded)", total)
	}

	if got := st.Activity("missing", 8, 2*time.Minute); len(got) != 8 {
		t.Fatalf("unknown session should still return 8 empty buckets, got %d", len(got))
	}
}

func TestTaskBackedToolCallStaysPendingUntilTerminalState(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
	handle := s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
		`"result":{"resultType":"task","taskId":"task-7","status":"working","ttl":60000,"pollIntervalMs":100}`))
	if handle.Call == nil || handle.Call.State != Pending || handle.Call.TaskID != "task-7" || handle.Call.TaskStatus != "working" {
		t.Fatalf("task handle completed the call: %+v", handle.Call)
	}
	if got := s.Sessions()[0].Pending; got != 1 {
		t.Fatalf("pending after task handle = %d, want 1", got)
	}

	poll := s.Ingest(req(3, t0.Add(time.Second), proxy.ClientToServer, "2", "tasks/get", `{"taskId":"task-7"}`))
	if poll.TaskCall == nil || poll.TaskCall.ID != "1" || poll.TaskID != "task-7" {
		t.Fatalf("tasks/get is not linked to the originating call: %+v", poll)
	}
	terminal := s.Ingest(resp(4, t0.Add(10*time.Second), proxy.ServerToClient, "2",
		`"result":{"taskId":"task-7","status":"completed","result":{"content":[{"type":"text","text":"done"}]}}`))
	if terminal.TaskCall == nil || terminal.TaskCall.State != Completed {
		t.Fatalf("terminal task state did not complete the originating call: %+v", terminal.TaskCall)
	}
	if got := terminal.TaskCall.Duration(); got != 10*time.Second {
		t.Fatalf("task-backed duration = %s, want 10s", got)
	}
	if got := s.Sessions()[0].Pending; got != 0 {
		t.Fatalf("pending after terminal state = %d, want 0", got)
	}
}

// The terminal result of a task is whatever the call would have returned
// synchronously, so a tool that failed inside a task must read as failed rather
// than as an empty success.
func TestTaskCompletedWithToolErrorIsAFailure(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"tool-err-1","status":"working"}`))
	s.Ingest(req(3, t0.Add(time.Second), proxy.ClientToServer, "2", "tasks/get", `{"taskId":"tool-err-1"}`))
	ev := s.Ingest(resp(4, t0.Add(2*time.Second), proxy.ServerToClient, "2",
		`"result":{"taskId":"tool-err-1","status":"completed","result":{"content":[{"type":"text","text":"nope"}],"isError":true}}`))

	if ev.TaskCall == nil {
		t.Fatal("the terminal frame should link its originating call")
	}
	if !ev.TaskCall.ToolErr || !ev.TaskCall.Failed() {
		t.Fatalf("a tool error inside a task must not read as a success: %+v", ev.TaskCall)
	}
	if got := s.Sessions()[0].Errors; got != 1 {
		t.Fatalf("session errors = %d, want 1", got)
	}
}

// A terminal failure carrying no error object still has to read as a failure,
// otherwise the state says one thing and every consumer of Failed() says another.
func TestTaskFailedWithoutErrorObjectStillReadsAsFailed(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"bare-1","status":"working"}`))
	ev := s.Ingest(proxy.Envelope{SessionID: "s1", Seq: 3, TS: t0.Add(time.Second), Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"bare-1","status":"failed"}}`)})

	if ev.TaskCall == nil || ev.TaskCall.State != Failed {
		t.Fatalf("task state = %+v, want Failed", ev.TaskCall)
	}
	if !ev.TaskCall.Failed() {
		t.Fatal("Failed() must agree with a Failed state")
	}
}

// Cancelling is terminal and delivers no result, but the user stopping work is
// neither a protocol nor a tool error, so it must not fail a default check run.
func TestCancelledTaskIsTerminalButNotASessionError(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"cancel-2","status":"working"}`))
	s.Ingest(req(3, t0.Add(time.Second), proxy.ClientToServer, "2", "tasks/cancel", `{"taskId":"cancel-2"}`))
	s.Ingest(resp(4, t0.Add(2*time.Second), proxy.ServerToClient, "2", `"result":{}`))
	ev := s.Ingest(proxy.Envelope{SessionID: "s1", Seq: 5, TS: t0.Add(3 * time.Second), Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"cancel-2","status":"cancelled"}}`)})

	if ev.TaskCall == nil || ev.TaskCall.TaskStatus != "cancelled" {
		t.Fatalf("task status = %+v, want cancelled", ev.TaskCall)
	}
	header := s.Sessions()[0]
	if header.Pending != 0 {
		t.Fatalf("a cancelled task must settle its call, pending = %d", header.Pending)
	}
	if header.Errors != 0 {
		t.Fatalf("a deliberate cancel is not a session error, errors = %d", header.Errors)
	}
}

func TestTaskFailureCancelInputAndOrphanHandling(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	t.Run("failed task", func(t *testing.T) {
		s := New()
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
		s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"failed-1","status":"working"}`))
		s.Ingest(req(3, t0.Add(time.Second), proxy.ClientToServer, "2", "tasks/get", `{"taskId":"failed-1"}`))
		ev := s.Ingest(resp(4, t0.Add(2*time.Second), proxy.ServerToClient, "2", `"result":{"taskId":"failed-1","status":"failed","error":{"code":-32001,"message":"boom"}}`))
		if ev.TaskCall == nil || ev.TaskCall.State != Failed || ev.TaskCall.Err == nil || ev.TaskCall.Err.Message != "boom" {
			t.Fatalf("failed task outcome = %+v", ev.TaskCall)
		}
	})

	t.Run("cancel is cooperative", func(t *testing.T) {
		s := New()
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
		s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"cancel-1","status":"working"}`))
		s.Ingest(req(3, t0.Add(time.Second), proxy.ClientToServer, "2", "tasks/cancel", `{"taskId":"cancel-1"}`))
		s.Ingest(resp(4, t0.Add(2*time.Second), proxy.ServerToClient, "2", `"result":{}`))
		s.Ingest(req(5, t0.Add(3*time.Second), proxy.ClientToServer, "3", "tasks/get", `{"taskId":"cancel-1"}`))
		ev := s.Ingest(resp(6, t0.Add(4*time.Second), proxy.ServerToClient, "3", `"result":{"taskId":"cancel-1","status":"completed","result":{"content":[]}}`))
		if ev.TaskCall == nil || ev.TaskCall.State != Completed || ev.TaskCall.TaskStatus != "completed" {
			t.Fatalf("cooperative cancel hid actual outcome: %+v", ev.TaskCall)
		}
	})

	t.Run("input required notification and orphan", func(t *testing.T) {
		s := New()
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
		s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"input-1","status":"working"}`))
		ev := s.Ingest(proxy.Envelope{SessionID: "s1", Seq: 3, TS: t0.Add(time.Second), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"input-1","status":"input_required","inputRequests":{"answer":{}}}}`)})
		if ev.TaskCall == nil || ev.TaskCall.TaskStatus != "input_required" || ev.TaskCall.State != Pending {
			t.Fatalf("input-required notification = %+v", ev.TaskCall)
		}
		update := s.Ingest(req(4, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tasks/update", `{"taskId":"input-1","values":{"answer":"yes"}}`))
		if update.TaskCall == nil || update.TaskCall.ID != "1" {
			t.Fatalf("tasks/update is not linked: %+v", update)
		}
		ack := s.Ingest(resp(5, t0.Add(3*time.Second), proxy.ServerToClient, "2", `"result":{}`))
		if ack.TaskCall == nil || ack.TaskCall.TaskStatus != "input_required" {
			t.Fatalf("tasks/update acknowledgement lost its link: %+v", ack)
		}
		working := s.Ingest(proxy.Envelope{SessionID: "s1", Seq: 6, TS: t0.Add(4 * time.Second), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"input-1","status":"working"}}`)})
		if working.TaskCall == nil || working.TaskCall.TaskStatus != "working" {
			t.Fatalf("task did not resume after input: %+v", working.TaskCall)
		}
		orphan := s.Ingest(req(7, t0.Add(5*time.Second), proxy.ClientToServer, "9", "tasks/get", `{"taskId":"missing"}`))
		if orphan.TaskCall != nil || orphan.TaskID != "missing" {
			t.Fatalf("orphan task invented a parent: %+v", orphan)
		}
	})
}

func TestCorrelationAndTiming(t *testing.T) {
	s := New()
	t0 := time.Now()

	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`))
	// Response 200ms later, in the opposite direction.
	ev := s.Ingest(resp(2, t0.Add(200*time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))

	if ev.Kind != EventResponse || ev.Call == nil {
		t.Fatalf("expected matched response event, got %+v", ev)
	}
	c := ev.Call
	if c.State != Completed {
		t.Fatalf("state = %v, want Completed", c.State)
	}
	if !c.IsTool || c.ToolName != "echo" {
		t.Fatalf("tool extraction failed: isTool=%v name=%q", c.IsTool, c.ToolName)
	}
	if got := c.Duration(); got != 200*time.Millisecond {
		t.Fatalf("duration = %v, want 200ms", got)
	}

	calls := s.Calls("s1")
	if len(calls) != 1 || calls[0].State != Completed {
		t.Fatalf("Calls() = %+v", calls)
	}
}

// TestDuplicateResponseDoesNotDoubleCountPending guards against a second
// response for an already-answered id decrementing the pending counter twice.
func TestDuplicateResponseDoesNotDoubleCountPending(t *testing.T) {
	s := New()
	t0 := time.Now()

	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo"}`))
	// First response completes the call, pending returns to zero.
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	// A duplicate or late second response for the same id must not recount.
	ev := s.Ingest(resp(3, t0.Add(2*time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))

	if h := s.Sessions()[0]; h.Pending != 0 {
		t.Fatalf("pending = %d, want 0 (duplicate response must not double-decrement)", h.Pending)
	}
	if ev.Call == nil || ev.Call.State != Completed {
		t.Fatalf("duplicate response should still link to the completed call, got %+v", ev.Call)
	}
	if ev.Warning != "duplicate response for the same id" {
		t.Fatalf("duplicate response should be flagged, warning = %q", ev.Warning)
	}
}

// TestDuplicateErrorResponseDoesNotDoubleCountErrors guards the error counter
// against a re-sent error response for the same id.
func TestDuplicateErrorResponseDoesNotDoubleCountErrors(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "7", "tools/call", `{"name":"nope"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "7", `"error":{"code":-32601,"message":"no"}`))
	s.Ingest(resp(3, t0.Add(2*time.Millisecond), proxy.ServerToClient, "7", `"error":{"code":-32601,"message":"no"}`))
	if h := s.Sessions()[0]; h.Errors != 1 {
		t.Fatalf("errors = %d, want 1 (duplicate error must not double-count)", h.Errors)
	}
}

// TestReusedInFlightRequestIDIsFlagged checks that a request reusing an id whose
// earlier request is still pending is flagged, without leaking the pending count.
func TestReusedInFlightRequestIDIsFlagged(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"a"}`))
	ev := s.Ingest(req(2, t0.Add(time.Millisecond), proxy.ClientToServer, "1", "tools/call", `{"name":"b"}`))
	if ev.Warning != "request reuses an id already in flight" {
		t.Fatalf("reused in-flight id should be flagged, warning = %q", ev.Warning)
	}
	if h := s.Sessions()[0]; h.Pending != 1 {
		t.Fatalf("pending = %d, want 1 (reused id must not leak pending)", h.Pending)
	}
	s.Ingest(resp(3, t0.Add(2*time.Millisecond), proxy.ServerToClient, "1", `"result":{}`))
	if h := s.Sessions()[0]; h.Pending != 0 {
		t.Fatalf("pending = %d, want 0 after the response clears it", h.Pending)
	}
}

func TestErrorResponse(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "7", "tools/call", `{"name":"nope"}`))
	ev := s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "7", `"error":{"code":-32601,"message":"unknown tool"}`))
	if ev.Call == nil || ev.Call.State != Failed || ev.Call.Err == nil {
		t.Fatalf("expected failed call with error, got %+v", ev.Call)
	}
	if h := s.Sessions()[0]; h.Errors != 1 {
		t.Fatalf("session errors = %d, want 1", h.Errors)
	}
}

func TestToolLevelError(t *testing.T) {
	// MCP tool failures arrive as a 200-OK response with result.isError=true,
	// NOT as a JSON-RPC error. They must still count/flag as errors.
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"add"}`))
	ev := s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
		`"result":{"content":[{"type":"text","text":"Tool add not found"}],"isError":true}`))
	if ev.Call == nil || ev.Call.State != Failed || !ev.Call.ToolErr || !ev.Call.Failed() {
		t.Fatalf("tool-level error not detected: %+v", ev.Call)
	}
	if ev.Call.Err != nil {
		t.Fatalf("tool error must not be a JSON-RPC error: %+v", ev.Call.Err)
	}
	if h := s.Sessions()[0]; h.Errors != 1 {
		t.Fatalf("session errors = %d, want 1", h.Errors)
	}
}

func TestToolSummarySkipsSupersededCallLatency(t *testing.T) {
	t0 := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	withSuperseded := New()
	// One completed echo call at 25ms.
	withSuperseded.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo"}`))
	withSuperseded.Ingest(resp(2, t0.Add(25*time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	// id 2 is reused a full second later, so the first of the pair is superseded.
	withSuperseded.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/call", `{"name":"echo"}`))
	withSuperseded.Ingest(req(4, t0.Add(time.Second), proxy.ClientToServer, "2", "tools/call", `{"name":"echo"}`))

	sum, ok := withSuperseded.ToolSummary("s1")
	if !ok || len(sum.Tools) != 1 {
		t.Fatalf("ToolSummary = %+v ok %v", sum, ok)
	}
	echo := sum.Tools[0]
	// The superseded call still counts as a call (like a pending one) but feeds no
	// duration and no error, so the percentiles come only from the completed call.
	if echo.Calls != 3 || echo.Pending != 1 || echo.Errors != 0 {
		t.Fatalf("echo calls/pending/errors = %d/%d/%d, want 3/1/0", echo.Calls, echo.Pending, echo.Errors)
	}
	if echo.P50 != 25*time.Millisecond || echo.P95 != 25*time.Millisecond {
		t.Fatalf("echo percentiles = %s/%s, want 25ms from the completed call only", echo.P50, echo.P95)
	}
	for _, sc := range sum.Slowest {
		if sc.Duration >= time.Second {
			t.Fatalf("a fabricated superseded duration leaked into slowest: %+v", sc)
		}
	}

	// The percentiles match a run without the superseded call at all.
	control := New()
	control.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo"}`))
	control.Ingest(resp(2, t0.Add(25*time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	cs, _ := control.ToolSummary("s1")
	if cs.Tools[0].P50 != echo.P50 || cs.Tools[0].P95 != echo.P95 || cs.Tools[0].P99 != echo.P99 {
		t.Fatalf("percentiles differ from a run without the superseded call: %+v vs %+v", echo, cs.Tools[0])
	}
}

func TestToolSummaryAggregatesLatencyErrorsAndPendingCalls(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	durations := []time.Duration{10, 20, 30, 40, 100}
	for i, milliseconds := range durations {
		id := fmt.Sprintf("%d", i+1)
		s.Ingest(req(uint64(i*2+1), t0, proxy.ClientToServer, id, "tools/call", `{"name":"echo"}`))
		body := `"result":{"content":[]}`
		if i == len(durations)-1 {
			body = `"error":{"code":-32000,"message":"timeout"}`
		}
		s.Ingest(resp(uint64(i*2+2), t0.Add(milliseconds*time.Millisecond), proxy.ServerToClient, id, body))
	}
	s.Ingest(req(20, t0, proxy.ClientToServer, "6", "tools/call", `{"name":"search"}`))
	s.Ingest(req(21, t0, proxy.ClientToServer, "7", "tools/list", ""))

	summary, ok := s.ToolSummary("s1")
	if !ok {
		t.Fatal("ToolSummary should find the session")
	}
	if len(summary.Tools) != 2 {
		t.Fatalf("tools = %d, want 2: %+v", len(summary.Tools), summary.Tools)
	}
	echo := summary.Tools[0]
	if echo.Name != "echo" || echo.Calls != 5 || echo.Errors != 1 || echo.Pending != 0 {
		t.Fatalf("echo summary = %+v", echo)
	}
	if echo.P50 != 30*time.Millisecond || echo.P95 != 100*time.Millisecond || echo.P99 != 100*time.Millisecond {
		t.Fatalf("echo percentiles = %s/%s/%s, want 30ms/100ms/100ms", echo.P50, echo.P95, echo.P99)
	}
	search := summary.Tools[1]
	if search.Name != "search" || search.Calls != 1 || search.Pending != 1 || search.P50 != 0 {
		t.Fatalf("search summary = %+v", search)
	}
	if len(summary.Slowest) != 5 || summary.Slowest[0].ToolName != "echo" || summary.Slowest[0].Duration != 100*time.Millisecond || !summary.Slowest[0].Failed {
		t.Fatalf("slowest calls = %+v", summary.Slowest)
	}
	if _, ok := s.ToolSummary("missing"); ok {
		t.Fatal("ToolSummary should report an unknown session")
	}
}

// The summary must count what the stream and the CI gate count. A task that ends
// failed with no error object, and a tool error inside a completed task, both go
// through the error axis, so ToolSummary counts them exactly as the session error
// counter does. Before the axis was stored, the failed-no-error case counted in the
// stream but not here, so the summary showed zero errors for a call painted red.
func TestToolSummaryCountsFailedAndToolErrorTasks(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	s := New()

	// "slow" ends failed with no error object and no isError.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"bare-1","status":"working"}`))
	s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0.Add(time.Second), Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"bare-1","status":"failed"}}`)})

	// "grep" completes as a task whose result carries a tool-level error.
	s.Ingest(req(4, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call", `{"name":"grep"}`))
	s.Ingest(resp(5, t0.Add(2*time.Second+time.Millisecond), proxy.ServerToClient, "2", `"result":{"resultType":"task","taskId":"toolerr-1","status":"working"}`))
	s.Ingest(req(6, t0.Add(3*time.Second), proxy.ClientToServer, "3", "tasks/get", `{"taskId":"toolerr-1"}`))
	s.Ingest(resp(7, t0.Add(4*time.Second), proxy.ServerToClient, "3", `"result":{"taskId":"toolerr-1","status":"completed","result":{"content":[{"type":"text","text":"boom"}],"isError":true}}`))

	sum, ok := s.ToolSummary("s1")
	if !ok {
		t.Fatal("ToolSummary should find the session")
	}
	total := 0
	for _, tool := range sum.Tools {
		total += tool.Errors
	}
	if total != 2 {
		t.Fatalf("ToolSummary errors = %d across %+v, want 2 (failed task + tool error task)", total, sum.Tools)
	}
	if got := s.Sessions()[0].Errors; got != total {
		t.Fatalf("ToolSummary errors %d disagree with the session error counter %d", total, got)
	}
	for _, sc := range sum.Slowest {
		if !sc.Failed {
			t.Fatalf("both task calls are on the error axis, so each slowest entry should read failed: %+v", sc)
		}
	}
}

// A cancelled task delivered no result, so its call is Failed(), but the user
// stopped the work on purpose. That is not on the error axis, so ToolSummary and
// the session error counter must both leave it uncounted and unflagged.
func TestToolSummaryDoesNotCountCancelledTask(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"resultType":"task","taskId":"cancel-9","status":"working"}`))
	s.Ingest(req(3, t0.Add(time.Second), proxy.ClientToServer, "2", "tasks/cancel", `{"taskId":"cancel-9"}`))
	s.Ingest(resp(4, t0.Add(2*time.Second), proxy.ServerToClient, "2", `"result":{}`))
	s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 5, TS: t0.Add(3 * time.Second), Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"cancel-9","status":"cancelled"}}`)})

	sum, ok := s.ToolSummary("s1")
	if !ok || len(sum.Tools) != 1 {
		t.Fatalf("ToolSummary = %+v ok %v, want one tool", sum, ok)
	}
	if sum.Tools[0].Errors != 0 {
		t.Fatalf("a cancelled task is not an error, ToolSummary errors = %d", sum.Tools[0].Errors)
	}
	for _, sc := range sum.Slowest {
		if sc.Failed {
			t.Fatalf("a cancelled call must not read as failed in the summary: %+v", sc)
		}
	}
	if got := s.Sessions()[0].Errors; got != 0 {
		t.Fatalf("a deliberate cancel must not touch the session error counter, got %d", got)
	}
}

func TestServerToClientRequest(t *testing.T) {
	// Server-initiated request (e.g. sampling) must correlate with the client's
	// response travelling the other way.
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ServerToClient, "99", "sampling/createMessage", `{}`))
	ev := s.Ingest(resp(2, t0.Add(5*time.Millisecond), proxy.ClientToServer, "99", `"result":{"ok":true}`))
	if ev.Call == nil || ev.Call.State != Completed {
		t.Fatalf("server->client request not correlated: %+v", ev.Call)
	}
}

func TestCapabilitiesCapture(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "initialize",
		`{"protocolVersion":"2025-06-18","capabilities":{"sampling":{}},"clientInfo":{"name":"cli"}}`))
	if _, ok := s.Capabilities("s1"); !ok {
		t.Fatal("expected client caps captured after initialize request")
	}
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
		`"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"srv"}}`))
	caps, ok := s.Capabilities("s1")
	if !ok {
		t.Fatal("caps missing")
	}
	if caps.ProtocolVersion != "2025-06-18" {
		t.Fatalf("protocolVersion = %q", caps.ProtocolVersion)
	}
	if len(caps.Client) == 0 || len(caps.Server) == 0 {
		t.Fatalf("client/server caps not both captured: %+v", caps)
	}
}

func TestCapabilitiesFromStatelessMeta(t *testing.T) {
	s := New()
	t0 := time.Now()
	// The 2026-07-28 model removed initialize: the client's identity, version, and
	// capabilities ride every request's _meta instead. Here on a server/discover.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "server/discover",
		`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"ExampleClient","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}}}`))
	caps, ok := s.Capabilities("s1")
	if !ok {
		t.Fatal("client _meta should populate capabilities without an initialize handshake")
	}
	if caps.ProtocolVersion != "2026-07-28" {
		t.Fatalf("protocolVersion = %q, want 2026-07-28 from _meta", caps.ProtocolVersion)
	}
	if !strings.Contains(string(caps.ClientInfo), "ExampleClient") {
		t.Fatalf("clientInfo not read from _meta: %s", caps.ClientInfo)
	}
	if !strings.Contains(string(caps.Client), "elicitation") {
		t.Fatalf("client capabilities not read from _meta: %s", caps.Client)
	}

	// The server side arrives in a server/discover response. serverInfo rides the
	// result's _meta, the canonical location per the draft schema (servers SHOULD
	// send io.modelcontextprotocol/serverInfo on every response).
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
		`"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{},"resources":{}},"instructions":"Call search before answering.","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"ExampleServer","version":"2.0"}}}`))
	caps, _ = s.Capabilities("s1")
	if !strings.Contains(string(caps.ServerInfo), "ExampleServer") {
		t.Fatalf("serverInfo not read from discover _meta: %s", caps.ServerInfo)
	}
	if !strings.Contains(string(caps.Server), "tools") || !strings.Contains(string(caps.Server), "resources") {
		t.Fatalf("server capabilities not read from server/discover: %s", caps.Server)
	}
	if caps.Instructions != "Call search before answering." {
		t.Fatalf("instructions not read from server/discover: %q", caps.Instructions)
	}
}

func TestCapabilitiesDiscoverOnlyFallsBackToSupportedVersion(t *testing.T) {
	s := New()
	t0 := time.Now()
	// A server/discover response with no prior client _meta: the protocol version
	// falls back to the first supported version. This also covers the defensive
	// top-level serverInfo path (not in the schema, but honored if a server sends it).
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "server/discover", ""))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
		`"result":{"supportedVersions":["2026-07-28","2025-11-25"],"capabilities":{"tools":{}},"serverInfo":{"name":"Srv","version":"9"}}`))
	caps, ok := s.Capabilities("s1")
	if !ok {
		t.Fatal("server/discover response should populate capabilities")
	}
	if caps.ProtocolVersion != "2026-07-28" {
		t.Fatalf("protocolVersion = %q, want first of supportedVersions", caps.ProtocolVersion)
	}
	if !strings.Contains(string(caps.ServerInfo), "Srv") {
		t.Fatalf("top-level serverInfo not honored: %s", caps.ServerInfo)
	}
}

func TestCapabilitiesNeitherPathIsUnknown(t *testing.T) {
	s := New()
	t0 := time.Now()
	// Plain calls with no initialize, no client _meta, and no server/discover leave
	// capabilities undeclared, so the inspector shows unknown rather than an error.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	if _, ok := s.Capabilities("s1"); ok {
		t.Fatal("a session that declared no capabilities should report none")
	}
}

func TestServerInfoFromResponseMeta(t *testing.T) {
	s := New()
	t0 := time.Now()
	// A stateless session that never calls server/discover or initialize: the
	// client identifies itself in a tools/call request _meta, and the server's
	// identity rides the tools/call response _meta (which servers SHOULD send on
	// every response per $defs.ResultMetaObject in the draft schema).
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call",
		`{"name":"echo","_meta":{"io.modelcontextprotocol/clientInfo":{"name":"cli","version":"1.0"}}}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
		`"result":{"content":[],"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"StatelessSrv","version":"3.1"}}}`))

	caps, ok := s.Capabilities("s1")
	if !ok {
		t.Fatal("stateless session should have capabilities")
	}
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(caps.ServerInfo, &info) != nil || info.Name != "StatelessSrv" || info.Version != "3.1" {
		t.Fatalf("serverInfo not read from response _meta: %s", caps.ServerInfo)
	}

	// A response without _meta serverInfo leaves serverInfo unset.
	s2 := New()
	s2.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call",
		`{"name":"echo","_meta":{"io.modelcontextprotocol/clientInfo":{"name":"cli"}}}`))
	s2.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	if caps2, _ := s2.Capabilities("s1"); len(caps2.ServerInfo) != 0 {
		t.Fatalf("plain response must not set serverInfo, got %s", caps2.ServerInfo)
	}
}

func TestNotificationAndUnmatchedResponse(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)})
	// Response with no prior request.
	ev := s.Ingest(resp(2, t0, proxy.ServerToClient, "404", `"result":{}`))
	if ev.Call != nil {
		t.Fatalf("unmatched response should have nil Call, got %+v", ev.Call)
	}
	h := s.Sessions()[0]
	if h.Notifications != 1 {
		t.Fatalf("notifications = %d, want 1", h.Notifications)
	}
}

// TestInvalidProtocolFrame checks that a non-JSON-RPC frame on the protocol
// channel is flagged as EventInvalid. On stdio this is the classic failure of a
// server printing a stray line to stdout, which corrupts the stream.
func TestInvalidProtocolFrame(t *testing.T) {
	s := New()
	t0 := time.Now()

	// A stray log line printed to stdout is not JSON, so the shim carries it as
	// text, it is still flagged as invalid rather than shown as a frame.
	ev := s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0,
		Direction: proxy.ServerToClient, Text: "Listening on port 3000"})
	if ev.Kind != EventInvalid {
		t.Fatalf("stray stdout line kind = %d, want EventInvalid (%d)", ev.Kind, EventInvalid)
	}

	// Well-formed JSON that carries no jsonrpc, method, result, or error travels
	// in Raw and is not a JSON-RPC message either.
	ev = s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0,
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"hello":"world"}`)})
	if ev.Kind != EventInvalid {
		t.Fatalf("non-JSON-RPC object kind = %d, want EventInvalid (%d)", ev.Kind, EventInvalid)
	}

	// stderr is a side channel, not stream corruption.
	ev = s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0,
		Direction: proxy.ServerStderr, Text: "debug: starting up"})
	if ev.Kind != EventStderr {
		t.Fatalf("stderr kind = %d, want EventStderr (%d)", ev.Kind, EventStderr)
	}
}

func TestValidationWarnings(t *testing.T) {
	s := New()
	t0 := time.Now()

	ev := s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"id":1,"method":"tools/list"}`)})
	if ev.Kind != EventRequest || ev.Warning != "missing jsonrpc=2.0" {
		t.Fatalf("missing jsonrpc warning = kind %d warning %q", ev.Kind, ev.Warning)
	}

	ev = s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0,
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":99,"result":{}}`)})
	if ev.Kind != EventResponse || ev.Call != nil || ev.Warning != "response id has no matching request" {
		t.Fatalf("unmatched response warning = kind %d call %+v warning %q", ev.Kind, ev.Call, ev.Warning)
	}

	ev = s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0,
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":100}`)})
	if ev.Kind != EventOther || ev.Warning != "response has neither result nor error" {
		t.Fatalf("malformed response warning = kind %d warning %q", ev.Kind, ev.Warning)
	}
}

func TestIngestDeprecatedMethods(t *testing.T) {
	s := New()
	t0 := time.Now()

	cases := []struct {
		method string
		want   string
		kind   EventKind
	}{
		{"roots/list", "roots is deprecated", EventRequest},
		{"sampling/createMessage", "sampling is deprecated", EventRequest},
		{"logging/setLevel", "logging is deprecated", EventRequest},
		{"notifications/roots/list_changed", "roots is deprecated", EventNotification},
		{"notifications/message", "logging notifications/message is deprecated", EventNotification},
	}
	for i, tc := range cases {
		raw := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q`, tc.method)
		if tc.kind == EventRequest {
			raw += fmt.Sprintf(`,"id":%d`, i+1)
		}
		raw += `}`
		ev := s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: uint64(i + 1), TS: t0,
			Direction: proxy.ClientToServer, Raw: json.RawMessage(raw),
		})
		if ev.Kind != tc.kind {
			t.Fatalf("%s: kind = %d, want %d", tc.method, ev.Kind, tc.kind)
		}
		if !strings.Contains(ev.Deprecated, tc.want) {
			t.Fatalf("%s: deprecated = %q, want substring %q", tc.method, ev.Deprecated, tc.want)
		}
		if ev.Warning != "" {
			t.Fatalf("%s: must not ride the warning field, got %q", tc.method, ev.Warning)
		}
	}
}

func TestIngestDeprecatedMethodNegative(t *testing.T) {
	s := New()
	t0 := time.Now()

	ev := s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{}`))
	if ev.Deprecated != "" {
		t.Fatalf("tools/list should not be deprecated, got %q", ev.Deprecated)
	}
}

// TestConcurrentIngest exercises the lock under -race, many goroutines ingest
// while another reads snapshots.
func TestConcurrentIngest(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			sess := fmt.Sprintf("sess-%d", g)
			t0 := time.Now()
			for i := range 200 {
				id := fmt.Sprintf("%d", i)
				s.Ingest(proxy.Envelope{SessionID: sess, ServerLabel: sess, Seq: uint64(2 * i), TS: t0, Direction: proxy.ClientToServer,
					Raw: json.RawMessage(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)})
				s.Ingest(proxy.Envelope{SessionID: sess, ServerLabel: sess, Seq: uint64(2*i + 1), TS: t0, Direction: proxy.ServerToClient,
					Raw: json.RawMessage(`{"jsonrpc":"2.0","id":` + id + `,"result":{}}`)})
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			for _, h := range s.Sessions() {
				_ = s.Timeline(h.ID)
			}
		}
	}()
	wg.Wait()

	if got := len(s.Sessions()); got != 8 {
		t.Fatalf("sessions = %d, want 8", got)
	}
	for _, h := range s.Sessions() {
		if h.Pending != 0 {
			t.Fatalf("session %s has %d pending, want 0", h.ID, h.Pending)
		}
		if h.Requests != 200 || h.Responses != 200 {
			t.Fatalf("session %s req=%d resp=%d, want 200/200", h.ID, h.Requests, h.Responses)
		}
	}
}

func TestToolUsageDistinguishesUsedFromUnused(t *testing.T) {
	s := New()
	t0 := time.Now()

	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"echo"},{"name":"sum"},{"name":"search"}]}`))

	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/call", `{"name":"echo"}`))
	s.Ingest(resp(4, t0, proxy.ServerToClient, "2", `"result":{}`))

	s.Ingest(req(5, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"search"}`))
	s.Ingest(resp(6, t0, proxy.ServerToClient, "3", `"result":{}`))

	used, unused, unadvertised, ok := s.ToolUsage("s1")
	if !ok {
		t.Fatal("expected tool usage")
	}
	if len(used) != 2 {
		t.Fatalf("used = %v, want 2 tools", used)
	}
	if used[0] != "echo" || used[1] != "search" {
		t.Fatalf("used = %v, want [echo search]", used)
	}
	if len(unused) != 1 || unused[0] != "sum" {
		t.Fatalf("unused = %v, want [sum]", unused)
	}
	if len(unadvertised) != 0 {
		t.Fatalf("unadvertised = %v, want none", unadvertised)
	}
}

func TestToolUsagePaginatesAcrossCursor(t *testing.T) {
	s := New()
	t0 := time.Now()

	// Page one is cursorless, page two carries the cursor, so the two responses
	// build one tool set rather than the second replacing the first.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"echo"}],"nextCursor":"p2"}`))

	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/list", `{"cursor":"p2"}`))
	s.Ingest(resp(4, t0, proxy.ServerToClient, "2",
		`"result":{"tools":[{"name":"sum"}]}`))

	_, unused, unadvertised, ok := s.ToolUsage("s1")
	if !ok {
		t.Fatal("expected tool usage")
	}
	if len(unused) != 2 || unused[0] != "echo" || unused[1] != "sum" {
		t.Fatalf("unused = %v, want [echo sum]", unused)
	}
	if len(unadvertised) != 0 {
		t.Fatalf("unadvertised = %v, want none", unadvertised)
	}
}

func TestToolUsageReplacesToolsOnFreshList(t *testing.T) {
	s := New()
	t0 := time.Now()

	// A first listing offers echo and sum. A later cursorless listing (a
	// tools/list_changed re-fetch) no longer offers sum. The fresh list is
	// authoritative, so sum drops out instead of lingering in unused.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"echo"},{"name":"sum"}]}`))

	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/list", ""))
	s.Ingest(resp(4, t0, proxy.ServerToClient, "2",
		`"result":{"tools":[{"name":"echo"}]}`))

	_, unused, unadvertised, ok := s.ToolUsage("s1")
	if !ok {
		t.Fatal("expected tool usage")
	}
	if len(unused) != 1 || unused[0] != "echo" {
		t.Fatalf("unused = %v, want [echo] with sum withdrawn", unused)
	}
	if len(unadvertised) != 0 {
		t.Fatalf("unadvertised = %v, want none", unadvertised)
	}
}

func TestToolUsageWithdrawnCalledToolBecomesUnadvertised(t *testing.T) {
	s := New()
	t0 := time.Now()

	// The client calls sum while it is advertised, then the server re-lists
	// without it. sum was used but is no longer advertised, so it surfaces as
	// called-but-not-advertised rather than as an unused tool.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"echo"},{"name":"sum"}]}`))
	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/call", `{"name":"sum"}`))
	s.Ingest(resp(4, t0, proxy.ServerToClient, "2", `"result":{}`))

	s.Ingest(req(5, t0, proxy.ClientToServer, "3", "tools/list", ""))
	s.Ingest(resp(6, t0, proxy.ServerToClient, "3",
		`"result":{"tools":[{"name":"echo"}]}`))

	used, unused, unadvertised, ok := s.ToolUsage("s1")
	if !ok {
		t.Fatal("expected tool usage")
	}
	if len(used) != 0 {
		t.Fatalf("used = %v, want none", used)
	}
	if len(unused) != 1 || unused[0] != "echo" {
		t.Fatalf("unused = %v, want [echo]", unused)
	}
	if len(unadvertised) != 1 || unadvertised[0] != "sum" {
		t.Fatalf("unadvertised = %v, want [sum]", unadvertised)
	}
}

func TestToolUsageReportsCalledButNotAdvertised(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"echo"}]}`))
	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/call", `{"name":"search"}`))
	s.Ingest(resp(4, t0, proxy.ServerToClient, "2", `"result":{}`))

	s.Ingest(req(5, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"weather"}`))
	s.Ingest(resp(6, t0, proxy.ServerToClient, "3", `"result":{}`))

	used, unused, unadvertised, ok := s.ToolUsage("s1")
	if !ok {
		t.Fatal("expected tool usage")
	}
	if len(used) != 0 {
		t.Fatalf("used = %v, want none", used)
	}
	if len(unused) != 1 || unused[0] != "echo" {
		t.Fatalf("unused = %v, want [echo]", unused)
	}
	if len(unadvertised) != 2 ||
		unadvertised[0] != "search" ||
		unadvertised[1] != "weather" {
		t.Fatalf("unadvertised = %v, want [search weather]", unadvertised)
	}
}

func TestToolDefinitionsCaptureDescriptionsSchemasAndCompletePagination(t *testing.T) {
	s := New()
	t0 := time.Now()

	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1", `"result":{"tools":[{"name":"search","description":"Search docs","inputSchema":{"type":"object","properties":{"query":{"type":"string"}}}}],"nextCursor":"p2"}`))
	if _, ok := s.ToolDefinitions("s1"); ok {
		t.Fatal("partial tools/list pagination must not be treated as a complete definition set")
	}

	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/list", `{"cursor":"p2"}`))
	s.Ingest(resp(4, t0, proxy.ServerToClient, "2", `"result":{"tools":[{"name":"fetch","description":"Fetch a page","inputSchema":{"type":"object","properties":{"url":{"oneOf":[{"type":"object"},{"type":"string"}]}}}}]}`))

	definitions, ok := s.ToolDefinitions("s1")
	if !ok {
		t.Fatal("complete paginated tools/list was not exposed")
	}
	if len(definitions) != 2 {
		t.Fatalf("definitions = %+v, want two tools", definitions)
	}
	if definitions[0].Name != "search" || definitions[0].Description != "Search docs" || string(definitions[0].InputSchema) == "" {
		t.Fatalf("search definition = %+v", definitions[0])
	}
	if len(definitions[0].Findings) != 0 {
		t.Fatalf("search findings = %v, want none (flat typed schema)", definitions[0].Findings)
	}
	if definitions[1].Name != "fetch" || definitions[1].Description != "Fetch a page" {
		t.Fatalf("fetch definition = %+v", definitions[1])
	}
	got := definitions[1].Findings
	if len(got) != 1 || got[0].Kind != FindingOneOf {
		t.Fatalf("fetch findings = %v, want one oneOf finding", got)
	}
}

func TestNonObjectRootInputSchemaWarnsOnToolsList(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"read_file","description":"Read a file","inputSchema":{"$schema":"http://json-schema.org/draft-07/schema#"}}]}`))

	events := s.Timeline("s1")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	// The reason names what was actually on the wire. "the schema is wrong" would
	// leave a reader bisecting a listing, which is the cost issue #199 measured in
	// weeks.
	for _, want := range []string{`tool "read_file" advertises`, "no root type", `root type is "object"`} {
		if !strings.Contains(events[1].Warning, want) {
			t.Fatalf("warning = %q, want it to mention %q", events[1].Warning, want)
		}
	}
	definitions, ok := s.ToolDefinitions("s1")
	if !ok || len(definitions) != 1 {
		t.Fatalf("definitions = %+v, ok=%v", definitions, ok)
	}
	found := false
	for _, f := range definitions[0].Findings {
		if f.Kind == FindingNonObjectRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %v, want nonObjectRoot", definitions[0].Findings)
	}
}

func TestNonObjectRootOnPartialToolsListStillWarns(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", ""))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"read_file","inputSchema":{}}],"nextCursor":"p2"}`))

	events := s.Timeline("s1")
	if !strings.Contains(events[1].Warning, `tool "read_file" advertises an inputSchema with no root type`) {
		t.Fatalf("warning = %q, want the root violation on a partial listing", events[1].Warning)
	}
}

func TestToolDriftIsExposedOnSessionHeader(t *testing.T) {
	s := New()
	s.Ingest(req(1, time.Now(), proxy.ClientToServer, "1", "tools/list", ""))
	drift := ToolDrift{}
	drift.Add(DriftDescription, "search")
	s.SetToolDrift("s1", drift)

	headers := s.Sessions()
	if len(headers) != 1 || !headers[0].HasToolDrift {
		t.Fatalf("session header drift = %+v", headers)
	}
	report, ok := s.ToolDrift("s1")
	if changed := report.Names(DriftDescription); !ok || len(changed) != 1 || changed[0] != "search" {
		t.Fatalf("tool drift = %+v, ok=%v", report, ok)
	}
}

// A server that needs more input answers the original request and the client
// retries under a different id, so the operation has to stay one call with one
// duration that includes the time the user spent answering.
func TestMRTRRetryContinuesTheSameOperation(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s.Ingest(resp(2, t0.Add(10*time.Millisecond), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","inputRequests":{"login":{"method":"elicitation/create"}},"requestState":"opaque-blob"}`))

	if h := s.Sessions()[0]; h.Pending != 1 {
		t.Fatalf("an operation waiting on the client is still pending, got %d", h.Pending)
	}

	// The user takes 5s to answer, then the client retries under a new id.
	ev := s.Ingest(req(3, t0.Add(5*time.Second), proxy.ClientToServer, "2", "tools/call",
		`{"name":"book","inputResponses":{"login":{"action":"accept"}},"requestState":"opaque-blob"}`))
	if ev.MRTRRoot != "1" {
		t.Fatalf("retry should point at the request it continues, got %q", ev.MRTRRoot)
	}
	if ev.Call == nil || ev.Call.ID != "1" {
		t.Fatalf("retry should continue the original call, got %+v", ev.Call)
	}

	done := s.Ingest(resp(4, t0.Add(6*time.Second), proxy.ServerToClient, "2", `"result":{"content":[]}`))
	if done.Call == nil || done.Call.State != Completed {
		t.Fatalf("the terminal answer should complete the operation, got %+v", done.Call)
	}
	if got := done.Call.Duration(); got < 6*time.Second {
		t.Fatalf("duration = %v, want the whole exchange including the user wait", got)
	}
	h := s.Sessions()[0]
	if h.Pending != 0 {
		t.Fatalf("pending = %d, want 0", h.Pending)
	}
	if h.Requests != 2 {
		t.Fatalf("both frames are requests on the wire, requests = %d", h.Requests)
	}
	if n := len(s.Calls("s1")); n != 1 {
		t.Fatalf("one logical operation must stay one call, got %d", n)
	}
}

func TestMRTRFlagsRequestStateIntegrityViolations(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	const issued = "server-opaque-state-never-render"
	const changed = "client-altered-state-never-render"

	tests := []struct {
		name         string
		resultState  string
		retryState   string
		wantCategory string
	}{
		{
			name:         "changed",
			resultState:  `,"requestState":"` + issued + `"`,
			retryState:   `,"requestState":"` + changed + `"`,
			wantCategory: "changed",
		},
		{
			name:         "missing",
			resultState:  `,"requestState":"` + issued + `"`,
			wantCategory: "missing",
		},
		{
			name:         "invented",
			retryState:   `,"requestState":"` + changed + `"`,
			wantCategory: "invented",
		},
		{
			name:        "correct",
			resultState: `,"requestState":"` + issued + `"`,
			retryState:  `,"requestState":"` + issued + `"`,
		},
		{
			name:        "present empty correct",
			resultState: `,"requestState":""`,
			retryState:  `,"requestState":""`,
		},
		{
			name:         "present empty missing",
			resultState:  `,"requestState":""`,
			wantCategory: "missing",
		},
		{
			name:         "present empty invented",
			retryState:   `,"requestState":""`,
			wantCategory: "invented",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
			s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
				`"result":{"resultType":"input_required","inputRequests":{"login":{"method":"elicitation/create"}}`+tc.resultState+`}`))
			ev := s.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call",
				`{"name":"book","inputResponses":{"login":{"action":"accept"}}`+tc.retryState+`}`))

			if ev.MRTRRoot != "1" {
				t.Fatal("retry with unique MRTR evidence was not linked to its root")
			}
			if ev.MRTRStateIssue != MRTRStateIssue(tc.wantCategory) {
				t.Fatalf("requestState issue = %q, want %q", ev.MRTRStateIssue, tc.wantCategory)
			}
			if tc.wantCategory == "" {
				if strings.Contains(ev.Warning, "requestState") {
					t.Fatal("a correct requestState echo produced a finding")
				}
			} else if !strings.Contains(ev.Warning, "requestState") ||
				!strings.Contains(ev.Warning, tc.wantCategory) {
				t.Fatalf("requestState finding did not identify the %s category", tc.wantCategory)
			}
			if strings.Contains(ev.Warning, issued) || strings.Contains(ev.Warning, changed) {
				t.Fatal("requestState finding disclosed an opaque value")
			}
			if n := len(s.Calls("s1")); n != 1 {
				t.Fatalf("linked retry created %d calls, want one logical operation", n)
			}
			done := s.Ingest(resp(4, t0.Add(3*time.Second), proxy.ServerToClient, "2", `"result":{"content":[]}`))
			if done.Call == nil || done.Call.State != Completed || s.Sessions()[0].Pending != 0 {
				t.Fatal("requestState finding disturbed terminal MRTR completion")
			}
		})
	}
}

func TestMRTRDoesNotGuessARequestStateViolationBetweenAmbiguousCalls(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	for i, id := range []string{"1", "2"} {
		s.Ingest(req(uint64(2*i+1), t0, proxy.ClientToServer, id, "tools/call", `{"name":"book"}`))
		s.Ingest(resp(uint64(2*i+2), t0.Add(time.Second), proxy.ServerToClient, id,
			fmt.Sprintf(`"result":{"resultType":"input_required","inputRequests":{"login":{"method":"elicitation/create"}},"requestState":"issued-%s"}`, id)))
	}

	ev := s.Ingest(req(5, t0.Add(2*time.Second), proxy.ClientToServer, "3", "tools/call",
		`{"name":"book","inputResponses":{"login":{"action":"accept"}},"requestState":"unknown"}`))
	if ev.MRTRRoot != "" || ev.MRTRStateIssue != "" || strings.Contains(ev.Warning, "requestState") {
		t.Fatal("ambiguous structural evidence must stay unlinked and unclassified")
	}
	if n := len(s.Calls("s1")); n != 3 {
		t.Fatalf("ambiguous retry produced %d calls, want three unlinked calls", n)
	}
}

// A chain can run to any length, and every retry links back to the same root.
func TestMRTRHandlesAChainLongerThanTwo(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","requestState":"st-1"}`))
	ev2 := s.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call",
		`{"name":"book","requestState":"st-1"}`))
	s.Ingest(resp(4, t0.Add(3*time.Second), proxy.ServerToClient, "2",
		`"result":{"resultType":"input_required","requestState":"st-2"}`))
	ev3 := s.Ingest(req(5, t0.Add(4*time.Second), proxy.ClientToServer, "3", "tools/call",
		`{"name":"book","requestState":"st-2"}`))
	done := s.Ingest(resp(6, t0.Add(5*time.Second), proxy.ServerToClient, "3", `"result":{"content":[]}`))

	for i, ev := range []EventView{ev2, ev3} {
		if ev.MRTRRoot != "1" {
			t.Fatalf("retry %d should link to the root, got %q", i+1, ev.MRTRRoot)
		}
	}
	if done.Call.State != Completed || done.Call.Duration() < 5*time.Second {
		t.Fatalf("chain should close once and span the whole exchange, got %+v", done.Call)
	}
	if n := len(s.Calls("s1")); n != 1 {
		t.Fatalf("a three-step chain is still one call, got %d", n)
	}
}

// Without a requestState the fallback needs method, operation name and the full
// answered key set to agree.
func TestMRTRLinksOnKeySetWhenNoRequestState(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "resources/read", `{"uri":"file:///a"}`))
	s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","inputRequests":{"who":{"method":"elicitation/create"}}}`))
	ev := s.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "resources/read",
		`{"uri":"file:///a","inputResponses":{"who":{"action":"accept"}}}`))
	if ev.MRTRRoot != "1" {
		t.Fatalf("a non-tool request should link too, got %q", ev.MRTRRoot)
	}
}

// Two identical operations in flight cannot be told apart without a state, so
// neither is linked. A wrong link is worse than none.
func TestMRTRRefusesAnAmbiguousLink(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	for i, id := range []string{"1", "2"} {
		s.Ingest(req(uint64(2*i+1), t0, proxy.ClientToServer, id, "tools/call", `{"name":"book"}`))
		s.Ingest(resp(uint64(2*i+2), t0.Add(time.Second), proxy.ServerToClient, id,
			`"result":{"resultType":"input_required","inputRequests":{"who":{"method":"elicitation/create"}}}`))
	}
	ev := s.Ingest(req(9, t0.Add(2*time.Second), proxy.ClientToServer, "3", "tools/call",
		`{"name":"book","inputResponses":{"who":{"action":"accept"}}}`))
	if ev.MRTRRoot != "" {
		t.Fatalf("an ambiguous retry must not be linked, got %q", ev.MRTRRoot)
	}
	if n := len(s.Calls("s1")); n != 3 {
		t.Fatalf("an unlinked retry stays its own call, got %d", n)
	}
}

// An ordinary request that happens to follow one must not be swallowed.
func TestMRTRLeavesUnrelatedRequestsAlone(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","requestState":"st-1"}`))
	ev := s.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call", `{"name":"other"}`))
	if ev.MRTRRoot != "" {
		t.Fatalf("an unrelated call must not be linked, got %q", ev.MRTRRoot)
	}
	if n := len(s.Calls("s1")); n != 2 {
		t.Fatalf("want two independent calls, got %d", n)
	}
}

// The per-tool statistics must see one call, not one per round trip, or a
// chatty elicitation would inflate both the call count and the percentiles.
func TestMRTRCountsOneCallInTheToolSummary(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","requestState":"st-1"}`))
	s.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call",
		`{"name":"book","requestState":"st-1"}`))
	s.Ingest(resp(4, t0.Add(3*time.Second), proxy.ServerToClient, "2", `"result":{"content":[]}`))

	summary, ok := s.ToolSummary("s1")
	if !ok || len(summary.Tools) != 1 {
		t.Fatalf("want one tool, got %+v", summary.Tools)
	}
	if got := summary.Tools[0].Calls; got != 1 {
		t.Fatalf("calls = %d, want 1: a retry is a continuation, not another call", got)
	}
}

// The 2026-07-28 revision routes sampling and roots only through MRTR, where the
// method sits inside inputRequests, so a top-level check alone would leave the
// deprecation flag blind for the one remaining way they are used.
func TestDeprecationFlagsMethodsNestedInsideMRTR(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		result  string
		want    string
		wantNot string
	}{
		{
			name:   "sampling",
			result: `"result":{"resultType":"input_required","inputRequests":{"ask":{"method":"sampling/createMessage"}}}`,
			want:   "sampling is deprecated",
		},
		{
			name:   "roots",
			result: `"result":{"resultType":"input_required","inputRequests":{"where":{"method":"roots/list"}}}`,
			want:   "roots is deprecated",
		},
		{
			// Elicitation is the replacement, not a deprecated feature.
			name:    "elicitation stays clean",
			result:  `"result":{"resultType":"input_required","inputRequests":{"who":{"method":"elicitation/create"}}}`,
			wantNot: "deprecated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
			ev := s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1", tc.result))
			if tc.want != "" && !strings.Contains(ev.Deprecated, tc.want) {
				t.Fatalf("Deprecated = %q, want it to mention %q", ev.Deprecated, tc.want)
			}
			if tc.wantNot != "" && strings.Contains(ev.Deprecated, tc.wantNot) {
				t.Fatalf("Deprecated = %q, want nothing", ev.Deprecated)
			}
		})
	}
}

// A result may ask for several things at once. Every deprecated feature is
// named, but a feature used twice is named once.
func TestDeprecationNamesEachNestedFeatureOnce(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	ev := s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","inputRequests":{`+
			`"a":{"method":"sampling/createMessage"},`+
			`"b":{"method":"sampling/createMessage"},`+
			`"c":{"method":"roots/list"},`+
			`"d":{"method":"elicitation/create"}}}`))

	if got := strings.Count(ev.Deprecated, "sampling is deprecated"); got != 1 {
		t.Fatalf("sampling named %d times, want once: %q", got, ev.Deprecated)
	}
	if !strings.Contains(ev.Deprecated, "roots is deprecated") {
		t.Fatalf("roots should be named too: %q", ev.Deprecated)
	}
	if strings.Contains(ev.Deprecated, "elicitation") {
		t.Fatalf("elicitation is not deprecated: %q", ev.Deprecated)
	}
}

// The flag stays a heads-up, so a session using a deprecated feature through
// MRTR must not fail a default check run.
func TestNestedDeprecationIsNotAProtocolWarning(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	ev := s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","inputRequests":{"ask":{"method":"sampling/createMessage"}}}`))

	if ev.Deprecated == "" {
		t.Fatal("the frame should carry the heads-up")
	}
	if ev.Warning != "" {
		t.Fatalf("it must not become a protocol warning, got %q", ev.Warning)
	}
}

// Flagging must not disturb the correlation the same result drives.
func TestNestedDeprecationLeavesMRTRCorrelationIntact(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","requestState":"st","inputRequests":{"ask":{"method":"sampling/createMessage"}}}`))
	ev := s.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call",
		`{"name":"book","requestState":"st"}`))
	if ev.MRTRRoot != "1" {
		t.Fatalf("the retry should still link to its root, got %q", ev.MRTRRoot)
	}
}

// A server may answer with requestState and no inputRequests, and then a
// tampered retry has nothing left to link it by: the state matches nothing and
// there are no answered keys to fall back on. It reads as an unrelated call
// rather than a violation. This pins that limit so widening the fallback later
// is a deliberate choice rather than an accident.
func TestMRTRCannotSeeTamperingOnAStateOnlyExchange(t *testing.T) {
	t0 := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","requestState":"st-1"}`))

	// The correct echo still links, so the limit is only about tampering.
	ok := s.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call",
		`{"name":"book","requestState":"st-1"}`))
	if ok.MRTRRoot != "1" || ok.MRTRStateIssue != "" {
		t.Fatalf("a correct echo must still link cleanly, got root=%q issue=%q", ok.MRTRRoot, ok.MRTRStateIssue)
	}

	s2 := New()
	s2.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s2.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","requestState":"st-1"}`))
	tampered := s2.Ingest(req(3, t0.Add(2*time.Second), proxy.ClientToServer, "2", "tools/call",
		`{"name":"book","requestState":"tampered"}`))

	if tampered.MRTRRoot != "" {
		t.Fatalf("with nothing to link by, the retry must not be attached, got root=%q", tampered.MRTRRoot)
	}
	if tampered.MRTRStateIssue != "" {
		t.Fatalf("an unlinked retry cannot be classified, got %q", tampered.MRTRStateIssue)
	}
	if n := len(s2.Calls("s1")); n != 2 {
		t.Fatalf("it reads as two independent calls, got %d", n)
	}
}

// sessionErrors is the session's error counter, which is what a default check
// run gates on.
func sessionErrors(t *testing.T, s *Store) int {
	t.Helper()
	headers := s.Sessions()
	if len(headers) != 1 {
		t.Fatalf("expected one session, got %d", len(headers))
	}
	return headers[0].Errors
}

// TestIngestClassifiesATransportFailure covers both halves: a status-only frame
// becomes a transport event rather than nothing, and it counts toward the error
// signal so a default check run gates on it.
func TestIngestClassifiesATransportFailure(t *testing.T) {
	const challenge = `Bearer resource_metadata="https://auth.example/.well-known/oauth-protected-resource"`
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: "http",
		Status: 401, AuthChallenge: challenge,
	})
	if ev.Kind != EventTransport {
		t.Fatalf("a bodiless 401 should be a transport event, got kind %v", ev.Kind)
	}
	if ev.HTTPStatus != 401 || ev.AuthChallenge != challenge {
		t.Fatalf("the status and challenge must reach the view, got %d %q", ev.HTTPStatus, ev.AuthChallenge)
	}
	if got := sessionErrors(t, s); got != 1 {
		t.Fatalf("a 401 should count toward the error signal, got %d", got)
	}
}

// TestIngestDoesNotCountASuccessfulTransportFrame pins the other side of the
// counter. A 202 acknowledging a notification has no body by spec requirement,
// so it must be visible without turning a correct session red.
func TestIngestDoesNotCountASuccessfulTransportFrame(t *testing.T) {
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: "http", Status: 202,
	})
	if ev.Kind != EventTransport {
		t.Fatalf("a bodiless 202 should still be visible, got kind %v", ev.Kind)
	}
	if got := sessionErrors(t, s); got != 0 {
		t.Fatalf("a 202 is not a failure, got %d errors", got)
	}
}

// TestIngestDoesNotCallAnHTTPErrorPageStreamCorruption keeps the diagnosis
// honest. A gateway's HTML 502 is not a server printing to stdout, and saying so
// sends the reader after the wrong problem.
func TestIngestDoesNotCallAnHTTPErrorPageStreamCorruption(t *testing.T) {
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: "http", Status: 502,
		Text: "<html><body>502 Bad Gateway</body></html>",
	})
	if ev.Kind == EventInvalid {
		t.Fatal("an HTTP error page is a transport failure, not a corrupted stream")
	}
	if ev.Kind != EventTransport {
		t.Fatalf("expected a transport event, got kind %v", ev.Kind)
	}
	if got := sessionErrors(t, s); got != 1 {
		t.Fatalf("a 502 should count toward the error signal, got %d", got)
	}
}

// TestIngestStillReportsStdioStreamCorruption pins the other side: with no
// status there is no transport layer to blame, so stray stdout stays invalid.
func TestIngestStillReportsStdioStreamCorruption(t *testing.T) {
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: "stdio",
		Text: "Listening on port 3000...",
	})
	if ev.Kind != EventInvalid {
		t.Fatalf("stray stdout on stdio is still stream corruption, got kind %v", ev.Kind)
	}
}

// TestIngestCountsAnHTTPFailureOnce guards against double counting when a 400
// carries a JSON-RPC error body of its own: the body takes the response branch,
// which already counts it.
func TestIngestCountsAnHTTPFailureOnce(t *testing.T) {
	s := New()
	req := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: "http",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`),
	}
	s.Ingest(req)
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: "http", Status: 400,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"bad request"}}`),
	})
	if ev.Kind != EventResponse {
		t.Fatalf("a JSON-RPC error body is a response, not a transport event, got kind %v", ev.Kind)
	}
	if ev.HTTPStatus != 400 {
		t.Fatalf("the status should still ride the response frame, got %d", ev.HTTPStatus)
	}
	if got := sessionErrors(t, s); got != 1 {
		t.Fatalf("one failure must be counted once, got %d", got)
	}
}

func TestSubscriptionsListenDoesNotCountAsPending(t *testing.T) {
	s := New()
	t0 := time.Now()
	ev := s.Ingest(req(1, t0, proxy.ClientToServer, "5", "subscriptions/listen", `{}`))
	if ev.Call == nil || ev.Call.State != Streaming {
		t.Fatalf("listen call state = %v, want streaming", ev.Call.State)
	}
	if h := s.Sessions()[0]; h.Pending != 0 {
		t.Fatalf("pending = %d, want 0 for an open listen stream", h.Pending)
	}
}

func TestSubscriptionsListenCompletesWithoutPendingLeak(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "5", "subscriptions/listen", `{}`))
	done := s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "5", `"result":{}`))
	if done.Call == nil || done.Call.State != Completed {
		t.Fatalf("completed listen state = %v, want completed", done.Call.State)
	}
	if h := s.Sessions()[0]; h.Pending != 0 {
		t.Fatalf("pending = %d, want 0 after the stream ends", h.Pending)
	}
}

func TestSubscriptionsListenClosesOnCancelledNotification(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "5", "subscriptions/listen", `{}`))
	cancel := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(500 * time.Millisecond),
		Direction: proxy.ServerToClient,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":5}}`),
	}
	s.Ingest(cancel)
	if h := s.Sessions()[0]; h.Pending != 0 {
		t.Fatalf("pending = %d, want 0 after the server closes the stream", h.Pending)
	}
	events := s.Timeline("s1")
	// The teardown is a cancellation, not a completion. What completes the call is
	// the graceful empty result the server SHOULD send after it, which arrives on
	// its own frame and is covered by
	// TestGracefulSubscriptionClosureIsNotALateResult.
	if events[0].Call == nil || events[0].Call.State != Cancelled {
		t.Fatalf("listen call should be cancelled after notifications/cancelled, got %v", events[0].Call.State)
	}
}

// TestSubscriptionsListenClosesOnAStringRequestID is the id-form regression. The
// store keys a call on the raw JSON text of its id, so a string id keeps its
// quotes, and a cancellation that decodes requestId into a Go string strips them
// and matches nothing. The specification's own cancellation example uses a string
// id, so this is the common shape rather than the exotic one, and a miss leaves
// the stream reading as open forever, which is the symptom this change removes.
func TestSubscriptionsListenClosesOnAStringRequestID(t *testing.T) {
	for _, tc := range []struct{ name, id, requestID string }{
		{"string id", `"sub-1"`, `"sub-1"`},
		{"numeric id", `5`, `5`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			t0 := time.Now()
			s.Ingest(req(1, t0, proxy.ClientToServer, tc.id, "subscriptions/listen", `{}`))
			// The spec makes this a server MUST when it tears down a subscription.
			s.Ingest(proxy.Envelope{
				SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Second),
				Direction: proxy.ServerToClient,
				Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":` +
					`{"requestId":` + tc.requestID + `,"reason":"shutting down"}}`),
			})

			events := s.Timeline("s1")
			// A teardown is a cancellation rather than a completion, and it carries
			// the reason the server gave. The graceful empty result the server SHOULD
			// send afterwards is the completion signal, and it arrives separately.
			if events[0].Call == nil || events[0].Call.State != Cancelled {
				t.Fatalf("the teardown should cancel the stream, got %v", events[0].Call.State)
			}
			if events[0].Call.CancelReason != "shutting down" {
				t.Fatalf("cancel reason = %q, want the server's own", events[0].Call.CancelReason)
			}
			if h := s.Sessions()[0]; h.Pending != 0 {
				t.Fatalf("pending = %d, want 0", h.Pending)
			}
		})
	}
}

// TestReusingAStreamIdKeepsThePendingCounterHonest. A Streaming call never took a
// pending slot, so the reuse path must not hand its slot to the new call: doing
// that leaves the new pending request uncounted and then decrements on its answer,
// driving the counter negative. A negative pending is nonsense in the footer and
// it also masks a genuinely hung call from the CI gate.
func TestReusingAStreamIdKeepsThePendingCounterHonest(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"pending then pending", "tools/list", "tools/list"},
		{"pending then stream", "tools/list", "subscriptions/listen"},
		{"stream then pending", "subscriptions/listen", "tools/list"},
		{"stream then stream", "subscriptions/listen", "subscriptions/listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			t0 := time.Now()
			s.Ingest(req(1, t0, proxy.ClientToServer, "7", tc.first, `{}`))
			s.Ingest(req(2, t0.Add(time.Second), proxy.ClientToServer, "7", tc.second, `{}`))
			if got := s.Sessions()[0].Pending; got < 0 {
				t.Fatalf("pending went negative on the reuse: %d", got)
			}
			s.Ingest(resp(3, t0.Add(2*time.Second), proxy.ServerToClient, "7", `"result":{}`))
			if got := s.Sessions()[0].Pending; got != 0 {
				t.Fatalf("every call is settled, pending = %d, want 0", got)
			}
		})
	}
}

// TestIngestDoesNotFlagADowngradedHandshake is the regression. initialize carries
// the version the client proposes, and applyRequest folds it into the session
// before the server has answered. A legacy client proposing 2026-07-28 to a
// 2025-11-25 server would otherwise be told the perfectly correct downgrade
// response was missing a required field, and warnings fail a default check run,
// so that is a red build on a healthy handshake.
func TestIngestDoesNotFlagADowngradedHandshake(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","clientInfo":{"name":"cli"},"capabilities":{}}}`),
	})
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"srv"}}}`),
	})

	if strings.Contains(ev.Warning, "resultType") {
		t.Fatalf("a server negotiating down is not missing a field it never had to send: %q", ev.Warning)
	}
	// And the downgrade must stick, so later results are judged by the server's
	// revision rather than the client's proposal.
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`),
	})
	res := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 4, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`),
	})
	if strings.Contains(res.Warning, "resultType") {
		t.Fatalf("the negotiated 2025-11-25 does not require resultType: %q", res.Warning)
	}
}

// TestIngestExemptsTheInitializeResponseOutright covers the case the version
// gate alone would miss: a server that answers initialize while agreeing to
// 2026-07-28. The handshake was removed in that revision, so a server answering
// one is speaking an earlier one whatever it claims, and it is the wrong frame
// to make the point on.
func TestIngestExemptsTheInitializeResponseOutright(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","clientInfo":{"name":"cli"},"capabilities":{}}}`),
	})
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-07-28","capabilities":{},"serverInfo":{"name":"srv"}}}`),
	})
	if strings.Contains(ev.Warning, "resultType") {
		t.Fatalf("initialize is exempt whatever version it agrees to: %q", ev.Warning)
	}
}

// TestIngestFlagsAMissingResultTypeOnAStatelessSession is the feature itself, on
// the path 2026-07-28 actually uses: no handshake, the version riding _meta.
func TestIngestFlagsAMissingResultTypeOnAStatelessSession(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`),
	})
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	if !strings.Contains(ev.Warning, "resultType") {
		t.Fatalf("a 2026-07-28 server must send resultType, got %q", ev.Warning)
	}

	// Present is silent, on either allowed value.
	for _, value := range []string{"complete", "input_required"} {
		ok := s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: now, Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":9,"result":{"resultType":"` + value + `"}}`),
		})
		if strings.Contains(ok.Warning, "resultType") {
			t.Fatalf("resultType %q is present, got %q", value, ok.Warning)
		}
	}
}

// TestIngestIgnoresResultTypeOnAnErrorResponse. An error response carries no
// result, so there is no field to require and nothing to say.
func TestIngestIgnoresResultTypeOnAnErrorResponse(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`),
	})
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"boom"}}`),
	})
	if strings.Contains(ev.Warning, "resultType") {
		t.Fatalf("an error response has no result to check: %q", ev.Warning)
	}
}

// versionedRequest is a client request declaring a revision in _meta, which is
// where the specification says every request supplies it.
func versionedRequest(seq uint64, id, method, version string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(), Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":` + id + `,"method":"` + method +
			`","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"` + version + `"}}}`),
	}
}

func plainResult(seq uint64, id string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(), Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":` + id + `,"result":{"tools":[]}}`),
	}
}

// TestResultTypeJudgesEachRequestByItsOwnRevision is the regression, and it is
// the reason the session's version cannot be the source. MCP is stateless: the
// spec forbids a server inferring the protocol version from prior requests on
// the same connection, and says clients may interleave unrelated requests on one
// transport. mcpsnoop sees more of that than anyone, since one `mcpsnoop http`
// proxies every client through a single session.
//
// Both directions of the mistake are here. Reading the session's version flags a
// correct older answer, and excuses a genuinely non-conformant newer one, purely
// on the order the requests happened to arrive in.
func TestResultTypeJudgesEachRequestByItsOwnRevision(t *testing.T) {
	t.Run("older answer is not flagged by a newer neighbour", func(t *testing.T) {
		s := New()
		s.Ingest(versionedRequest(1, "1", "tools/list", "2025-11-25"))
		s.Ingest(versionedRequest(2, "2", "tools/list", "2026-07-28")) // still in flight
		ev := s.Ingest(plainResult(3, "1"))
		if strings.Contains(ev.Warning, "resultType") {
			t.Fatalf("a 2025-11-25 request's answer needs no resultType: %q", ev.Warning)
		}
	})

	t.Run("newer answer is still flagged behind an older neighbour", func(t *testing.T) {
		s := New()
		s.Ingest(versionedRequest(1, "1", "tools/list", "2026-07-28"))
		s.Ingest(versionedRequest(2, "2", "tools/list", "2025-11-25"))
		ev := s.Ingest(plainResult(3, "1"))
		if !strings.Contains(ev.Warning, "missing required resultType") {
			t.Fatalf("a 2026-07-28 request's answer must carry resultType: %q", ev.Warning)
		}
	})
}

// TestResultTypeReadsTheProtocolVersionHeader covers the second request-scoped
// source. The header is required on every Streamable HTTP request from
// 2026-07-28, so a client that sets it and omits the _meta copy is still saying
// which revision this request is.
func TestResultTypeReadsTheProtocolVersionHeader(t *testing.T) {
	s := New()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(), Direction: proxy.ClientToServer,
		Transport: proxy.TransportHTTP, MCPProtocolVersion: "2026-07-28", MCPMethod: "tools/list",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`),
	})
	ev := s.Ingest(plainResult(2, "1"))
	if !strings.Contains(ev.Warning, "missing required resultType") {
		t.Fatalf("the header declares the revision too: %q", ev.Warning)
	}
}

// TestResultTypeIgnoresAnUnmatchedResponse. Without its request there is nothing
// that says which revision a response belongs to, and inventing one from session
// state is exactly what this check stopped doing.
func TestResultTypeIgnoresAnUnmatchedResponse(t *testing.T) {
	s := New()
	s.Ingest(versionedRequest(1, "1", "tools/list", "2026-07-28"))
	ev := s.Ingest(plainResult(2, "9")) // no request ever carried id 9
	if strings.Contains(ev.Warning, "resultType") {
		t.Fatalf("an orphaned response cannot be judged: %q", ev.Warning)
	}
}

// TestResultTypeChecksADiscoverResult keeps the stateless entry point covered:
// server/discover is the first thing a 2026-07-28 client sends, and its result
// is a result like any other.
func TestResultTypeChecksADiscoverResult(t *testing.T) {
	s := New()
	s.Ingest(versionedRequest(1, "1", "server/discover", "2026-07-28"))
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(), Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`),
	})
	if !strings.Contains(ev.Warning, "missing required resultType") {
		t.Fatalf("a discover result must carry resultType too: %q", ev.Warning)
	}
}

// TestResultTypeRejectsAValueItCannotHold. A key alone is not the field: null
// and a number read as "present, fine" to a check that only looks for the key,
// and no extension can make either valid, so refusing them invents no false
// alarm.
func TestResultTypeRejectsAValueItCannotHold(t *testing.T) {
	for _, value := range []string{`null`, `42`, `""`, `{}`, `["complete"]`} {
		s := New()
		s.Ingest(versionedRequest(1, "1", "tools/list", "2026-07-28"))
		ev := s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"resultType":` + value + `}}`),
		})
		if !strings.Contains(ev.Warning, "invalid resultType") {
			t.Fatalf("resultType %s is not a value the field can hold: %q", value, ev.Warning)
		}
	}
}

// TestResultTypeDoesNotPoliceTheValueSet is the other half, and the reason the
// check stops at the type. Extensions may add ResultType values, and the valid
// set is whatever the pair negotiated, so a string mcpsnoop does not recognise
// is not evidence of anything. Pinned so the gap reads as a decision.
func TestResultTypeDoesNotPoliceTheValueSet(t *testing.T) {
	for _, value := range []string{`"complete"`, `"input_required"`, `"com.example/deferred"`} {
		s := New()
		s.Ingest(versionedRequest(1, "1", "tools/list", "2026-07-28"))
		ev := s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"resultType":` + value + `}}`),
		})
		if strings.Contains(ev.Warning, "resultType") {
			t.Fatalf("an unrecognised value is an extension's business, not ours: %s gave %q", value, ev.Warning)
		}
	}
}

// TestResultTypeSurvivesAnMRTRRetry. A retry is matched through matchRetry
// rather than openCall, so the revision has to live on the call object to reach
// it. Without that the second leg of every multi-round-trip call would go
// unchecked.
func TestResultTypeSurvivesAnMRTRRetry(t *testing.T) {
	s := New()
	s.Ingest(versionedRequest(1, "1", "tools/call", "2026-07-28"))
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(), Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required",` +
			`"requestState":"st-1","requiredKeys":["token"]}}`),
	})
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: time.Now(), Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search",` +
			`"requestState":"st-1","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`),
	})
	ev := s.Ingest(plainResult(4, "2"))
	if !strings.Contains(ev.Warning, "missing required resultType") {
		t.Fatalf("the retry's answer is judged by the retry's own revision: %q", ev.Warning)
	}
}

// TestTruncatedRequestDoesNotOrphanItsResponse. A request past the frame-size cap
// is stored as a truncated frame and opens no call, so its ordinary response had
// nothing to match and was reported as orphaned. The traffic was correct in both
// directions and the gap was mcpsnoop's own, which is the same reasoning that
// keeps truncation itself out of the warning field.
func TestTruncatedRequestDoesNotOrphanItsResponse(t *testing.T) {
	s := New()
	t0 := time.Now()
	capped := req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"big"}`)
	capped.Truncated = true
	s.Ingest(capped)
	if ev := s.Ingest(resp(2, t0, proxy.ServerToClient, "1", `"result":{}`)); ev.Warning != "" {
		t.Fatalf("the response to a capped request must be silent, got %q", ev.Warning)
	}
	// A genuinely orphaned response after it is still reported.
	if ev := s.Ingest(resp(3, t0, proxy.ServerToClient, "99", `"result":{}`)); ev.Warning == "" {
		t.Fatal("a response with no request at all must still be reported")
	}
}

// TestTwoClientsThroughOneProxyKeepSeparateIDSpaces. One `mcpsnoop http` run
// carries every client that connects to it, and JSON-RPC scopes id uniqueness to
// the sender rather than to the wire, so two conforming clients both starting at
// id 1 were reported for reusing an id in flight and for answering it twice.
func TestTwoClientsThroughOneProxyKeepSeparateIDSpaces(t *testing.T) {
	conn := func(e proxy.Envelope, id string) proxy.Envelope {
		e.Transport = proxy.TransportHTTP
		e.ConnID = id
		return e
	}
	s := New()
	t0 := time.Now()
	a := s.Ingest(conn(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{}`), "10.0.0.1:5001"))
	b := s.Ingest(conn(req(2, t0, proxy.ClientToServer, "1", "tools/list", `{}`), "10.0.0.2:5002"))
	ra := s.Ingest(conn(resp(3, t0, proxy.ServerToClient, "1", `"result":{}`), "10.0.0.1:5001"))
	rb := s.Ingest(conn(resp(4, t0, proxy.ServerToClient, "1", `"result":{}`), "10.0.0.2:5002"))
	for name, ev := range map[string]EventView{"A req": a, "B req": b, "A resp": ra, "B resp": rb} {
		if ev.Warning != "" {
			t.Fatalf("%s warned on two conforming clients: %q", name, ev.Warning)
		}
	}

	// One client genuinely reusing an id in flight is still reported.
	s2 := New()
	s2.Ingest(conn(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{}`), "10.0.0.1:5001"))
	if ev := s2.Ingest(conn(req(2, t0, proxy.ClientToServer, "1", "tools/list", `{}`), "10.0.0.1:5001")); ev.Warning == "" {
		t.Fatal("one sender reusing an id in flight must still be reported")
	}
}

// TestNullResultResponseNamesTheRealViolation. Separating null-id requests means
// a null-id response matches none of them, and "response id has no matching
// request" then sends the reader after a frame that is not missing. The result
// carrying a null id is the actual violation, since the spec requires a result to
// carry the same id as its request. An error response stays exempt, because the
// spec lets one omit the id "in error cases where the ID could not be read due a
// malformed request".
func TestNullResultResponseNamesTheRealViolation(t *testing.T) {
	s := New()
	ev := s.Ingest(resp(1, time.Now(), proxy.ServerToClient, "null", `"result":{}`))
	if !strings.Contains(ev.Warning, "result response id is null") {
		t.Fatalf("warning = %q, want the null result id named", ev.Warning)
	}

	errored := New().Ingest(resp(1, time.Now(), proxy.ServerToClient, "null",
		`"error":{"code":-32700,"message":"parse error"}`))
	if strings.Contains(errored.Warning, "id is null") {
		t.Fatalf("an error response may omit the id, got %q", errored.Warning)
	}
}

// TestLateResultKeepsTheErrorAxis. A cancellation settles the call, but the
// answer that arrives afterwards is still the server's answer, and an error in
// it is still an error. Leaving it off the error axis made a real -32603 read as
// errors=0 and pass a default check run, which is a regression against the
// behaviour before cancellation had a state of its own.
func TestLateResultKeepsTheErrorAxis(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		wantErrored    bool
	}{
		{"protocol error", `"error":{"code":-32603,"message":"boom"}`, true},
		{"tool error", `"result":{"isError":true,"content":[]}`, true},
		{"an ordinary late result", `"result":{"content":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			t0 := time.Now()
			s.Ingest(req(1, t0, proxy.ClientToServer, "7", "tools/call", `{"name":"slow"}`))
			s.Ingest(notif(2, t0, proxy.ClientToServer, "notifications/cancelled", `{"requestId":7}`))
			s.Ingest(resp(3, t0.Add(time.Second), proxy.ServerToClient, "7", tc.response))

			calls := s.Calls("s1")
			if len(calls) != 1 || calls[0].State != Cancelled || !calls[0].LateResult {
				t.Fatalf("call = %+v, want one cancelled call with a late result", calls)
			}
			if calls[0].Errored != tc.wantErrored {
				t.Fatalf("errored = %v, want %v", calls[0].Errored, tc.wantErrored)
			}
			want := 0
			if tc.wantErrored {
				want = 1
			}
			if got := s.Sessions()[0].Errors; got != want {
				t.Fatalf("session errors = %d, want %d", got, want)
			}
		})
	}
}

// TestCancelLeavesATaskAugmentedCallAlone. A tools/call answered with a task
// handle stays Pending on purpose while the work continues under its taskId, and
// its terminal state arrives through notifications/tasks. Settling it on a
// cancellation threw that state away, so a task that ended failed reported
// errors=0. The spec puts this among the cancellations a server may ignore,
// "Processing has already completed", and cancelling the work is tasks/cancel.
func TestCancelLeavesATaskAugmentedCallAlone(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"slow"}`))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "3", `"result":{"resultType":"task","taskId":"t1","status":"working"}`))
	s.Ingest(notif(3, t0, proxy.ClientToServer, "notifications/cancelled", `{"requestId":3}`))
	s.Ingest(notif(4, t0.Add(time.Second), proxy.ServerToClient, "notifications/tasks",
		`{"taskId":"t1","status":"failed","error":{"code":-32603,"message":"boom"}}`))

	calls := s.Calls("s1")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	if calls[0].State == Cancelled {
		t.Fatal("a cancellation must not settle a call whose work continues under a task")
	}
	if calls[0].TaskStatus != "failed" || !calls[0].Errored {
		t.Fatalf("call = %+v, want the failed task state to have landed", calls[0])
	}
	if got := s.Sessions()[0].Errors; got != 1 {
		t.Fatalf("session errors = %d, want 1", got)
	}
}

// TestLateResultStillDeclaresWhatTheServerHas. The payload of a late response is
// still the server's answer. Dropping the per-method side effects left a late
// tools/list with an empty inventory, and `check --fail-on drift` then compared
// nothing and printed green.
func TestLateResultStillDeclaresWhatTheServerHas(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{}`))
	s.Ingest(notif(2, t0, proxy.ClientToServer, "notifications/cancelled", `{"requestId":1}`))
	s.Ingest(resp(3, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}],"resultType":"complete"}`))

	defs, ok := s.ToolDefinitions("s1")
	if !ok || len(defs) != 1 {
		t.Fatalf("tool definitions = %v %d, want the late listing to have landed", ok, len(defs))
	}
}

// TestGracefulSubscriptionClosureIsNotALateResult. A server tearing down a
// subscription sends notifications/cancelled and then, per the spec, "SHOULD
// respond to the original subscriptions/listen request with an empty result
// before closing the stream". That result is the intended end of the exchange, so
// it must be neither a late result nor the duplicate the response branch used to
// report on it.
func TestGracefulSubscriptionClosureIsNotALateResult(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "subscriptions/listen",
		`{"notifications":{"toolsListChanged":true}}`))
	s.Ingest(notif(2, t0.Add(time.Second), proxy.ServerToClient, "notifications/cancelled",
		`{"requestId":1,"reason":"shutdown"}`))
	ev := s.Ingest(resp(3, t0.Add(2*time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"complete"}`))

	if ev.Warning != "" {
		t.Fatalf("the graceful closure must be silent, got %q", ev.Warning)
	}
	calls := s.Calls("s1")
	if len(calls) != 1 || calls[0].State != Cancelled || calls[0].LateResult {
		t.Fatalf("call = %+v, want one cancelled call with no late result", calls)
	}
	if h := s.Sessions()[0]; h.LateResults != 0 {
		t.Fatalf("late results = %d, want 0", h.LateResults)
	}
}

// TestPercentilesSkipCallsThatNeverAnswered. A superseded or cancelled call has
// no end, so its duration is a negative epoch span. Letting either into the
// aggregate put -2562047h47m16.854775808s into the per-tool percentiles and the
// slowest-calls list. A cancelled call whose result arrived late does have a real
// duration and must still count.
func TestPercentilesSkipCallsThatNeverAnswered(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
	s.Ingest(notif(2, t0, proxy.ClientToServer, "notifications/cancelled", `{"requestId":1}`))
	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/call", `{"name":"slow"}`))
	s.Ingest(notif(4, t0, proxy.ClientToServer, "notifications/cancelled", `{"requestId":2}`))
	s.Ingest(resp(5, t0.Add(2*time.Second), proxy.ServerToClient, "2", `"result":{"content":[]}`))

	summary, ok := s.ToolSummary("s1")
	if !ok || len(summary.Tools) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	tool := summary.Tools[0]
	if tool.P50 < 0 || tool.P95 < 0 || tool.P99 < 0 {
		t.Fatalf("a call that never answered reached the percentiles: %+v", tool)
	}
	// Only the one that did answer contributes, and it contributes its real span.
	if tool.P50 != 2*time.Second {
		t.Fatalf("p50 = %v, want the late result's own 2s", tool.P50)
	}
	for _, slow := range summary.Slowest {
		if slow.Duration < 0 {
			t.Fatalf("slowest calls carry a negative duration: %+v", slow)
		}
	}
}

// TestBoundedStoreReleasesOldBodiesAndKeepsEveryCount. A hub left watching a
// chatty server grew until it was killed, because nothing bounded what the store
// kept and every event held the full observed frame. The bound is on bytes, not
// on history: every frame keeps its row and its verdict, and only the bodies of
// the oldest ones go.
func TestBoundedStoreReleasesOldBodiesAndKeepsEveryCount(t *testing.T) {
	const pairs = 200
	const limit = 8 << 10

	t0 := time.Now() // shared, so the two headers differ only if a count does
	build := func(s *Store) *Store {
		big := strings.Repeat("x", 512)
		for i := range pairs {
			id := strconv.Itoa(i + 1)
			s.Ingest(req(uint64(2*i+1), t0, proxy.ClientToServer, id, "tools/call",
				`{"name":"echo","arguments":{"text":"`+big+`"}}`))
			s.Ingest(resp(uint64(2*i+2), t0, proxy.ServerToClient, id,
				`"result":{"content":[{"type":"text","text":"`+big+`"}]}`))
		}
		return s
	}
	held := func(s *Store) int {
		n := 0
		for _, sess := range s.sessions {
			for _, ev := range sess.events {
				n += len(ev.raw) + len(ev.text)
			}
			for _, c := range sess.calls {
				n += len(c.params) + len(c.result)
			}
		}
		return n
	}

	full, bounded := build(New()), build(NewBounded(limit, 0))

	if held(bounded) > limit {
		t.Fatalf("bounded store holds %d bytes of bodies, over its %d budget", held(bounded), limit)
	}
	if held(full) <= limit {
		t.Fatalf("the fixture does not exceed the budget, so it proves nothing: %d bytes", held(full))
	}

	// Nothing a reader counts may move. The frames, the calls, the session
	// counters and the context-cost figures all have to match the unbounded store
	// exactly, or a bound has quietly turned into an under-report.
	a, b := full.Timeline("s1"), bounded.Timeline("s1")
	if len(a) != len(b) {
		t.Fatalf("frames = %d bounded against %d unbounded", len(b), len(a))
	}
	if len(full.Calls("s1")) != len(bounded.Calls("s1")) {
		t.Fatalf("calls = %d bounded against %d unbounded", len(bounded.Calls("s1")), len(full.Calls("s1")))
	}
	if got, want := bounded.Sessions()[0], full.Sessions()[0]; got != want {
		t.Fatalf("session header differs:\n%+v\n%+v", got, want)
	}
	fullSum, _ := full.ToolSummary("s1")
	boundSum, _ := bounded.ToolSummary("s1")
	if !reflect.DeepEqual(fullSum.Tools, boundSum.Tools) {
		t.Fatalf("tool statistics differ once bodies were released:\n%+v\n%+v", boundSum.Tools, fullSum.Tools)
	}

	// The oldest frames say their body is gone rather than reading as a server
	// that sent nothing, and the newest still carry theirs.
	released := 0
	for _, e := range b {
		if e.BodyReleased {
			released++
			if len(e.Raw) != 0 || e.Text != "" {
				t.Fatal("a frame marked released still holds its body")
			}
		}
	}
	if released == 0 {
		t.Fatal("nothing was released, so the budget was never enforced")
	}
	if b[len(b)-1].BodyReleased {
		t.Fatal("the newest frame lost its body, so eviction is not oldest-first")
	}
}

// TestUnboundedStoreIsWhatCheckAndExportBuild. check reads a finite log once and
// gates on what it finds, so a bound there would make it under-report on a large
// capture, which is the one thing a gate must never do.
func TestUnboundedStoreIsWhatCheckAndExportBuild(t *testing.T) {
	s := New()
	t0 := time.Now()
	big := strings.Repeat("y", 4096)
	for i := range 50 {
		id := strconv.Itoa(i + 1)
		s.Ingest(req(uint64(2*i+1), t0, proxy.ClientToServer, id, "tools/call", `{"name":"echo","arguments":{"t":"`+big+`"}}`))
		s.Ingest(resp(uint64(2*i+2), t0, proxy.ServerToClient, id, `"result":{"content":[{"type":"text","text":"`+big+`"}]}`))
	}
	for _, e := range s.Timeline("s1") {
		if e.BodyReleased {
			t.Fatal("an unbounded store released a body")
		}
		if len(e.Raw) == 0 {
			t.Fatal("an unbounded store dropped a frame's bytes")
		}
	}
}

// TestBoundedStoreAccountingStaysConsistent. The release walks a cursor per
// session while one running total decides when to stop, so the two have to agree
// or the loop either stops early and the budget is a lie, or runs off the end of
// a timeline. A budget of one byte forces every frame through the path.
func TestBoundedStoreAccountingStaysConsistent(t *testing.T) {
	s := NewBounded(1, 0)
	t0 := time.Now()
	body := strings.Repeat("z", 256)

	// Two sessions interleaved, since release is oldest-first across the store
	// rather than within one session, and a hub carries many at once.
	for i := range 40 {
		id := strconv.Itoa(i + 1)
		for _, sid := range []string{"s1", "s2"} {
			r := req(uint64(2*i+1), t0.Add(time.Duration(i)*time.Millisecond), proxy.ClientToServer, id, "tools/call",
				`{"name":"echo","arguments":{"t":"`+body+`"}}`)
			r.SessionID = sid
			s.Ingest(r)
			p := resp(uint64(2*i+2), t0.Add(time.Duration(i)*time.Millisecond), proxy.ServerToClient, id,
				`"result":{"content":[{"type":"text","text":"`+body+`"}]}`)
			p.SessionID = sid
			s.Ingest(p)
		}
	}

	// Everything but the newest frame is releasable, so the total must come down
	// to what that one frame weighs and no further.
	if s.payloadBytes < 0 {
		t.Fatalf("the running total went negative at %d, so a frame was released twice", s.payloadBytes)
	}
	var held int
	for _, sess := range s.sessions {
		if sess.evictCursor > len(sess.events) {
			t.Fatalf("cursor %d is past the %d frames it indexes", sess.evictCursor, len(sess.events))
		}
		for _, ev := range sess.events {
			held += bodyWeight(ev)
		}
		if sess.bodyBytes < 0 {
			t.Fatalf("session total went negative at %d", sess.bodyBytes)
		}
	}
	if held != s.payloadBytes {
		t.Fatalf("the store says %d bytes are held, the frames hold %d", s.payloadBytes, held)
	}
	for _, sid := range []string{"s1", "s2"} {
		if got := len(s.Timeline(sid)); got != 80 {
			t.Fatalf("%s has %d frames, want every one of them kept", sid, got)
		}
	}
}

// TestBoundedStoreDropsOldFramesAndKeepsTheToolStatistics. Releasing bodies
// bounds the bytes but not the count, and a frame whose body is gone still costs
// its own struct, so a chatty stream of small notifications grew without ever
// reaching the byte budget. The oldest frames go entirely past a frame limit,
// and what they said about a tool call is folded into the running statistics
// first, or the tool summary would quietly describe only recent calls.
func TestBoundedStoreDropsOldFramesAndKeepsTheToolStatistics(t *testing.T) {
	const pairs = 300
	const keepFrames = 100

	t0 := time.Now() // shared, so the two summaries differ only if a figure does
	build := func(s *Store) *Store {
		for i := range pairs {
			id := strconv.Itoa(i + 1)
			ts := t0.Add(time.Duration(i) * time.Millisecond)
			s.Ingest(req(uint64(2*i+1), ts, proxy.ClientToServer, id, "tools/call", `{"name":"echo","arguments":{"i":`+id+`}}`))
			s.Ingest(resp(uint64(2*i+2), ts.Add(time.Millisecond), proxy.ServerToClient, id, `"result":{"content":[{"type":"text","text":"ok"}]}`))
		}
		return s
	}

	full := build(New())
	bounded := build(NewBounded(0, keepFrames))

	if got := len(bounded.Timeline("s1")); got != keepFrames {
		t.Fatalf("timeline holds %d frames, want the %d cap", got, keepFrames)
	}
	if got := len(full.Timeline("s1")); got != 2*pairs {
		t.Fatalf("the unbounded store dropped frames: %d", got)
	}

	// The statistics are the whole point. Every call the session made has to be in
	// them, whether or not its frames are still held.
	fullSum, _ := full.ToolSummary("s1")
	boundSum, _ := bounded.ToolSummary("s1")
	if !reflect.DeepEqual(fullSum.Tools, boundSum.Tools) {
		t.Fatalf("tool statistics shrank with the timeline:\n bounded %+v\n full    %+v", boundSum.Tools, fullSum.Tools)
	}
	if len(boundSum.Tools) == 0 || boundSum.Tools[0].Calls != pairs {
		t.Fatalf("the summary counts %+v, want all %d calls", boundSum.Tools, pairs)
	}
	if !reflect.DeepEqual(fullSum.Slowest, boundSum.Slowest) {
		t.Fatalf("the slowest calls differ:\n bounded %+v\n full    %+v", boundSum.Slowest, fullSum.Slowest)
	}

	// The session counters come from the ingest path, not from the timeline, so
	// dropping frames must not move them either.
	if got, want := bounded.Sessions()[0].Requests, full.Sessions()[0].Requests; got != want {
		t.Fatalf("requests = %d bounded against %d unbounded", got, want)
	}

	// A settled call nothing points at any more is not left in the map.
	sess := bounded.sessions["s1"]
	if len(sess.calls) > keepFrames {
		t.Fatalf("%d calls retained for %d frames, so the map grows unbounded too", len(sess.calls), keepFrames)
	}
}

// TestBothBoundsHoldTogether. The two bounds move the same timeline from both
// ends, the body release walking a cursor forward and the frame cap removing
// entries from the front, so the cursor has to be rebased as the slice shifts or
// it points at a frame that has already moved. This drives both at once and
// checks the accounting still describes the frames that are actually there.
func TestBothBoundsHoldTogether(t *testing.T) {
	s := NewBounded(4<<10, 60)
	t0 := time.Now()
	body := strings.Repeat("q", 400)

	for i := range 300 {
		id := strconv.Itoa(i + 1)
		ts := t0.Add(time.Duration(i) * time.Millisecond)
		s.Ingest(req(uint64(2*i+1), ts, proxy.ClientToServer, id, "tools/call",
			`{"name":"echo","arguments":{"t":"`+body+`"}}`))
		s.Ingest(resp(uint64(2*i+2), ts.Add(time.Millisecond), proxy.ServerToClient, id,
			`"result":{"content":[{"type":"text","text":"`+body+`"}]}`))

		sess := s.sessions["s1"]
		if sess.evictCursor > len(sess.events) {
			t.Fatalf("after %d pairs the cursor is %d against %d frames", i, sess.evictCursor, len(sess.events))
		}
		held := 0
		for _, ev := range sess.events {
			held += bodyWeight(ev)
		}
		if held != s.payloadBytes {
			t.Fatalf("after %d pairs the store says %d bytes, the frames hold %d", i, s.payloadBytes, held)
		}
		// Everything before the cursor has already given up its body, or the cursor
		// is not describing what it claims to.
		for j := range sess.evictCursor {
			if len(sess.events[j].raw) != 0 {
				t.Fatalf("frame %d is behind the cursor and still holds its body", j)
			}
		}
	}

	if got := len(s.Timeline("s1")); got != 60 {
		t.Fatalf("timeline holds %d frames, want the 60 cap", got)
	}
	if s.payloadBytes > 4<<10 {
		t.Fatalf("bodies weigh %d, over the 4 KiB budget", s.payloadBytes)
	}
	// Every call still reaches the summary, whichever bound removed its frames.
	sum, _ := s.ToolSummary("s1")
	if len(sum.Tools) != 1 || sum.Tools[0].Calls != 300 {
		t.Fatalf("summary = %+v, want all 300 calls", sum.Tools)
	}
}

// storeBudgetHolds checks the accounting against the frames that are actually
// there. The two counters are store-wide while the work that changes them is
// per session, so every path that moves a frame has to settle both, and the way
// this goes wrong is silent: the store believes it is fuller than it is and
// starts releasing bodies it did not need to.
func storeBudgetHolds(t *testing.T, s *Store, where string) {
	t.Helper()
	bytes, frames := 0, 0
	for _, sess := range s.sessions {
		sessBytes := 0
		for _, ev := range sess.events {
			sessBytes += bodyWeight(ev)
		}
		if sess.bodyBytes != sessBytes {
			t.Fatalf("%s: session %s accounts %d bytes, its frames weigh %d", where, sess.id, sess.bodyBytes, sessBytes)
		}
		bytes += sessBytes
		frames += len(sess.events)
	}
	if s.payloadBytes != bytes {
		t.Fatalf("%s: store accounts %d bytes, the frames weigh %d, %d phantom", where, s.payloadBytes, bytes, s.payloadBytes-bytes)
	}
	if s.frames != frames {
		t.Fatalf("%s: store counts %d frames, holds %d", where, s.frames, frames)
	}
}

// TestBoundedStoreGivesTheBudgetBack. Both counters are store-wide, so any path
// that removes a frame without settling them leaves budget nothing can ever
// release. The two that did were deleting a session, which the TUI does on
// ctrl+d, and the frame cap dropping a frame the byte budget had not reached
// yet. Neither had a test: the existing ones either delete from an unbounded
// store or set a byte budget so tight that the release always runs first.
func TestBoundedStoreGivesTheBudgetBack(t *testing.T) {
	// A roomy byte budget with a frame cap that bites, which is the shape the
	// live TUI actually runs (64 MiB against 200,000 frames) and the shape no
	// other test covers.
	s := NewBounded(1<<30, 12)
	t0 := time.Now()
	frame := func(session string, seq uint64, size int) proxy.Envelope {
		return proxy.Envelope{
			SessionID: session, ServerLabel: "srv", Seq: seq,
			TS:        t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"t":"` +
				strings.Repeat("x", size) + `"}}`),
		}
	}

	for i := range 40 {
		s.Ingest(frame("a", uint64(i+1), 200))
		storeBudgetHolds(t, s, "ingest into one session")
	}
	// The cap is doing the work here, not the byte budget, or the case this test
	// exists for is not the case being run.
	if got := len(s.Timeline("a")); got != 12 {
		t.Fatalf("timeline holds %d frames, want the cap of 12", got)
	}
	if s.payloadBytes == 0 {
		t.Fatal("every body was released, so the byte budget bit and the frame cap did not")
	}

	// A second session, so the cap starts moving frames between sessions.
	for i := range 20 {
		s.Ingest(frame("b", uint64(i+41), 300))
		storeBudgetHolds(t, s, "ingest into a second session")
	}

	// Deleting a session has to hand back what it was holding.
	s.Delete("a")
	storeBudgetHolds(t, s, "after deleting a session")
	s.Delete("b")
	storeBudgetHolds(t, s, "after deleting the last session")
	if s.payloadBytes != 0 || s.frames != 0 {
		t.Fatalf("an empty store still accounts %d bytes and %d frames", s.payloadBytes, s.frames)
	}

	// And the store still works afterwards. This is the symptom that made it
	// worth finding: with the budget still spent on a deleted session, every
	// frame ingested next was released the moment it arrived and the inspector
	// showed no payload at all for the rest of the process.
	for i := range 5 {
		s.Ingest(frame("c", uint64(i+100), 200))
	}
	storeBudgetHolds(t, s, "after reusing the store")
	for _, ev := range s.Timeline("c") {
		if ev.BodyReleased {
			t.Fatal("a fresh frame was released immediately, so the budget never came back")
		}
	}
}

// TestDeleteOnAnUnboundedStoreTouchesNoBudget. check, export, diff and open all
// build an unbounded store, and giving back budget it never took would make the
// counters negative.
func TestDeleteOnAnUnboundedStoreTouchesNoBudget(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{}`))
	s.Delete("s1")
	if s.payloadBytes != 0 || s.frames != 0 {
		t.Fatalf("an unbounded store moved its budget counters to %d/%d", s.payloadBytes, s.frames)
	}
}

// TestSessionCarriesItsTransportAndEndpoint covers the whole path a capture uses
// to say what it captured. The label cannot say it: the http command defaults it
// to the target host alone, so two proxies pointed at two paths of one host
// produce the same one.
func TestSessionCarriesItsTransportAndEndpoint(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	meta, err := json.Marshal(proxy.SessionMeta{Target: "https://api.example.com/tenant-a/mcp"})
	if err != nil {
		t.Fatal(err)
	}

	s := New()
	s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "api.example.com", Seq: 1, TS: base, Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: meta})
	e := req(2, base.Add(time.Millisecond), proxy.ClientToServer, "1", "tools/list", "")
	e.Transport = proxy.TransportHTTP
	s.Ingest(e)

	headers := s.Sessions()
	if len(headers) != 1 {
		t.Fatalf("sessions = %d, want 1", len(headers))
	}
	if got := headers[0].Transport; got != proxy.TransportHTTP {
		t.Fatalf("Transport = %q, want %q", got, proxy.TransportHTTP)
	}
	if got, want := headers[0].Endpoint, "https://api.example.com/tenant-a/mcp"; got != want {
		t.Fatalf("Endpoint = %q, want %q", got, want)
	}
}

// TestAStdioSessionClaimsNoEndpoint keeps the absence meaningful. A reader that
// sees an endpoint has to be able to conclude the session was captured over HTTP.
func TestAStdioSessionClaimsNoEndpoint(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "server.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}

	s := New()
	s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: base, Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta})

	headers := s.Sessions()
	if len(headers) != 1 || headers[0].Endpoint != "" {
		t.Fatalf("stdio session reported endpoint %q, want none", headers[0].Endpoint)
	}
	if headers[0].Transport != proxy.TransportStdio {
		t.Fatalf("Transport = %q, want %q", headers[0].Transport, proxy.TransportStdio)
	}
	if cmd, cwd, ok := s.Command("s1"); !ok || len(cmd) != 2 || cwd != "/srv" {
		t.Fatalf("Command = %v, %q, %v, want the recorded stdio command", cmd, cwd, ok)
	}
}

// TestAPreEndpointHTTPLogStaysReadable is the forward-compatibility rule that
// SessionMeta grows by. A log written before mcpsnoop recorded the target has no
// target, and that must read as an absence rather than as a decode failure that
// loses the command beside it.
func TestAPreEndpointHTTPLogStaysReadable(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	s := New()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: base,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP,
		Raw: json.RawMessage(`{"command":null,"future_field":{"nested":1}}`),
	})

	headers := s.Sessions()
	if len(headers) != 1 {
		t.Fatalf("sessions = %d, want 1", len(headers))
	}
	if headers[0].Endpoint != "" {
		t.Fatalf("Endpoint = %q, want empty for a log that predates the field", headers[0].Endpoint)
	}
	if headers[0].Transport != proxy.TransportHTTP {
		t.Fatalf("Transport = %q, want the frame's own", headers[0].Transport)
	}
}

// TestAFrameWithNoTransportLeavesTheKnownOneAlone is why the transport is taken
// from the first frame that names one rather than from the latest. Not every
// envelope carries the field, and an unconditional assignment lets a frame that
// knows nothing erase what the meta frame already said.
func TestAFrameWithNoTransportLeavesTheKnownOneAlone(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	s := New()
	s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: base, Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: json.RawMessage(`{}`)})
	s.Ingest(req(2, base.Add(time.Millisecond), proxy.ClientToServer, "1", "tools/list", "")) // no Transport set

	headers := s.Sessions()
	if len(headers) != 1 || headers[0].Transport != proxy.TransportHTTP {
		t.Fatalf("Transport = %q, want it to survive a frame that named none", headers[0].Transport)
	}
}

// TestToolSummarySplitsBrokenServerFromReportedFailure covers the reason the two
// counts exist. A tool answering isError is doing its job, a server answering a
// JSON-RPC error is broken, and one number for both makes the first look like
// the second.
func TestToolSummarySplitsBrokenServerFromReportedFailure(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	// search answers isError twice: it looked, and found nothing.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"search"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[],"isError":true}`))
	s.Ingest(req(3, t0.Add(2*time.Millisecond), proxy.ClientToServer, "2", "tools/call", `{"name":"search"}`))
	s.Ingest(resp(4, t0.Add(3*time.Millisecond), proxy.ServerToClient, "2", `"result":{"content":[],"isError":true}`))
	// write is broken: the server returns a JSON-RPC error.
	s.Ingest(req(5, t0.Add(4*time.Millisecond), proxy.ClientToServer, "3", "tools/call", `{"name":"write"}`))
	s.Ingest(resp(6, t0.Add(5*time.Millisecond), proxy.ServerToClient, "3", `"error":{"code":-32603,"message":"boom"}`))
	// mixed carries one of each.
	s.Ingest(req(7, t0.Add(6*time.Millisecond), proxy.ClientToServer, "4", "tools/call", `{"name":"mixed"}`))
	s.Ingest(resp(8, t0.Add(7*time.Millisecond), proxy.ServerToClient, "4", `"result":{"content":[],"isError":true}`))
	s.Ingest(req(9, t0.Add(8*time.Millisecond), proxy.ClientToServer, "5", "tools/call", `{"name":"mixed"}`))
	s.Ingest(resp(10, t0.Add(9*time.Millisecond), proxy.ServerToClient, "5", `"error":{"code":-32603,"message":"boom"}`))

	summary, ok := s.ToolSummary("s1")
	if !ok {
		t.Fatal("no tool summary")
	}
	byName := make(map[string]ToolStats, len(summary.Tools))
	for _, tool := range summary.Tools {
		byName[tool.Name] = tool
	}
	for _, want := range []ToolStats{
		{Name: "search", Errors: 2, ProtocolErrors: 0, ToolErrors: 2},
		{Name: "write", Errors: 1, ProtocolErrors: 1, ToolErrors: 0},
		{Name: "mixed", Errors: 2, ProtocolErrors: 1, ToolErrors: 1},
	} {
		got, found := byName[want.Name]
		if !found {
			t.Fatalf("tool %q is missing from the summary", want.Name)
		}
		if got.Errors != want.Errors || got.ProtocolErrors != want.ProtocolErrors || got.ToolErrors != want.ToolErrors {
			t.Fatalf("%s = %d errors (%d protocol, %d tool), want %d (%d, %d)",
				want.Name, got.Errors, got.ProtocolErrors, got.ToolErrors,
				want.Errors, want.ProtocolErrors, want.ToolErrors)
		}
	}
}

// TestEveryToolErrorLandsInExactlyOneBucket is the invariant the split is built
// to hold rather than to be remembered. ProtocolErrors is the complement of
// ToolErrors, not its own tally, so the third way errored is set, a task that
// ends failed carrying neither an error object nor isError, is counted without
// anyone having had to think of it here.
func TestEveryToolErrorLandsInExactlyOneBucket(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s := New()

	// A JSON-RPC error and an isError, one of each of the two obvious kinds.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"t"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"error":{"code":-32603,"message":"boom"}`))
	s.Ingest(req(3, t0.Add(2*time.Millisecond), proxy.ClientToServer, "2", "tools/call", `{"name":"t"}`))
	s.Ingest(resp(4, t0.Add(3*time.Millisecond), proxy.ServerToClient, "2", `"result":{"content":[],"isError":true}`))
	// A plain success, which must land in neither.
	s.Ingest(req(5, t0.Add(4*time.Millisecond), proxy.ClientToServer, "3", "tools/call", `{"name":"t"}`))
	s.Ingest(resp(6, t0.Add(5*time.Millisecond), proxy.ServerToClient, "3", `"result":{"content":[]}`))
	// The third kind. The call is parked as a task and the task later ends failed
	// with no error object and no isError, so nothing about it says which bucket
	// it belongs in.
	s.Ingest(req(7, t0.Add(6*time.Millisecond), proxy.ClientToServer, "4", "tools/call", `{"name":"t"}`))
	s.Ingest(resp(8, t0.Add(7*time.Millisecond), proxy.ServerToClient, "4", `"result":{"resultType":"task","taskId":"t1","status":"working"}`))
	s.Ingest(req(9, t0.Add(8*time.Millisecond), proxy.ClientToServer, "5", "tasks/get", `{"taskId":"t1"}`))
	s.Ingest(resp(10, t0.Add(9*time.Millisecond), proxy.ServerToClient, "5", `"result":{"taskId":"t1","status":"failed"}`))

	summary, ok := s.ToolSummary("s1")
	if !ok {
		t.Fatal("no tool summary")
	}
	if len(summary.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(summary.Tools))
	}
	got := summary.Tools[0]
	// Four calls went wrong, and only one of them was the tool saying so.
	if got.Errors != 3 || got.ToolErrors != 1 || got.ProtocolErrors != 2 {
		t.Fatalf("errors = %d (%d protocol, %d tool), want 3 (2, 1); the undiagnosed task failure has to land somewhere",
			got.Errors, got.ProtocolErrors, got.ToolErrors)
	}
	if got.ProtocolErrors+got.ToolErrors != got.Errors {
		t.Fatalf("%d + %d does not add up to the %d total", got.ProtocolErrors, got.ToolErrors, got.Errors)
	}
}

// mrtrProbe builds a session with n MRTR exchanges the client walked away from,
// then one genuine exchange on the same tool that the client does complete. The
// abandoned ones carry a requestState and the genuine one does not, which is the
// ordinary shape: a server that mints state for the operations it expects back,
// and one it answers statelessly.
func mrtrProbe(t *testing.T, abandoned int) (*Store, *session) {
	t.Helper()
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	var seq uint64
	ask := func(id, state string) {
		seq++
		s.Ingest(req(seq, t0.Add(time.Duration(seq)*time.Millisecond), proxy.ClientToServer, id, "tools/call", `{"name":"delete_files","arguments":{}}`))
		seq++
		s.Ingest(resp(seq, t0.Add(time.Duration(seq)*time.Millisecond), proxy.ServerToClient, id,
			fmt.Sprintf(`"result":{"resultType":"input_required","requestState":%q,"inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}`, state)))
	}
	for i := range abandoned {
		ask(fmt.Sprintf("%d", i), fmt.Sprintf("state-%d", i))
	}
	// The genuine exchange, answered and retried the way the pattern intends.
	seq++
	s.Ingest(req(seq, t0.Add(time.Duration(seq)*time.Millisecond), proxy.ClientToServer, "200", "tools/call", `{"name":"delete_files","arguments":{}}`))
	seq++
	s.Ingest(resp(seq, t0.Add(time.Duration(seq)*time.Millisecond), proxy.ServerToClient, "200",
		`"result":{"resultType":"input_required","inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}`))
	seq++
	s.Ingest(req(seq, t0.Add(time.Duration(seq)*time.Millisecond), proxy.ClientToServer, "201", "tools/call",
		`{"name":"delete_files","arguments":{},"inputResponses":{"k1":{"result":{}}}}`))
	seq++
	s.Ingest(resp(seq, t0.Add(time.Duration(seq)*time.Millisecond), proxy.ServerToClient, "201", `"result":{"content":[]}`))
	return s, s.sessions["s1"]
}

// TestAnAbandonedExchangeDoesNotBreakTheNextOne is the defect the two-pass
// fallback fixes. MRTR says servers "MUST NOT assume that clients will fulfill
// the inputRequests or retry the original request", so an abandoned exchange is
// an ordinary outcome, and one of them was enough to make the next conforming
// retry on the same tool look ambiguous. The retry then stayed its own call, and
// the operation was counted and timed as two, which is exactly what correlating
// MRTR was written to prevent.
func TestAnAbandonedExchangeDoesNotBreakTheNextOne(t *testing.T) {
	for _, abandoned := range []int{0, 1, 10} {
		t.Run(fmt.Sprintf("%d abandoned", abandoned), func(t *testing.T) {
			s, _ := mrtrProbe(t, abandoned)
			summary, ok := s.ToolSummary("s1")
			if !ok {
				t.Fatal("no tool summary")
			}
			var calls int
			for _, tool := range summary.Tools {
				if tool.Name == "delete_files" {
					calls = tool.Calls
				}
			}
			// One call per abandoned root, plus one for the completed exchange. The
			// retry must not add a second.
			if want := abandoned + 1; calls != want {
				t.Fatalf("delete_files calls = %d, want %d; the retry was not linked to the operation it continues", calls, want)
			}
		})
	}
}

// TestParkedOperationsDoNotAccumulate covers the other half. A settled operation
// is not awaiting a retry, and an abandoned one produces no frame to retire it
// by, so the list is append only without both a prune and a cap.
func TestParkedOperationsDoNotAccumulate(t *testing.T) {
	t.Run("a settled operation is retired", func(t *testing.T) {
		t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
		s := New()
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"delete_files","arguments":{}}`))
		s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
			`"result":{"resultType":"input_required","requestState":"blob","inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}`))
		sess := s.sessions["s1"]
		if len(sess.awaiting) != 1 {
			t.Fatalf("awaiting = %d, want the operation parked", len(sess.awaiting))
		}
		// The user declines, and the client says so.
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0.Add(2 * time.Millisecond),
			Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"user declined"}}`)})
		if sess.awaiting[0].state != Cancelled {
			t.Fatalf("the cancel did not settle the call, state = %v", sess.awaiting[0].state)
		}
		// A later exchange is what runs the prune, since that is where the list grows.
		s.Ingest(req(4, t0.Add(3*time.Millisecond), proxy.ClientToServer, "2", "tools/call", `{"name":"other","arguments":{}}`))
		s.Ingest(resp(5, t0.Add(4*time.Millisecond), proxy.ServerToClient, "2",
			`"result":{"resultType":"input_required","requestState":"blob2","inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}`))
		if len(sess.awaiting) != 1 {
			t.Fatalf("awaiting = %d, want only the live operation; a cancelled one waits for nothing", len(sess.awaiting))
		}
		if sess.awaiting[0].state != Pending {
			t.Fatalf("the retired entry is the wrong one, kept state = %v", sess.awaiting[0].state)
		}
	})

	t.Run("abandoned operations are capped", func(t *testing.T) {
		_, sess := mrtrProbe(t, 3*awaitingKept)
		if len(sess.awaiting) > awaitingKept {
			t.Fatalf("awaiting = %d, want at most the cap of %d", len(sess.awaiting), awaitingKept)
		}
		if len(sess.awaiting) == 0 {
			t.Fatal("the cap retired everything, so no retry could ever link again")
		}
		for i, c := range sess.awaiting {
			if c.state != Pending {
				t.Fatalf("entry %d is %v, so a settled call survived the prune", i, c.state)
			}
		}
		// The oldest go first, since the oldest requestState is the one the server
		// itself is likeliest to have expired.
		abandoned := 3 * awaitingKept
		if got := sess.awaiting[len(sess.awaiting)-1].mrtrState; got != fmt.Sprintf("state-%d", abandoned-1) {
			t.Fatalf("newest kept = %q, want the most recent exchange to survive", got)
		}
		if got := sess.awaiting[0].mrtrState; got == "state-0" {
			t.Fatalf("oldest kept = %q, want the front of the list to have been retired", got)
		}
	})
}

// TestTheParkingCapSaysWhatItRetired keeps the bound from being silent. Retiring
// a settled operation loses nothing, but one dropped while still open can no
// longer be linked to, so a retry that does arrive for it reads as its own call
// and the operation is counted and timed as two. That is what correlating MRTR
// exists to prevent, so a session it happened in has to be able to say so.
func TestTheParkingCapSaysWhatItRetired(t *testing.T) {
	t.Run("a clean session retires nothing", func(t *testing.T) {
		s, _ := mrtrProbe(t, 1)
		if got := s.Sessions()[0].RetiredExchanges; got != 0 {
			t.Fatalf("RetiredExchanges = %d, want 0; one abandoned exchange is far below the cap", got)
		}
	})

	t.Run("the cap reports the open ones it dropped", func(t *testing.T) {
		abandoned := 3 * awaitingKept
		s, sess := mrtrProbe(t, abandoned)
		got := s.Sessions()[0].RetiredExchanges
		if got == 0 {
			t.Fatal("the cap dropped open operations and said nothing")
		}
		// Everything parked and then dropped is accounted for: what was abandoned,
		// less what is still held.
		if want := abandoned - len(sess.awaiting); got != want {
			t.Fatalf("RetiredExchanges = %d, want %d abandoned less the %d still parked", got, want, len(sess.awaiting))
		}
	})

	t.Run("retiring a settled operation is not reported as a loss", func(t *testing.T) {
		t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
		s := New()
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"a","arguments":{}}`))
		s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1",
			`"result":{"resultType":"input_required","requestState":"blob","inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}`))
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0.Add(2 * time.Millisecond),
			Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"user declined"}}`)})
		s.Ingest(req(4, t0.Add(3*time.Millisecond), proxy.ClientToServer, "2", "tools/call", `{"name":"b","arguments":{}}`))
		s.Ingest(resp(5, t0.Add(4*time.Millisecond), proxy.ServerToClient, "2",
			`"result":{"resultType":"input_required","requestState":"blob2","inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}`))
		if got := s.Sessions()[0].RetiredExchanges; got != 0 {
			t.Fatalf("RetiredExchanges = %d, want 0; a cancelled operation waits for nothing and losing it costs nothing", got)
		}
	})
}

// TestARetiredOperationStopsBeingHeld is the other half of the parking cap. The
// cap stops an abandoned exchange from being a match candidate, but the call
// itself stayed in the session's call map, because dropOldestFrame refuses to
// forget a Pending one and a parked multi round-trip operation is Pending by
// design, so its duration can span the whole exchange.
//
// The result was a bounded store that was not bounded: a server answering every
// call with an input request grew it with each one, however small the frame
// window was. Measured before the fix at five thousand calls held against a two
// hundred frame window.
func TestARetiredOperationStopsBeingHeld(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const frames = 200
	const exchanges = 2000
	s := NewBounded(64<<10, frames)
	seq := uint64(0)
	emit := func(dir proxy.Direction, raw string) {
		seq++
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "d", Seq: seq,
			TS: t0.Add(time.Duration(seq) * time.Millisecond), Direction: dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)})
	}
	for i := range exchanges {
		id := fmt.Sprintf("c%d", i)
		emit(proxy.ClientToServer, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"t"}}`, id))
		emit(proxy.ServerToClient, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%q,"result":{"resultType":"input_required","requestState":"s%d","inputRequests":{"k":{"method":"elicitation/create","params":{"message":"m"}}}}}`, id, i))
	}

	sess := s.sessions["s1"]
	// Bounded by the frame window and the parking cap, not by how many exchanges
	// went unfinished. The window holds two frames per exchange, so a hundred
	// calls are still reachable through it.
	if held := len(sess.calls); held > frames {
		t.Fatalf("calls held = %d after %d abandoned exchanges, want no more than the %d-frame window",
			held, exchanges, frames)
	}
	if len(sess.awaiting) > awaitingKept {
		t.Fatalf("awaiting = %d, want at most the cap of %d", len(sess.awaiting), awaitingKept)
	}

	// Nothing about the session's verdict moved. Releasing a call the store can no
	// longer reach is a memory decision, and a reader is told what happened by the
	// pending count and by RetiredExchanges, both of which still count every one.
	header := s.Sessions()[0]
	if header.Pending != exchanges {
		t.Fatalf("pending = %d, want %d; forgetting a call must not change what the session reports",
			header.Pending, exchanges)
	}
	if header.RetiredExchanges != exchanges-awaitingKept {
		t.Fatalf("RetiredExchanges = %d, want %d", header.RetiredExchanges, exchanges-awaitingKept)
	}
}

// TestAPendingCallIsStillHeldWhileItCanBeAnswered keeps the release narrow. Only
// an operation the cap has retired may be forgotten, because only that one can
// never be continued. An ordinary call still waiting for its response has to stay
// reachable, or the response that arrives will open a second call instead of
// settling the first.
func TestAPendingCallIsStillHeldWhileItCanBeAnswered(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := NewBounded(64<<10, 4) // a window far smaller than the exchange
	seq := uint64(0)
	emit := func(dir proxy.Direction, raw string) {
		seq++
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "d", Seq: seq,
			TS: t0.Add(time.Duration(seq) * time.Millisecond), Direction: dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)})
	}
	emit(proxy.ClientToServer, `{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{"name":"t"}}`)
	// Enough traffic to push the request frame out of the window entirely.
	for range 10 {
		emit(proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
	}
	emit(proxy.ServerToClient, `{"jsonrpc":"2.0","id":"slow","result":{"content":[]}}`)

	header := s.Sessions()[0]
	if header.Pending != 0 {
		t.Fatalf("pending = %d, want 0; the response could not find the call it answers", header.Pending)
	}
	if header.Responses != 1 {
		t.Fatalf("responses = %d, want 1", header.Responses)
	}
}
