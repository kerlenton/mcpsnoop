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
