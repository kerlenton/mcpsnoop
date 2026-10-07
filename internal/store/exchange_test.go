package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestAnHTTPFailureSettlesTheCallItAnswered. A 401, a gateway's 502 and an error
// that names no request all answer the request their exchange carried, so the
// call is failed rather than pending, the status says why, and the error the
// failure frame already counted is not counted twice.
func TestAnHTTPFailureSettlesTheCallItAnswered(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response proxy.Envelope
		status   int
	}{
		{"a bodiless 401", proxy.Envelope{Status: 401, AuthChallenge: `Bearer realm="mcp"`}, 401},
		{"a gateway's error page", proxy.Envelope{Status: 502, Text: "<html>502 Bad Gateway</html>"}, 502},
		{"an error naming no request", proxy.Envelope{Status: 401, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"unauthorized"}}`)}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			t0 := time.Now()
			s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0, Direction: proxy.ClientToServer,
				Transport: proxy.TransportHTTP, Exchange: 7,
				Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write"}}`)})
			response := tc.response
			response.SessionID, response.ServerLabel, response.Seq, response.TS = "s1", "srv", 2, t0.Add(time.Millisecond)
			response.Direction, response.Transport, response.Exchange = proxy.ServerToClient, proxy.TransportHTTP, 7
			s.Ingest(response)

			calls := s.Calls("s1")
			if len(calls) != 1 || calls[0].State != Failed || calls[0].HTTPStatus != tc.status || !calls[0].Errored {
				t.Fatalf("call = %+v, want it failed with HTTP %d", calls, tc.status)
			}
			header := s.Sessions()[0]
			if header.Pending != 0 || header.Errors != 1 {
				t.Fatalf("pending = %d, errors = %d, want 0 and the one error the failure frame counted", header.Pending, header.Errors)
			}
		})
	}
}

// TestAnExchangeOnlySettlesItsOwnCalls. Without a number, which is every log
// written before mcpsnoop recorded one, a failure cannot say which call it
// answered, and guessing would settle the wrong one, so the call stays as it
// was. A success settles nothing, and a failure on one exchange leaves a call on
// another alone.
func TestAnExchangeOnlySettlesItsOwnCalls(t *testing.T) {
	s := New()
	t0 := time.Now()
	request := func(seq uint64, id string, exchange uint64) {
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: t0, Direction: proxy.ClientToServer,
			Transport: proxy.TransportHTTP, Exchange: exchange,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"write"}}`)})
	}
	status := func(seq uint64, code int, exchange uint64) {
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: t0, Direction: proxy.ServerToClient,
			Transport: proxy.TransportHTTP, Status: code, Exchange: exchange})
	}
	request(1, "1", 0)
	status(2, 401, 0)
	request(3, "2", 5)
	status(4, 202, 5)
	request(5, "3", 6)
	status(6, 503, 8)

	for _, c := range s.Calls("s1") {
		if c.State != Pending || c.HTTPStatus != 0 {
			t.Errorf("call %s = %v with HTTP %d, want it left pending", c.ID, c.State, c.HTTPStatus)
		}
	}
}
