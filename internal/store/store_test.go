package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

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

func TestCorrelationAndTiming(t *testing.T) {
	s := New(100 * time.Millisecond)
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
	if !c.Slow(100 * time.Millisecond) {
		t.Fatalf("call should be flagged slow (200ms > 100ms threshold)")
	}

	calls := s.Calls("s1")
	if len(calls) != 1 || calls[0].State != Completed {
		t.Fatalf("Calls() = %+v", calls)
	}
}

// TestDuplicateResponseDoesNotDoubleCountPending guards against a second
// response for an already-answered id decrementing the pending counter twice.
func TestDuplicateResponseDoesNotDoubleCountPending(t *testing.T) {
	s := New(0)
	t0 := time.Now()

	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo"}`))
	// First response completes the call; pending returns to zero.
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
	s := New(0)
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
	s := New(0)
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
	s := New(0)
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
	s := New(0)
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

func TestServerToClientRequest(t *testing.T) {
	// Server-initiated request (e.g. sampling) must correlate with the client's
	// response travelling the other way.
	s := New(0)
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ServerToClient, "99", "sampling/createMessage", `{}`))
	ev := s.Ingest(resp(2, t0.Add(5*time.Millisecond), proxy.ClientToServer, "99", `"result":{"ok":true}`))
	if ev.Call == nil || ev.Call.State != Completed {
		t.Fatalf("server->client request not correlated: %+v", ev.Call)
	}
}

func TestCapabilitiesCapture(t *testing.T) {
	s := New(0)
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

func TestNotificationAndUnmatchedResponse(t *testing.T) {
	s := New(0)
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
	s := New(0)
	t0 := time.Now()

	// A stray log line printed to stdout is not JSON, so the shim carries it as
	// text; it is still flagged as invalid rather than shown as a frame.
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
	s := New(0)
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

// TestConcurrentIngest exercises the lock under -race: many goroutines ingest
// while another reads snapshots.
func TestConcurrentIngest(t *testing.T) {
	s := New(0)
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
