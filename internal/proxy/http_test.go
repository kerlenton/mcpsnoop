package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// emitterTo adapts a captureSink into the emit func httpProxyHandler expects.
func emitterTo(sink *captureSink) func(Direction, []byte, route) {
	return func(d Direction, raw []byte, rt route) {
		sink.Emit(Envelope{Direction: d, Raw: append([]byte(nil), raw...), MCPMethod: rt.method, MCPName: rt.name})
	}
}

func TestHTTPProxyJSON(t *testing.T) {
	const wantResp = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wantResp)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	resp, err := http.Post(front.URL, "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != wantResp {
		t.Fatalf("client got %q, want %q", got, wantResp)
	}

	c2s := sink.byDir(ClientToServer)
	s2c := sink.byDir(ServerToClient)
	if len(c2s) != 1 || string(c2s[0].Raw) != reqBody {
		t.Fatalf("c2s = %+v", c2s)
	}
	if len(s2c) != 1 || string(s2c[0].Raw) != wantResp {
		t.Fatalf("s2c = %+v", s2c)
	}
}

func TestHTTPProxyForwardsAndCapturesRoutingHeaders(t *testing.T) {
	var gotMethod, gotName string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotName = r.Header.Get("Mcp-Method"), r.Header.Get("Mcp-Name")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "echo")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Forwarded verbatim to the target.
	if gotMethod != "tools/call" || gotName != "echo" {
		t.Fatalf("target received Mcp-Method=%q Mcp-Name=%q, want tools/call / echo", gotMethod, gotName)
	}
	// Captured onto the observed client->server frame.
	c2s := sink.byDir(ClientToServer)
	if len(c2s) != 1 || c2s[0].MCPMethod != "tools/call" || c2s[0].MCPName != "echo" {
		t.Fatalf("captured frame headers = %+v", c2s)
	}
}

func TestHTTPProxyWithoutRoutingHeadersDegrades(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	// No routing headers (an older client): the frame's header fields stay empty.
	resp, err := http.Post(front.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	c2s := sink.byDir(ClientToServer)
	if len(c2s) != 1 || c2s[0].MCPMethod != "" || c2s[0].MCPName != "" {
		t.Fatalf("absent headers should stay empty, got %+v", c2s)
	}
}

func TestHTTPProxyTargetPathIsEndpoint(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Fatalf("backend path = %q, want /mcp", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL + "/mcp")
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	resp, err := http.Post(front.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestHTTPProxySSE(t *testing.T) {
	msgs := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"step":1}}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"p":0.5}}`,
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("backend ResponseWriter is not a Flusher")
		}
		for _, m := range msgs {
			fmt.Fprintf(w, "data: %s\n\n", m)
			fl.Flush()
		}
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	resp, err := http.Post(front.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), `"step":1`) {
		t.Fatalf("client did not receive SSE payload: %q", body)
	}
	s2c := sink.byDir(ServerToClient)
	if len(s2c) != 2 {
		t.Fatalf("expected 2 SSE frames observed, got %d: %+v", len(s2c), s2c)
	}
	if string(s2c[0].Raw) != msgs[0] || string(s2c[1].Raw) != msgs[1] {
		t.Fatalf("SSE frames mismatch: %q / %q", s2c[0].Raw, s2c[1].Raw)
	}
}

func TestSSETapMultiChunk(t *testing.T) {
	var got []string
	tap := newSSETap(io.NopCloser(strings.NewReader("")), func(d []byte) { got = append(got, string(d)) })
	// Feed split across arbitrary chunk boundaries.
	for _, chunk := range []string{"data: {\"a\":", "1}\n", "\nda", "ta: {\"b\":2}\n\n"} {
		tap.feed([]byte(chunk))
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("sseTap parsed %v", got)
	}
}

func TestSSETapMultilineData(t *testing.T) {
	var got []string
	tap := newSSETap(io.NopCloser(strings.NewReader("")), func(d []byte) { got = append(got, string(d)) })

	tap.feed([]byte("data: first line\ndata: second line\n\n"))

	if len(got) != 1 || got[0] != "first line\nsecond line" {
		t.Fatalf("sseTap parsed %v", got)
	}
}
