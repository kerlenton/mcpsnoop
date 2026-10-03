package exporter

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// attrMap flattens a span's attributes into key to decoded value, so a test can
// say both that a key is present and that it carries the type the convention
// states. The raw form matters: a number where the registry says string is the
// failure that passes every Go-side assertion and gets rejected at a collector.
func attrMap(t *testing.T, attrs []otlpAttribute) map[string]otlpAnyValue {
	t.Helper()
	out := make(map[string]otlpAnyValue, len(attrs))
	for _, a := range attrs {
		if _, dup := out[a.Key]; dup {
			t.Errorf("attribute %q emitted twice", a.Key)
		}
		out[a.Key] = a.Value
	}
	return out
}

func str(t *testing.T, v otlpAnyValue, key string) string {
	t.Helper()
	if v.StringValue == nil {
		t.Fatalf("%s is not a string attribute: %+v", key, v)
	}
	return *v.StringValue
}

// TestSemconvRequiredAttributeIsAlwaysPresent covers the one attribute the MCP
// span model marks required. Everything else is conditional on data a capture
// may not hold, so this is the only unconditional claim, and a span without it
// is not an MCP span as far as any consumer is concerned.
func TestSemconvRequiredAttributeIsAlwaysPresent(t *testing.T) {
	for _, call := range []CallExport{
		{Method: "initialize"},
		{Method: "tools/call", IsTool: true, ToolName: "echo"},
		{Method: "notifications/cancelled"},
		{Method: "some/extension-method"},
	} {
		got := attrMap(t, mcpSemconvAttrs(call, SessionExport{}))
		if v, ok := got["mcp.method.name"]; !ok || str(t, v, "mcp.method.name") != call.Method {
			t.Errorf("mcp.method.name missing or wrong for %q: %+v", call.Method, got["mcp.method.name"])
		}
	}
}

// TestSemconvSpanName pins the naming convention, "{mcp.method.name} {target}",
// with the target being the tool or prompt name. A bare method for everything
// would collapse every tool into one row in a backend that groups by span name,
// and the resource URI must stay out of the name because a URI per span is the
// high-cardinality case the model tells instrumentation to avoid by default.
func TestSemconvSpanName(t *testing.T) {
	for _, tc := range []struct {
		name string
		call CallExport
		want string
	}{
		{"a tool call carries its tool", CallExport{Method: "tools/call", IsTool: true, ToolName: "echo"}, "tools/call echo"},
		{"a prompt fetch carries its prompt", CallExport{Method: "prompts/get", Params: json.RawMessage(`{"name":"summarize"}`)}, "prompts/get summarize"},
		{"a handshake is the bare method", CallExport{Method: "initialize"}, "initialize"},
		{"a listing is the bare method", CallExport{Method: "tools/list"}, "tools/list"},
		{
			"a resource read does not put the uri in the name",
			CallExport{Method: "resources/read", Params: json.RawMessage(`{"uri":"file:///tmp/a"}`)},
			"resources/read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpSpanName(tc.call); got != tc.want {
				t.Fatalf("mcpSpanName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSemconvOperationNameOnlyForToolCalls covers the model's wording, that
// gen_ai.operation.name SHOULD be execute_tool for a tool call and SHOULD NOT be
// set otherwise. Setting it everywhere would file a handshake as a tool call in
// any GenAI dashboard that groups on it.
func TestSemconvOperationNameOnlyForToolCalls(t *testing.T) {
	tool := attrMap(t, mcpSemconvAttrs(CallExport{Method: "tools/call", IsTool: true, ToolName: "echo"}, SessionExport{}))
	if got := str(t, tool["gen_ai.operation.name"], "gen_ai.operation.name"); got != "execute_tool" {
		t.Errorf("gen_ai.operation.name = %q, want execute_tool", got)
	}
	if _, ok := tool["gen_ai.tool.name"]; !ok {
		t.Error("a tool call must name its tool")
	}
	plain := attrMap(t, mcpSemconvAttrs(CallExport{Method: "initialize"}, SessionExport{}))
	if _, ok := plain["gen_ai.operation.name"]; ok {
		t.Error("gen_ai.operation.name must not be set for a non-tool operation")
	}
	if _, ok := plain["gen_ai.tool.name"]; ok {
		t.Error("gen_ai.tool.name must not be set for an operation with no tool")
	}
}

// TestSemconvErrorTypeTracksFailureExactly covers the model's "if and only if
// the operation fails". A span the exporter marks OK carrying error.type is a
// false alarm in every error-rate panel, and a failed span without it is a
// silent one, so the two have to move together.
func TestSemconvErrorTypeTracksFailureExactly(t *testing.T) {
	for _, tc := range []struct {
		name string
		call CallExport
		want string
	}{
		{"success", CallExport{Method: "tools/call"}, ""},
		{
			"a JSON-RPC error carries its code",
			CallExport{Method: "tools/call", IsError: true, Error: &proxy.RPCError{Code: -32602, Message: "bad params"}},
			"-32602",
		},
		{
			"a tool error is named by the model",
			CallExport{Method: "tools/call", IsError: true, ToolError: true},
			"tool_error",
		},
		{
			"a failure with no error object falls back",
			CallExport{Method: "tools/call", IsError: true},
			"_OTHER",
		},
		{"a pending call has not failed", CallExport{Method: "tools/call", State: "pending"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := attrMap(t, mcpSemconvAttrs(tc.call, SessionExport{}))
			v, ok := got["error.type"]
			if tc.want == "" {
				if ok {
					t.Fatalf("error.type set to %q on an operation that did not fail", str(t, v, "error.type"))
				}
				return
			}
			if !ok {
				t.Fatalf("error.type missing on a failed operation")
			}
			if s := str(t, v, "error.type"); s != tc.want {
				t.Fatalf("error.type = %q, want %q", s, tc.want)
			}
		})
	}
}

// TestSemconvRequestIDOmittedWhenAbsent covers the registry's explicit rule, that
// instrumentation should not record jsonrpc.request.id when the id is null or
// omitted. A literal "null" in the attribute reads as an id that was sent.
func TestSemconvRequestIDOmittedWhenAbsent(t *testing.T) {
	for _, id := range []string{"", "null"} {
		got := attrMap(t, mcpSemconvAttrs(CallExport{Method: "tools/list", ID: id}, SessionExport{}))
		if v, ok := got["jsonrpc.request.id"]; ok {
			t.Errorf("jsonrpc.request.id emitted as %q for id %q", str(t, v, "jsonrpc.request.id"), id)
		}
	}
	got := attrMap(t, mcpSemconvAttrs(CallExport{Method: "tools/list", ID: "7"}, SessionExport{}))
	if s := str(t, got["jsonrpc.request.id"], "jsonrpc.request.id"); s != "7" {
		t.Errorf("jsonrpc.request.id = %q, want 7", s)
	}
}

// TestSemconvResourceURIOnlyWhereTheModelSaysSo covers the method list the model
// gives. Reading a uri parameter from any method would file a tool argument that
// happens to be called uri as a resource the server exposes.
func TestSemconvResourceURIOnlyWhereTheModelSaysSo(t *testing.T) {
	const params = `{"uri":"postgres://db/customers/schema"}`
	for _, method := range []string{"resources/read", "resources/subscribe", "resources/unsubscribe", "notifications/resources/updated"} {
		got := attrMap(t, mcpSemconvAttrs(CallExport{Method: method, Params: json.RawMessage(params)}, SessionExport{}))
		if v, ok := got["mcp.resource.uri"]; !ok {
			t.Errorf("%s must carry mcp.resource.uri", method)
		} else if s := str(t, v, "mcp.resource.uri"); s != "postgres://db/customers/schema" {
			t.Errorf("%s resource uri = %q", method, s)
		}
	}
	tool := attrMap(t, mcpSemconvAttrs(CallExport{
		Method: "tools/call", IsTool: true, ToolName: "fetch",
		Params: json.RawMessage(`{"name":"fetch","arguments":{"uri":"https://example.com"},"uri":"https://example.com"}`),
	}, SessionExport{}))
	if _, ok := tool["mcp.resource.uri"]; ok {
		t.Error("a tools/call parameter named uri is not an MCP resource")
	}
}

// TestSemconvPromptNameOnlyForPromptsGet guards the same confusion in the other
// direction. tools/call also puts a name in params, and reading it generically
// would file every tool under gen_ai.prompt.name.
func TestSemconvPromptNameOnlyForPromptsGet(t *testing.T) {
	got := attrMap(t, mcpSemconvAttrs(CallExport{Method: "prompts/get", Params: json.RawMessage(`{"name":"summarize"}`)}, SessionExport{}))
	if s := str(t, got["gen_ai.prompt.name"], "gen_ai.prompt.name"); s != "summarize" {
		t.Errorf("gen_ai.prompt.name = %q, want summarize", s)
	}
	tool := attrMap(t, mcpSemconvAttrs(CallExport{
		Method: "tools/call", IsTool: true, ToolName: "echo",
		Params: json.RawMessage(`{"name":"echo"}`),
	}, SessionExport{}))
	if _, ok := tool["gen_ai.prompt.name"]; ok {
		t.Error("a tool name is not a prompt name")
	}
}

// TestSemconvTransportAndEndpoint covers network.transport, which the model pins
// to pipe for stdio, and the address pair. The port is the one the request went
// to, so an implicit one resolves to the scheme's default instead of being
// dropped, and a stdio capture has no endpoint to report at all.
func TestSemconvTransportAndEndpoint(t *testing.T) {
	stdio := attrMap(t, mcpSemconvAttrs(CallExport{Method: "ping"}, SessionExport{
		Session: SessionSummary{Transport: proxy.TransportStdio},
	}))
	if s := str(t, stdio["network.transport"], "network.transport"); s != "pipe" {
		t.Errorf("stdio network.transport = %q, want pipe", s)
	}
	for _, key := range []string{"server.address", "server.port"} {
		if _, ok := stdio[key]; ok {
			t.Errorf("a stdio capture has no endpoint, so %s must be absent", key)
		}
	}

	for _, tc := range []struct {
		endpoint string
		host     string
		port     int64
	}{
		{"https://mcp.example.com/rpc", "mcp.example.com", 443},
		{"http://mcp.example.com/rpc", "mcp.example.com", 80},
		{"http://127.0.0.1:8931/mcp", "127.0.0.1", 8931},
	} {
		got := attrMap(t, mcpSemconvAttrs(CallExport{Method: "ping"}, SessionExport{
			Session: SessionSummary{Transport: proxy.TransportHTTP, Endpoint: tc.endpoint},
		}))
		if s := str(t, got["network.transport"], "network.transport"); s != "tcp" {
			t.Errorf("%s network.transport = %q, want tcp", tc.endpoint, s)
		}
		if s := str(t, got["server.address"], "server.address"); s != tc.host {
			t.Errorf("%s server.address = %q, want %q", tc.endpoint, s, tc.host)
		}
		// server.port is an int in the registry, so it has to arrive as one.
		v := got["server.port"]
		if v.IntValue == nil {
			t.Fatalf("%s server.port is not an int attribute: %+v", tc.endpoint, v)
		}
		if *v.IntValue != strconv.FormatInt(tc.port, 10) {
			t.Errorf("%s server.port = %s, want %d", tc.endpoint, *v.IntValue, tc.port)
		}
	}

	legacy := attrMap(t, mcpSemconvAttrs(CallExport{Method: "ping"}, SessionExport{}))
	if _, ok := legacy["network.transport"]; ok {
		t.Error("a log whose frames named no transport must not have one guessed for it")
	}
}

// TestSemconvNeverClaimsAnMCPSession is the deliberate omission. mcp.session.id
// identifies a connection-scoped MCP session, and revision 2026-07-28 removed
// that concept, so a current capture has nothing to put there. mcpsnoop's own
// capture id is a different identifier and must not wear the convention's name,
// because a consumer joining on mcp.session.id would be joining on the wrong key.
func TestSemconvNeverClaimsAnMCPSession(t *testing.T) {
	for _, transport := range []string{proxy.TransportStdio, proxy.TransportHTTP, ""} {
		got := attrMap(t, mcpSemconvAttrs(CallExport{Method: "tools/list", ID: "1"}, SessionExport{
			Session: SessionSummary{ID: "srv-12345-abcdef", Transport: transport},
		}))
		if v, ok := got["mcp.session.id"]; ok {
			t.Errorf("mcp.session.id emitted as %q for transport %q", str(t, v, "mcp.session.id"), transport)
		}
	}
}

// TestSemconvProtocolVersion reports the negotiated revision when the capture saw
// one, which is what lets a consumer tell an era apart, and stays absent rather
// than guessing when no handshake was captured.
func TestSemconvProtocolVersion(t *testing.T) {
	got := attrMap(t, mcpSemconvAttrs(CallExport{Method: "tools/list"}, SessionExport{
		Capabilities: &CapabilitiesExport{ProtocolVersion: "2026-07-28"},
	}))
	if s := str(t, got["mcp.protocol.version"], "mcp.protocol.version"); s != "2026-07-28" {
		t.Errorf("mcp.protocol.version = %q", s)
	}
	none := attrMap(t, mcpSemconvAttrs(CallExport{Method: "tools/list"}, SessionExport{}))
	if _, ok := none["mcp.protocol.version"]; ok {
		t.Error("no handshake was captured, so no protocol version may be claimed")
	}
}

// TestSemconvStatusCodeIsAStringInTheWirePayload is the type check that only the
// serialized form can make. rpc.response.status_code is a string across every RPC
// system, and a JSON number there is the mistake that passes in Go and is
// rejected at the collector.
func TestSemconvStatusCodeIsAStringInTheWirePayload(t *testing.T) {
	ended := time.Unix(1783339200, 25_000_000).UTC()
	data := SessionExport{
		Session: SessionSummary{ID: "s1", Label: "srv", Transport: proxy.TransportStdio},
		Calls: []CallExport{{
			Method: "tools/call", IsTool: true, ToolName: "echo", ID: "3",
			Direction: proxy.ClientToServer,
			StartedAt: ended.Add(-25 * time.Millisecond), EndedAt: &ended,
			IsError: true, Error: &proxy.RPCError{Code: -32602, Message: "bad params"},
		}},
	}
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, data); err != nil {
		t.Fatal(err)
	}

	var payload struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					Name   string `json:"name"`
					Kind   string `json:"kind"`
					Status struct {
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
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("invalid OTLP JSON: %v", err)
	}
	span := payload.ResourceSpans[0].ScopeSpans[0].Spans[0]

	raw := map[string]map[string]json.RawMessage{}
	for _, a := range span.Attributes {
		raw[a.Key] = a.Value
	}
	code, ok := raw["rpc.response.status_code"]
	if !ok {
		t.Fatal("a response carrying an error code must set rpc.response.status_code")
	}
	if _, isString := code["stringValue"]; !isString {
		t.Fatalf("rpc.response.status_code must be a stringValue, got %v", code)
	}
	if string(code["stringValue"]) != `"-32602"` {
		t.Fatalf("rpc.response.status_code = %s, want \"-32602\"", code["stringValue"])
	}
	if _, isString := raw["jsonrpc.request.id"]["stringValue"]; !isString {
		t.Fatal("jsonrpc.request.id must be a stringValue")
	}

	// The status description is where a reader of the trace learns what the server
	// actually said, and the model says it SHOULD match JSONRPCError.message.
	if span.Status.Code != "STATUS_CODE_ERROR" || span.Status.Message != "bad params" {
		t.Fatalf("status = %+v, want ERROR carrying the JSON-RPC message", span.Status)
	}
	if span.Name != "tools/call echo" {
		t.Fatalf("span name = %q", span.Name)
	}
}

// TestSemconvSpanKindIsAlwaysTheInitiatorSide covers the one place the convention
// and a reading of the wire pull apart. The model fixes the mcp.client span at
// kind client whichever peer initiated, because what it measures is the wait for
// the peer's reply, and that is the only span mcpsnoop can report from outside
// both peers. Keying kind off the direction of travel instead would emit a lone
// server-kind span with no client span above it.
func TestSemconvSpanKindIsAlwaysTheInitiatorSide(t *testing.T) {
	ended := time.Unix(1783339200, 0).UTC()
	for _, dir := range []proxy.Direction{proxy.ClientToServer, proxy.ServerToClient} {
		data := SessionExport{
			Session: SessionSummary{ID: "s1", Transport: proxy.TransportStdio},
			Calls: []CallExport{{
				Method: "elicitation/create", ID: "9", Direction: dir,
				StartedAt: ended, EndedAt: &ended,
			}},
		}
		var buf bytes.Buffer
		if err := WriteOTLP(&buf, data); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), `"kind": "SPAN_KIND_CLIENT"`) {
			t.Errorf("direction %q did not produce an initiator-side span:\n%s", dir, buf.String())
		}
		if !strings.Contains(buf.String(), `"mcpsnoop.call.direction"`) {
			t.Errorf("direction %q is no longer recorded anywhere", dir)
		}
	}
}
