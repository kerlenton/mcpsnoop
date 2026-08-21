package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
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

// mcpMethodHeader and mcpNameHeader are the SEP-2243 Streamable HTTP routing
// headers a client sets so a gateway can route without parsing the JSON-RPC body.
const (
	mcpMethodHeader          = "Mcp-Method"
	mcpNameHeader            = "Mcp-Name"
	mcpProtocolVersionHeader = "MCP-Protocol-Version"
)

// route carries the routing headers observed on a request, empty when the frame
// is a response or the client did not send them. batch marks a frame
// split out of a JSON-RPC batch array, which routing headers cannot address.
type route struct {
	method          string // Mcp-Method
	name            string // Mcp-Name
	protocolVersion string // MCP-Protocol-Version (request-scoped, not per operation)
	params          []MCPParamHeader
	batch           bool
	// conn identifies the client connection this frame was observed on. One proxy
	// process carries every client that connects to it, and JSON-RPC id
	// uniqueness is scoped to the sender rather than to the wire, so two
	// conforming clients both starting at id 1 collided in one id space.
	conn      string
	truncated bool   // the observed copy was cut at the frame-size cap
	status    int    // HTTP status of the response carrying this frame
	challenge string // WWW-Authenticate on that response
	// headers are the spec-mandated transport headers of this frame. A pointer so
	// a frame mcpsnoop captured with none is distinguishable from a log written
	// before it recorded them at all.
	headers *TransportHeaders
}

// wwwAuthenticateHeader carries the challenge on a 401. It is kept verbatim
// because it names the auth scheme and, under the MCP authorization spec, the
// resource metadata URL a client is meant to follow next.
const wwwAuthenticateHeader = "WWW-Authenticate"

// newHTTPEmitter returns an emit function bound to a session and sink.
func newHTTPEmitter(cfg HTTPConfig, sink Sink) func(Direction, []byte, route) {
	// The lock is held across the emit, not just the increment, the same way
	// RunStdio holds one. An atomic counter makes every Seq distinct but says
	// nothing about the order they reach the sink in, and two concurrent requests
	// arriving as 6 then 5 make the store's gap detector infer a dropped frame:
	// it advances lastSeq to 6, counts five missing, and then ignores 5 entirely
	// because it is no longer greater. A capture that lost nothing then fails
	// check --fail-on incomplete.
	var (
		seqMu    sync.Mutex
		seq      uint64
		metaSent bool
	)
	// The meta frame is built once here and emitted on the first observed frame
	// rather than at startup, so a proxy nobody has called yet leaves a zero-byte
	// log. ResolveSessionPath skips those on purpose: a bare check or export means
	// the last real capture, and a listener started an hour ago and never used is
	// the newest file in the directory. Emitting eagerly would put one line in it
	// and walk straight back into the bug that skip exists for.
	//
	// jsonwire, because this and RunStdio's copy are the only Envelope.Raw the
	// proxy builds instead of copying, and a target containing & would otherwise be
	// the one escaped frame in an otherwise verbatim log.
	metaRaw, metaErr := jsonwire.Marshal(SessionMeta{Target: EndpointForLog(cfg.Target)})
	return func(dir Direction, body []byte, r route) {
		raw, text := splitObserved(body)
		env := Envelope{
			SessionID:          cfg.SessionID,
			ServerLabel:        cfg.Label,
			TS:                 time.Now(),
			Direction:          dir,
			Transport:          TransportHTTP,
			Text:               text,
			MCPMethod:          r.method,
			MCPName:            r.name,
			MCPProtocolVersion: r.protocolVersion,
			MCPParamHeaders:    slices.Clone(r.params),
			Batch:              r.batch,
			ConnID:             r.conn,
			Truncated:          r.truncated,
			Status:             r.status,
			AuthChallenge:      r.challenge,
			TransportHeaders:   r.headers,
		}
		if raw != nil {
			env.Raw = append([]byte(nil), raw...)
		}
		seqMu.Lock()
		defer seqMu.Unlock()
		// Under the same lock as the frame it precedes, so the meta reaches the sink
		// first and takes seq 1 with no window for a second goroutine to claim it.
		// Its timestamp is the frame's own, since the frame read the clock before
		// taking the lock and a later reading here would order seq 1 after seq 2.
		if !metaSent {
			metaSent = true
			if metaErr == nil {
				seq++
				sink.Emit(Envelope{
					SessionID:   cfg.SessionID,
					ServerLabel: cfg.Label,
					Seq:         seq,
					TS:          env.TS,
					Direction:   DirectionMeta,
					Transport:   TransportHTTP,
					Raw:         metaRaw,
				})
			}
		}
		seq++
		env.Seq = seq
		sink.Emit(env)
	}
}

func mcpParamHeaders(header http.Header) []MCPParamHeader {
	var params []MCPParamHeader
	for name, values := range header {
		if !strings.HasPrefix(strings.ToLower(name), "mcp-param-") {
			continue
		}
		for _, value := range values {
			params = append(params, MCPParamHeader{Name: name, Value: value})
		}
	}
	slices.SortStableFunc(params, func(a, b MCPParamHeader) int {
		if n := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	return params
}

// bodyTap forwards an HTTP body verbatim while copying at most cap bytes for
// observation, so a huge upload or response is never held whole in memory. When
// the body runs past cap the extra bytes still forward, but the observed copy is
// flagged truncated. onDone fires exactly once, on EOF or Close, with the copy.
type bodyTap struct {
	rc     io.ReadCloser
	cap    int
	onDone func(observed []byte, truncated bool)

	// net/http may Close a request body from a goroutine other than the one
	// reading it (e.g. on cancellation), so the buffer and done flag are guarded.
	mu   sync.Mutex
	buf  bytes.Buffer
	cut  bool
	done bool
}

func newBodyTap(rc io.ReadCloser, cap int, onDone func([]byte, bool)) *bodyTap {
	return &bodyTap{rc: rc, cap: cap, onDone: onDone}
}

func (t *bodyTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.mu.Lock()
		if room := t.cap - t.buf.Len(); room <= 0 {
			t.cut = true
		} else if n > room {
			t.buf.Write(p[:room])
			t.cut = true
		} else {
			t.buf.Write(p[:n])
		}
		t.mu.Unlock()
	}
	if err != nil { // EOF or a read error both end the body
		t.finish()
	}
	return n, err
}

func (t *bodyTap) Close() error {
	t.finish()
	return t.rc.Close()
}

// finish delivers the observed copy exactly once, on EOF, a read error, or Close,
// whichever comes first. The snapshot is taken under the lock so it is never read
// while Read is writing, but onDone runs outside the lock since it emits into the
// sink and must not hold the tap.
func (t *bodyTap) finish() {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	observed := append([]byte(nil), t.buf.Bytes()...)
	truncated := t.cut
	t.mu.Unlock()
	t.onDone(observed, truncated)
}

// observeBody emits the observed copy of a body. A truncated copy is incomplete,
// so it is emitted as one flagged frame rather than split and parsed as if whole.
func observeBody(emit func(Direction, []byte, route), dir Direction, observed []byte, truncated bool, rt route) {
	if truncated {
		rt.truncated = true
		emit(dir, observed, rt)
		return
	}
	// A response with an empty body still says something, and until now it said
	// it to nobody: emitFrames returns early on empty, so a 401 challenge and the
	// 202 that acknowledges a notification produced no envelope at all. Emit one
	// frame carrying just the status. Gated on the status being set, which is
	// true only for responses, so an empty request body stays silent.
	if rt.status != 0 && len(bytes.TrimSpace(observed)) == 0 {
		emit(dir, nil, rt)
		return
	}
	emitFrames(emit, dir, observed, rt)
}

// appendForwardedFor reproduces what a Director-based ReverseProxy did on its
// own: add this hop to the client's X-Forwarded-For chain rather than replace
// it. Rewrite strips the inbound value before it is called, so without this the
// target would stop seeing where a request came from.
func appendForwardedFor(r *httputil.ProxyRequest) {
	hop, _, err := net.SplitHostPort(r.In.RemoteAddr)
	if err != nil {
		return // no address to add, which is not a reason to invent one
	}
	if prior := r.In.Header.Values("X-Forwarded-For"); len(prior) > 0 {
		hop = strings.Join(prior, ", ") + ", " + hop
	}
	r.Out.Header.Set("X-Forwarded-For", hop)
}

// joinChallenges keeps every WWW-Authenticate line a response carried. Get
// returns only the first, so a server offering Bearer alongside DPoP lost half
// of what it said, on the one response header this proxy keeps verbatim because
// a 401 without it is a dead end. RFC 9110 makes several field lines equivalent
// to one comma-joined line, so a reader parses the join exactly as it would have
// parsed the lines.
func joinChallenges(values []string) string {
	return strings.Join(values, ", ")
}

// httpProxyHandler builds the reverse-proxy handler that taps request and
// response bodies. Exposed (unexported) for testing with httptest.
func httpProxyHandler(target *url.URL, emit func(Direction, []byte, route)) http.Handler {
	rp := &httputil.ReverseProxy{
		// Rewrite rather than the deprecated Director, and deliberately not
		// SetXForwarded. This proxy sits in the real data path, so a migration off
		// a deprecated hook has to leave the target seeing exactly what it saw
		// before. Rewrite's own defaults do not: it strips the client's
		// X-Forwarded-* headers, and SetXForwarded would replace the forwarding
		// chain with this one hop and invent an X-Forwarded-Host and
		// X-Forwarded-Proto the target never used to receive.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = target.Scheme
			r.Out.URL.Host = target.Host
			r.Out.Host = target.Host
			if target.Path != "" && target.Path != "/" {
				r.Out.URL.Path = target.Path
			}
			// Rewrite drops query parameters net/url cannot parse. Dropping part of
			// a request is the one thing a transparent proxy must not do, so the
			// query the client wrote is put back verbatim.
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			// mcpsnoop has to read the response body to observe it, so it must not
			// arrive compressed. Force identity rather than let the client's gzip
			// preference reach the target and turn every observed frame into noise.
			// identity is always acceptable to the client.
			r.Out.Header.Set("Accept-Encoding", "identity")
			appendForwardedFor(r)
			// The routing headers are not hop-by-hop, so the reverse proxy forwards
			// them to the target verbatim; nothing here touches them.
		},
		ModifyResponse: func(resp *http.Response) error {
			// The status and the challenge ride every frame observed on this
			// response, so the transport layer is visible even when the body is not.
			rt := route{
				status:    resp.StatusCode,
				challenge: joinChallenges(resp.Header.Values(wwwAuthenticateHeader)),
				// Read a few lines below to pick the SSE branch, and until now
				// discarded afterwards, so nothing could check the rule it decides.
				headers: &TransportHeaders{ContentType: resp.Header.Get("Content-Type")},
			}
			if resp.Request != nil {
				rt.conn = resp.Request.RemoteAddr
			}
			// Defensive fallback. If the target ignored the identity request and
			// still compressed the body, skip observation rather than push binary
			// into a frame. Forwarding is untouched, the client gets the original body.
			if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
				return nil
			}
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				// Streaming case, tap SSE data frames as bytes flow to the client.
				resp.Body = newSSETap(resp.Body, maxFrameBytes, func(data []byte, truncated bool) {
					r := rt
					if truncated {
						// A cut event is a fragment, so it is emitted whole and flagged
						// rather than split and parsed as if complete. emit routes it
						// through splitObserved, which puts a fragment in Text: Raw is a
						// json.RawMessage and an invalid one makes the envelope fail to
						// marshal in the sinks.
						r.truncated = true
						emit(ServerToClient, data, r)
						return
					}
					emitFrames(emit, ServerToClient, data, r)
				})
				return nil
			}
			// Plain JSON case. Stream the body through a tap that copies at most
			// maxFrameBytes for observation, so it is never buffered whole. The body
			// passes through unchanged, so Content-Length stays correct with no rewrite.
			resp.Body = newBodyTap(resp.Body, maxFrameBytes, func(observed []byte, truncated bool) {
				observeBody(emit, ServerToClient, observed, truncated, rt)
			})
			return nil
		},
		// A target that refuses the connection, times out, or speaks garbage never
		// reaches ModifyResponse: the reverse proxy synthesises a 502 and writes it
		// straight to the client. Pointing mcpsnoop at the wrong port is a common
		// first mistake, and an empty screen is the worst possible answer to it.
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			// A cancelled context means the client went away or mcpsnoop is shutting
			// down, not that the target failed. ReverseProxy routes both through here
			// because a cancelled request context surfaces as a RoundTrip error, and
			// srv.Shutdown cancels whatever is still in flight. Inventing a 502 out of
			// a normal disconnect would count an error and fail a check run on a clean
			// session, which is the failure mode this whole change exists to avoid.
			// A deadline is not excluded: a target that timed out did fail.
			if !errors.Is(err, context.Canceled) {
				emit(ServerToClient, nil, route{status: http.StatusBadGateway, conn: req.RemoteAddr})
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method == http.MethodPost {
			rt := route{
				method:          r.Header.Get(mcpMethodHeader),
				name:            r.Header.Get(mcpNameHeader),
				protocolVersion: r.Header.Get(mcpProtocolVersionHeader),
				headers: &TransportHeaders{
					Accept:      r.Header.Get("Accept"),
					ContentType: r.Header.Get("Content-Type"),
					Origin:      r.Header.Get("Origin"),
				},
				params: mcpParamHeaders(r.Header),
				conn:   r.RemoteAddr,
			}
			// Same streaming tap for the request body, so a large upload is forwarded
			// without being held in memory whole.
			r.Body = newBodyTap(r.Body, maxFrameBytes, func(observed []byte, truncated bool) {
				observeBody(emit, ClientToServer, observed, truncated, rt)
			})
		}
		rp.ServeHTTP(w, r)
	})
}

// emitFrames emits one envelope per JSON-RPC message in body, splitting a batch
// array into its elements. A request's routing headers describe a single
// operation, so they cannot be copied onto every batch element (that would
// fabricate a method mismatch on all but the matching one). Instead each element
// is flagged as batched and the routing headers ride only the first, letting the
// store surface a single "invalid on a batch" warning rather than per-element
// noise. The protocol version is request-scoped, so it rides every element.
func emitFrames(emit func(Direction, []byte, route), dir Direction, body []byte, rt route) {
	b := bytes.TrimSpace(body)
	if len(b) == 0 {
		return
	}
	if b[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(b, &arr) == nil {
			for i, m := range arr {
				// The status and the challenge belong to the response as a whole, not
				// to one operation in it, so unlike the routing headers they ride every
				// element. Dropping them here would make a batched response the one
				// place the transport layer went missing again.
				er := route{batch: true, protocolVersion: rt.protocolVersion, status: rt.status,
					challenge: rt.challenge, conn: rt.conn}
				if i == 0 {
					er.method, er.name, er.params = rt.method, rt.name, rt.params
				}
				emit(dir, m, er)
			}
			return
		}
	}
	emit(dir, b, rt)
}

// sseTap passes SSE bytes through unchanged while extracting each event's
// `data:` payload (one JSON-RPC message per event) for observation.
//
// cap bounds what is kept, never what is forwarded, and it bounds two distinct
// pathologies with one number: a server that never sends a newline, which would
// grow lineBuf, and a stream that never sends its terminating blank line, which
// would grow dataBuf. Either cut sets truncated, so a fragment is reported as
// short rather than read as a corrupted frame.
type sseTap struct {
	rc        io.ReadCloser
	cap       int
	onData    func([]byte, bool)
	lineBuf   bytes.Buffer
	dataBuf   bytes.Buffer
	truncated bool
}

func newSSETap(rc io.ReadCloser, cap int, onData func([]byte, bool)) *sseTap {
	return &sseTap{rc: rc, cap: cap, onData: onData}
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
			// Cap the observed line so a server that never sends a newline cannot
			// grow this buffer without bound. Forwarding is unaffected.
			if t.lineBuf.Len() < t.cap {
				t.lineBuf.WriteByte(c)
			} else {
				t.truncated = true
			}
		}
	}
}

func (t *sseTap) line(l []byte) {
	if len(l) == 0 { // blank line ends an event
		if t.dataBuf.Len() > 0 {
			t.onData(append([]byte(nil), t.dataBuf.Bytes()...), t.truncated)
			t.dataBuf.Reset()
			t.truncated = false
		}
		return
	}
	if rest, ok := bytes.CutPrefix(l, []byte("data:")); ok {
		// Trim the optional single space before measuring, so the cap bounds the
		// payload rather than the payload plus SSE framing.
		rest = bytes.TrimPrefix(rest, []byte(" "))
		room := t.cap - t.dataBuf.Len()
		if room <= 0 {
			t.truncated = true
			return
		}
		if t.dataBuf.Len() > 0 {
			// A multi-line event joins its data fields with a newline, and that
			// newline counts against the cap like any other kept byte.
			t.dataBuf.WriteByte('\n')
			room--
		}
		if len(rest) > room {
			t.dataBuf.Write(rest[:room])
			t.truncated = true
			return
		}
		t.dataBuf.Write(rest)
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
		t.onData(append([]byte(nil), t.dataBuf.Bytes()...), t.truncated)
		t.dataBuf.Reset()
		t.truncated = false
	}
	return t.rc.Close()
}
