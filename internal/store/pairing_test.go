package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// httpFrame is one envelope as `mcpsnoop http` records it, on a connection and
// an exchange of the test's choosing.
func httpFrame(seq uint64, dir proxy.Direction, conn string, exchange uint64, raw string) proxy.Envelope {
	e := proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(),
		Direction: dir, Transport: proxy.TransportHTTP, ConnID: conn, Exchange: exchange,
		Raw: json.RawMessage(raw),
	}
	if dir == proxy.ServerToClient {
		e.Status = 200
	}
	return e
}

// TestAServerRequestAnsweredOnAnotherConnectionIsAnswered. Over HTTP/1.1 a client
// holding a tool call's event stream open on one connection has to POST its
// answer to the server's elicitation on another, because the first is busy. Ids
// are scoped to the session, never to a connection, so every such answer was
// reported as matching no request and every such server request as never
// answered. mcp-conformance measured exactly this, five of each, on a session
// mcpsnoop recorded against the everything server.
func TestAServerRequestAnsweredOnAnotherConnectionIsAnswered(t *testing.T) {
	s := New()
	s.Ingest(httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"confirm"}}`))
	s.Ingest(httpFrame(2, proxy.ServerToClient, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","id":0,"method":"elicitation/create","params":{"message":"sure?","requestedSchema":{"type":"object","properties":{}}}}`))
	answer := s.Ingest(httpFrame(3, proxy.ClientToServer, "10.0.0.1:5002", 2,
		`{"jsonrpc":"2.0","id":0,"result":{"action":"accept","content":{}}}`))
	s.Ingest(httpFrame(4, proxy.ServerToClient, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`))

	if answer.Warning != "" {
		t.Fatalf("the answer on another connection was reported: %q", answer.Warning)
	}
	for _, c := range s.Calls("s1") {
		if c.State != Completed {
			t.Fatalf("%s %s is %v, want completed", c.Method, c.ID, c.State)
		}
	}
	if pending := s.Sessions()[0].Pending; pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
}

// TestTwoWaitingCallsUnderOneIdAreNotGuessedBetween. Two clients sharing one
// proxy each have a server request numbered 0 waiting, and an answer arrives on a
// third connection. Either could be meant, and pairing it with the wrong one is
// worse than saying it matched nothing, so it still does.
func TestTwoWaitingCallsUnderOneIdAreNotGuessedBetween(t *testing.T) {
	s := New()
	elicit := `{"jsonrpc":"2.0","id":0,"method":"elicitation/create","params":{"message":"sure?","requestedSchema":{"type":"object","properties":{}}}}`
	s.Ingest(httpFrame(1, proxy.ServerToClient, "10.0.0.1:5001", 1, elicit))
	s.Ingest(httpFrame(2, proxy.ServerToClient, "10.0.0.2:6001", 2, elicit))
	answer := s.Ingest(httpFrame(3, proxy.ClientToServer, "10.0.0.1:5002", 3,
		`{"jsonrpc":"2.0","id":0,"result":{"action":"decline"}}`))
	if !strings.Contains(answer.Warning, "no matching request") {
		t.Fatalf("an answer two waiting calls could own must stay unmatched, got %q", answer.Warning)
	}
	for _, c := range s.Calls("s1") {
		if c.State != Pending {
			t.Fatalf("%s on %s was settled by a guess", c.Method, c.ID)
		}
	}
}

// TestACancellationOnAnotherConnectionCancels. The same holds for the client's
// own requests. On 2025-11-25 a client cancels a call by POSTing
// notifications/cancelled, which cannot ride the connection the call's stream
// occupies, so the call never read as cancelled, and a result after it never read
// as late, which is the evidence the duplicate check reads.
func TestACancellationOnAnotherConnectionCancels(t *testing.T) {
	s := New()
	s.Ingest(httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow"}}`))
	cancel := s.Ingest(httpFrame(2, proxy.ClientToServer, "10.0.0.1:5002", 2,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3,"reason":"timeout"}}`))
	if cancel.Warning != "" {
		t.Fatalf("cancellation warned: %q", cancel.Warning)
	}
	calls := s.Calls("s1")
	if len(calls) != 1 || calls[0].State != Cancelled {
		t.Fatalf("calls = %+v, want the call cancelled", calls)
	}
	late := s.Ingest(httpFrame(3, proxy.ServerToClient, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`))
	if !late.LateResult {
		t.Fatal("the result after the cancellation should read as late")
	}
}

// TestAnIdlessErrorAnswersTheRequestItsExchangeCarried. A server that answers a
// POST in its own body with an error whose id is null or missing has broken the
// rule that an error carries the id it could read, and that is still reported.
// But the client has its answer, so the request is not in flight, and a later
// request reusing the id is not reusing one in flight.
func TestAnIdlessErrorAnswersTheRequestItsExchangeCarried(t *testing.T) {
	for _, tc := range []struct{ name, idField string }{
		{"a null id", `"id":null,`},
		{"no id", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Ingest(httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1,
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write"}}`))
			answer := s.Ingest(httpFrame(2, proxy.ServerToClient, "10.0.0.1:5001", 1,
				`{"jsonrpc":"2.0",`+tc.idField+`"error":{"code":-32602,"message":"bad arguments"}}`))
			if !strings.Contains(answer.Warning, "does not carry the id of the request it answers") {
				t.Fatalf("warning = %q, want the missing id named", answer.Warning)
			}
			if strings.Contains(answer.Warning, "no matching request") {
				t.Fatalf("the error has a request, its exchange's, got %q", answer.Warning)
			}
			again := s.Ingest(httpFrame(3, proxy.ClientToServer, "10.0.0.1:5001", 2,
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write"}}`))
			if again.Warning != "" {
				t.Fatalf("reusing an answered id was reported: %q", again.Warning)
			}
			first := s.Calls("s1")[0]
			if first.State != Failed || !first.Errored || first.Err == nil || first.Err.Code != -32602 {
				t.Fatalf("first call = %+v, want it failed with the error", first)
			}
			if header := s.Sessions()[0]; header.Errors != 1 {
				t.Fatalf("errors = %d, want the one error counted once", header.Errors)
			}
		})
	}
}

// TestAnIdlessErrorOnABatchIsNotGuessed. An exchange that carried two requests
// gives an error with no id two possible owners, so neither is picked.
func TestAnIdlessErrorOnABatchIsNotGuessed(t *testing.T) {
	s := New()
	for i, id := range []string{"1", "2"} {
		e := httpFrame(uint64(i+1), proxy.ClientToServer, "10.0.0.1:5001", 1,
			`{"jsonrpc":"2.0","id":`+id+`,"method":"tools/list"}`)
		e.Batch = true
		s.Ingest(e)
	}
	answer := s.Ingest(httpFrame(3, proxy.ServerToClient, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"no"}}`))
	if !strings.Contains(answer.Warning, "no matching request") {
		t.Fatalf("warning = %q, want it unmatched", answer.Warning)
	}
	for _, c := range s.Calls("s1") {
		if c.State != Pending {
			t.Fatalf("call %s was settled by a guess", c.ID)
		}
	}
}

// TestAnOriginRefusalIsNotAnUnmatchedResponse. Both transports say a server
// refusing an invalid Origin "MUST respond with HTTP 403 Forbidden" and that the
// body "MAY comprise a JSON-RPC error response that has no id". The refusal comes
// before the request is read, so there is no id to echo, and the spec permits the
// body that results. The call is still refused, and counted as an error once.
func TestAnOriginRefusalIsNotAnUnmatchedResponse(t *testing.T) {
	s := New()
	request := httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	request.TransportHeaders = &proxy.TransportHeaders{Origin: "http://evil.example.com"}
	s.Ingest(request)
	refusal := httpFrame(2, proxy.ServerToClient, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","error":{"code":-32600,"message":"Forbidden: Origin not allowed"}}`)
	refusal.Status = 403
	ev := s.Ingest(refusal)
	if ev.Warning != "" {
		t.Fatalf("an Origin refusal is the body the spec allows, got %q", ev.Warning)
	}
	calls := s.Calls("s1")
	if len(calls) != 1 || calls[0].State != Failed || calls[0].HTTPStatus != 403 {
		t.Fatalf("calls = %+v, want the request refused with 403", calls)
	}
	if header := s.Sessions()[0]; header.Errors != 1 || header.Pending != 0 {
		t.Fatalf("errors = %d, pending = %d, want 1 and 0", header.Errors, header.Pending)
	}

	// The permission is the Origin clause's. A 403 to a request that sent no
	// Origin is not covered by it, so its id-less body is reported.
	s2 := New()
	s2.Ingest(httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	other := httpFrame(2, proxy.ServerToClient, "10.0.0.1:5001", 1,
		`{"jsonrpc":"2.0","error":{"code":-32600,"message":"Forbidden"}}`)
	other.Status = 403
	if ev := s2.Ingest(other); !strings.Contains(ev.Warning, "does not carry the id") {
		t.Fatalf("warning = %q, want the missing id named", ev.Warning)
	}
}

// TestAConnectionsOwnCallWinsOverAnotherClients. Looking past a connection is
// for a frame whose connection never carried the id. A duplicate answer on the
// connection that already had its answer stays a duplicate, and does not settle
// another client's call that happens to wait under the same number.
func TestAConnectionsOwnCallWinsOverAnotherClients(t *testing.T) {
	s := New()
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	s.Ingest(httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1, list))
	s.Ingest(httpFrame(2, proxy.ServerToClient, "10.0.0.1:5001", 1, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	s.Ingest(httpFrame(3, proxy.ClientToServer, "10.0.0.2:6001", 2, list))
	again := s.Ingest(httpFrame(4, proxy.ServerToClient, "10.0.0.1:5001", 1, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	if !strings.Contains(again.Warning, "duplicate response") {
		t.Fatalf("warning = %q, want the duplicate named", again.Warning)
	}
	calls := s.Calls("s1")
	if len(calls) != 2 || calls[1].State != Pending {
		t.Fatalf("the other client's call was settled: %+v", calls)
	}
}

// TestAClientThatClosedItsStreamCancelledOn2026. On 2026-07-28 a client over HTTP
// cancels by closing the stream its answer was coming on, and the proxy records
// that as an abandoned frame on the exchange. The call it carried is cancelled,
// not pending, so a cancellation does not read as a hung call. On 2025-11-25 a
// disconnect is not a cancellation, and the call stays open.
func TestAClientThatClosedItsStreamCancelledOn2026(t *testing.T) {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`
	left := func(seq, exchange uint64) proxy.Envelope {
		e := httpFrame(seq, proxy.ClientToServer, "10.0.0.1:5001", exchange, "")
		e.Raw, e.Abandoned = nil, true
		return e
	}
	cases := []struct {
		name    string
		request proxy.Envelope
		want    CallState
		pending int
		says    string
	}{
		{
			name: "a 2026 call",
			request: httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1,
				`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow",`+meta+`}}`),
			want: Cancelled, says: `the client closed the stream, which cancels tools/call "slow" (id 3)`,
		},
		{
			name: "a 2026 subscription",
			request: httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1,
				`{"jsonrpc":"2.0","id":3,"method":"subscriptions/listen","params":{`+meta+`}}`),
			want: Cancelled, says: "the client closed the stream, which cancels subscriptions/listen (id 3)",
		},
		{
			name: "a 2025 call",
			request: func() proxy.Envelope {
				e := httpFrame(1, proxy.ClientToServer, "10.0.0.1:5001", 1,
					`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow"}}`)
				e.MCPProtocolVersion = "2025-11-25"
				return e
			}(),
			want: Pending, pending: 1, says: `the client disconnected, which on 2025-11-25 does not cancel tools/call "slow" (id 3)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Ingest(tc.request)
			ev := s.Ingest(left(2, 1))
			if !ev.Abandoned || ev.Kind != EventTransport || ev.Observation != tc.says || ev.Warning != "" || ev.Errored {
				t.Fatalf("event = %+v", ev)
			}
			calls := s.Calls("s1")
			if len(calls) != 1 || calls[0].State != tc.want {
				t.Fatalf("calls = %+v, want %v", calls, tc.want)
			}
			if pending := s.Sessions()[0].Pending; pending != tc.pending {
				t.Fatalf("pending = %d, want %d", pending, tc.pending)
			}
		})
	}
}
