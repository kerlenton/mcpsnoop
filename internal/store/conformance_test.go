package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientCapabilities":{"elicitation":{},"sampling":{}}}`

// modern opens a session by observing one client request that declares the
// revision, which is what makes its rules apply to the frames around it.
func modern(s *Store, seq uint64, method, extra string) EventView {
	params := "{" + modernMeta + "}"
	if extra != "" {
		params = "{" + extra + "," + modernMeta + "}"
	}
	return s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		MCPProtocolVersion: "2026-07-28",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":` + strconvItoa(seq) + `,"method":"` + method +
			`","params":` + params + `}`),
	})
}

func strconvItoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func serverFrame(s *Store, seq uint64, raw string) EventView {
	return s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: proxy.TransportHTTP,
		Raw: json.RawMessage(raw),
	})
}

// TestServerInitiatedRequestIsReported. 2026-07-28 routes every server-to-client
// request through MRTR and says the previous pattern "is no longer supported.
// This is a breaking change." Both transports repeat it, and each states the
// mirror half for the client.
func TestServerInitiatedRequestIsReported(t *testing.T) {
	s := New()
	modern(s, 1, "tools/call", `"name":"echo"`)
	ev := serverFrame(s, 2, `{"jsonrpc":"2.0","id":99,"method":"elicitation/create","params":{}}`)
	if !strings.Contains(ev.Warning, "server sent a JSON-RPC request") {
		t.Fatalf("a server-initiated request must be reported, got %q", ev.Warning)
	}

	// The client half of the same rule.
	back := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":99,"result":{}}`),
	})
	if !strings.Contains(back.Warning, "client sent a JSON-RPC response") {
		t.Fatalf("a client-sent response must be reported, got %q", back.Warning)
	}

	// A session that never declared the revision says nothing, which is the answer
	// for a legacy server and for a capture that starts mid-session.
	legacy := New()
	legacy.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`),
	})
	quiet := legacy.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":99,"method":"sampling/createMessage","params":{}}`),
	})
	if strings.Contains(quiet.Warning, "MRTR") || strings.Contains(quiet.Warning, "2026-07-28") {
		t.Fatalf("a legacy session must not be judged by this revision, got %q", quiet.Warning)
	}
}

// TestProgressMustIncrease. "The progress value MUST increase with each
// notification, even if the total is unknown" and "Progress notifications MUST
// stop after completion."
func TestProgressMustIncrease(t *testing.T) {
	progress := func(s *Store, seq uint64, value string) EventView {
		return serverFrame(s, seq, `{"jsonrpc":"2.0","method":"notifications/progress","params":`+
			`{"progressToken":"abc","progress":`+value+`}}`)
	}

	s := New()
	modern(s, 1, "tools/call", `"name":"slow","_meta_ignored":0`)
	// The request declares the token through its own _meta.
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		MCPProtocolVersion: "2026-07-28",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow",` +
			`"_meta":{"progressToken":"abc","io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
			`"io.modelcontextprotocol/clientCapabilities":{}}}}`),
	})
	if ev := progress(s, 3, "50"); ev.Warning != "" {
		t.Fatalf("the first value has nothing to compare against, got %q", ev.Warning)
	}
	if ev := progress(s, 4, "20"); !strings.Contains(ev.Warning, "went from 50 to 20") {
		t.Fatalf("progress going backwards must be reported, got %q", ev.Warning)
	}
	if ev := progress(s, 5, "20"); !strings.Contains(ev.Warning, "requires it to increase") {
		t.Fatalf("a repeated value is not an increase, got %q", ev.Warning)
	}

	// After the request completes, a further notification is its own violation,
	// and a fresh request may spend the token again.
	serverFrame(s, 6, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	if ev := progress(s, 7, "99"); !strings.Contains(ev.Warning, "after its request completed") {
		t.Fatalf("a notification after completion must be reported, got %q", ev.Warning)
	}
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 8, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		MCPProtocolVersion: "2026-07-28",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow",` +
			`"_meta":{"progressToken":"abc","io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
			`"io.modelcontextprotocol/clientCapabilities":{}}}}`),
	})
	if ev := progress(s, 9, "1"); ev.Warning != "" {
		t.Fatalf("a token is unique only across active requests, so reuse is legal: %q", ev.Warning)
	}
}

// TestInputRequiredResultRules covers the three MRTR MUSTs about the shape of an
// InputRequiredResult and the one about the client's declared capabilities.
func TestInputRequiredResultRules(t *testing.T) {
	for _, tc := range []struct {
		name, method, result, want string
	}{
		{
			name: "on a method that may not receive one", method: "tools/list",
			result: `{"resultType":"input_required","requestState":"st"}`,
			want:   "allows only on prompts/get, resources/read and tools/call",
		},
		{
			name: "carrying neither field", method: "tools/call",
			result: `{"resultType":"input_required"}`,
			want:   "neither inputRequests nor requestState",
		},
		{
			name: "naming a request object that is not one of the three", method: "tools/call",
			result: `{"resultType":"input_required","inputRequests":{"a":{"method":"tools/call"}}}`,
			want:   "allows only elicitation/create, sampling/createMessage and roots/list",
		},
		{
			name: "a legal one is silent", method: "tools/call",
			result: `{"resultType":"input_required","requestState":"st",` +
				`"inputRequests":{"a":{"method":"elicitation/create"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			modern(s, 1, tc.method, "")
			ev := serverFrame(s, 2, `{"jsonrpc":"2.0","id":1,"result":`+tc.result+`}`)
			if tc.want == "" {
				if ev.Warning != "" {
					t.Fatalf("a conforming result must be silent, got %q", ev.Warning)
				}
				return
			}
			if !strings.Contains(ev.Warning, tc.want) {
				t.Fatalf("warning = %q, want one containing %q", ev.Warning, tc.want)
			}
		})
	}

	// "Servers MUST NOT send an inputRequests that the client has not declared
	// support for in its capabilities", read from the request's own declaration.
	t.Run("an undeclared capability", func(t *testing.T) {
		s := New()
		s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
			Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
			MCPProtocolVersion: "2026-07-28",
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x",` +
				`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
				`"io.modelcontextprotocol/clientCapabilities":{}}}}`),
		})
		ev := serverFrame(s, 2, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required",`+
			`"inputRequests":{"a":{"method":"elicitation/create"}}}}`)
		if !strings.Contains(ev.Warning, "declared no elicitation capability") {
			t.Fatalf("warning = %q, want the undeclared capability named", ev.Warning)
		}
	})
}

// TestServerCancelOnlyTearsDownAListen. "A server MUST send
// notifications/cancelled referencing a subscriptions/listen request ID when it
// tears down that subscription stream ... Servers MUST NOT send
// notifications/cancelled for any other purpose."
func TestServerCancelOnlyTearsDownAListen(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		want         bool
	}{
		{"an ordinary request", "tools/call", true},
		{"the one it is for", "subscriptions/listen", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			modern(s, 1, tc.method, "")
			ev := serverFrame(s, 2, `{"jsonrpc":"2.0","method":"notifications/cancelled",`+
				`"params":{"requestId":1}}`)
			if got := strings.Contains(ev.Warning, "server cancelled"); got != tc.want {
				t.Fatalf("warning = %q, want it reported = %v", ev.Warning, tc.want)
			}
		})
	}

	// A client cancelling is the ordinary case, and an id mcpsnoop never saw
	// decides nothing.
	s := New()
	modern(s, 1, "tools/call", "")
	client := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`),
	})
	if strings.Contains(client.Warning, "server cancelled") {
		t.Fatalf("a client cancelling its own request is ordinary, got %q", client.Warning)
	}
	unknown := serverFrame(s, 3, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":404}}`)
	if unknown.Warning != "" {
		t.Fatalf("an id never observed must decide nothing, got %q", unknown.Warning)
	}

	// Earlier revisions let either party cancel any request, and the gate reads
	// the revision that request itself declared rather than the session's.
	legacy := New()
	legacy.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x",` +
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}}`),
	})
	old := legacy.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`),
	})
	if strings.Contains(old.Warning, "server cancelled") {
		t.Fatalf("a pre-2026-07-28 request must not be judged by this rule, got %q", old.Warning)
	}
}

// TestRequestMetaMustCarryTheProtocolVersion. The field is Required in the
// per-request table, and its sibling has been checked all along. It escaped
// because requestProtocolVersion falls back to the header, so an absent field
// still opened the revision gate and nothing then asked whether it was there.
func TestRequestMetaMustCarryTheProtocolVersion(t *testing.T) {
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		MCPMethod: "tools/call", MCPName: "x", MCPProtocolVersion: "2026-07-28",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x",` +
			`"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}`),
	})
	if !strings.Contains(ev.Warning, "missing required io.modelcontextprotocol/protocolVersion") {
		t.Fatalf("warning = %q, want the missing version reported", ev.Warning)
	}

	// On stdio there is no header, so an absent field is indistinguishable from a
	// legacy request and this must stay silent.
	quiet := New().Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x",` +
			`"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}`),
	})
	if strings.Contains(quiet.Warning, "protocolVersion") {
		t.Fatalf("stdio with no header must stay silent, got %q", quiet.Warning)
	}
}

// TestSubscriptionStreamRules covers the three rules a listen stream carries.
// The filter shape is the one the spec prints, params.notifications with
// toolsListChanged, promptsListChanged, resourcesListChanged and
// resourceSubscriptions, and the subscription id is the JSON-RPC id of the
// subscriptions/listen request itself.
func TestSubscriptionStreamRules(t *testing.T) {
	listen := func(s *Store, filter string) {
		s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
			Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
			MCPProtocolVersion: "2026-07-28",
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{` +
				`"notifications":` + filter + `,` + modernMeta + `}}`),
		})
	}
	ack := func(s *Store, seq uint64) {
		serverFrame(s, seq, `{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged",`+
			`"params":{"_meta":{"io.modelcontextprotocol/subscriptionId":1}}}`)
	}
	note := func(s *Store, seq uint64, method, meta string) EventView {
		params := `{"_meta":{"io.modelcontextprotocol/subscriptionId":1}}`
		if meta == "" {
			params = `{}`
		}
		return serverFrame(s, seq, `{"jsonrpc":"2.0","method":"`+method+`","params":`+params+`}`)
	}

	t.Run("before the acknowledgment", func(t *testing.T) {
		s := New()
		listen(s, `{"toolsListChanged":true}`)
		ev := note(s, 2, "notifications/tools/list_changed", "id")
		if !strings.Contains(ev.Warning, "before its acknowledgment") {
			t.Fatalf("warning = %q, want the ordering rule reported", ev.Warning)
		}
	})

	t.Run("a type the client never asked for", func(t *testing.T) {
		s := New()
		listen(s, `{"toolsListChanged":true}`)
		ack(s, 2)
		ev := note(s, 3, "notifications/resources/updated", "id")
		if !strings.Contains(ev.Warning, "was not requested") {
			t.Fatalf("warning = %q, want the filter rule reported", ev.Warning)
		}
	})

	t.Run("no subscription id at all", func(t *testing.T) {
		s := New()
		listen(s, `{"toolsListChanged":true}`)
		ack(s, 2)
		ev := note(s, 3, "notifications/tools/list_changed", "")
		if !strings.Contains(ev.Warning, "carries no io.modelcontextprotocol/subscriptionId") {
			t.Fatalf("warning = %q, want the correlation rule reported", ev.Warning)
		}
	})

	t.Run("a conforming stream is silent", func(t *testing.T) {
		s := New()
		listen(s, `{"toolsListChanged":true,"resourceSubscriptions":["file:///a"]}`)
		ack(s, 2)
		for seq, method := range map[uint64]string{
			3: "notifications/tools/list_changed",
			4: "notifications/resources/updated",
		} {
			if ev := note(s, seq, method, "id"); ev.Warning != "" {
				t.Fatalf("%s on a conforming stream warned: %q", method, ev.Warning)
			}
		}
	})

	// Nothing is decided without a subscriptions/listen mcpsnoop observed, which
	// is the answer for a capture that starts mid-stream.
	t.Run("no listen observed", func(t *testing.T) {
		s := New()
		modern(s, 1, "tools/call", "")
		if ev := note(s, 2, "notifications/tools/list_changed", ""); ev.Warning != "" {
			t.Fatalf("without an observed listen nothing can be judged, got %q", ev.Warning)
		}
	})

	// A notification that is not carried on a listen stream is not judged here.
	// Progress and logging ride the response stream of their own request.
	t.Run("a request-scoped notification is not a stream notification", func(t *testing.T) {
		s := New()
		listen(s, `{"toolsListChanged":true}`)
		ev := serverFrame(s, 2, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`)
		if ev.Warning != "" {
			t.Fatalf("a request-scoped notification must not be judged as a stream one, got %q", ev.Warning)
		}
	})
}

// TestInputRequestsAreUnverifiableAfterRedaction. The method names inside an
// InputRequiredResult are ordinary string values, so --redact-path and
// --redact-value reach them. Reading the placeholder as a method the spec does
// not allow reports a conforming server for the user's own privacy setting, on a
// signal that fails a default check run.
func TestInputRequestsAreUnverifiableAfterRedaction(t *testing.T) {
	answer := func(method string, redacted bool) string {
		s := New()
		t0 := time.Now()
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"slow"}`))
		e := resp(2, t0, proxy.ServerToClient, "1",
			`"result":{"resultType":"input_required","inputRequests":{"q1":{"method":"`+method+`","params":{}}}}`)
		e.Redacted = redacted
		return s.Ingest(e).Warning
	}

	if got := answer("[REDACTED]", true); got != "" {
		t.Fatalf("a scrubbed method was reported as a violation: %q", got)
	}
	if got := answer("prefix/[REDACTED]", true); got != "" {
		t.Fatalf("a partly scrubbed method was reported as a violation: %q", got)
	}
	// Only mcpsnoop's own rewriting earns that silence. The placeholder is a
	// string a server may legally send, so a check that stops at those bytes alone
	// is a check the traffic can switch off.
	if got := answer("[REDACTED]", false); got == "" {
		t.Fatal("an unredacted capture spelling the placeholder must still be judged")
	}
	// And a real violation is still reported.
	if got := answer("tools/call", false); got == "" {
		t.Fatal("a method the spec does not allow in inputRequests went unreported")
	}
	// A conforming one stays silent either way.
	for _, redacted := range []bool{false, true} {
		if got := answer("elicitation/create", redacted); got != "" {
			t.Fatalf("a conforming inputRequests was reported (redacted=%v): %q", redacted, got)
		}
	}
}
