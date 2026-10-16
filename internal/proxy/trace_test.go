package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestProxyRecordsTheTraceContextHeaders. OpenTelemetry's HTTP instrumentation
// propagates a trace in these two headers and never in the JSON-RPC body, so a
// capture without them could not tie a call to the trace of the client that
// made it. They describe the request, so every element of a batch carries them,
// and a request that sent neither carries nothing.
func TestProxyRecordsTheTraceContextHeaders(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	sink := &captureSink{}
	// The proxy's own emitter rather than the test helper, since what this pins
	// is that the real envelope carries the headers.
	front := httptest.NewServer(httpProxyHandler(u, newHTTPEmitter(HTTPConfig{SessionID: "s", Label: "l", Target: target.URL}, sink)))
	defer front.Close()

	send := func(body string, headers map[string][]string) {
		req, _ := http.NewRequest(http.MethodPost, front.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for name, values := range headers {
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	send(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`, map[string][]string{
		"traceparent": {traceparent},
		"tracestate":  {"vendor=a", "other=b"},
		"baggage":     {"user=alice"},
	})
	send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`, nil)

	c2s := sink.byDir(ClientToServer)
	if len(c2s) != 3 {
		t.Fatalf("client frames = %d, want 3", len(c2s))
	}
	for _, e := range c2s[:2] {
		if e.TraceContext == nil || e.TraceContext.Traceparent != traceparent || e.TraceContext.Tracestate != "vendor=a,other=b" {
			t.Fatalf("a batch element lost the request's trace context: %+v", e.TraceContext)
		}
	}
	if c2s[2].TraceContext != nil {
		t.Fatalf("a request that sent no trace context recorded one: %+v", c2s[2].TraceContext)
	}
	for _, e := range sink.byDir(ServerToClient) {
		if e.TraceContext != nil {
			t.Fatalf("a server frame carries the request's trace context: %+v", e)
		}
	}
}
