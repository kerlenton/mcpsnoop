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
	// Superseded means the request's id was reused by a later in-flight request, so
	// this one can never be matched to a response. It is no longer pending.
	Superseded
)

func (s CallState) String() string {
	switch s {
	case Completed:
		return "completed"
	case Failed:
		return "failed"
	case Superseded:
		return "superseded"
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
	// errored is the "something went wrong" axis: a protocol error, a tool-level
	// error, or a task that ended failed. It drives the session error counter and
	// the CI gate, and is deliberately distinct from the Failed state, which also
	// covers a cancelled task, work the user stopped on purpose that delivered no
	// result but is not an error. One place (completeCall, applyParsedTaskState)
	// decides this so every consumer reads the same answer.
	errored    bool
	taskID     string
	taskStatus string
}

// event is the mutable internal timeline entry.
type event struct {
	seq                uint64
	ts                 time.Time
	dir                proxy.Direction
	kind               EventKind
	method             string
	id                 string
	raw                json.RawMessage
	text               string
	warning            string
	mcpMethod          string // Mcp-Method routing header (HTTP transport, SEP-2243)
	mcpName            string // Mcp-Name routing header
	mcpProtocolVersion string // MCP-Protocol-Version request header
	batch              bool   // one element of a JSON-RPC batch (routing headers cannot address it)
	mismatch           bool   // a routing header disagrees with the body (structured flag for warning)
	truncated          bool   // the observed copy was capped at the frame-size limit
	deprecated         string // a deprecated MCP feature was used (structured, not a protocol warning)
	call               *call  // set for request/response events
	taskCall           *call  // originating call for a task lifecycle frame
	taskID             string
}

// capabilities holds what each side declared, whether through the legacy
// initialize handshake or the 2026-07-28 stateless model, where the client's
// info/version/capabilities ride every request's _meta and the server's are
// fetched with server/discover (the initialize handshake was removed, SEP-2575).
type capabilities struct {
	set             bool
	protocolVersion string
	clientInfo      json.RawMessage
	serverInfo      json.RawMessage
	client          json.RawMessage
	server          json.RawMessage
	instructions    string // server's usage guidance (initialize or server/discover)
}

// session aggregates everything observed for one proxied server instance.
type session struct {
	id    string
	label string
	first time.Time
	last  time.Time
	caps  capabilities

	advertisedTools  []string
	advertisedSet    map[string]struct{}
	toolDefinitions  map[string]ToolDefinition
	toolListComplete bool
	toolDrift        ToolDrift
	toolDriftSet     bool

	command []string
	cwd     string
	calls   map[callKey]*call
	tasks   map[string]*call
	events  []*event

	requests, responses, notifications, errors, pending int

	lastSeq uint64 // highest per-session Seq seen, for gap detection
	missing uint64 // envelopes dropped upstream, inferred from Seq gaps
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

	// Track gaps in the monotonic per-session Seq. A jump larger than one means
	// envelopes were dropped upstream (a full sink buffer) or lost from the log.
	// Doing it here surfaces drops whether the frames arrive live or are read back
	// from a gappy file through open or export.
	if e.Seq > sess.lastSeq {
		if expected := sess.lastSeq + 1; e.Seq > expected {
			sess.missing += e.Seq - expected
		}
		sess.lastSeq = e.Seq
	}

	if e.Direction == proxy.DirectionMeta {
		var meta proxy.SessionMeta
		if json.Unmarshal(e.Raw, &meta) == nil {
			sess.command = meta.Command
			sess.cwd = meta.CWD
		}
		return EventView{Kind: EventOther} // control frame, not shown in the stream
	}

	ev := &event{seq: e.Seq, ts: e.TS, dir: e.Direction, raw: e.Raw, text: e.Text, mcpMethod: e.MCPMethod, mcpName: e.MCPName, mcpProtocolVersion: e.MCPProtocolVersion, batch: e.Batch}

	if e.Direction == proxy.ServerStderr {
		ev.kind = EventStderr
		sess.events = append(sess.events, ev)
		return ev.view(sess)
	}

	if e.Truncated {
		// mcpsnoop capped its own observed copy of a large body, so the bytes are
		// incomplete by design. Flag it structurally rather than through the warning
		// field: it is not a protocol warning, and routing it there would fail a
		// default `check --fail-on warn` over a perfectly valid oversized body.
		ev.kind = EventOther
		ev.truncated = true
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
		sess.caps.applyResponseMeta(msg.Result)
		c, matched := sess.completeCall(ev.id, e.Direction, e.TS, msg)
		ev.call = c
		if c != nil && c.taskID != "" {
			ev.taskID = c.taskID
			if c.method != "tools/call" {
				ev.taskCall = sess.tasks[c.taskID]
				if matched {
					if parent, failed := sess.applyTaskState(c.taskID, msg.Result, e.TS); parent != nil {
						ev.taskCall = parent
						if failed {
							sess.errors++
						}
					}
				}
			}
		}
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
			if c.errored {
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
		if ev.call.taskID != "" {
			ev.taskID = ev.call.taskID
			ev.taskCall = sess.tasks[ev.taskID]
		}
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
		if msg.Method == "notifications/tasks" {
			if state, ok := parseTaskState(msg.Params); ok {
				ev.taskID = state.TaskID
				if parent, failed := sess.applyParsedTaskState(state, e.TS); parent != nil {
					ev.taskCall = parent
					if failed {
						sess.errors++
					}
				}
			}
		}
	default:
		ev.kind = EventOther
		ev.warning = validationWarning(msg)
	}

	// In the stateless model the client's identity and capabilities ride every
	// request's _meta rather than a one-time initialize, so read them from any
	// client frame that carries them (a no-op otherwise).
	if e.Direction == proxy.ClientToServer && msg.Method != "" {
		sess.caps.applyRequestMeta(msg.Params)
	}

	// A gateway routes on the Mcp-Method/Mcp-Name headers, and a compliant server
	// rejects a request whose headers disagree with the body, so flag the mismatch
	// rather than let it look fine. The check is surfaced as a warning (a spec
	// violation CI should catch) but also carried on ev.mismatch so filters and
	// exporters can key on it structurally instead of on the warning text.
	if ev.mcpMethod != "" || ev.mcpName != "" {
		switch {
		case ev.batch:
			// One header cannot route N methods, so routing a batch is invalid by
			// construction. Say so once rather than fabricating a per-element mismatch.
			ev.warning = appendWarning(ev.warning, "routing headers are invalid on a JSON-RPC batch")
			ev.mismatch = true
		default:
			if ev.mcpMethod != "" && msg.Method != "" && ev.mcpMethod != msg.Method {
				ev.warning = appendWarning(ev.warning, "routing header Mcp-Method "+ev.mcpMethod+" disagrees with body method "+msg.Method)
				ev.mismatch = true
			}
			// Mcp-Name names the target operation (params.name for tools/call and
			// prompts/get, params.uri for resources/read). A lie here is the
			// tool-shadowing case, so it matters more than a bare method mismatch.
			if name := operationName(msg); ev.mcpName != "" && name != "" && ev.mcpName != name {
				ev.warning = appendWarning(ev.warning, "routing header Mcp-Name "+ev.mcpName+" disagrees with body name "+name)
				ev.mismatch = true
			}
		}
	}

	// The MCP-Protocol-Version header must match the version the request repeats in
	// its _meta; a gateway routes on the header while the server reads the body, so
	// a disagreement is the same class of spec violation as a routing-header
	// mismatch. This is request-scoped, so unlike the routing headers it is valid on
	// a batch and gated only on the header being present.
	if ev.mcpProtocolVersion != "" {
		if mv := metaProtocolVersion(msg.Params); mv != "" && mv != ev.mcpProtocolVersion {
			ev.warning = appendWarning(ev.warning,
				"MCP-Protocol-Version header "+ev.mcpProtocolVersion+
					" disagrees with _meta protocolVersion "+mv)
			ev.mismatch = true
		}
	}

	if note := deprecatedMethodNote(msg.Method); note != "" {
		ev.deprecated = note
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
			id:              e.SessionID,
			label:           e.ServerLabel,
			first:           e.TS,
			advertisedSet:   make(map[string]struct{}),
			toolDefinitions: make(map[string]ToolDefinition),
			calls:           make(map[callKey]*call),
			tasks:           make(map[string]*call),
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
	if reused {
		// The earlier in-flight call keeps this id, so it will never be matched now.
		// Mark it superseded (not pending) so the timeline stops rendering it as a
		// hanging request and agrees with the pending counter, which Ingest leaves
		// unchanged on a reuse. The "reuses an id already in flight" warning on the
		// new request is the explanation.
		prev.state = Superseded
		prev.end = e.TS
	}
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
	if isTaskMethod(msg.Method) {
		c.taskID = taskID(msg.Params)
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
	if c.method == "tools/call" && c.taskID != "" {
		return c, false // the task handle already continued this call
	}
	if c.method == "tools/call" {
		if state, ok := parseTaskState(msg.Result); ok && state.ResultType == "task" {
			c.result = msg.Result
			c.taskID = state.TaskID
			c.taskStatus = state.Status
			sess.tasks[state.TaskID] = c
			return c, true
		}
	}
	c.end = ts
	c.result = msg.Result
	c.err = msg.Error
	switch {
	case msg.Error != nil:
		c.state = Failed // JSON-RPC / protocol error
		c.errored = true
	case isToolError(msg.Result):
		c.state = Failed // tool-level error, a 200-OK response with result.isError=true
		c.toolErr = true
		c.errored = true
	default:
		c.state = Completed
	}
	sess.pending--
	switch c.method {
	case "initialize":
		sess.caps.applyResponse(msg.Result)
	case "server/discover":
		sess.caps.applyDiscover(msg.Result)
	case "tools/list":
		sess.applyToolsList(c.params, msg.Result)
	}
	return c, true
}

type taskState struct {
	ResultType string          `json:"resultType"`
	TaskID     string          `json:"taskId"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result"`
	Error      *proxy.RPCError `json:"error"`
}

func parseTaskState(raw json.RawMessage) (taskState, bool) {
	var state taskState
	if json.Unmarshal(raw, &state) != nil || state.TaskID == "" {
		return taskState{}, false
	}
	return state, true
}

func isTaskMethod(method string) bool {
	return method == "tasks/get" || method == "tasks/update" || method == "tasks/cancel"
}

func taskID(params json.RawMessage) string {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(params, &p)
	return p.TaskID
}

func (sess *session) applyTaskState(taskID string, raw json.RawMessage, ts time.Time) (*call, bool) {
	state, ok := parseTaskState(raw)
	if !ok || state.TaskID != taskID {
		return sess.tasks[taskID], false
	}
	return sess.applyParsedTaskState(state, ts)
}

// applyParsedTaskState advances the originating call only when an observed task
// reaches a terminal state. The bool reports a newly recorded error, which is not
// the same as a failed state: a cancelled task delivered no result, but the user
// stopping work is not a protocol or tool error and must not fail check.
func (sess *session) applyParsedTaskState(state taskState, ts time.Time) (*call, bool) {
	c := sess.tasks[state.TaskID]
	if c == nil {
		return nil, false
	}
	c.taskStatus = state.Status
	if c.state != Pending {
		return c, false
	}
	countsAsError := false
	switch state.Status {
	case "completed":
		c.result = state.Result
		// The terminal result is whatever the call would have returned
		// synchronously, so a tool-level failure arrives here in exactly the same
		// shape the fast path checks for. Without this a tool that failed inside a
		// task reads as a success everywhere: Failed() is false, the session error
		// count is short, status:err misses it and check --fail-on error passes.
		if isToolError(state.Result) {
			c.state = Failed
			c.toolErr = true
			countsAsError = true
		} else {
			c.state = Completed
		}
	case "failed":
		c.state = Failed
		c.err = state.Error
		c.result = state.Result
		if c.err == nil && isToolError(state.Result) {
			c.toolErr = true
		}
		countsAsError = true
	case "cancelled":
		// Terminal and without a result, so the call did not succeed, but the
		// session error count and the CI gate track protocol and tool errors, and a
		// deliberate cancel is neither. TaskStatus carries the real outcome.
		c.state = Failed
		c.err = state.Error
		c.result = state.Result
	default:
		return c, false
	}
	c.end = ts
	c.errored = countsAsError // the failed and completed-with-isError cases, not cancelled
	sess.pending--
	return c, countsAsError
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
		Instructions    string          `json:"instructions"`
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
	if r.Instructions != "" {
		c.instructions = r.Instructions
	}
}

// applyRequestMeta reads the client's protocol version, info, and capabilities
// from a request's _meta, where the stateless model repeats them on every
// request instead of exchanging them once in initialize. The reverse-DNS keys
// are sourced verbatim from the draft schema. It is a no-op when the request
// carries none of them, so a plain call never fabricates a handshake.
func (c *capabilities) applyRequestMeta(params json.RawMessage) {
	var p struct {
		Meta struct {
			ProtocolVersion string          `json:"io.modelcontextprotocol/protocolVersion"`
			ClientInfo      json.RawMessage `json:"io.modelcontextprotocol/clientInfo"`
			Capabilities    json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	m := p.Meta
	if m.ProtocolVersion == "" && len(m.ClientInfo) == 0 && len(m.Capabilities) == 0 {
		return
	}
	c.set = true
	if m.ProtocolVersion != "" {
		c.protocolVersion = m.ProtocolVersion
	}
	if len(m.ClientInfo) > 0 {
		c.clientInfo = m.ClientInfo
	}
	if len(m.Capabilities) > 0 {
		c.client = m.Capabilities
	}
}

// applyDiscover reads server capabilities from a server/discover result, the
// stateless replacement for the initialize response. serverInfo is read from the
// result's _meta first, since the normative draft JSON schema ($defs.ResultMetaObject)
// makes io.modelcontextprotocol/serverInfo the canonical location that servers
// SHOULD send on every response, and DiscoverResult has no top-level serverInfo.
// The top-level read is only a defensive fallback for the shape in the
// non-normative docs example, so this precedence must not be flipped back. The
// server lists the versions it supports, of which we surface the first when no
// version is otherwise known.
func (c *capabilities) applyDiscover(result json.RawMessage) {
	var r struct {
		SupportedVersions []string        `json:"supportedVersions"`
		Capabilities      json.RawMessage `json:"capabilities"`
		ServerInfo        json.RawMessage `json:"serverInfo"`
		Instructions      string          `json:"instructions"`
		Meta              struct {
			ServerInfo json.RawMessage `json:"io.modelcontextprotocol/serverInfo"`
		} `json:"_meta"`
	}
	if json.Unmarshal(result, &r) != nil {
		return
	}
	c.set = true
	if len(r.Capabilities) > 0 {
		c.server = r.Capabilities
	}
	switch {
	case len(r.Meta.ServerInfo) > 0:
		c.serverInfo = r.Meta.ServerInfo
	case len(r.ServerInfo) > 0:
		c.serverInfo = r.ServerInfo
	}
	if c.protocolVersion == "" && len(r.SupportedVersions) > 0 {
		c.protocolVersion = r.SupportedVersions[0]
	}
	if r.Instructions != "" {
		c.instructions = r.Instructions
	}
}

// applyResponseMeta reads the server's identity from any response's _meta.
// The normative draft schema ($defs.ResultMetaObject) says servers SHOULD send
// io.modelcontextprotocol/serverInfo on every response, so capture it even when
// the session never calls server/discover. No-op when absent, so legacy
// (2025-11-25) responses, which carry serverInfo at the top level only, are
// untouched.
func (c *capabilities) applyResponseMeta(result json.RawMessage) {
	if len(result) == 0 {
		return
	}
	var r struct {
		Meta struct {
			ServerInfo json.RawMessage `json:"io.modelcontextprotocol/serverInfo"`
		} `json:"_meta"`
	}
	if json.Unmarshal(result, &r) != nil {
		return
	}
	if len(r.Meta.ServerInfo) == 0 {
		return
	}
	c.set = true
	c.serverInfo = r.Meta.ServerInfo
}

// applyToolsList records the tools a tools/list response advertised. A cursorless
// request is a fresh page one, so its response is the server's current tool set
// and supersedes what we had (a tools/list_changed re-list can drop tools). A
// cursored request is a pagination continuation, so it extends the set.
func (sess *session) applyToolsList(reqParams, result json.RawMessage) {
	var r struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		NextCursor string `json:"nextCursor"`
	}

	if json.Unmarshal(result, &r) != nil {
		return
	}

	if !hasListCursor(reqParams) {
		clear(sess.advertisedSet)
		clear(sess.toolDefinitions)
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
		sess.toolDefinitions[tool.Name] = ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
		}
	}
	sess.toolListComplete = r.NextCursor == ""
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

// operationName returns the target that the Mcp-Name routing header addresses
// for a given request, so a lie can be caught. Only methods whose target lives
// in a well-known params field are considered (tools/call and prompts/get name
// it in params.name, resources/read in params.uri); everything else returns ""
// so a method that merely happens to carry a "name" param is never falsely
// flagged, keeping the mismatch signal safe for a CI gate.
func operationName(msg proxy.RPCMessage) string {
	if len(msg.Params) == 0 {
		return ""
	}
	var p struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	if json.Unmarshal(msg.Params, &p) != nil {
		return ""
	}
	switch msg.Method {
	case "tools/call", "prompts/get":
		return p.Name
	case "resources/read", "resources/subscribe", "resources/unsubscribe":
		return p.URI
	default:
		return ""
	}
}

// metaProtocolVersion returns the protocol version a request repeats in its
// _meta (io.modelcontextprotocol/protocolVersion), or "" when absent, so the
// MCP-Protocol-Version header can be checked against it.
func metaProtocolVersion(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		Meta struct {
			ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return p.Meta.ProtocolVersion
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
