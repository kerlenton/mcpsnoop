package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

// slowByteReader yields one byte per Read with a small delay, so a Read and a
// concurrent Close overlap long enough for the race detector to see the tap's
// shared state if it is unguarded.
type slowByteReader struct {
	data  []byte
	pos   int
	delay time.Duration
}

func (r *slowByteReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	if r.pos >= len(r.data) || len(p) == 0 {
		if r.pos >= len(r.data) {
			return 0, io.EOF
		}
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func (r *slowByteReader) Close() error { return nil }

// TestBodyTapConcurrentReadAndClose drives Read and Close from two goroutines,
// which net/http can do on a request body. onDone must fire exactly once, and the
// shared buffer and done flag must not race (fails under -race before the fix).
func TestBodyTapConcurrentReadAndClose(t *testing.T) {
	var count atomic.Int32
	tap := newBodyTap(&slowByteReader{data: bytes.Repeat([]byte("x"), 400), delay: 20 * time.Microsecond}, 10,
		func([]byte, bool) { count.Add(1) })

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(io.Discard, tap) }()
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond) // let some reads run first, then close mid-stream
		_ = tap.Close()
	}()
	wg.Wait()

	if got := count.Load(); got != 1 {
		t.Fatalf("onDone fired %d times, want exactly 1", got)
	}
}

// emitterTo adapts a captureSink into the emit func httpProxyHandler expects.
func emitterTo(sink *captureSink) func(Direction, []byte, route) {
	return func(d Direction, raw []byte, rt route) {
		sink.Emit(Envelope{Direction: d, Raw: append([]byte(nil), raw...), MCPMethod: rt.method, MCPName: rt.name, MCPProtocolVersion: rt.protocolVersion, MCPParamHeaders: rt.params, Batch: rt.batch, Truncated: rt.truncated, Status: rt.status, AuthChallenge: rt.challenge})
	}
}

// TestBodyTapForwardsFullyAndBoundsObservation covers the memory bound: the tap
// yields every byte to the forwarder while copying at most cap bytes for
// observation, flagging the copy truncated once the body runs past cap. Bytes are
// checked against the original, never against the (bounded) observed copy.
func TestBodyTapForwardsFullyAndBoundsObservation(t *testing.T) {
	run := func(t *testing.T, rc io.ReadCloser, input []byte, cap int, wantTrunc bool) {
		var observed []byte
		var truncated bool
		tap := newBodyTap(rc, cap, func(o []byte, tr bool) {
			observed = append([]byte(nil), o...)
			truncated = tr
		})
		got, err := io.ReadAll(tap)
		if err != nil {
			t.Fatal(err)
		}
		_ = tap.Close()
		// Byte-for-byte forwarding is unchanged, whether or not the copy was cut.
		if !bytes.Equal(got, input) {
			t.Fatalf("forwarded %q, want the full %q", got, input)
		}
		if truncated != wantTrunc {
			t.Fatalf("truncated = %v, want %v", truncated, wantTrunc)
		}
		want := input
		if wantTrunc {
			want = input[:cap]
		}
		if !bytes.Equal(observed, want) {
			t.Fatalf("observed %q, want %q", observed, want)
		}
	}

	input := []byte("0123456789abcdef") // 16 bytes
	t.Run("under cap", func(t *testing.T) {
		run(t, io.NopCloser(bytes.NewReader(input)), input, 100, false)
	})
	t.Run("over cap", func(t *testing.T) {
		run(t, io.NopCloser(bytes.NewReader(input)), input, 10, true)
	})
	t.Run("over cap across single-byte reads", func(t *testing.T) {
		run(t, io.NopCloser(iotest.OneByteReader(bytes.NewReader(input))), input, 10, true)
	})
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

func TestHTTPProxyObservesIdentityDespiteClientGzip(t *testing.T) {
	const msg = `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			// A server that would compress if asked. mcpsnoop must have forced
			// identity in the Director, so this branch should not be taken.
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(msg))
			_ = gz.Close()
			return
		}
		_, _ = io.WriteString(w, msg)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip") // the client prefers gzip
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	s2c := sink.byDir(ServerToClient)
	if len(s2c) != 1 {
		t.Fatalf("s2c frames = %d, want 1", len(s2c))
	}
	if string(s2c[0].Raw) != msg {
		t.Fatalf("observed frame = raw %q text %q, want the decoded JSON %q", s2c[0].Raw, s2c[0].Text, msg)
	}
}

func TestHTTPProxySkipsObservingAStillCompressedBody(t *testing.T) {
	const msg = `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A stubborn server that compresses even though identity was requested.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(msg))
		_ = gz.Close()
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	resp, err := http.Post(front.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// The body is still compressed, so mcpsnoop observes nothing for it rather than
	// pushing binary noise into a frame.
	if s2c := sink.byDir(ServerToClient); len(s2c) != 0 {
		t.Fatalf("expected no observed s2c frame for a compressed body, got %+v", s2c)
	}
}

func TestHTTPProxyForwardsAndCapturesRoutingHeaders(t *testing.T) {
	var gotMethod, gotName, gotRegion string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotName = r.Header.Get("Mcp-Method"), r.Header.Get("Mcp-Name")
		gotRegion = r.Header.Get("Mcp-Param-Region")
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
	req.Header["mCp-pArAm-Region"] = []string{"us-west1"}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Forwarded verbatim to the target.
	if gotMethod != "tools/call" || gotName != "echo" || gotRegion != "us-west1" {
		t.Fatalf("target received Mcp-Method=%q Mcp-Name=%q Mcp-Param-Region=%q", gotMethod, gotName, gotRegion)
	}
	// Captured onto the observed client->server frame.
	c2s := sink.byDir(ClientToServer)
	if len(c2s) != 1 || c2s[0].MCPMethod != "tools/call" || c2s[0].MCPName != "echo" {
		t.Fatalf("captured frame headers = %+v", c2s)
	}
	if got := c2s[0].MCPParamHeaders; !reflect.DeepEqual(got, []MCPParamHeader{{Name: "Mcp-Param-Region", Value: "us-west1"}}) {
		t.Fatalf("captured parameter headers = %+v", got)
	}
}

// TestMCPParamHeadersMatchThePrefixAndSort exercises the prefix match and the
// ordering on a synthetic map. It is not a statement about the wire: net/http
// canonicalises every field name before mcpsnoop sees it, so a real request can
// never produce the uncanonicalised keys below. The test one above sends real
// bytes and correctly expects the canonical spelling back.
func TestMCPParamHeadersMatchThePrefixAndSort(t *testing.T) {
	header := http.Header{
		"mCp-pArAm-Zone":   {"west"},
		"MCP-PARAM-Region": {"us", "backup"},
		"X-Other":          {"ignored"},
	}
	want := []MCPParamHeader{
		{Name: "MCP-PARAM-Region", Value: "us"},
		{Name: "MCP-PARAM-Region", Value: "backup"},
		{Name: "mCp-pArAm-Zone", Value: "west"},
	}
	if got := mcpParamHeaders(header); !reflect.DeepEqual(got, want) {
		t.Fatalf("mcpParamHeaders() = %+v, want %+v", got, want)
	}
}

func TestHTTPProxyForwardsAndCapturesProtocolVersion(t *testing.T) {
	var gotVersion string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("MCP-Protocol-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Forwarded verbatim to the target.
	if gotVersion != "2026-07-28" {
		t.Fatalf("target received MCP-Protocol-Version=%q, want 2026-07-28", gotVersion)
	}
	// Captured onto the observed client->server frame.
	c2s := sink.byDir(ClientToServer)
	if len(c2s) != 1 || c2s[0].MCPProtocolVersion != "2026-07-28" {
		t.Fatalf("captured frame protocol version = %+v", c2s)
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
	if len(c2s) != 1 || c2s[0].MCPMethod != "" || c2s[0].MCPName != "" || c2s[0].MCPProtocolVersion != "" {
		t.Fatalf("absent headers should stay empty, got %+v", c2s)
	}
}

func TestHTTPProxyBatchHeadersRideFirstElementOnly(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","id":2,"result":{}}]`)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	// A batch carrying routing headers: the headers name one operation but the
	// batch has two, so they cannot be copied onto every element.
	batch := `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}]`
	req, _ := http.NewRequest(http.MethodPost, front.URL, strings.NewReader(batch))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Method", "tools/list")
	req.Header.Set("Mcp-Name", "search")
	req.Header.Set("Mcp-Param-Region", "us-west1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	c2s := sink.byDir(ClientToServer)
	if len(c2s) != 2 {
		t.Fatalf("expected 2 split batch frames, got %d: %+v", len(c2s), c2s)
	}
	if !c2s[0].Batch || !c2s[1].Batch {
		t.Fatalf("both batch elements should be flagged batched: %+v", c2s)
	}
	// Headers ride only the first element, so the store flags the batch once
	// rather than fabricating a per-element method mismatch on the rest.
	if c2s[0].MCPMethod != "tools/list" || c2s[0].MCPName != "search" {
		t.Fatalf("first element should carry the headers, got %+v", c2s[0])
	}
	if c2s[1].MCPMethod != "" || c2s[1].MCPName != "" {
		t.Fatalf("later elements must not carry the headers, got %+v", c2s[1])
	}
	if len(c2s[0].MCPParamHeaders) != 1 || c2s[0].MCPParamHeaders[0].Value != "us-west1" {
		t.Fatalf("first element should carry parameter headers, got %+v", c2s[0])
	}
	if len(c2s[1].MCPParamHeaders) != 0 {
		t.Fatalf("later elements must not carry parameter headers, got %+v", c2s[1])
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
	tap := newSSETap(io.NopCloser(strings.NewReader("")), maxFrameBytes, func(d []byte, _ bool) { got = append(got, string(d)) })
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
	tap := newSSETap(io.NopCloser(strings.NewReader("")), maxFrameBytes, func(d []byte, _ bool) { got = append(got, string(d)) })

	tap.feed([]byte("data: first line\ndata: second line\n\n"))

	if len(got) != 1 || got[0] != "first line\nsecond line" {
		t.Fatalf("sseTap parsed %v", got)
	}
}

// TestSSETapFlagsTruncated checks that an SSE event whose observed copy exceeds
// the cap is reported as truncated rather than parsed as a whole frame.
//
// The assertion is the invariant, not an exact length. One cap now bounds two
// distinct pathologies, a line that never ends and an event that never ends, so
// where the cut lands depends on which one is hit first; pinning a number would
// test that arithmetic rather than the contract.
func TestSSETapFlagsTruncated(t *testing.T) {
	const observeCap = 16

	var observed []byte
	var truncated bool
	tap := newSSETap(io.NopCloser(strings.NewReader("")), observeCap, func(d []byte, tr bool) {
		observed = append([]byte(nil), d...)
		truncated = tr
	})
	tap.feed([]byte("data: " + strings.Repeat("x", 32) + "\n\n"))

	if !truncated {
		t.Fatal("expected truncated=true for an oversized SSE event")
	}
	if len(observed) == 0 || len(observed) > observeCap {
		t.Fatalf("observed len = %d, want between 1 and %d", len(observed), observeCap)
	}
}

// TestSSETapKeepsAWholeEventUnflagged pins the other side, since a cap that is
// always tripped would satisfy the test above while flagging every frame of a
// real session.
func TestSSETapKeepsAWholeEventUnflagged(t *testing.T) {
	const msg = `{"jsonrpc":"2.0","id":1,"result":{}}`

	var observed []byte
	var truncated bool
	tap := newSSETap(io.NopCloser(strings.NewReader("")), maxFrameBytes, func(d []byte, tr bool) {
		observed = append([]byte(nil), d...)
		truncated = tr
	})
	tap.feed([]byte("data: " + msg + "\n\n"))

	if truncated {
		t.Fatal("an event well inside the cap must not be flagged")
	}
	if string(observed) != msg {
		t.Fatalf("observed %q, want the whole event %q", observed, msg)
	}
}

// TestHTTPObservesAnEmptyBodiedFailure is the regression. A 401 carries its
// challenge in a header and no body, and emitFrames returns early on an empty
// body, so the most common failure of a remote MCP server produced no envelope
// at all: an empty session with no explanation.
func TestHTTPObservesAnEmptyBodiedFailure(t *testing.T) {
	const challenge = `Bearer resource_metadata="https://auth.example/.well-known/oauth-protected-resource"`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set(wwwAuthenticateHeader, challenge)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	resp, err := http.Post(front.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the client must still receive the real status, got %d", resp.StatusCode)
	}

	s2c := sink.byDir(ServerToClient)
	if len(s2c) != 1 {
		t.Fatalf("a 401 with no body must still produce one frame; producing none is the bug, got %d", len(s2c))
	}
	if s2c[0].Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", s2c[0].Status)
	}
	if s2c[0].AuthChallenge != challenge {
		t.Fatalf("the challenge should survive verbatim, got %q", s2c[0].AuthChallenge)
	}
	if len(s2c[0].Raw) != 0 {
		t.Fatalf("a bodiless response should carry no raw bytes, got %q", s2c[0].Raw)
	}
}

// TestHTTPObservesAnUnreachableTarget covers the path that never reaches
// ModifyResponse: the reverse proxy synthesises the 502 itself and writes it
// straight to the client. Pointing mcpsnoop at the wrong port is a common first
// mistake, and an empty screen is the worst answer to it.
func TestHTTPObservesAnUnreachableTarget(t *testing.T) {
	// Closed before use, so the address is real and nothing is listening on it.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target, _ := url.Parse(dead.URL)
	dead.Close()

	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	resp, err := http.Post(front.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("the client should get the synthesised 502, got %d", resp.StatusCode)
	}

	s2c := sink.byDir(ServerToClient)
	if len(s2c) != 1 {
		t.Fatalf("an unreachable target must not be silent, got %d frames", len(s2c))
	}
	if s2c[0].Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", s2c[0].Status)
	}
}

// TestHTTPStatusRidesEveryBatchElement pins the response-scoped half of the
// route. Routing headers describe one operation so they ride only the first
// element, but the status belongs to the response as a whole, and dropping it
// would make a batched response the one place the transport layer went missing.
func TestHTTPStatusRidesEveryBatchElement(t *testing.T) {
	const body = `[{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"nope"}},{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"nope"}}]`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, body)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, emitterTo(sink)))
	defer front.Close()

	resp, err := http.Post(front.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	s2c := sink.byDir(ServerToClient)
	if len(s2c) != 2 {
		t.Fatalf("a two-element batch should split into two frames, got %d", len(s2c))
	}
	for i, env := range s2c {
		if env.Status != http.StatusInternalServerError {
			t.Fatalf("batch element %d lost the status: %d", i, env.Status)
		}
	}
}

// TestHTTPDoesNotInventAFailureOnClientCancellation. ReverseProxy routes a
// cancelled request context through ErrorHandler exactly like an unreachable
// target, because a cancelled context surfaces as a RoundTrip error. A 502
// counts as an error, so a client that simply hung up, or a shutdown that
// cancelled what was still in flight, would fail a check run on an otherwise
// clean session.
func TestHTTPDoesNotInventAFailureOnClientCancellation(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release // never answers before the client gives up
	}))
	// LIFO: the handler is released before Close waits on it.
	defer backend.Close()
	defer close(release)

	target, _ := url.Parse(backend.URL)
	sink := &captureSink{}
	// Wrapped so the test can wait for the proxy handler to unwind instead of
	// sleeping: once ServeHTTP has returned, ErrorHandler has run if it ran at all.
	proxied := httpProxyHandler(target, emitterTo(sink))
	done := make(chan struct{})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		proxied.ServeHTTP(w, r)
	}))
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-arrived
		cancel()
	}()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("the cancelled request should not have completed")
	}
	<-done

	for _, env := range sink.byDir(ServerToClient) {
		t.Fatalf("a client hanging up is not a target failure, got status %d", env.Status)
	}
}

// TestProxyForwardsTheRequestUnchanged pins what the target actually receives.
// mcpsnoop sits in the real data path and CONTRIBUTING calls that transparency
// invariant, so this exists to make any change to the forwarding path, including
// a mechanical one like migrating off a deprecated hook, prove it altered
// nothing. Only Accept-Encoding is deliberately rewritten, because an observer
// cannot read a compressed body.
func TestProxyForwardsTheRequestUnchanged(t *testing.T) {
	var got *http.Request
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer target.Close()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}

	var conns []string
	front := httptest.NewServer(httpProxyHandler(u, func(_ Direction, _ []byte, rt route) {
		if rt.conn != "" {
			conns = append(conns, rt.conn)
		}
	}))
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/mcp?a=1&raw=%zz&b=2",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "ping")
	req.Header.Set("Origin", "http://localhost:1234")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got == nil {
		t.Fatal("the target was never reached")
	}
	if got.Host != u.Host {
		t.Fatalf("Host = %q, want the target's %q", got.Host, u.Host)
	}
	// The routing headers and everything else the client wrote arrive verbatim.
	for header, want := range map[string]string{
		"Accept":          "application/json, text/event-stream",
		"Mcp-Method":      "ping",
		"Origin":          "http://localhost:1234",
		"Accept-Encoding": "identity",
	} {
		if got := got.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// This hop is appended to whatever chain the client sent rather than replacing
	// it, and no X-Forwarded-Host or X-Forwarded-Proto is invented.
	if xff := got.Header.Get("X-Forwarded-For"); !strings.HasPrefix(xff, "203.0.113.9, ") {
		t.Errorf("X-Forwarded-For = %q, want the client's chain with this hop appended", xff)
	}
	for _, invented := range []string{"X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if v := got.Header.Get(invented); v != "" {
			t.Errorf("%s = %q, a header the target never used to see", invented, v)
		}
	}
	// A query the net/url parser rejects still reaches the target, since dropping
	// part of a request is exactly what a transparent proxy must not do.
	if got.URL.RawQuery != "a=1&raw=%zz&b=2" {
		t.Errorf("RawQuery = %q, want it forwarded byte for byte", got.URL.RawQuery)
	}
	// The observed frames carry the client's address, which is what keeps two
	// clients that both start at JSON-RPC id 1 in separate id spaces.
	if len(conns) == 0 || conns[0] == "" {
		t.Fatalf("no frame carried a connection identity: %v", conns)
	}
}

// TestProxyCapturesTheSpecMandatedHeaders. check already gates on routing
// headers, but the rest of the transport's mandatory headers reached no frame,
// so no signal could be written against them. Content-Type was the sharpest
// case: the response side already read it to pick the SSE branch and then threw
// it away.
func TestProxyCapturesTheSpecMandatedHeaders(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer target.Close()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}

	var got []Envelope
	front := httptest.NewServer(httpProxyHandler(u, func(dir Direction, body []byte, rt route) {
		got = append(got, Envelope{Direction: dir, TransportHeaders: rt.headers})
	}))
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:1234")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var request, response *TransportHeaders
	for _, e := range got {
		if e.Direction == ClientToServer {
			request = e.TransportHeaders
		} else {
			response = e.TransportHeaders
		}
	}
	if request == nil {
		t.Fatal("the client frame carried no transport headers")
	}
	for field, want := range map[string]string{
		"Accept":      "application/json, text/event-stream",
		"ContentType": "application/json",
		"Origin":      "http://localhost:1234",
	} {
		var got string
		switch field {
		case "Accept":
			got = request.Accept
		case "ContentType":
			got = request.ContentType
		case "Origin":
			got = request.Origin
		}
		if got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if response == nil || response.ContentType != "application/json" {
		t.Fatalf("the response frame did not carry its Content-Type: %+v", response)
	}
	// A pointer, not a value, so a log written before this existed stays
	// distinguishable from a request that genuinely carried none.
	if got := (&Envelope{}).TransportHeaders; got != nil {
		t.Fatalf("an envelope with nothing captured is not nil: %+v", got)
	}
}

// TestProxyKeepsEveryAuthChallenge. AuthChallenge is the one response header
// this proxy keeps verbatim, because a 401 without it is a dead end. Reading it
// with Header.Get kept only the first line, so a server offering DPoP beside
// Bearer had half its answer dropped before anything could read it.
func TestProxyKeepsEveryAuthChallenge(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("WWW-Authenticate", `Bearer realm="mcp", resource_metadata="https://x/.well-known/oauth-protected-resource"`)
		w.Header().Add("WWW-Authenticate", `DPoP algs="ES256"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"unauthorized"}}`)
	}))
	defer target.Close()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}

	var challenge string
	front := httptest.NewServer(httpProxyHandler(u, func(dir Direction, _ []byte, rt route) {
		if dir == ServerToClient && rt.challenge != "" {
			challenge = rt.challenge
		}
	}))
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for _, want := range []string{"Bearer", "resource_metadata", "DPoP", "ES256"} {
		if !strings.Contains(challenge, want) {
			t.Errorf("the captured challenge lost %q: %q", want, challenge)
		}
	}
}

// TestHTTPFramesReachTheSinkInSequenceOrder. An atomic counter makes every Seq
// distinct but says nothing about the order frames reach the sink in. Two
// concurrent requests arriving as 6 then 5 make the store's gap detector infer a
// dropped frame, and a capture that lost nothing then fails check --fail-on
// incomplete. RunStdio has held a lock across the emit for this reason since it
// hit the same thing.
func TestHTTPFramesReachTheSinkInSequenceOrder(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer target.Close()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []uint64
	// The sink is deliberately slow. Holding the lock across the emit serializes
	// these by construction, while releasing it after the increment lets every
	// waiting goroutine pile into the sink at once, so the reorder is reliable
	// rather than a race the test has to be lucky to catch.
	emit := newHTTPEmitter(HTTPConfig{SessionID: "s1", Label: "l"}, &orderSink{fn: func(e Envelope) {
		time.Sleep(time.Millisecond)
		mu.Lock()
		seen = append(seen, e.Seq)
		mu.Unlock()
	}})
	front := httptest.NewServer(httpProxyHandler(u, emit))
	defer front.Close()

	var wg sync.WaitGroup
	for i := range 60 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, front.URL+"/mcp",
				strings.NewReader(`{"jsonrpc":"2.0","id":`+strconv.Itoa(i)+`,"method":"ping"}`))
			if err != nil {
				return
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// The store's own gap arithmetic, run over the order the sink actually saw.
	var last, missing uint64
	for _, s := range seen {
		if s > last {
			if expected := last + 1; s > expected {
				missing += s - expected
			}
			last = s
		}
	}
	if missing > 0 {
		t.Fatalf("a capture that lost nothing reports %d missing frames out of %d, arrival order %v",
			missing, len(seen), seen)
	}
}

type orderSink struct{ fn func(Envelope) }

func (o *orderSink) Emit(e Envelope) { o.fn(e) }
func (o *orderSink) Close() error    { return nil }

// TestHTTPEmitterWritesNothingUntilTheFirstFrame is the reason the meta frame is
// built at startup but emitted lazily. ResolveSessionPath skips zero-byte logs so
// that a bare check or export means the last real capture rather than a listener
// somebody started and never called, and that listener is always the newest file
// in the sessions directory. Emitting eagerly would put a line in it and defeat
// the skip.
func TestHTTPEmitterWritesNothingUntilTheFirstFrame(t *testing.T) {
	sink := &captureSink{}
	emit := newHTTPEmitter(HTTPConfig{SessionID: "s", Label: "l", Target: "https://h/mcp"}, sink)

	if got := len(sink.byDir(DirectionMeta)); got != 0 {
		t.Fatalf("an idle proxy emitted %d meta frames, want 0", got)
	}

	emit(ClientToServer, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), route{})

	meta := sink.byDir(DirectionMeta)
	if len(meta) != 1 {
		t.Fatalf("meta frames after the first request = %d, want 1", len(meta))
	}
	if meta[0].Seq != 1 {
		t.Fatalf("meta Seq = %d, want 1, since the store reads the session from the frame before the traffic", meta[0].Seq)
	}
	if meta[0].Transport != TransportHTTP {
		t.Fatalf("meta Transport = %q, want %q", meta[0].Transport, TransportHTTP)
	}
	if meta[0].SessionID != "s" || meta[0].ServerLabel != "l" {
		t.Fatalf("meta identity = %q/%q, want s/l", meta[0].SessionID, meta[0].ServerLabel)
	}

	first := sink.byDir(ClientToServer)
	if len(first) != 1 || first[0].Seq != 2 {
		t.Fatalf("first traffic frame = %+v, want one frame at Seq 2", first)
	}
	// The meta borrows the frame's own timestamp rather than reading the clock
	// under the lock. The frame reads it before locking, so a second reading here
	// would always be later and seq 1 would sort after seq 2.
	if !meta[0].TS.Equal(first[0].TS) {
		t.Fatalf("meta TS %v is not the frame's own %v", meta[0].TS, first[0].TS)
	}

	emit(ServerToClient, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), route{})
	if got := len(sink.byDir(DirectionMeta)); got != 1 {
		t.Fatalf("meta frames after a second request = %d, want it emitted exactly once", got)
	}
}

// TestHTTPEmitterRecordsTheStrippedEndpoint checks the bytes that actually reach
// the log, not just EndpointForLog's return value, since the meta frame is the
// only place a target is written down and redaction is off by default.
func TestHTTPEmitterRecordsTheStrippedEndpoint(t *testing.T) {
	sink := &captureSink{}
	emit := newHTTPEmitter(HTTPConfig{SessionID: "s", Target: "https://u:sk-live-secret@api.example.com:8443/mcp?api_key=sk-live-secret&tenant=acme#frag"}, sink)
	emit(ClientToServer, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), route{})

	meta := sink.byDir(DirectionMeta)
	if len(meta) != 1 {
		t.Fatalf("meta frames = %d, want 1", len(meta))
	}
	if strings.Contains(string(meta[0].Raw), "sk-live-secret") {
		t.Fatalf("the log carries the credential: %s", meta[0].Raw)
	}
	var got SessionMeta
	if err := json.Unmarshal(meta[0].Raw, &got); err != nil {
		t.Fatalf("meta frame is not a SessionMeta: %v", err)
	}
	const want = "https://[stripped]@api.example.com:8443/mcp?api_key=[stripped]&tenant=[stripped]"
	if got.Target != want {
		t.Fatalf("Target = %q, want %q", got.Target, want)
	}
	if len(got.Command) != 0 || got.CWD != "" {
		t.Fatalf("an HTTP session recorded a command to run: %+v", got)
	}
}

// TestHTTPTargetFragmentReachesNeitherTheServerNorTheLog pins the assumption
// behind dropping the fragment, which is that it was never on the wire to begin
// with. If Go ever forwarded it, dropping it would be losing something.
func TestHTTPTargetFragmentReachesNeitherTheServerNorTheLog(t *testing.T) {
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.URL.RequestURI() + "|" + r.URL.Fragment:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	targetURL := backend.URL + "/mcp#never-sent"
	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	front := httptest.NewServer(httpProxyHandler(target, newHTTPEmitter(HTTPConfig{SessionID: "s", Target: targetURL}, sink)))
	defer front.Close()

	resp, err := http.Post(front.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	got := <-seen
	if strings.Contains(got, "never-sent") {
		t.Fatalf("the backend saw the fragment in %q, so dropping it from the log loses what was on the wire", got)
	}
	meta := sink.byDir(DirectionMeta)
	if len(meta) != 1 || strings.Contains(string(meta[0].Raw), "never-sent") {
		t.Fatalf("meta = %+v, want exactly one frame with no fragment", meta)
	}
}
