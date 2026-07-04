package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HTTPConfig configures a transparent MCP streamable-HTTP proxy run.
type HTTPConfig struct {
	// Listen is the address mcpsnoop serves on, e.g. ":7000".
	Listen string
	// Target is the real MCP server endpoint, e.g. "http://localhost:3000/mcp".
	Target string
	// Label identifies this server in the hub/TUI.
	Label string
	// SessionID uniquely identifies this proxy instance.
	SessionID string
	// Sink receives observed envelopes (best-effort). If nil, tracing is off.
	Sink Sink
}

// RunHTTP runs a reverse proxy in front of an MCP streamable-HTTP server,
// observing every JSON-RPC frame in both directions (plain JSON responses and
// text/event-stream SSE) while forwarding traffic transparently. It blocks
// until ctx is cancelled.
func RunHTTP(ctx context.Context, cfg HTTPConfig) error {
	if cfg.Target == "" {
		return errors.New("proxy: empty target")
	}
	target, err := url.Parse(cfg.Target)
	if err != nil {
		return err
	}
	sink := cfg.Sink
	if sink == nil {
		sink = NopSink()
	}

	handler := httpProxyHandler(target, newHTTPEmitter(cfg, sink))

	srv := &http.Server{Addr: cfg.Listen, Handler: handler}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newHTTPEmitter returns an emit function bound to a session and sink.
func newHTTPEmitter(cfg HTTPConfig, sink Sink) func(Direction, []byte) {
	var seq atomic.Uint64
	return func(dir Direction, raw []byte) {
		sink.Emit(Envelope{
			SessionID:   cfg.SessionID,
			ServerLabel: cfg.Label,
			Seq:         seq.Add(1),
			TS:          time.Now(),
			Direction:   dir,
			Transport:   "http",
			Raw:         append([]byte(nil), raw...),
		})
	}
}

// httpProxyHandler builds the reverse-proxy handler that taps request and
// response bodies. Exposed (unexported) for testing with httptest.
func httpProxyHandler(target *url.URL, emit func(Direction, []byte)) http.Handler {
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			if target.Path != "" && target.Path != "/" {
				req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				// Stream: tap SSE data frames as bytes flow to the client.
				resp.Body = newSSETap(resp.Body, func(data []byte) {
					emitFrames(emit, ServerToClient, data)
				})
				return nil
			}
			// Plain JSON: buffer, observe, restore.
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return err
			}
			emitFrames(emit, ServerToClient, body)
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err == nil {
				emitFrames(emit, ClientToServer, body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
		}
		rp.ServeHTTP(w, r)
	})
}

// emitFrames emits one envelope per JSON-RPC message in body, splitting a batch
// array into its elements.
func emitFrames(emit func(Direction, []byte), dir Direction, body []byte) {
	b := bytes.TrimSpace(body)
	if len(b) == 0 {
		return
	}
	if b[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(b, &arr) == nil {
			for _, m := range arr {
				emit(dir, m)
			}
			return
		}
	}
	emit(dir, b)
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

// sseTap passes SSE bytes through unchanged while extracting each event's
// `data:` payload (one JSON-RPC message per event) for observation.
type sseTap struct {
	rc      io.ReadCloser
	onData  func([]byte)
	lineBuf bytes.Buffer
	dataBuf bytes.Buffer
}

func newSSETap(rc io.ReadCloser, onData func([]byte)) *sseTap {
	return &sseTap{rc: rc, onData: onData}
}

func (t *sseTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.feed(p[:n])
	}
	return n, err
}

func (t *sseTap) feed(b []byte) {
	for _, c := range b {
		switch c {
		case '\n':
			t.line(t.lineBuf.Bytes())
			t.lineBuf.Reset()
		case '\r':
			// ignore
		default:
			t.lineBuf.WriteByte(c)
		}
	}
}

func (t *sseTap) line(l []byte) {
	if len(l) == 0 { // blank line ends an event
		if t.dataBuf.Len() > 0 {
			t.onData(append([]byte(nil), t.dataBuf.Bytes()...))
			t.dataBuf.Reset()
		}
		return
	}
	if rest, ok := bytes.CutPrefix(l, []byte("data:")); ok {
		if t.dataBuf.Len() > 0 {
			t.dataBuf.WriteByte('\n')
		}
		t.dataBuf.Write(bytes.TrimPrefix(rest, []byte(" ")))
	}
	// other SSE fields (event:, id:, retry:) are ignored
}

func (t *sseTap) Close() error {
	// flush any trailing partial event
	if t.lineBuf.Len() > 0 {
		t.line(t.lineBuf.Bytes())
		t.lineBuf.Reset()
	}
	if t.dataBuf.Len() > 0 {
		t.onData(append([]byte(nil), t.dataBuf.Bytes()...))
		t.dataBuf.Reset()
	}
	return t.rc.Close()
}
