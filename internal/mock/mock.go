// Package mock serves a capture back as a stdio MCP server.
//
// It is the inverse of internal/replay: replay is an MCP client that launches
// the real server and sends one request, while mock is a server backed entirely
// by an existing capture and answers a whole session. The capture is loaded
// through the same machinery as every other reader (proxy envelopes folded into
// an unbounded store.Store), and the store stays the authority on correlation:
// the mock never re-implements matchRetry or invents its own MRTR linkage. It
// only pairs the individual wire exchanges the timeline already correlated, so
// an MRTR retry the store linked stays a distinct hop of the same operation and
// one it refused stays its own call.
//
// Matching is strict by decision of issue #208's conservative direction:
// method plus structurally normalized params, with only the volatile _meta keys
// stripped (plus top-level clientInfo on initialize alone). An unmatched
// request gets a JSON-RPC error naming the method, never a silent unrelated
// answer. Nothing is synthesised: a server/discover the capture never recorded
// is answered with an error (the spec's own stdio fallback then lets a legacy
// client fall back to initialize), and a request the capture never answered
// has no exchange to serve. Only stdio captures serve; HTTP is refused.
//
// Only responses are served. A stdio server also writes notifications that
// relate to an in-flight request, progress and logging among them, and the
// capture holds the ones the real server sent. Replaying those means rewriting
// each recorded progressToken onto the incoming one and dropping the lot when
// the replaying client did not opt in, which is its own piece of work rather
// than a line here, so a mocked call completes without the progress the
// original reported.
//
// Stdout is protocol-only. Diagnostics go to stderr.
package mock

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// volatileMetaKeys are the _meta entries that legitimately differ between the
// client that was recorded and the client replaying now. Client identity and
// capabilities ride every request, and progress, logging level and the W3C
// trace keys share the same object, so two requests that are the same logical
// call differ in bytes. These come out before comparison and everything else
// compares whole, which keeps protocolVersion in the key, where it belongs,
// since the era genuinely changes what an answer means.
//
// The set is drawn from the reserved _meta keys the spec lists at
// https://modelcontextprotocol.io/specification/2026-07-28/basic/index#_meta
// and holds only the ones that cannot change what the server replied.
// subscriptionId is absent because it rides server notifications rather than
// client requests.
var volatileMetaKeys = map[string]struct{}{
	"progressToken":                              {},
	"io.modelcontextprotocol/clientInfo":         {},
	"io.modelcontextprotocol/clientCapabilities": {},
	// logLevel is the minimum log level the server should emit for a request.
	// It selects which notifications/message frames a live server sends and
	// cannot touch the result, and the mock answers with responses only, so a
	// client that configures logging would otherwise miss every recorded call
	// over a key that changes nothing about the answer.
	"io.modelcontextprotocol/logLevel": {},
	"traceparent":                      {},
	"tracestate":                       {},
	"baggage":                          {},
}

// UnmatchedPolicy decides what a request the capture never recorded gets. Only
// the strict arm exists: a JSON-RPC error naming the method. A looser
// method-only fallback (answer with another params' response) is what the
// cassette tools ship by default and what issue #208 treats as unfalsifiable,
// so it stays out unless a maintainer explicitly chooses it. Adding it means a
// new enumerant here plus a flag in cmd/mcpsnoop/mock.go, not a second matcher.
type UnmatchedPolicy int

const (
	// UnmatchedError answers every unrecorded request with a JSON-RPC error
	// naming the method and reports the miss on stderr.
	UnmatchedError UnmatchedPolicy = iota
)

// Policy bundles the two decisions issue #208 leaves open, so either can be
// switched during review without touching the serve loop.
type Policy struct {
	// AllowRedacted serves a capture containing redacted frames. The default
	// refuses: redacted bytes are placeholders, not genuine recorded output.
	AllowRedacted bool
	// OnUnmatched is the unmatched-request policy. Only UnmatchedError exists.
	OnUnmatched UnmatchedPolicy
}

// exchange is one recorded wire exchange: a request the capture answered and
// the complete recorded response object. The bytes are the wire truth, not a
// reconstruction: a mock reproduces what the server actually did, including
// malformed or extension-bearing frames, so only the id is ever swapped.
// MRTR hops are separate exchanges sharing one logical call; the store
// already decided the linkage and this only replays it.
type exchange struct {
	response json.RawMessage
}

// group is the FIFO queue of recorded exchanges behind one match key.
// Repeated identical requests replay in recorded order rather than collapsing
// to one entry: each incoming request consumes the next exchange.
type group struct {
	exchanges []exchange
	next      int
}

// Server is a capture ready to answer on stdio.
type Server struct {
	sessionID string
	groups    map[string]*group
	exchanges int
	redacted  bool
}

// SessionID is the capture's first session, the one being served.
func (s *Server) SessionID() string { return s.sessionID }

// Redacted reports whether any frame of the capture carries the structured
// redaction mark. Read off proxy.Envelope.Redacted (and the per-value header
// mark), never off the payload bytes, since "[REDACTED]" is a legal argument a
// client can send.
func (s *Server) Redacted() bool { return s.redacted }

// Exchanges is how many recorded request->response pairs the capture holds.
func (s *Server) Exchanges() int { return s.exchanges }

// Load reads a JSONL envelope stream and builds the serving index for its
// first session, the same rule exporter.Load applies to a multi-session log.
// A capture with no meta frame still loads: the recorded command is what
// replay needs and mock needs nothing from it.
//
// Loading is strict: unlike the live proxy.Decode path, a truncated final
// envelope is a corrupt cassette and fails here rather than reading as a
// clean end. Only stdio captures serve: a session explicitly captured on
// another transport (HTTP) is refused, while a legacy log whose frames name
// no transport is accepted.
func Load(r io.Reader, source string) (*Server, error) {
	st := store.New()
	var firstSession string
	redacted := false
	// Decoded with a plain decoder rather than proxy.Decode on purpose: Decode
	// treats a torn tail as a clean end, which is right for a live stream and
	// wrong for a cassette, where the missing bytes are the finding.
	dec := json.NewDecoder(r)
	for {
		var env proxy.Envelope
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("%s: invalid JSONL envelope: %w", source, err)
		}
		if firstSession == "" {
			firstSession = env.SessionID
		}
		// Scoped to the served session only, so a redacted second session in a
		// concatenated file never makes a clean first session unservable.
		if env.SessionID == firstSession {
			if env.Redacted {
				redacted = true
			}
			for _, h := range env.MCPParamHeaders {
				if h.Redacted {
					redacted = true
					break
				}
			}
		}
		st.Ingest(env)
	}
	if firstSession == "" {
		return nil, fmt.Errorf("%s: no envelopes found", source)
	}
	if transport := sessionTransport(st, firstSession); transport != "" && transport != proxy.TransportStdio {
		return nil, fmt.Errorf("%s: session %q was captured on %q transport, mock serves stdio captures only", source, firstSession, transport)
	}
	return build(st, firstSession, redacted), nil
}

// sessionTransport is the channel the session was captured on, empty for a
// legacy log whose frames named none.
func sessionTransport(st *store.Store, sessionID string) string {
	for _, h := range st.Sessions() {
		if h.ID == sessionID {
			return h.Transport
		}
	}
	return ""
}

// LoadFile is Load over a capture file.
func LoadFile(path string) (*Server, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f, path)
}

// wireRequest is a recorded client request frame the mock may have to answer.
type wireRequest struct {
	method string
	params json.RawMessage
}

// wireKey identifies one physical request frame: its JSON-RPC id plus the
// store call it opened. The call half is what keeps this from becoming a
// second correlation engine.
type wireKey struct {
	id      string
	callSeq uint64
}

// build indexes the recorded wire exchanges off the store's own correlation.
// Only the server role is served: client-to-server requests and the
// server-to-client responses the store matched to them. A response is paired
// with the request its event's call names, which is how an id reused in flight
// resolves: the store marks the earlier request Superseded and the later one
// owns the eventual response, so the earlier one can never consume it. MRTR
// hops stay separate physical exchanges sharing one logical call, and an
// ambiguous retry the store refused to link stays its own exchange, because
// the linkage is only ever read here, never decided.
func build(st *store.Store, sessionID string, redacted bool) *Server {
	srv := &Server{sessionID: sessionID, groups: make(map[string]*group), redacted: redacted}
	requests := make(map[wireKey]wireRequest)
	for _, ev := range st.Timeline(sessionID) {
		switch ev.Kind {
		case store.EventRequest:
			if ev.Dir != proxy.ClientToServer {
				continue
			}
			if ev.ID == "" || isNullID(ev.ID) || len(ev.Raw) == 0 || ev.Call == nil {
				continue
			}
			msg, ok := proxy.ParseRPC(ev.Raw)
			if !ok || msg.Method == "" {
				continue
			}
			requests[wireKey{ev.ID, ev.Call.RequestSeq}] = wireRequest{method: msg.Method, params: msg.Params}
		case store.EventResponse:
			if ev.Dir != proxy.ServerToClient {
				continue
			}
			if ev.ID == "" || len(ev.Raw) == 0 || ev.Call == nil {
				continue // no recorded request behind it, nothing to serve
			}
			key := wireKey{ev.ID, ev.Call.RequestSeq}
			req, ok := requests[key]
			if !ok {
				continue
			}
			delete(requests, key) // one wire request is answered once
			// Parsing only establishes that this is a correlated response;
			// the bytes stored for serving are the recorded object itself.
			msg, ok := proxy.ParseRPC(ev.Raw)
			if !ok || (len(msg.Result) == 0 && msg.Error == nil) {
				continue // no answer payload, not an exchange to replay
			}
			match := matchKey(req.method, req.params)
			g := srv.groups[match]
			if g == nil {
				g = &group{}
				srv.groups[match] = g
			}
			g.exchanges = append(g.exchanges, exchange{response: append(json.RawMessage(nil), ev.Raw...)})
			srv.exchanges++
		}
	}
	return srv
}

func isNullID(id string) bool { return string(bytes.TrimSpace([]byte(id))) == "null" }

// matchKey is the strict match identity: method plus structurally normalized
// params. The incoming JSON-RPC id is never part of it.
func matchKey(method string, params json.RawMessage) string {
	return method + "\x00" + normalizeParams(method, params)
}

// normalizeParams canonicalizes params so key order and insignificant
// whitespace never affect matching, while only the volatile identity keys are
// dropped. Omitted params, explicit null and {} stay three different keys: an
// object left empty by volatile stripping is still an explicit empty object,
// not absent params. Numbers decode exactly (UseNumber), so distinct integers
// can never collapse to one key the way float64 would merge them. Anything
// unparseable, or with trailing garbage, falls back to its raw bytes, which
// can only miss, never falsely match.
func normalizeParams(method string, params json.RawMessage) string {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 {
		return "absent"
	}
	if string(trimmed) == "null" {
		return "null"
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "raw:" + string(trimmed)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return "raw:" + string(trimmed)
	}
	stripVolatileMeta(v)
	if method == "initialize" {
		stripLegacyClientIdentity(v)
	}
	b, err := jsonwire.Marshal(v)
	if err != nil {
		return "raw:" + string(trimmed)
	}
	return string(b)
}

// stripLegacyClientIdentity drops the legacy handshake's clientInfo from
// initialize params. Before the stateless revision there was no _meta to ride
// on, so the client name travelled top-level, and a different client replaying
// the handshake is the ordinary case. Capabilities stay strict: they are a
// semantic declaration the server may legitimately answer differently, as does
// the proposed protocolVersion.
func stripLegacyClientIdentity(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	delete(m, "clientInfo")
}

// stripVolatileMeta removes the volatile _meta entries in place. Only the
// top-level params._meta object is touched, and only when it is an object: any
// other shape is semantic and preserved whole.
func stripVolatileMeta(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	raw, ok := m["_meta"]
	if !ok {
		return
	}
	meta, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for k := range volatileMetaKeys {
		delete(meta, k)
	}
	if len(meta) == 0 {
		delete(m, "_meta")
	}
}

// Serve reads newline-delimited JSON-RPC from in and writes newline-delimited
// JSON-RPC to out. Notifications produce no response. Requests are answered
// from the recorded exchanges in order; unmatched ones get a JSON-RPC error
// naming the method, never an unrelated recorded answer. Only protocol frames
// reach out, diagnostics go to errW. It returns the process exit code: 0 on a
// clean EOF, 1 on refusal or I/O failure.
func (s *Server) Serve(in io.Reader, out io.Writer, errW io.Writer, policy Policy) int {
	if s.redacted && !policy.AllowRedacted {
		fmt.Fprintln(errW, "mock: refusing to serve a capture containing redacted frames (use --allow-redacted to serve the redacted placeholders anyway)")
		return 1
	}
	if s.redacted {
		fmt.Fprintln(errW, "mock: warning: serving redacted placeholders as recorded output")
	}
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if werr := s.serveLine(line, out, errW, policy); werr != nil {
				fmt.Fprintln(errW, "mock:", werr)
				return 1
			}
		}
		if err != nil {
			if err == io.EOF {
				return 0
			}
			fmt.Fprintln(errW, "mock:", err)
			return 1
		}
	}
}

// serveLine answers one input line. Blank lines are skipped.
func (s *Server) serveLine(line []byte, out io.Writer, errW io.Writer, policy Policy) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	msg, ok := proxy.ParseRPC(line)
	if !ok {
		fmt.Fprintln(errW, "mock: ignoring a line that is not a JSON-RPC message")
		return writeFrame(out, map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "parse error"},
		})
	}
	if msg.Method != "" && len(msg.ID) == 0 {
		return nil // a notification draws no reply
	}
	if msg.Method == "" {
		fmt.Fprintln(errW, "mock: ignoring an unexpected response from the client")
		return nil
	}
	key := matchKey(msg.Method, msg.Params)
	if g := s.groups[key]; g != nil && g.next < len(g.exchanges) {
		ex := g.exchanges[g.next]
		g.next++
		return writeResponse(out, msg.ID, ex)
	}
	return serveUnmatched(out, errW, msg, policy)
}

// serveUnmatched answers a request the capture never recorded. The strict arm
// is the only one: a JSON-RPC error naming the method plus a stderr line, so
// the miss is visible twice and an unfalsifiable answer is never served.
func serveUnmatched(out io.Writer, errW io.Writer, msg proxy.RPCMessage, policy Policy) error {
	// UnmatchedError is the only policy, and any unknown future value degrades
	// to it here rather than inventing behaviour. A looser fallback becomes a
	// new case arm, not a second matcher.
	switch policy.OnUnmatched {
	default:
		fmt.Fprintf(errW, "mock: no recorded response for %s\n", msg.Method)
		return writeFrame(out, map[string]any{
			"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": -32601, "message": "no recorded response for " + msg.Method},
		})
	}
}

// writeResponse replays one recorded exchange under the incoming request id.
// Only the top-level id is replaced; the recorded object is otherwise
// preserved byte-for-byte in meaning. A missing or wrong jsonrpc stays as the
// server sent it, extra top-level fields survive, a response carrying both
// result and error is not collapsed, and error members are never pruned. A
// request the capture never answered has no exchange and never reaches here.
func writeResponse(out io.Writer, incomingID json.RawMessage, ex exchange) error {
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(ex.response, &frame); err != nil {
		return err
	}
	if frame == nil {
		frame = make(map[string]json.RawMessage)
	}
	frame["id"] = append(json.RawMessage(nil), bytes.TrimSpace(incomingID)...)
	return writeRawFrame(out, frame)
}

func writeFrame(out io.Writer, frame map[string]any) error {
	b, err := jsonwire.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}

func writeRawFrame(out io.Writer, frame map[string]json.RawMessage) error {
	b, err := jsonwire.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}
