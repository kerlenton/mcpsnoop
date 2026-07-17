package store

import (
	"encoding/json"
	"fmt"
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

func TestActivityBuckets(t *testing.T) {
	st := New()
	now := time.Now()
	// Two frames in the most recent bucket, one about a minute old, and one well
	// outside the two minute window that must be ignored.
	st.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", ""))
	st.Ingest(req(2, now, proxy.ClientToServer, "2", "tools/list", ""))
	st.Ingest(req(3, now.Add(-60*time.Second), proxy.ClientToServer, "3", "tools/list", ""))
	st.Ingest(req(4, now.Add(-10*time.Minute), proxy.ClientToServer, "4", "tools/list", ""))

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
