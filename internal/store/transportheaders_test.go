package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestAcceptHeaderIsCheckedAgainstWhatTheTransportRequires. The transport says
// the client MUST list both application/json and text/event-stream, in
// 2025-11-25 and 2026-07-28 alike, so this needs no revision gate. A client that
// offers only one will reject half the legal answers, and the symptom is a
// stream that never opens with no error anywhere.
func TestAcceptHeaderIsCheckedAgainstWhatTheTransportRequires(t *testing.T) {
	for name, tc := range map[string]struct {
		headers *proxy.TransportHeaders
		want    string
	}{
		"both types":         {&proxy.TransportHeaders{Accept: "application/json, text/event-stream"}, ""},
		"both, reordered":    {&proxy.TransportHeaders{Accept: "text/event-stream, application/json"}, ""},
		"both with q-values": {&proxy.TransportHeaders{Accept: "text/event-stream;q=0.9, application/json;q=1.0"}, ""},
		// A client offering everything has offered both, and reporting it would be
		// mcpsnoop inventing a stricter rule than the one written down.
		"the */* wildcard": {&proxy.TransportHeaders{Accept: "*/*"}, ""},
		"type wildcards":   {&proxy.TransportHeaders{Accept: "application/*, text/*"}, ""},
		"only json":        {&proxy.TransportHeaders{Accept: "application/json"}, "text/event-stream"},
		"only the stream":  {&proxy.TransportHeaders{Accept: "text/event-stream"}, "application/json"},
		"neither":          {&proxy.TransportHeaders{Accept: "text/plain"}, "application/json or text/event-stream"},
		"no Accept at all": {&proxy.TransportHeaders{ContentType: "application/json"}, "sent no Accept header"},
		// nil is a log written before mcpsnoop recorded these headers. Reading one
		// back must not report every frame in it for a header nobody wrote down.
		"a log from before this existed": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := acceptWarning(proxy.ClientToServer, tc.headers)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("warned about a conforming request: %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want it to name %q", got, tc.want)
			}
		})
	}

	// The rule is the client's, so a server frame carrying the same header is not
	// under it.
	if got := acceptWarning(proxy.ServerToClient, &proxy.TransportHeaders{Accept: "text/plain"}); got != "" {
		t.Fatalf("a server frame was judged by the client's rule: %q", got)
	}
}

// TestResponseContentTypeIsCheckedAgainstTheTwoAllowedTypes. Answering a
// JSON-RPC request with anything but application/json or text/event-stream is a
// MUST violation, and the value was already read to pick the SSE branch and then
// thrown away, so nothing could check the rule it decides.
func TestResponseContentTypeIsCheckedAgainstTheTwoAllowedTypes(t *testing.T) {
	for name, tc := range map[string]struct {
		headers *proxy.TransportHeaders
		want    string
	}{
		"json":                {&proxy.TransportHeaders{ContentType: "application/json"}, ""},
		"json with a charset": {&proxy.TransportHeaders{ContentType: "application/json; charset=utf-8"}, ""},
		"an event stream":     {&proxy.TransportHeaders{ContentType: "text/event-stream"}, ""},
		"plain text":          {&proxy.TransportHeaders{ContentType: "text/plain"}, "is text/plain"},
		// Not "anything under application", which the transport does not say.
		"another application type":                 {&proxy.TransportHeaders{ContentType: "application/xml"}, "is application/xml"},
		"html, the shape a proxy error page takes": {&proxy.TransportHeaders{ContentType: "text/html"}, "is text/html"},
		"none at all":                    {&proxy.TransportHeaders{}, "carried no Content-Type"},
		"a log from before this existed": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := responseContentTypeWarning(proxy.ServerToClient, tc.headers)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("warned about a conforming response: %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want it to name %q", got, tc.want)
			}
		})
	}

	// The rule is the server's.
	if got := responseContentTypeWarning(proxy.ClientToServer, &proxy.TransportHeaders{ContentType: "text/plain"}); got != "" {
		t.Fatalf("a client frame was judged by the server's rule: %q", got)
	}
}

// TestTransportHeaderChecksNeverFireOnStdio. stdio has no HTTP headers at all,
// and these warnings fail a default check run, so a stdio capture reporting one
// would turn every such CI job red for a rule that does not apply to it.
func TestTransportHeaderChecksNeverFireOnStdio(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{}`))
	for _, ev := range s.Timeline("s1") {
		if ev.Warning != "" {
			t.Fatalf("a stdio frame was warned about: %q", ev.Warning)
		}
	}
	// The response half too.
	got := s.Ingest(resp(2, t0, proxy.ServerToClient, "1", `"result":{"tools":[]}`))
	if got.Warning != "" {
		t.Fatalf("a stdio response was warned about: %q", got.Warning)
	}
}

// TestTransportHeaderChecksReachTheStream drives the ingest path rather than the
// two functions, since a check nothing calls is a check that does not exist.
func TestTransportHeaderChecksReachTheStream(t *testing.T) {
	s := New()
	t0 := time.Now()
	request := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		Raw:              json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
		TransportHeaders: &proxy.TransportHeaders{Accept: "application/json", ContentType: "application/json"},
	})
	if !strings.Contains(request.Warning, "text/event-stream") {
		t.Fatalf("the Accept rule never reached the stream: %q", request.Warning)
	}
	response := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0, Status: 200,
		Direction: proxy.ServerToClient, Transport: proxy.TransportHTTP,
		Raw:              json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`),
		TransportHeaders: &proxy.TransportHeaders{ContentType: "text/html"},
	})
	if !strings.Contains(response.Warning, "text/html") {
		t.Fatalf("the Content-Type rule never reached the stream: %q", response.Warning)
	}
	if h := s.Sessions()[0]; h.Errors != 0 {
		t.Fatalf("a header rule was counted as an error, not a warning: %+v", h)
	}
}
