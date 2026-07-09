// Package store turns the raw envelope stream into the model the TUI renders:
// it correlates each JSON-RPC request with its response (and so derives
// durations), extracts tool calls, captures the negotiated capabilities, and
// flags errors and slow calls.
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

// DefaultSlowThreshold is the duration above which a completed call is "slow".
// Sub-second tool calls are normal, so only flag calls that take longer.
const DefaultSlowThreshold = 1 * time.Second

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
	seq    uint64
	ts     time.Time
	dir    proxy.Direction
	kind   EventKind
	method string
	id     string
	raw    json.RawMessage
	text   string
	call   *call // set for request/response events
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
	id      string
	label   string
	first   time.Time
	last    time.Time
	caps    capabilities
	command []string
	cwd     string

	calls  map[callKey]*call
	events []*event

	requests, responses, notifications, errors, pending int
}

// Store is the concurrency-safe collector the hub feeds and the TUI reads.
type Store struct {
	mu            sync.RWMutex
	slowThreshold time.Duration
	sessions      map[string]*session
	order         []string // session ids in first-seen order
}

// New returns an empty store. slowThreshold <= 0 uses DefaultSlowThreshold.
func New(slowThreshold time.Duration) *Store {
	if slowThreshold <= 0 {
		slowThreshold = DefaultSlowThreshold
	}
	return &Store{
		slowThreshold: slowThreshold,
		sessions:      make(map[string]*session),
	}
}

// SlowThreshold is the cutoff used by CallView.Slow.
func (s *Store) SlowThreshold() time.Duration { return s.slowThreshold }

// Delete drops a session from the store. A still-live shim will recreate it on
// its next frame; callers that want it gone for good should also delete its log.
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
		return EventView{Kind: EventOther} // control frame; not shown in the stream
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
		ev.call = sess.completeCall(ev.id, e.Direction, e.TS, msg)
		sess.responses++
		if msg.Error != nil || (ev.call != nil && ev.call.toolErr) {
			sess.errors++
		}
	case msg.Method != "" && len(msg.ID) > 0:
		ev.kind = EventRequest
		ev.method = msg.Method
		ev.id = string(msg.ID)
		ev.call = sess.openCall(ev.id, msg, e)
		sess.requests++
		sess.pending++
		if msg.Method == "initialize" {
			sess.caps.applyRequest(msg.Params)
		}
	case msg.Method != "":
		ev.kind = EventNotification
		ev.method = msg.Method
		sess.notifications++
	default:
		ev.kind = EventOther
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
			id:    e.SessionID,
			label: e.ServerLabel,
			first: e.TS,
			calls: make(map[callKey]*call),
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

// openCall records a new pending request. Caller holds the write lock.
func (sess *session) openCall(id string, msg proxy.RPCMessage, e proxy.Envelope) *call {
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
	sess.calls[callKey{dir: e.Direction, id: id}] = c
	return c
}

// completeCall matches a response to its pending request. Caller holds the lock.
func (sess *session) completeCall(id string, respDir proxy.Direction, ts time.Time, msg proxy.RPCMessage) *call {
	c := sess.calls[callKey{dir: opposite(respDir), id: id}]
	if c == nil {
		return nil // unmatched response (request missed or before backfill)
	}
	c.end = ts
	c.result = msg.Result
	c.err = msg.Error
	switch {
	case msg.Error != nil:
		c.state = Failed // JSON-RPC / protocol error
	case isToolError(msg.Result):
		c.state = Failed // tool-level error: a 200-OK response with result.isError=true
		c.toolErr = true
	default:
		c.state = Completed
	}
	sess.pending--
	if c.method == "initialize" {
		sess.caps.applyResponse(msg.Result)
	}
	return c
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

func opposite(d proxy.Direction) proxy.Direction {
	if d == proxy.ClientToServer {
		return proxy.ServerToClient
	}
	return proxy.ClientToServer
}
