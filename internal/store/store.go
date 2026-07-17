// Package store turns the raw envelope stream into the model the TUI renders.
// It correlates each JSON-RPC request with its response (and so derives
// durations), extracts tool calls, captures the negotiated capabilities, and
// flags errors.
//
// The hub calls Ingest concurrently from several connection goroutines, so the
// store is internally synchronised. Reads return value snapshots (raw JSON is
// never mutated after creation, so it is shared freely) which the TUI can hold
// without racing an in-flight Ingest.
package store

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// CallState is the lifecycle of a request/response pair.
type CallState int

const (
	// Pending means the request has been seen but no response yet.
	Pending CallState = iota
	// Completed means a successful response arrived.
	Completed
	// Failed means an error response arrived.
	Failed
)

func (s CallState) String() string {
	switch s {
	case Completed:
		return "completed"
	case Failed:
		return "failed"
	default:
		return "pending"
	}
}

// EventKind classifies a single timeline entry.
type EventKind int

const (
	// EventRequest is a JSON-RPC request (method + id).
	EventRequest EventKind = iota
	// EventResponse is a JSON-RPC response (result or error).
	EventResponse
	// EventNotification is a JSON-RPC notification (method, no id).
	EventNotification
	// EventStderr is a line the server wrote to stderr.
	EventStderr
	// EventInvalid is a non-meta frame on the protocol channel that is not valid
	// JSON-RPC. On stdio this usually means the server printed a stray line to
	// stdout, which corrupts the MCP stream.
	EventInvalid
	// EventOther is a frame we could not classify.
	EventOther
)

// callKey identifies a request awaiting its response. The response travels in
// the opposite direction with the same id.
type callKey struct {
	dir proxy.Direction
	id  string
}

// call is the mutable internal record for one request/response pair.
type call struct {
	id       string
	method   string
	reqDir   proxy.Direction
	params   json.RawMessage
	result   json.RawMessage
	err      *proxy.RPCError
	start    time.Time
	end      time.Time
	state    CallState
	isTool   bool
	toolName string
	toolErr  bool // result.isError == true (MCP tool-level failure)
}

// event is the mutable internal timeline entry.
type event struct {
	seq     uint64
	ts      time.Time
	dir     proxy.Direction
	kind    EventKind
	method  string
	id      string
	raw     json.RawMessage
	text    string
	warning string
	call    *call // set for request/response events
}

// capabilities holds what the handshake negotiated.
type capabilities struct {
	set             bool
	protocolVersion string
	clientInfo      json.RawMessage
	serverInfo      json.RawMessage
	client          json.RawMessage
	server          json.RawMessage
}

// session aggregates everything observed for one proxied server instance.
type session struct {
	id    string
	label string
	first time.Time
	last  time.Time
	caps  capabilities

	advertisedTools []string
	advertisedSet   map[string]struct{}

	command []string
	cwd     string
	calls   map[callKey]*call
	events  []*event

	requests, responses, notifications, errors, pending int
}

// Store is the concurrency-safe collector the hub feeds and the TUI reads.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*session
	order    []string // session ids in first-seen order
}

// New returns an empty store.
func New() *Store {
	return &Store{
		sessions: make(map[string]*session),
	}
}

// Delete drops a session from the store. A still-live shim will recreate it on
// its next frame. Callers that want it gone for good should also delete its log.
func (s *Store) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return
	}
	delete(s.sessions, sessionID)
	for i, id := range s.order {
		if id == sessionID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Ingest folds one envelope into the model and returns the resulting timeline
// entry (with its correlated call resolved, if any).
func (s *Store) Ingest(e proxy.Envelope) EventView {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.sessionFor(e)

	if e.Direction == proxy.DirectionMeta {
		var meta proxy.SessionMeta
		if json.Unmarshal(e.Raw, &meta) == nil {
			sess.command = meta.Command
			sess.cwd = meta.CWD
		}
		return EventView{Kind: EventOther} // control frame, not shown in the stream
	}

	ev := &event{seq: e.Seq, ts: e.TS, dir: e.Direction, raw: e.Raw, text: e.Text}

	if e.Direction == proxy.ServerStderr {
		ev.kind = EventStderr
		sess.events = append(sess.events, ev)
		return ev.view(sess)
	}

	msg, ok := proxy.ParseRPC(e.Raw)
	switch {
	case !ok:
		ev.kind = EventInvalid
	case msg.Method == "" && msg.IsResponse():
		ev.kind = EventResponse
		ev.id = string(msg.ID)
		ev.warning = validationWarning(msg)
		c, matched := sess.completeCall(ev.id, e.Direction, e.TS, msg)
		ev.call = c
		sess.responses++
		switch {
		case c == nil:
			ev.warning = appendWarning(ev.warning, "response id has no matching request")
			if msg.Error != nil {
				sess.errors++
			}
		case !matched:
			ev.warning = appendWarning(ev.warning, "duplicate response for the same id")
		default:
			if msg.Error != nil || c.toolErr {
				sess.errors++
			}
		}
	case msg.Method != "" && len(msg.ID) > 0:
		ev.kind = EventRequest
		ev.method = msg.Method
		ev.id = string(msg.ID)
		ev.warning = validationWarning(msg)
		var reused bool
		ev.call, reused = sess.openCall(ev.id, msg, e)
		sess.requests++
		if reused {
			ev.warning = appendWarning(ev.warning, "request reuses an id already in flight")
		} else {
			sess.pending++
		}
		if msg.Method == "initialize" {
			sess.caps.applyRequest(msg.Params)
		}
	case msg.Method != "":
		ev.kind = EventNotification
		ev.method = msg.Method
		ev.warning = validationWarning(msg)
		sess.notifications++
	default:
		ev.kind = EventOther
		ev.warning = validationWarning(msg)
	}

	sess.events = append(sess.events, ev)
	return ev.view(sess)
}

// sessionFor returns (creating if needed) the session for an envelope and bumps
// its first/last timestamps. Caller holds the write lock.
func (s *Store) sessionFor(e proxy.Envelope) *session {
	sess, ok := s.sessions[e.SessionID]
	if !ok {
		sess = &session{
			id:            e.SessionID,
			label:         e.ServerLabel,
			first:         e.TS,
			advertisedSet: make(map[string]struct{}),
			calls:         make(map[callKey]*call),
		}
		s.sessions[e.SessionID] = sess
		s.order = append(s.order, e.SessionID)
	}
	if e.ServerLabel != "" {
		sess.label = e.ServerLabel
	}
	sess.last = e.TS
	return sess
}

// openCall records a new pending request. The bool reports whether it displaced
// a still-pending call for the same id and direction, meaning the client reused
// an id while its earlier request was in flight. Caller holds the write lock.
func (sess *session) openCall(id string, msg proxy.RPCMessage, e proxy.Envelope) (*call, bool) {
	key := callKey{dir: e.Direction, id: id}
	prev, ok := sess.calls[key]
	reused := ok && prev.state == Pending
	c := &call{
		id:     id,
		method: msg.Method,
		reqDir: e.Direction,
		params: msg.Params,
		start:  e.TS,
		state:  Pending,
	}
	if msg.Method == "tools/call" {
		c.isTool = true
		c.toolName = toolName(msg.Params)
	}
	sess.calls[key] = c
	return c, reused
}

// completeCall matches a response to its pending request. The bool reports
// whether it completed a pending call, where false means the response was unmatched or
// a duplicate of an already-answered id. Caller holds the lock.
func (sess *session) completeCall(id string, respDir proxy.Direction, ts time.Time, msg proxy.RPCMessage) (*call, bool) {
	c := sess.calls[callKey{dir: opposite(respDir), id: id}]
	if c == nil {
		return nil, false // unmatched response (request missed or before backfill)
	}
	if c.state != Pending {
		return c, false // already answered, a duplicate or late response must not recount
	}
	c.end = ts
	c.result = msg.Result
	c.err = msg.Error
	switch {
	case msg.Error != nil:
		c.state = Failed // JSON-RPC / protocol error
	case isToolError(msg.Result):
		c.state = Failed // tool-level error, a 200-OK response with result.isError=true
		c.toolErr = true
	default:
		c.state = Completed
	}
	sess.pending--
	switch c.method {
	case "initialize":
		sess.caps.applyResponse(msg.Result)
	case "tools/list":
		sess.applyToolsList(c.params, msg.Result)
	}
	return c, true
}

func (c *capabilities) applyRequest(params json.RawMessage) {
	var p struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      json.RawMessage `json:"clientInfo"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.set = true
	if p.ProtocolVersion != "" {
		c.protocolVersion = p.ProtocolVersion
	}
	c.client = p.Capabilities
	c.clientInfo = p.ClientInfo
}

func (c *capabilities) applyResponse(result json.RawMessage) {
	var r struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	if json.Unmarshal(result, &r) != nil {
		return
	}
	c.set = true
	if r.ProtocolVersion != "" {
		c.protocolVersion = r.ProtocolVersion
	}
	c.server = r.Capabilities
	c.serverInfo = r.ServerInfo
}

// applyToolsList records the tools a tools/list response advertised. A cursorless
// request is a fresh page one, so its response is the server's current tool set
// and supersedes what we had (a tools/list_changed re-list can drop tools). A
// cursored request is a pagination continuation, so it extends the set.
func (sess *session) applyToolsList(reqParams, result json.RawMessage) {
	var r struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}

	if json.Unmarshal(result, &r) != nil {
		return
	}

	if !hasListCursor(reqParams) {
		clear(sess.advertisedSet)
		sess.advertisedTools = nil
	}

	for _, tool := range r.Tools {
		if tool.Name == "" {
			continue
		}
		if _, ok := sess.advertisedSet[tool.Name]; ok {
			continue
		}

		sess.advertisedSet[tool.Name] = struct{}{}
		sess.advertisedTools = append(sess.advertisedTools, tool.Name)
	}
}

// hasListCursor reports whether a tools/list request carries a pagination cursor,
// marking its response a continuation of an earlier page rather than a fresh
// listing that supersedes the set.
func hasListCursor(params json.RawMessage) bool {
	var p struct {
		Cursor string `json:"cursor"`
	}
	return json.Unmarshal(params, &p) == nil && p.Cursor != ""
}

func toolName(params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params, &p)
	return p.Name
}

// isToolError reports whether a tools/call result carries result.isError == true
// (MCP signals tool failures inside a successful response, not as a JSON-RPC error).
func isToolError(result json.RawMessage) bool {
	var r struct {
		IsError bool `json:"isError"`
	}
	return json.Unmarshal(result, &r) == nil && r.IsError
}

func validationWarning(msg proxy.RPCMessage) string {
	var warning string
	switch {
	case msg.JSONRPC == "":
		warning = appendWarning(warning, "missing jsonrpc=2.0")
	case msg.JSONRPC != "2.0":
		warning = appendWarning(warning, "jsonrpc must be 2.0")
	}
	if msg.Method == "" && len(msg.ID) > 0 {
		if len(msg.Result) == 0 && msg.Error == nil {
			warning = appendWarning(warning, "response has neither result nor error")
		}
		if len(msg.Result) > 0 && msg.Error != nil {
			warning = appendWarning(warning, "response has both result and error")
		}
	}
	return warning
}

func appendWarning(existing, next string) string {
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	return existing + ", " + next
}

func opposite(d proxy.Direction) proxy.Direction {
	if d == proxy.ClientToServer {
		return proxy.ServerToClient
	}
	return proxy.ClientToServer
}
