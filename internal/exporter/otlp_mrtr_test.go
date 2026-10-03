package exporter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// decodedSpan is a span as a collector receives it, with each attribute's value
// left in its wire form so a test can see the type as well as the value.
type decodedSpan struct {
	SpanID       string
	ParentSpanID string
	TraceID      string
	Name         string
	Kind         string
	Start, End   int64
	StatusCode   string
	StatusMsg    string
	Attrs        map[string]map[string]json.RawMessage
}

func (s decodedSpan) has(key string) bool { _, ok := s.Attrs[key]; return ok }

func (s decodedSpan) str(t *testing.T, key string) string {
	t.Helper()
	raw, ok := s.Attrs[key]["stringValue"]
	if !ok {
		t.Fatalf("span %q: %s is not a string attribute: %v", s.Name, key, s.Attrs[key])
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (s decodedSpan) num(t *testing.T, key string) float64 {
	t.Helper()
	if raw, ok := s.Attrs[key]["doubleValue"]; ok {
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if raw, ok := s.Attrs[key]["intValue"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return float64(n)
	}
	t.Fatalf("span %q: %s is not a number: %v", s.Name, key, s.Attrs[key])
	return 0
}

func (s decodedSpan) duration() time.Duration { return time.Duration(s.End - s.Start) }

func decodeSpans(t *testing.T, payload []byte) []decodedSpan {
	t.Helper()
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string `json:"traceId"`
					SpanID       string `json:"spanId"`
					ParentSpanID string `json:"parentSpanId"`
					Name         string `json:"name"`
					Kind         string `json:"kind"`
					Start        string `json:"startTimeUnixNano"`
					End          string `json:"endTimeUnixNano"`
					Status       struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"status"`
					Attributes []struct {
						Key   string                     `json:"key"`
						Value map[string]json.RawMessage `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("invalid OTLP JSON: %v", err)
	}
	var out []decodedSpan
	for _, rs := range doc.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				start, _ := strconv.ParseInt(sp.Start, 10, 64)
				end, _ := strconv.ParseInt(sp.End, 10, 64)
				d := decodedSpan{
					SpanID: sp.SpanID, ParentSpanID: sp.ParentSpanID, TraceID: sp.TraceID,
					Name: sp.Name, Kind: sp.Kind, Start: start, End: end,
					StatusCode: sp.Status.Code, StatusMsg: sp.Status.Message,
					Attrs: make(map[string]map[string]json.RawMessage),
				}
				for _, a := range sp.Attributes {
					if _, dup := d.Attrs[a.Key]; dup {
						t.Errorf("span %q carries %s twice", sp.Name, a.Key)
					}
					d.Attrs[a.Key] = a.Value
				}
				out = append(out, d)
			}
		}
	}
	return out
}

// exportCaptureOTLP loads a capture the way `mcpsnoop export -T otlp` does.
func exportCaptureOTLP(t *testing.T, path string) []decodedSpan {
	t.Helper()
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, data); err != nil {
		t.Fatal(err)
	}
	return decodeSpans(t, buf.Bytes())
}

type captureFrame struct {
	dir proxy.Direction
	ms  int
	raw string
}

func writeCapture(t *testing.T, path string, frames []captureFrame) {
	t.Helper()
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
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

func byName(spans []decodedSpan, name string) []decodedSpan {
	var out []decodedSpan
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// TestMultiRoundTripToolCallIsAnExecuteToolSpanOverItsHops is the case the
// whole change exists for. book_flight took three requests, and the server
// worked 1.2 seconds while a person took 37 to answer. As one span stretched over
// all three, a latency panel read 38.2 seconds of server time and the second and
// third request ids appeared nowhere. The convention defines mcp.client as one
// request and its answer, so each hop is its own span with its own id and the
// server's time alone, and the GenAI execute_tool span above them carries the
// whole operation, so neither number pollutes the other.
func TestMultiRoundTripToolCallIsAnExecuteToolSpanOverItsHops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mrtr.jsonl")
	writeMRTRCapture(t, path)
	spans := exportCaptureOTLP(t, path)

	parents := byName(spans, "execute_tool book_flight")
	hops := byName(spans, "tools/call book_flight")
	if len(parents) != 1 || len(hops) != 3 {
		t.Fatalf("want one execute_tool span over three hops, got %d and %d among %d spans", len(parents), len(hops), len(spans))
	}
	parent := parents[0]
	if parent.Kind != "SPAN_KIND_INTERNAL" {
		t.Errorf("execute_tool kind = %s, the model fixes it at internal", parent.Kind)
	}
	if got := parent.duration(); got != 38200*time.Millisecond {
		t.Errorf("execute_tool covers %s, want the whole operation, 38.2s", got)
	}
	if got := parent.str(t, "gen_ai.operation.name"); got != "execute_tool" {
		t.Errorf("gen_ai.operation.name = %q, the model requires execute_tool", got)
	}
	if got := parent.str(t, "gen_ai.tool.name"); got != "book_flight" {
		t.Errorf("gen_ai.tool.name = %q", got)
	}
	// Its hops carry mcp.method.name. Carrying it here too would count every
	// operation once more in anything that counts tools/call by it.
	if parent.has("mcp.method.name") {
		t.Error("execute_tool must not carry mcp.method.name, its hops already do")
	}
	if got := parent.num(t, "mcpsnoop.call.round_trips"); got != 3 {
		t.Errorf("round_trips = %v", got)
	}
	if got := parent.num(t, "mcpsnoop.call.server_time_ms"); got != 1200 {
		t.Errorf("server_time_ms = %v, want the server's 1.2s", got)
	}
	if got := parent.num(t, "mcpsnoop.call.client_turnaround_ms"); got != 37000 {
		t.Errorf("client_turnaround_ms = %v, want the person's 37s", got)
	}

	wantHops := []struct {
		id         string
		server     time.Duration
		turnaround float64
		asked      bool
	}{
		{"1", 400 * time.Millisecond, 0, true},
		{"2", 300 * time.Millisecond, 12000, true},
		{"3", 500 * time.Millisecond, 25000, false},
	}
	for i, want := range wantHops {
		h := hops[i]
		if h.Kind != "SPAN_KIND_CLIENT" {
			t.Errorf("hop %d kind = %s, want client", i, h.Kind)
		}
		if h.ParentSpanID != parent.SpanID || h.TraceID != parent.TraceID {
			t.Errorf("hop %d hangs off %s in %s, want execute_tool %s in %s", i, h.ParentSpanID, h.TraceID, parent.SpanID, parent.TraceID)
		}
		if got := h.str(t, "jsonrpc.request.id"); got != want.id {
			t.Errorf("hop %d jsonrpc.request.id = %q, want %q", i, got, want.id)
		}
		if got := h.duration(); got != want.server {
			t.Errorf("hop %d lasts %s, want the server's %s for that request alone", i, got, want.server)
		}
		if h.has("gen_ai.operation.name") {
			t.Errorf("hop %d claims to be a tool execution, which would count one execution as several", i)
		}
		if got := h.str(t, "mcp.method.name"); got != "tools/call" {
			t.Errorf("hop %d mcp.method.name = %q", i, got)
		}
		if got := h.str(t, "mcpsnoop.call.id"); got != "1" {
			t.Errorf("hop %d groups under call %q, want the operation's first id", i, got)
		}
		if want.turnaround > 0 {
			if got := h.num(t, "mcpsnoop.hop.client_turnaround_ms"); got != want.turnaround {
				t.Errorf("hop %d waited %vms before it, want %v", i, got, want.turnaround)
			}
		} else if h.has("mcpsnoop.hop.client_turnaround_ms") {
			t.Errorf("hop %d is the first, nothing preceded it", i)
		}
		if got := h.has("mcpsnoop.hop.asked"); got != want.asked {
			t.Errorf("hop %d asked present = %v, want %v", i, got, want.asked)
		}
		if h.StatusCode != "STATUS_CODE_OK" {
			t.Errorf("hop %d status = %s, a request answered with input_required succeeded", i, h.StatusCode)
		}
	}
	// The list is an array on the wire, so a backend can filter on one member.
	var asked struct {
		Values []map[string]string `json:"values"`
	}
	raw, ok := hops[0].Attrs["mcpsnoop.hop.asked"]["arrayValue"]
	if !ok || json.Unmarshal(raw, &asked) != nil || len(asked.Values) != 1 || asked.Values[0]["stringValue"] != "elicitation/create" {
		t.Errorf("hop 0 asked = %s, want an arrayValue holding elicitation/create", raw)
	}

	// An operation that took one request is exactly what it was before.
	single := byName(spans, "tools/call lookup_price")
	if len(single) != 1 || len(byName(spans, "execute_tool lookup_price")) != 0 {
		t.Fatalf("a single-request call must stay one span, got %d with %d parents", len(single), len(byName(spans, "execute_tool lookup_price")))
	}
	if got := single[0].str(t, "gen_ai.operation.name"); got != "execute_tool" {
		t.Errorf("a single-request tool call still stands for its execution, got %q", got)
	}
	if len(spans) != 5 {
		t.Errorf("spans = %d, want 4 for book_flight and 1 for lookup_price", len(spans))
	}
}

// TestMultiRoundTripFailureLandsOnTheLastHopAndTheParent keeps error.type to its
// rule, set if and only if the operation failed. The earlier hops were answered
// with input_required and succeeded as requests, so only the request that failed
// and the execution it ended carry the error.
func TestMultiRoundTripFailureLandsOnTheLastHopAndTheParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mrtr-fail.jsonl")
	writeCapture(t, path, []captureFrame{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight"}}`},
		{proxy.ServerToClient, 100, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"confirm":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 5100, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","requestState":"st-1","inputResponses":{"confirm":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 5300, `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"seat no longer available"}}`},
	})
	spans := exportCaptureOTLP(t, path)
	parents, hops := byName(spans, "execute_tool book_flight"), byName(spans, "tools/call book_flight")
	if len(parents) != 1 || len(hops) != 2 {
		t.Fatalf("got %d parents and %d hops", len(parents), len(hops))
	}
	first, last, parent := hops[0], hops[1], parents[0]
	if first.has("error.type") || first.has("rpc.response.status_code") || first.StatusCode != "STATUS_CODE_OK" {
		t.Errorf("the input_required hop succeeded as a request, got status %s and error.type present %v", first.StatusCode, first.has("error.type"))
	}
	if got := last.str(t, "error.type"); got != "-32602" {
		t.Errorf("last hop error.type = %q", got)
	}
	if got := last.str(t, "rpc.response.status_code"); got != "-32602" {
		t.Errorf("last hop rpc.response.status_code = %q", got)
	}
	if last.StatusCode != "STATUS_CODE_ERROR" || last.StatusMsg != "seat no longer available" {
		t.Errorf("last hop status = %s %q, want ERROR carrying the JSON-RPC message", last.StatusCode, last.StatusMsg)
	}
	if got := parent.str(t, "error.type"); got != "-32602" || parent.StatusCode != "STATUS_CODE_ERROR" {
		t.Errorf("execute_tool error.type = %q status %s, the execution failed", got, parent.StatusCode)
	}
}

// TestMultiRoundTripPromptKeepsItsHopsWithoutAnInventedParent covers the
// requests MRTR allows besides tools/call. The GenAI conventions define a span
// for one tool execution and nothing for a prompt fetch or a resource read, so
// mcpsnoop does not make one up. The hops share mcpsnoop.call.id, and the last
// one carries the operation as a whole.
func TestMultiRoundTripPromptKeepsItsHopsWithoutAnInventedParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mrtr-prompt.jsonl")
	writeCapture(t, path, []captureFrame{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":7,"method":"prompts/get","params":{"name":"summarize"}}`},
		{proxy.ServerToClient, 50, `{"jsonrpc":"2.0","id":7,"result":{"resultType":"input_required","requestState":"p-1","inputRequests":{"topic":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 3050, `{"jsonrpc":"2.0","id":8,"method":"prompts/get","params":{"name":"summarize","requestState":"p-1","inputResponses":{"topic":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 3150, `{"jsonrpc":"2.0","id":8,"result":{"messages":[]}}`},
	})
	spans := exportCaptureOTLP(t, path)
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want the two hops and nothing else", len(spans))
	}
	for i, h := range spans {
		if h.Kind != "SPAN_KIND_CLIENT" || h.Name != "prompts/get summarize" {
			t.Errorf("span %d is %s %q, want a client hop", i, h.Kind, h.Name)
		}
		if h.ParentSpanID != "" {
			t.Errorf("hop %d hangs off %s, there is no parent to hang off", i, h.ParentSpanID)
		}
		if got := h.str(t, "mcpsnoop.call.id"); got != "7" {
			t.Errorf("hop %d groups under %q, want 7", i, got)
		}
	}
	if spans[0].has("mcpsnoop.call.round_trips") {
		t.Error("the first hop describes one request, not the operation")
	}
	if got := spans[1].num(t, "mcpsnoop.call.round_trips"); got != 2 {
		t.Errorf("the last hop carries the operation, round_trips = %v", got)
	}
}

// TestMultiRoundTripWithoutItsHopsIsOneHonestSpan covers a breakdown the store
// could not give in full, which a live store that released old frames produces.
// Hops it does not have would be a partial breakdown passed off as the whole, so
// the operation goes out as one span, and the server and client split still
// comes along because the store accumulates it as frames arrive.
func TestMultiRoundTripWithoutItsHopsIsOneHonestSpan(t *testing.T) {
	ended := time.Date(2026, 7, 28, 12, 0, 38, 200_000_000, time.UTC)
	duration := 38200.0
	idx := 0
	data := SessionExport{
		Session: SessionSummary{ID: "demo", Transport: proxy.TransportStdio},
		Calls: []CallExport{{
			ID: "1", Method: "tools/call", IsTool: true, ToolName: "book_flight", State: "completed", Status: "ok",
			Direction: proxy.ClientToServer, StartedAt: ended.Add(-38200 * time.Millisecond), EndedAt: &ended,
			DurationMS: &duration, RoundTrips: 3,
		}},
		Interactions: []InteractionExport{{
			CallIndex: &idx, CallID: "1", Method: "tools/call", ToolName: "book_flight",
			RoundTrips: 3, ServerTimeMS: 1200, ClientTurnaroundMS: 37000, DurationMS: 38200,
			Hops: []HopExport{{RequestID: "3", RequestAt: ended.Add(-500 * time.Millisecond)}}, HopsComplete: false,
		}},
	}
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, data); err != nil {
		t.Fatal(err)
	}
	spans := decodeSpans(t, buf.Bytes())
	if len(spans) != 1 || spans[0].Name != "tools/call book_flight" {
		t.Fatalf("want one span for an operation whose hops are not all known, got %d", len(spans))
	}
	s := spans[0]
	if got := s.num(t, "mcpsnoop.call.server_time_ms"); got != 1200 {
		t.Errorf("server_time_ms = %v", got)
	}
	if got := s.num(t, "mcpsnoop.call.client_turnaround_ms"); got != 37000 {
		t.Errorf("client_turnaround_ms = %v", got)
	}
	raw, ok := s.Attrs["mcpsnoop.call.hops_complete"]["boolValue"]
	if !ok || string(raw) != "false" {
		t.Errorf("the span must say its hops are not all there, got %v", s.Attrs["mcpsnoop.call.hops_complete"])
	}
}

// TestMultiRoundTripJoinsTheCallersTrace keeps the operation in the trace the
// client propagated. The execute_tool span hangs off the caller's span and the
// hops hang off it, all in the caller's trace.
func TestMultiRoundTripJoinsTheCallersTrace(t *testing.T) {
	const tp = `"_meta":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`
	path := filepath.Join(t.TempDir(), "mrtr-trace.jsonl")
	writeCapture(t, path, []captureFrame{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight",` + tp + `}}`},
		{proxy.ServerToClient, 100, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"confirm":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 2100, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","requestState":"st-1","inputResponses":{"confirm":{"action":"accept"}},` + tp + `}}`},
		{proxy.ServerToClient, 2400, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`},
	})
	spans := exportCaptureOTLP(t, path)
	parent := byName(spans, "execute_tool book_flight")
	if len(parent) != 1 {
		t.Fatalf("want one execute_tool span, got %d", len(parent))
	}
	if parent[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || parent[0].ParentSpanID != "00f067aa0ba902b7" {
		t.Errorf("execute_tool sits at %s/%s, want the caller's trace and span", parent[0].TraceID, parent[0].ParentSpanID)
	}
	for i, h := range byName(spans, "tools/call book_flight") {
		if h.TraceID != parent[0].TraceID || h.ParentSpanID != parent[0].SpanID {
			t.Errorf("hop %d sits at %s/%s, want under execute_tool in the caller's trace", i, h.TraceID, h.ParentSpanID)
		}
	}
}

// TestCancelledCallSpanEndsWhereTheClientGaveUp covers the interval a
// cancellation usually reports. A client cancels a request that hung past its
// timeout, and that wait is the finding. A span ending where it began showed a
// request that cost nothing, which hides exactly the hang it was cancelled for.
func TestCancelledCallSpanEndsWhereTheClientGaveUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancelled.jsonl")
	writeCapture(t, path, []captureFrame{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"hang"}}`},
		{proxy.ClientToServer, 30000, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":5,"reason":"timed out"}}`},
	})
	spans := exportCaptureOTLP(t, path)
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want the one cancelled call", len(spans))
	}
	s := spans[0]
	if got := s.duration(); got != 30*time.Second {
		t.Errorf("the cancelled call lasts %s, want the 30s the client waited before giving up", got)
	}
	if s.StatusCode != "STATUS_CODE_UNSET" {
		t.Errorf("status = %s, a cancellation is the client's choice and not an error", s.StatusCode)
	}
	if got := s.str(t, "mcpsnoop.call.cancel_reason"); got != "timed out" {
		t.Errorf("cancel_reason = %q", got)
	}
}
