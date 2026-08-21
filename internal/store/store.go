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
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
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
	// Cancelled means a notifications/cancelled named this request, so it settled
	// without the answer the caller wanted. It is no longer pending, and it is not
	// an error, since the client stopping work on purpose is not something going
	// wrong. Reachable on stdio only: on Streamable HTTP closing the response
	// stream is itself the cancellation signal and, in the spec's words, "No
	// notifications/cancelled message is required or expected", so mcpsnoop never
	// sees one there.
	Cancelled
	// Streaming means a long-lived stream request (subscriptions/listen) whose
	// response arrives only when the stream ends. It is open but not pending.
	Streaming
)

func (s CallState) String() string {
	switch s {
	case Completed:
		return "completed"
	case Failed:
		return "failed"
	case Superseded:
		return "superseded"
	case Cancelled:
		return "cancelled"
	case Streaming:
		return "streaming"
	default:
		return "pending"
	}
}

// MRTRStateIssue classifies how a retry violated the requestState echo contract.
type MRTRStateIssue string

const (
	MRTRStateChanged  MRTRStateIssue = "changed"
	MRTRStateMissing  MRTRStateIssue = "missing"
	MRTRStateInvented MRTRStateIssue = "invented"
)

func (i MRTRStateIssue) warning() string {
	switch i {
	case MRTRStateChanged:
		return "MRTR retry changed requestState"
	case MRTRStateMissing:
		return "MRTR retry is missing requestState"
	case MRTRStateInvented:
		return "MRTR retry invented requestState"
	default:
		return ""
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
	// EventTransport is an HTTP-level event with no JSON-RPC message of its own:
	// a response the target answered with a status and no body, a non-2xx whose
	// body is not JSON-RPC, or a request the proxy could not forward at all. It
	// exists because those are otherwise invisible, and because calling them
	// invalid frames blames the wrong layer.
	EventTransport
)

// httpFailed reports whether a status is a failure worth counting. 4xx and 5xx
// only: a 3xx is a redirect the proxy forwards untouched, and a 2xx is fine
// even when it carries no body, which is exactly what a 202 acknowledging a
// notification looks like.
func httpFailed(status int) bool { return status >= 400 }

// callKey identifies a request awaiting its response. The response travels in
// the opposite direction with the same id.
type callKey struct {
	dir proxy.Direction
	seq uint64
	id  string
	// conn separates the id spaces of two clients that share one proxy process.
	// JSON-RPC scopes id uniqueness to the sender, "the ID MUST NOT match the ID
	// of any other request the sender has issued and not yet received a response
	// for", and one `mcpsnoop http` run carries every client that connects to it,
	// so two conforming clients both starting at id 1 collided here. Empty on
	// stdio, where there is only ever one peer.
	conn string
}

// call is the mutable internal record for one request/response pair.
type call struct {
	requestSeq   uint64
	id           string
	method       string
	reqDir       proxy.Direction
	params       json.RawMessage
	result       json.RawMessage
	err          *proxy.RPCError
	start        time.Time
	end          time.Time
	state        CallState
	cancelledAt  time.Time
	cancelReason string
	lateResult   bool
	isTool       bool
	toolName     string
	toolErr      bool // result.isError == true (MCP tool-level failure)
	// errored is the "something went wrong" axis: a protocol error, a tool-level
	// error, or a task that ended failed. It drives the session error counter and
	// the CI gate, and is deliberately distinct from the Failed state, which also
	// covers a cancelled task, work the user stopped on purpose that delivered no
	// result but is not an error. One place (completeCall, applyParsedTaskState)
	// decides this so every consumer reads the same answer.
	errored bool
	// releasedResultBytes is how big the result was when a bounded store dropped
	// it, so the context-cost figures the tool summary reports stay exact rather
	// than reading zero once the bytes are gone.
	releasedResultBytes int
	taskID              string
	taskStatus          string
	// opName is the tool, prompt or resource this call names, kept so a retry can
	// be matched back to it without re-parsing the original params.
	opName string
	// protocolVersion is the revision this one request declared, which is the only
	// version a response to it may be judged by. MCP is stateless: the spec says
	// outright that a server must not rely on prior requests over the same
	// connection to establish the protocol version, and that clients may interleave
	// unrelated requests on one transport. Reading the session's version instead
	// would judge this response by whatever the most recent unrelated request
	// happened to declare, which flags a correct older answer and excuses a
	// genuinely non-conformant newer one. Empty when the request declared none.
	protocolVersion string
	// mrtrState, its presence bit, and mrtrKeys park what an InputRequiredResult
	// asked for. The retry uses a different JSON-RPC id, so these are also the
	// evidence used to infer the link.
	mrtrState    string
	mrtrHasState bool
	mrtrKeys     string
	// progressToken is what this request asked progress to be reported under,
	// remembered so completion can close it and a later notification under the
	// same token is read as reuse rather than a violation.
	progressToken string
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
	observation        string
	mcpMethod          string // Mcp-Method routing header (HTTP transport, SEP-2243)
	mcpName            string // Mcp-Name routing header
	mcpProtocolVersion string // MCP-Protocol-Version request header
	mcpParamHeaders    []proxy.MCPParamHeader
	batch              bool   // one element of a JSON-RPC batch (routing headers cannot address it)
	transport          string // the channel this frame was observed on (proxy.TransportHTTP, …)
	status             int    // HTTP status of the response this frame arrived on (zero on stdio)
	authChallenge      string // WWW-Authenticate on that response
	mismatch           bool   // a routing header disagrees with the body (structured flag for warning)
	truncated          bool   // the observed copy was capped at the frame-size limit
	deprecated         string // a deprecated MCP feature was used (structured, not a protocol warning)
	cacheHint          CacheHint
	cacheStaleRefetch  string // client re-fetched inside a declared ttlMs window (structured, not a protocol warning)
	call               *call  // set for request/response events
	taskCall           *call  // originating call for a task lifecycle frame
	taskID             string
	// errored marks a frame this session counted an error on. It is set at every
	// site that increments sess.errors, so a reporter can name the frames behind
	// the count instead of re-deriving the conditions and drifting from them. A
	// transport failure and an unmatched error response carry no call at all, and
	// a task failure is counted on its terminal frame rather than on the call's
	// first response, so call.errored alone cannot stand in for this.
	errored bool
	// bodyReleased is set when a bounded store dropped this frame's observed body
	// to stay inside its budget. The frame keeps everything else, so it is still a
	// row in the stream with its own verdict, and only its bytes are gone.
	bodyReleased bool
	// lateResult marks the frame this session counted a late result on, the same
	// way errored marks a counted error. call.lateResult stays true for the rest
	// of the capture and is visible from the request frame too, so it cannot
	// stand in for this.
	lateResult bool
	// mrtrRoot is the id of the request this one continues, set when a multi
	// round-trip retry was recognised. Empty on an ordinary request.
	mrtrRoot string
	// mrtrStateIssue is set only when a linked retry changed the opaque state,
	// omitted an issued state, or supplied one the server never issued.
	mrtrStateIssue MRTRStateIssue
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
	// toolListSeen records that a tools/list response was observed at all, which
	// toolListComplete cannot express: a server advertising no tools leaves the
	// set empty and the list complete, exactly like a session that never listed.
	toolListSeen bool
	toolDrift    ToolDrift
	toolDriftSet bool
	// paramHeaderInvalid names tools whose latest advertised inputSchema carries an
	// x-mcp-header the spec forbids, and which constraint it breaks. applyToolsList
	// runs deep inside completeCall, which cannot widen its return, so the verdict
	// is parked here and Ingest drains it onto the tools/list response frame that
	// carried the definitions.
	paramHeaderInvalid []paramHeaderViolation
	// schemaRootInvalid names tools whose inputSchema is not an object schema,
	// with the reason in the words schemaRootViolation composed. Parked and
	// drained the same way as paramHeaderInvalid so a partly paginated listing
	// still reports the violation on the frame that carried it.
	schemaRootInvalid []paramHeaderViolation

	command []string
	cwd     string
	calls   map[callKey]*call
	// evictCursor is how far a bounded store's release has reached in this
	// session's timeline, and bodyBytes is what the frames from there on weigh.
	evictCursor int
	bodyBytes   int
	// dropped counts the frames a bounded store has removed from this timeline
	// entirely, and toolStats holds the statistics of the calls they carried, so
	// the summary still describes every call the session made.
	dropped   int
	toolStats toolStats
	tasks     map[string]*call
	// awaiting holds operations that answered with an InputRequiredResult and are
	// waiting for the client to retry. It stays tiny, only in-flight ones live here.
	awaiting []*call
	events   []*event

	requests, responses, notifications, errors, pending, lateResults int

	lastSeq uint64 // highest per-session Seq seen, for gap detection
	missing uint64 // envelopes dropped upstream, inferred from Seq gaps

	// cacheFresh records the most recent cacheable response per key (SEP-2549).
	cacheFresh map[string]cacheEntry
	// cacheListScope records the cacheScope a page declared, keyed by the cursor
	// that page issued, so a continuation finds the run it actually continues.
	cacheListScope map[string]string
	// truncatedRequests counts client frames mcpsnoop capped at its own frame-size
	// limit. Each opened no call, so each owes one response that will find none.
	truncatedRequests int
	// progress is the last value seen per progress token, and whether the request
	// that issued it has finished.
	progress map[string]progressTracker
	// subscriptions is what each observed subscriptions/listen asked for, keyed by
	// that request's own JSON-RPC id, which is the subscription id every message on
	// its stream carries.
	subscriptions map[string]*subscription
	// stateless records that this session was observed carrying at least one
	// request declaring 2026-07-28 or later, which is what makes the rules that
	// revision introduced applicable to the frames around it.
	stateless bool
}

// Store is the concurrency-safe collector the hub feeds and the TUI reads.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*session
	order    []string // session ids in first-seen order
	// payloadLimit caps the observed bodies a live store keeps in memory, zero
	// for no cap, and payloadBytes is what those bodies currently weigh.
	//
	// The frames themselves are the queue. Each session carries a cursor into its
	// own timeline marking how far the release has reached, so a capture that
	// never approaches the budget pays nothing for the mechanism.
	payloadLimit int
	payloadBytes int
	// frameLimit caps how many frames a live store keeps at all, and frames is
	// how many it currently holds. A frame with its body released still costs its
	// own struct, so the bytes budget alone does not bound a chatty session of
	// small frames.
	frameLimit int
	frames     int
	// bounded separates a live store from the batch one. Either limit may be left
	// unset on its own, so the flag rather than a zero limit is what says whether
	// this store forgets at all.
	bounded bool
}

// New returns an empty store that keeps every observed body.
//
// This is the batch store, the one check, export, diff and open build. They read
// a finite log once and every byte of it has to be there, since a bounded store
// would make check under-report on a large capture, which is the one thing a
// gate must never do.
func New() *Store {
	return &Store{
		sessions: make(map[string]*session),
	}
}

// DefaultLiveBodyLimit is how many bytes of observed bodies the live TUI keeps.
//
// It is a memory bound, not a history bound: every frame stays in the timeline
// whatever this is set to, and only the bodies of the oldest ones are dropped.
// 64 MiB is a few tens of thousands of ordinary frames, far more than anyone
// scrolls back through, while a day-long capture of a chatty server used to
// reach that in minutes and keep going.
const DefaultLiveBodyLimit = 64 << 20

// DefaultLiveFrameLimit is how many frames the live TUI keeps at all.
//
// Bodies are the bulk of a large-payload capture and this is the other half: a
// frame whose body is gone still costs its own struct, so a chatty stream of
// small notifications grew without ever reaching the byte budget. 200,000 is a
// long scrollback and about 80 MiB of frames, and the log on disk still holds
// everything past it.
const DefaultLiveFrameLimit = 200_000

// NewBounded returns a store that keeps at most limit bytes of observed bodies,
// releasing the oldest first. A limit of zero behaves like New.
//
// Only the live TUI builds one. A hub left watching a chatty server grows until
// it is killed, because nothing bounded what the store kept, and every frame is
// already durable on disk. What is released is the body alone: the frame stays
// in the timeline with its sequence, direction, method, status and warning, so
// every count, every row and the whole tool summary stay exact and only the
// inspector for an old frame has less to show. mcpsnoop open on the log is
// still the way to see all of it.
func NewBounded(bodyBytes, frames int) *Store {
	s := New()
	s.bounded = true
	s.payloadLimit = bodyBytes
	s.frameLimit = frames
	return s
}

// BodyLimit reports the byte budget this store keeps observed bodies within,
// zero when it keeps every one. It exists so the choice between the bounded live
// store and the unbounded batch store is readable from outside the package,
// which is the difference between check being exact and check under-reporting.
func (s *Store) BodyLimit() int { return s.payloadLimit }

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

	ev := &event{seq: e.Seq, ts: e.TS, dir: e.Direction, raw: e.Raw, text: e.Text, mcpMethod: e.MCPMethod, mcpName: e.MCPName, mcpProtocolVersion: e.MCPProtocolVersion, mcpParamHeaders: sortedMCPParamHeaders(e.MCPParamHeaders), batch: e.Batch, transport: e.Transport, status: e.Status, authChallenge: e.AuthChallenge}

	if e.Direction == proxy.ServerStderr {
		ev.kind = EventStderr
		s.appendEvent(sess, ev)
		return ev.view(sess)
	}

	if e.Truncated {
		// mcpsnoop capped its own observed copy of a large body, so the bytes are
		// incomplete by design. Flag it structurally rather than through the warning
		// field: it is not a protocol warning, and routing it there would fail a
		// default `check --fail-on warn` over a perfectly valid oversized body.
		ev.kind = EventOther
		ev.truncated = true
		if e.Direction == proxy.ClientToServer {
			// A capped request opens no call, so its perfectly ordinary response has
			// nothing to match. Remembered so that response is not reported as
			// unmatched, for the same reason truncation itself is kept out of the
			// warning field four lines above: the gap is mcpsnoop's own doing and the
			// traffic was correct in both directions.
			sess.truncatedRequests++
		}
		s.appendEvent(sess, ev)
		return ev.view(sess)
	}

	// A response with a status and no JSON-RPC message of its own. Counting a
	// failure here toward the session's errors is what lets a default check run
	// gate on a 401 or a 500; there is no double count, because a failure that
	// does carry a JSON-RPC error body takes the branch below instead.
	if e.Status != 0 && len(e.Raw) == 0 && e.Text == "" {
		ev.kind = EventTransport
		if httpFailed(e.Status) {
			sess.errors++
			ev.errored = true
		}
		s.appendEvent(sess, ev)
		return ev.view(sess)
	}

	msg, ok := proxy.ParseRPC(e.Raw)
	// Read before the switch, since the direction rule is about the frame rather
	// than about what it classifies as, but applied after it, because several
	// branches assign ev.warning outright.
	wrongDirection := ""
	if ok && !ev.batch {
		wrongDirection = wrongDirectionWarning(e.Direction, msg, sess.stateless)
	}
	switch {
	case !ok:
		if httpFailed(e.Status) {
			// Not stream corruption. The target answered with an HTTP error and a
			// body that is not JSON-RPC, typically a gateway's HTML page. Reporting
			// that as a corrupted stream sends the reader after the wrong problem,
			// and on stdio, where there is no status, nothing changes.
			ev.kind = EventTransport
			sess.errors++
			ev.errored = true
		} else {
			ev.kind = EventInvalid
		}
	case msg.Method == "" && msg.IsResponse():
		ev.kind = EventResponse
		ev.id = string(msg.ID)
		ev.warning = validationWarning(msg)
		sess.caps.applyResponseMeta(msg.Result)
		c, matched := sess.completeCall(ev.id, e.Direction, e.ConnID, e.TS, msg, e.Redacted)
		ev.call = c
		if matched && c != nil && c.lateResult {
			// One-for-one with sess.lateResults++: completeCall returns matched for the
			// frame that incremented it and false for any further response to the same
			// call, so this marks each counted late result exactly once.
			ev.lateResult = true
			ev.observation = "result arrived " + e.TS.Sub(c.cancelledAt).Round(time.Millisecond).String() + " after cancellation"
		}
		if c != nil && c.state != Pending && c.state != Streaming {
			sess.closeProgressToken(c.progressToken)
		}
		if state, ok := parseInputRequired(msg.Result); ok {
			for _, note := range inputRequiredWarnings(state, c) {
				ev.warning = appendWarning(ev.warning, note)
			}
			for _, note := range undeclaredInputRequestWarnings(state, c) {
				ev.warning = appendWarning(ev.warning, note)
			}
		}
		// After completeCall, because that is what supplies the call, and the call
		// is what carries the revision this response is judged by.
		if note := missingResultTypeWarning(msg, c); note != "" {
			ev.warning = appendWarning(ev.warning, note)
		}
		// Drained here rather than returned, because applyToolsList runs inside
		// completeCall. Each name is reported once, on the frame that advertised it.
		//
		// Gated the same two ways every other x-mcp-header check is, by the revision
		// the listing request itself declared and by the transport. The annotation
		// does not exist before 2026-07-28, where an x- prefixed key is only the JSON
		// Schema vendor-extension convention, and the released text scopes the duty
		// to reject to "clients using the Streamable HTTP transport", saying of the
		// rest that they "MAY ignore x-mcp-header annotations entirely". Without the
		// gates a stdio server, or any server on an older revision, was accused of
		// breaking a rule it is not under.
		if ev.transport == proxy.TransportHTTP && c != nil &&
			requiresRoutingHeaders(requestProtocolVersion(c.params, "")) {
			for _, invalid := range sess.paramHeaderInvalid {
				ev.warning = appendWarning(ev.warning,
					"tool "+strconv.Quote(invalid.tool)+" declares "+invalid.reason+
						", so its Mcp-Param headers are not checked")
			}
		}
		sess.paramHeaderInvalid = nil
		// A client validating a listing rejects the tool outright and never calls
		// it, with nothing on the wire to say why, so this is the frame to say it
		// on. The reason names what was actually there, since "the schema is wrong"
		// leaves a reader bisecting a listing.
		for _, invalid := range sess.schemaRootInvalid {
			ev.warning = appendWarning(ev.warning,
				"tool "+strconv.Quote(invalid.tool)+" advertises "+invalid.reason+
					`, and 2026-07-28 requires one whose root type is "object"`)
		}
		sess.schemaRootInvalid = nil
		if c != nil {
			hint, cacheWarnings := sess.recordCacheFromResponse(c, msg.Result, e.TS)
			if !hint.Empty() {
				ev.cacheHint = hint
			}
			for _, note := range cacheWarnings {
				ev.warning = appendWarning(ev.warning, note)
			}
		}
		if c != nil && c.taskID != "" {
			ev.taskID = c.taskID
			if c.method != "tools/call" {
				ev.taskCall = sess.tasks[c.taskID]
				if matched {
					if parent, failed := sess.applyTaskState(c.taskID, msg.Result, e.TS); parent != nil {
						ev.taskCall = parent
						if failed {
							sess.errors++
							ev.errored = true
						}
					}
				}
			}
		}
		sess.responses++
		switch {
		case c == nil:
			if sess.truncatedRequests > 0 {
				// One capped request accounted for. Counting down rather than latching
				// keeps the check alive for the rest of the session, so a genuinely
				// unmatched response after it is still reported.
				sess.truncatedRequests--
			} else {
				ev.warning = appendWarning(ev.warning, "response id has no matching request")
			}
			if msg.Error != nil {
				sess.errors++
				ev.errored = true
			}
		case !matched:
			ev.warning = appendWarning(ev.warning, "duplicate response for the same id")
		default:
			if c.errored {
				sess.errors++
				ev.errored = true
			}
		}
	case msg.Method != "" && len(msg.ID) > 0:
		ev.kind = EventRequest
		ev.method = msg.Method
		ev.id = string(msg.ID)
		ev.warning = validationWarning(msg)
		var reused bool
		if root, stateIssue := sess.matchRetry(msg); root != nil {
			// A continuation, not a new call. Mapping the retry id onto the same
			// call object lets completeCall find it, so the operation keeps one
			// pending slot and one duration however many round trips it takes.
			sess.calls[requestCallKey(ev.id, msg.ID, e)] = root
			root.mrtrState, root.mrtrKeys = "", ""
			root.mrtrHasState = false
			sess.unpark(root)
			ev.call = root
			ev.kind = EventRequest
			ev.method = msg.Method
			ev.mrtrRoot = root.id
			ev.warning = validationWarning(msg)
			ev.mrtrStateIssue = stateIssue
			if warning := stateIssue.warning(); warning != "" {
				// The spec makes the echo a MUST for the client, so a violation is a
				// protocol error on the wire rather than an observation of ours. It
				// therefore rides the warning field like any other protocol warning,
				// which means a default check run fails on it. That is deliberate: a
				// client mangling server state is worth stopping a build for.
				ev.warning = appendWarning(ev.warning, warning)
			}
			sess.requests++ // a request on the wire, even though not a new call
			break
		}
		ev.call, reused = sess.openCall(ev.id, msg, e)
		ev.call.progressToken = sess.noteProgressToken(msg.Params)
		if msg.Method == "subscriptions/listen" && e.Direction == proxy.ClientToServer {
			sess.noteSubscription(ev.id, msg.Params)
		}
		if ev.call.taskID != "" {
			ev.taskID = ev.call.taskID
			ev.taskCall = sess.tasks[ev.taskID]
		}
		sess.requests++
		if reused {
			ev.warning = appendWarning(ev.warning, "request reuses an id already in flight")
		}
		// Counted on what this call is, independently of the reuse. openCall has
		// already released the superseded call's slot if it held one, so the two
		// decisions no longer have to agree about who owns the accounting.
		if ev.call.state == Pending {
			sess.pending++
		}
		if msg.Method == "initialize" {
			sess.caps.applyRequest(msg.Params)
		}
		if ev.mrtrRoot == "" && cacheableMethod(msg.Method) {
			if note := sess.checkCacheStale(msg.Method, msg.Params, e.TS, requestProtocolVersion(msg.Params, e.MCPProtocolVersion)); note != "" {
				ev.cacheStaleRefetch = note
			}
		}
	case msg.Method != "":
		ev.kind = EventNotification
		ev.method = msg.Method
		ev.warning = validationWarning(msg)
		sess.notifications++
		sess.invalidateCache(msg.Method, msg.Params)
		for _, note := range sess.subscriptionWarnings(e.Direction, msg.Method, msg.Params) {
			ev.warning = appendWarning(ev.warning, note)
		}
		if msg.Method == "notifications/progress" {
			if note := sess.progressWarning(msg.Params); note != "" {
				ev.warning = appendWarning(ev.warning, note)
			}
		}
		if msg.Method == "notifications/cancelled" {
			if id, reason := cancelledRequest(msg.Params); id != "" {
				if note := serverCancelWarning(e.Direction, sess.calls[callKey{
					dir: opposite(e.Direction), id: id, conn: e.ConnID,
				}]); note != "" {
					ev.warning = appendWarning(ev.warning, note)
				}
				if c := sess.closeStreamingCall(id, e.ConnID, e.TS, reason); c != nil {
					ev.call = c
				} else if c := sess.cancelCall(id, e.Direction, e.ConnID, e.TS, reason); c != nil {
					ev.call = c
				}
			}
		}
		if msg.Method == "notifications/tasks" {
			if state, ok := parseTaskState(msg.Params); ok {
				ev.taskID = state.TaskID
				if parent, failed := sess.applyParsedTaskState(state, e.TS); parent != nil {
					ev.taskCall = parent
					if failed {
						sess.errors++
						ev.errored = true
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
			if ev.mcpMethod != "" && msg.Method != "" && ev.mcpMethod != msg.Method &&
				!unverifiableAfterRedaction(e.Redacted, ev.mcpMethod, msg.Method) {
				ev.warning = appendWarning(ev.warning, "routing header Mcp-Method "+ev.mcpMethod+" disagrees with body method "+msg.Method)
				ev.mismatch = true
			}
			// Mcp-Name names the target operation (params.name for tools/call and
			// prompts/get, params.uri for resources/read). A lie here is the
			// tool-shadowing case, so it matters more than a bare method mismatch.
			//
			// The header is decoded first. A name or URI that will not fit in an
			// HTTP field value travels Base64 in a sentinel, and the spec requires
			// it decoded before the comparison. Comparing the encoded form accused
			// every compliant client with a non-ASCII name or path, and since that
			// rides the warning signal it failed a default check run on correct
			// traffic. The message reports the decoded value, because base64 in a
			// warning tells a reader nothing.
			if name := operationName(msg); ev.mcpName != "" && name != "" {
				if header := decodeHeaderValue(ev.mcpName); header != name &&
					!unverifiableAfterRedaction(e.Redacted, header, name) {
					// Quoted, because a decoded value has none of the guarantees the
					// raw header had: the sentinel exists to carry what an HTTP field
					// value cannot, so this can hold newlines, control characters, or
					// an escape sequence. Warnings are rendered as a terminal cell and
					// written one per line by the text export, and either would break
					// on a raw one. Quoting the body name too keeps the pair readable
					// when a name contains a space.
					ev.warning = appendWarning(ev.warning,
						"routing header Mcp-Name "+strconv.Quote(header)+" disagrees with body name "+strconv.Quote(name))
					ev.mismatch = true
				}
			}
		}
	}

	// The MCP-Protocol-Version header must match the version the request repeats in
	// its _meta; a gateway routes on the header while the server reads the body, so
	// a disagreement is the same class of spec violation as a routing-header
	// mismatch. This is request-scoped, so unlike the routing headers it is valid on
	// a batch and gated only on the header being present.
	if ev.mcpProtocolVersion != "" {
		if mv := metaProtocolVersion(msg.Params); mv != "" && mv != ev.mcpProtocolVersion &&
			!unverifiableAfterRedaction(e.Redacted, mv, ev.mcpProtocolVersion) {
			ev.warning = appendWarning(ev.warning,
				"MCP-Protocol-Version header "+ev.mcpProtocolVersion+
					" disagrees with _meta protocolVersion "+mv)
			ev.mismatch = true
		}
	}

	// Absent headers, as opposed to disagreeing ones. A compliant server rejects
	// both with 400 and -32020, so they share a signal, but they are different
	// questions and the guards are not the same: a disagreement needs the header
	// to be there, this needs to know it should have been.
	//
	// Requests only, which is why this needs an id and not just a method. The
	// released 2026-07-28 transport says of a notification POST that "header
	// requirements for notification POSTs are not defined by this revision", and
	// that the core protocol defines no client-to-server notification on
	// Streamable HTTP at all. Demanding a header there would be inventing a rule
	// the spec declines to state, on a signal that fails a default check run.
	//
	// Judged by the revision this one request declared, never the session's. The
	// session value is last-write-wins across every client request, and the spec
	// forbids inferring a request's version from earlier requests on the same
	// connection while explicitly allowing clients to interleave unrelated ones,
	// so reading it would accuse an older client's request of omitting headers
	// that a newer neighbour's revision requires. This mirrors what
	// missingResultTypeWarning already does with call.protocolVersion.
	//
	// initialize needs no special case under that rule. Its version rides
	// params.protocolVersion rather than _meta or the header, so a handshake
	// request declares nothing here and the gate closes on its own. That is the
	// right answer twice over: 2026-07-28 removed the handshake, so a client
	// sending one is speaking an earlier revision whatever it hoped for.
	if ev.transport == proxy.TransportHTTP && e.Direction == proxy.ClientToServer &&
		!ev.batch && msg.Method != "" && len(msg.ID) > 0 &&
		requiresRoutingHeaders(requestProtocolVersion(msg.Params, ev.mcpProtocolVersion)) {
		for _, h := range missingRoutingHeaders(ev, msg.Method) {
			ev.warning = appendWarning(ev.warning, "required routing header "+h+" is missing")
			ev.mismatch = true
		}
	}

	// io.modelcontextprotocol/clientCapabilities is REQUIRED in every request's
	// _meta from 2026-07-28, alongside protocolVersion, and a server MUST reject a
	// request missing a required field with -32602. Unlike the routing headers this
	// holds on every transport, so it is checked separately rather than folded in
	// above, and it does not set ev.mismatch: that flag means the -32020
	// header-versus-body condition, and this is a different rejection.
	//
	// Same two guards as the headers, for the same reasons. Requests with an id
	// only, and judged by the revision this request declared, never the session's.
	if wrongDirection != "" {
		ev.warning = appendWarning(ev.warning, wrongDirection)
	}
	if e.Direction == proxy.ClientToServer && !ev.batch && msg.Method != "" && len(msg.ID) > 0 &&
		requiresRequestMeta(requestProtocolVersion(msg.Params, ev.mcpProtocolVersion)) {
		if !declaresClientCapabilities(msg.Params) {
			ev.warning = appendWarning(ev.warning,
				"request _meta is missing required io.modelcontextprotocol/clientCapabilities")
		}
		// The sibling field, checked the same way. It escaped notice because
		// requestProtocolVersion falls back to the header, so an absent field still
		// opens the revision gate and nothing then asked whether it was there.
		if note := missingProtocolVersionWarning(msg.Params); note != "" {
			ev.warning = appendWarning(ev.warning, note)
		}
		sess.stateless = true
	}

	if ev.transport == proxy.TransportHTTP && e.Direction == proxy.ClientToServer &&
		!ev.batch && msg.Method == "tools/call" && len(msg.ID) > 0 &&
		requiresRoutingHeaders(requestProtocolVersion(msg.Params, ev.mcpProtocolVersion)) {
		for _, warning := range mcpParamHeaderWarnings(sess, msg, ev.mcpParamHeaders, e.Redacted) {
			ev.warning = appendWarning(ev.warning, warning)
			ev.mismatch = true
		}
	}

	if note := deprecatedMethodNote(msg.Method); note != "" {
		ev.deprecated = note
	} else if state, ok := parseInputRequired(msg.Result); ok {
		// A response has no method of its own, and under MRTR the deprecated
		// features are named inside inputRequests rather than on the frame, so
		// the top-level check alone is blind for the one remaining way a server
		// can ask for sampling or roots.
		if note := deprecatedNestedNote(state.methods); note != "" {
			ev.deprecated = note
		}
	} else if msg.Error != nil {
		// An error response has no method and no result, so neither branch above
		// can see it. Its code is the one place a retired feature still shows.
		if note := deprecatedErrorCodeNote(msg.Error.Code); note != "" {
			ev.deprecated = note
		}
	}

	// The server's own verdict on the header checks mcpsnoop performs itself.
	// Flagged structurally rather than as a warning: the frame is already an
	// error and already counted, and what this adds is that the failure was a
	// routing one. It remains useful when mcpsnoop never observed the matching
	// tool definition and therefore could not map an Mcp-Param-{Name} header
	// back to its argument path.
	if msg.Error != nil && msg.Error.Code == ErrorCodeHeaderMismatch {
		ev.mismatch = true
	}

	s.appendEvent(sess, ev)
	return ev.view(sess)
}

// appendEvent records a frame and, on a bounded store, releases the oldest
// bodies until the budget is met. Caller holds the write lock.
//
// Every branch of Ingest goes through here rather than appending directly, so a
// branch added later cannot quietly opt out of the bound.
func (s *Store) appendEvent(sess *session, ev *event) {
	sess.events = append(sess.events, ev)
	if !s.bounded {
		return
	}
	n := bodyWeight(ev)
	sess.bodyBytes += n
	s.payloadBytes += n
	s.frames++
	for s.payloadLimit > 0 && s.payloadBytes > s.payloadLimit {
		oldest := s.oldestRetained()
		if oldest == nil {
			break // nothing left to release, which the budget cannot fix
		}
		released := oldest.releaseOldestBody()
		oldest.bodyBytes -= released
		s.payloadBytes -= released
	}
	// Releasing bodies bounds the bytes but not the count, and a frame with no
	// body still costs its own struct. A day of small notifications reached
	// hundreds of megabytes of those alone, so the oldest frames go entirely once
	// there are too many of them.
	for s.frameLimit > 0 && s.frames > s.frameLimit {
		oldest := s.oldestSession()
		if oldest == nil {
			break
		}
		oldest.dropOldestFrame()
		s.frames--
	}
}

// oldestSession returns the session holding the oldest frame in the store, so
// frames go in arrival order across every session a hub carries.
func (s *Store) oldestSession() *session {
	var pick *session
	for _, id := range s.order {
		sess := s.sessions[id]
		if sess == nil || len(sess.events) == 0 {
			continue
		}
		if pick == nil || sess.events[0].ts.Before(pick.events[0].ts) {
			pick = sess
		}
	}
	return pick
}

// dropOldestFrame removes this session's oldest frame from the timeline, after
// folding whatever it says about a tool call into the running statistics. The
// call itself goes with it when nothing else refers to it, since a settled call
// that no frame points at is only holding memory.
func (sess *session) dropOldestFrame() {
	ev := sess.events[0]
	sess.toolStats.fold(ev)
	sess.bodyBytes -= bodyWeight(ev)
	sess.events[0] = nil // do not pin the frame through the slice's own array
	sess.events = sess.events[1:]
	sess.dropped++
	if sess.evictCursor > 0 {
		sess.evictCursor--
	}
	if ev.call != nil && ev.call.state != Pending && ev.call.state != Streaming {
		for key, c := range sess.calls {
			if c == ev.call {
				delete(sess.calls, key)
			}
		}
	}
}

// oldestRetained returns the session whose next unreleased frame is the oldest
// in the store, so release is oldest-first across every session a hub carries
// rather than per session. A hub holds a handful of sessions, so the scan is
// cheaper than keeping a second index of every frame in the store.
func (s *Store) oldestRetained() *session {
	var pick *session
	for _, id := range s.order {
		sess := s.sessions[id]
		if sess == nil || sess.evictCursor >= len(sess.events) {
			continue
		}
		if pick == nil || sess.events[sess.evictCursor].ts.Before(pick.events[pick.evictCursor].ts) {
			pick = sess
		}
	}
	return pick
}

// releaseOldestBody drops the body of this session's oldest frame that still has
// one and returns the bytes freed. Frames that never carried a body are stepped
// over, so the cursor only ever moves forward.
func (sess *session) releaseOldestBody() int {
	for sess.evictCursor < len(sess.events) {
		ev := sess.events[sess.evictCursor]
		sess.evictCursor++
		if n := bodyWeight(ev); n > 0 {
			releaseBody(ev)
			return n
		}
	}
	return 0
}

// bodyWeight is what a frame's observed bytes cost, the frame's own copy plus
// the call's. json.Unmarshal copies rather than aliasing, so a request holds its
// params twice over and releasing one half frees nothing.
func bodyWeight(ev *event) int {
	n := len(ev.raw) + len(ev.text)
	if ev.call == nil {
		return n
	}
	switch ev.kind {
	case EventRequest:
		n += len(ev.call.params)
	case EventResponse:
		n += len(ev.call.result)
	}
	return n
}

// releaseBody drops a frame's observed body and the call's copy of the same
// bytes. The result size is remembered first, because the tool summary measures
// what a server costs in context from it and that figure must not shrink just
// because the bytes are no longer held.
func releaseBody(ev *event) {
	ev.raw, ev.text, ev.bodyReleased = nil, "", true
	if ev.call == nil {
		return
	}
	switch ev.kind {
	case EventRequest:
		ev.call.params = nil
	case EventResponse:
		ev.call.releasedResultBytes = len(ev.call.result)
		ev.call.result = nil
	}
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
	key := requestCallKey(id, msg.ID, e)
	prev, ok := sess.calls[key]
	reused := ok && (prev.state == Pending || prev.state == Streaming)
	if reused {
		// The earlier in-flight call keeps this id, so it will never be matched now.
		// Mark it superseded (not pending) so the timeline stops rendering it as a
		// hanging request. The "reuses an id already in flight" warning on the new
		// request is the explanation.
		//
		// It gives up its pending slot here, where the transition happens, and only
		// when it actually held one. A Streaming call never did, so transferring a
		// slot it never took would drive the counter negative once the new call is
		// answered.
		if prev.state == Pending {
			sess.pending--
		}
		prev.state = Superseded
		prev.end = e.TS
	}
	c := &call{
		requestSeq: e.Seq,
		id:         id,
		method:     msg.Method,
		reqDir:     e.Direction,
		params:     msg.Params,
		start:      e.TS,
		state:      Pending,
	}
	if isStreamOpeningMethod(msg.Method) {
		c.state = Streaming
	}
	if msg.Method == "tools/call" {
		c.isTool = true
		c.toolName = toolName(msg.Params)
	}
	if isTaskMethod(msg.Method) {
		c.taskID = taskID(msg.Params)
	}
	c.opName = operationName(msg)
	c.protocolVersion = requestProtocolVersion(msg.Params, e.MCPProtocolVersion)
	sess.calls[key] = c
	return c, reused
}

func requestCallKey(id string, rawID json.RawMessage, e proxy.Envelope) callKey {
	key := callKey{dir: e.Direction, id: id, conn: e.ConnID}
	if bytes.Equal(bytes.TrimSpace(rawID), []byte("null")) {
		key.seq = e.Seq
	}
	return key
}

// completeCall matches a response to a request. The bool is false for an
// unmatched response or a duplicate of an already-observed result. Caller holds the lock.
func (sess *session) completeCall(id string, respDir proxy.Direction, conn string, ts time.Time, msg proxy.RPCMessage, redacted bool) (*call, bool) {
	c := sess.calls[callKey{dir: opposite(respDir), id: id, conn: conn}]
	if c == nil {
		return nil, false // unmatched response (request missed or before backfill)
	}
	if c.state == Cancelled {
		if c.lateResult {
			return c, false
		}
		// The response to a torn-down subscription is the graceful closure the spec
		// asks the server for, "it SHOULD respond to the original
		// subscriptions/listen request with an empty result before closing the
		// stream". That is the intended end of the exchange, so it is neither a
		// late result nor the duplicate this branch used to report.
		if c.method == "subscriptions/listen" {
			c.end = ts
			c.result = msg.Result
			c.err = msg.Error
			return c, true
		}
		c.end = ts
		c.result = msg.Result
		c.err = msg.Error
		c.toolErr = isToolError(msg.Result)
		// Late or not, an error is an error. Leaving errored false made a real
		// -32603 arriving after a cancellation read as errors=0 and pass a default
		// check run, which is the one thing the error axis exists to stop. The
		// state stays Cancelled, because that is what happened to the call; the
		// axis says what the answer contained.
		c.errored = msg.Error != nil || c.toolErr
		c.lateResult = true
		sess.lateResults++
		// The payload is still the server's answer, so what it declares has to
		// land. Without this a late tools/list left the tool inventory empty and
		// `check --fail-on drift` compared nothing and printed green.
		sess.applyResponseSideEffects(c, msg, redacted)
		return c, true
	}
	if c.state != Pending && c.state != Streaming {
		return c, false // already answered, a duplicate or late response must not recount
	}
	if c.method == "tools/call" && c.taskID != "" {
		return c, false // the task handle already continued this call
	}
	if state, ok := parseInputRequired(msg.Result); ok {
		// The operation is not finished, it is waiting on the client. Keep it
		// pending and open so its duration ends up spanning the whole exchange,
		// including the time the user spends answering.
		c.result = msg.Result
		c.mrtrState = state.requestState
		c.mrtrHasState = state.hasRequestState
		c.mrtrKeys = state.keys
		sess.park(c)
		return c, true
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
	wasPending := c.state == Pending
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
	if wasPending {
		sess.pending--
	}
	sess.applyResponseSideEffects(c, msg, redacted)
	return c, true
}

// applyResponseSideEffects folds a result into the session state it describes.
// Shared by the ordinary path and the late one, because a response that arrived
// after a cancellation is still the server's answer and still declares what the
// server has.
func (sess *session) applyResponseSideEffects(c *call, msg proxy.RPCMessage, redacted bool) {
	switch c.method {
	case "initialize":
		sess.caps.applyResponse(msg.Result)
	case "server/discover":
		sess.caps.applyDiscover(msg.Result)
	case "tools/list":
		sess.applyToolsList(c.params, msg.Result, redacted)
	}
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
// are sourced verbatim from the released schema (schema/2026-07-28). It is a
// no-op when the request
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
// The normative schema (schema/2026-07-28, $defs.ResultMetaObject) says servers SHOULD send
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
func (sess *session) applyToolsList(reqParams, result json.RawMessage, redacted bool) {
	// The tools array is decoded element by element so each definition's exact
	// bytes stay available to measure, and so one malformed entry skips only
	// itself. Decoding straight into a typed slice did neither: it discarded the
	// whole list on a single bad element and left no handle on what was sent.
	var r struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor string            `json:"nextCursor"`
	}

	if json.Unmarshal(result, &r) != nil {
		return
	}
	sess.toolListSeen = true

	if !hasListCursor(reqParams) {
		clear(sess.advertisedSet)
		clear(sess.toolDefinitions)
		sess.advertisedTools = nil
	}

	var invalidParamHeaderTools []paramHeaderViolation
	var invalidSchemaRootTools []paramHeaderViolation
	for _, rawTool := range r.Tools {
		var tool struct {
			Name string `json:"name"`
			// Kept raw, not decoded to a string, because the cost below has to
			// measure the bytes the server actually spelled. Re-encoding a decoded
			// string cannot reproduce them in either direction: it would escape a
			// literal < that the server sent plain, and flatten a \u003c or a
			// \u00e9 that the server sent escaped.
			Description json.RawMessage `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
			// Title is raw for the same reason as Description: a plain string field
			// makes "title": 42 fail the decode below and drop the whole tool.
			Title        json.RawMessage `json:"title"`
			OutputSchema json.RawMessage `json:"outputSchema"`
			Annotations  json.RawMessage `json:"annotations"`
			Icons        json.RawMessage `json:"icons"`
		}
		if json.Unmarshal(rawTool, &tool) != nil || tool.Name == "" {
			continue
		}
		var description string
		// Absent, null, and not-a-string all leave this empty. The last of those is
		// a deliberate change: decoding into a string field failed the whole
		// tool-level Unmarshal above, so a server that sent an object or a number
		// as its description dropped the tool out of the inventory entirely, name
		// and schema included. Losing a tool from the list is a worse answer than
		// listing it with no description, and the raw bytes are still measured, so
		// the cost figure still says the description was there.
		_ = json.Unmarshal(tool.Description, &description)
		var title string
		_ = json.Unmarshal(tool.Title, &title)
		if _, ok := sess.advertisedSet[tool.Name]; ok {
			continue
		}

		sess.advertisedSet[tool.Name] = struct{}{}
		sess.advertisedTools = append(sess.advertisedTools, tool.Name)
		schema := append(json.RawMessage(nil), tool.InputSchema...)
		// The validity verdict is reported, not discarded. The spec makes an
		// x-mcp-header violating its constraints a definition a client MUST reject,
		// excluding the tool from the list and logging why. mcpsnoop observes rather
		// than serves, so the analogous answer is to say so on the frame that
		// advertised it and to compare no headers for that tool, which is what a
		// conforming client would then be doing anyway. Throwing the verdict away
		// left every one of those constraints computed and never reported.
		//
		// Only an actual violation is reported, and it says which constraint. A
		// schema mcpsnoop could not read is not the server's fault, and reporting the
		// two the same way turned the user's own --redact-secrets into an accusation
		// against traffic that was correct.
		paramHeaders, violation := mcpParamHeaderBindings(schema)
		if violation != "" {
			invalidParamHeaderTools = append(invalidParamHeaderTools,
				paramHeaderViolation{tool: tool.Name, reason: violation})
		}

		// Measured from the raw bytes, not gated on the decoded string being
		// non-empty: whatever the description field holds, those bytes were spent
		// inside the definition Bytes counts, and this number exists to say where
		// they went. compactJSONLen already answers 0 for an absent field, which
		// keeps the rule that no description weighs nothing rather than the two
		// bytes of an empty one.
		// Both schemas a tool can advertise are analysed and their kinds merged, so
		// a dialect declared only on the outputSchema is not invisible. Only the
		// inputSchema is judged against the root-object rule, since that is the only
		// one the Tool definition constrains.
		findings := mergeSchemaFindings(
			analyzeInputSchema(schema, redacted),
			analyzeOutputSchema(tool.OutputSchema, redacted))
		if reason := schemaRootViolation(schema, redacted); reason != "" {
			invalidSchemaRootTools = append(invalidSchemaRootTools,
				paramHeaderViolation{tool: tool.Name, reason: reason})
		}

		sess.toolDefinitions[tool.Name] = ToolDefinition{
			Name:         tool.Name,
			Description:  description,
			InputSchema:  schema,
			Title:        title,
			OutputSchema: append(json.RawMessage(nil), tool.OutputSchema...),
			Annotations:  append(json.RawMessage(nil), tool.Annotations...),
			Icons:        append(json.RawMessage(nil), tool.Icons...),
			Findings:     findings,
			paramHeaders: paramHeaders,
			Cost: ToolCost{
				Name:             tool.Name,
				Bytes:            compactJSONLen(rawTool),
				DescriptionBytes: compactJSONLen(tool.Description),
				SchemaBytes:      compactJSONLen(tool.InputSchema),
				FindingKinds:     findingKinds(findings),
			},
		}
	}
	sess.toolListComplete = r.NextCursor == ""
	sess.paramHeaderInvalid = append(sess.paramHeaderInvalid, invalidParamHeaderTools...)
	sess.schemaRootInvalid = append(sess.schemaRootInvalid, invalidSchemaRootTools...)
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

// compactJSONLen is the byte length of raw with insignificant whitespace
// removed, so a definition's measured cost is canonical and comparable between
// servers rather than inflated by whichever one pretty-prints its tools/list.
// Whitespace inside string values is untouched, so a description keeps its own
// spaces; only the server's indentation between tokens is normalised away.
func compactJSONLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return len(raw) // unparseable, fall back to the bytes as they arrived
	}
	return buf.Len()
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
// inputRequired is the parked half of a multi round-trip exchange (SEP-2322).
type inputRequired struct {
	requestState    string
	hasRequestState bool
	keys            string
	// methods names the server-to-client requests carried inside inputRequests.
	// The 2026-07-28 revision routes sampling and roots exclusively through this
	// map, so it is the only place their method names still appear.
	methods []string
}

// parseInputRequired recognises the result a server sends when it needs more
// input before it can finish. Only prompts/get, resources/read and tools/call
// can receive one, but the result shape is enough to tell, so the method is not
// re-checked here.
func parseInputRequired(raw json.RawMessage) (inputRequired, bool) {
	if len(raw) == 0 {
		return inputRequired{}, false
	}
	var r struct {
		ResultType    string                     `json:"resultType"`
		RequestState  *string                    `json:"requestState"`
		InputRequests map[string]json.RawMessage `json:"inputRequests"`
	}
	if json.Unmarshal(raw, &r) != nil || r.ResultType != "input_required" {
		return inputRequired{}, false
	}
	methods := make([]string, 0, len(r.InputRequests))
	for _, raw := range r.InputRequests {
		var req struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(raw, &req) == nil && req.Method != "" {
			methods = append(methods, req.Method)
		}
	}
	slices.Sort(methods) // stable order, the map iteration order is not
	state := inputRequired{
		hasRequestState: r.RequestState != nil,
		keys:            sortedKeySet(r.InputRequests),
		methods:         methods,
	}
	if r.RequestState != nil {
		state.requestState = *r.RequestState
	}
	return state, true
}

// retrySignals reads the two things a retry carries that can tie it back to the
// request it continues.
func retrySignals(params json.RawMessage) (state string, hasState bool, keys string) {
	if len(params) == 0 {
		return "", false, ""
	}
	var p struct {
		RequestState   *string                    `json:"requestState"`
		InputResponses map[string]json.RawMessage `json:"inputResponses"`
	}
	if json.Unmarshal(params, &p) != nil {
		return "", false, ""
	}
	if p.RequestState != nil {
		state, hasState = *p.RequestState, true
	}
	return state, hasState, sortedKeySet(p.InputResponses)
}

// sortedKeySet renders a key set so two of them can be compared as strings.
func sortedKeySet(m map[string]json.RawMessage) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, "\x00")
}

func (sess *session) park(c *call) {
	for _, p := range sess.awaiting {
		if p == c {
			return // a longer chain re-parks the same operation
		}
	}
	sess.awaiting = append(sess.awaiting, c)
}

func (sess *session) unpark(c *call) {
	for i, p := range sess.awaiting {
		if p == c {
			sess.awaiting = append(sess.awaiting[:i], sess.awaiting[i+1:]...)
			return
		}
	}
}

// matchRetry finds the operation a request continues, or nil when it is a new
// one. Because the spec requires the retry to use a different id, the link is
// inferred. An echoed requestState is opaque and server-minted, so an exact
// match is conclusive on its own. Otherwise the fallback needs the method, the
// operation name and the full set of answered keys to agree, and gives up when
// more than one parked operation fits, since a wrong link is worse than no link.
func (sess *session) matchRetry(msg proxy.RPCMessage) (*call, MRTRStateIssue) {
	state, hasState, keys := retrySignals(msg.Params)
	if !hasState && keys == "" {
		return nil, ""
	}
	if hasState {
		for _, c := range sess.awaiting {
			if c.state == Pending && c.mrtrHasState && c.mrtrState == state {
				return c, ""
			}
		}
	}

	name := operationName(msg)
	var fallback *call
	matches := 0
	for _, c := range sess.awaiting {
		if c.state == Pending && c.mrtrKeys != "" && c.mrtrKeys == keys &&
			c.method == msg.Method && c.opName == name {
			fallback = c
			matches++
		}
	}
	if matches == 1 {
		return fallback, classifyMRTRState(fallback, state, hasState)
	}
	return nil, ""
}

// classifyMRTRState compares the state a retry carried with the one the server
// issued. It only ever compares opaque bytes: the spec forbids the client from
// inspecting the contents, and an observer has no more reason to than a client
// does.
//
// One case is out of reach. A server may answer with requestState and no
// inputRequests at all, and then a tampered retry echoes a state that matches
// nothing and answers no keys, so there is nothing left to link it by and it
// reads as an unrelated call rather than a violation. Linking on the method and
// operation name alone would catch it, but would also fuse two genuinely
// separate calls to the same tool, and a wrong link is worse than a missed one.
// See TestMRTRCannotSeeTamperingOnAStateOnlyExchange.
func classifyMRTRState(c *call, retryState string, retryHasState bool) MRTRStateIssue {
	switch {
	case c.mrtrHasState && !retryHasState:
		return MRTRStateMissing
	case !c.mrtrHasState && retryHasState:
		return MRTRStateInvented
	case c.mrtrHasState && retryHasState && c.mrtrState != retryState:
		return MRTRStateChanged
	default:
		return ""
	}
}

func isStreamOpeningMethod(method string) bool {
	return method == "subscriptions/listen"
}

// cancelledRequest returns the id a notifications/cancelled names, in the form the
// store keys calls by. That form is the raw JSON text of the id, because Ingest
// keys a call on string(msg.ID), so a string id keeps its quotes. Decoding into a
// Go string here would strip them and never match the key, and a string id is
// what the specification's own cancellation example uses.
func cancelledRequest(params json.RawMessage) (string, string) {
	if len(params) == 0 {
		return "", ""
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if json.Unmarshal(params, &p) != nil {
		return "", ""
	}
	return string(bytes.TrimSpace(p.RequestID)), p.Reason
}

// cancelCall settles a request the client gave up on. The key is built bare
// rather than through requestCallKey because a cancellation naming a null id
// could not say which of several such requests it meant, and #209 made those
// deliberately unreachable by id alone.
func (sess *session) cancelCall(id string, dir proxy.Direction, conn string, ts time.Time, reason string) *call {
	c := sess.calls[callKey{dir: dir, id: id, conn: conn}]
	if c == nil || c.state != Pending {
		return nil
	}
	// A call whose result was a task handle is only nominally pending; the work
	// continues under its taskId and its terminal state arrives through
	// notifications/tasks. Taking it here settled the call early and threw that
	// state away, so a task that ended failed reported errors=0. The spec puts
	// this case among the cancellations a server may ignore, "Processing has
	// already completed", and cancelling the work itself is tasks/cancel.
	if c.taskID != "" {
		return nil
	}
	c.state = Cancelled
	c.cancelledAt = ts
	c.cancelReason = reason
	sess.pending--
	return c
}

// closeStreamingCall settles the long-lived call a notifications/cancelled names.
// It is only ever reached from that notification, so the outcome is a
// cancellation rather than a completion, whoever sent it. A server tearing down a
// subscription is the one cancellation the spec asks a server for, and a client
// ending its own subscription on stdio sends the same notification. The graceful
// empty result that a server SHOULD send afterwards arrives separately and is
// handled in completeCall.
//
// No pending slot is released, because a Streaming call never took one.
func (sess *session) closeStreamingCall(id, conn string, ts time.Time, reason string) *call {
	for _, dir := range []proxy.Direction{proxy.ClientToServer, proxy.ServerToClient} {
		key := callKey{dir: dir, id: id, conn: conn}
		c := sess.calls[key]
		if c != nil && c.state == Streaming {
			c.end = ts
			c.state = Cancelled
			c.cancelledAt = ts
			c.cancelReason = reason
			return c
		}
	}
	return nil
}

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

// routingHeadersRequiredFrom is the revision that introduced the Streamable HTTP
// routing headers and made them REQUIRED. They do not appear in any earlier
// revision at all, so a session speaking one of those is correct to omit them.
const routingHeadersRequiredFrom = "2026-07-28"

// atLeastRevision reports whether a protocol version is at or after a revision.
//
// Revisions are dated YYYY-MM-DD, so string order is chronological, and a named
// pre-release such as "draft" sorts after every date, which is the right answer:
// it is ahead of the last release, not behind it. An empty version is false by
// the same comparison, which is what we want, since a version we never saw is not
// evidence and guessing would mean accusing someone on none.
//
// One place for the comparison and this reasoning, so a second gate cannot copy
// the operator and leave the reasoning behind.
func atLeastRevision(version, from string) bool {
	return version >= from
}

// requiresRoutingHeaders reports whether a protocol version mandates them.
func requiresRoutingHeaders(version string) bool {
	return atLeastRevision(version, routingHeadersRequiredFrom)
}

// namedOperationMethods are the three methods whose target Mcp-Name must carry.
// Deliberately narrower than operationName, which also maps resources/subscribe
// and resources/unsubscribe: those are not in the spec's table, and demanding a
// header there would flag a client the spec does not oblige. (They are also gone
// from 2026-07-28, replaced by subscriptions/listen.)
var namedOperationMethods = []string{"tools/call", "resources/read", "prompts/get"}

// missingRoutingHeaders lists the standard headers this request had to carry and
// did not, in the order the spec tabulates them.
func missingRoutingHeaders(ev *event, method string) []string {
	var missing []string
	if ev.mcpProtocolVersion == "" {
		missing = append(missing, "MCP-Protocol-Version")
	}
	if ev.mcpMethod == "" {
		missing = append(missing, "Mcp-Method")
	}
	if ev.mcpName == "" && slices.Contains(namedOperationMethods, method) {
		missing = append(missing, "Mcp-Name")
	}
	return missing
}

// requestMetaRequiredFrom is the revision that made the per-request _meta fields
// mandatory. Named apart from the other two gates even though all three currently
// agree, so a later divergence in the spec cannot move one by editing another.
const requestMetaRequiredFrom = "2026-07-28"

// requiresRequestMeta reports whether a revision mandates the per-request _meta
// protocol fields.
func requiresRequestMeta(version string) bool {
	return atLeastRevision(version, requestMetaRequiredFrom)
}

// declaresClientCapabilities reports whether a request carried the required
// clientCapabilities field. An empty object counts: it declares that the client
// has no capabilities to offer, which is a statement. An absent field and an
// explicit null do not, since neither says anything a server could rely on, and
// the spec forbids a server relying on capabilities the client did not declare.
func declaresClientCapabilities(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var p struct {
		Meta struct {
			Capabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	raw := bytes.TrimSpace(p.Meta.Capabilities)
	return len(raw) > 0 && string(raw) != "null"
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
	if idWarning := requestIDWarning(msg); idWarning != "" {
		warning = appendWarning(warning, idWarning)
	}
	if msg.Method == "" && len(msg.ID) > 0 {
		// A result MUST carry the same id as its request, so a null one names no
		// request at all. An error response is exempt, since the spec lets it omit
		// the id "in error cases where the ID could not be read due a malformed
		// request", which is exactly what a null id says. Reported here because the
		// correlation failure downstream reads as a missing frame, which sends the
		// reader after the wrong problem.
		if len(msg.Result) > 0 && bytes.Equal(bytes.TrimSpace(msg.ID), []byte("null")) {
			warning = appendWarning(warning, "result response id is null; it names no request")
		}
		if len(msg.Result) == 0 && msg.Error == nil {
			warning = appendWarning(warning, "response has neither result nor error")
		}
		if len(msg.Result) > 0 && msg.Error != nil {
			warning = appendWarning(warning, "response has both result and error")
		}
	}
	return warning
}

func requestIDWarning(msg proxy.RPCMessage) string {
	if msg.Method == "" || len(msg.ID) == 0 {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(msg.ID))
	decoder.UseNumber()
	var id any
	if decoder.Decode(&id) != nil {
		return ""
	}
	switch value := id.(type) {
	case string:
		return ""
	case json.Number:
		// Judged on the value rather than the spelling. JSON has several ways to
		// write one integer, and JSON Schema's own "integer" matches any number
		// with a zero fractional part, so 1.0 and 1e3 are the ids 1 and 1000. A
		// client whose serializer round-trips an id through a float is conforming,
		// and refusing it would warn on correct traffic.
		if parseMCPInteger(string(value)).integer {
			return ""
		}
		return "request id " + string(value) + " is not an integer; MCP requires a string or integer id"
	case nil:
		return "request id is null; MCP requires a string or integer id"
	case bool:
		return "request id has type boolean; MCP requires a string or integer id"
	case []any:
		return "request id has type array; MCP requires a string or integer id"
	default:
		return "request id has type object; MCP requires a string or integer id"
	}
}

// resultTypeRequiredFrom is the revision that made resultType mandatory on every
// result. Named separately from routingHeadersRequiredFrom even though the two
// currently agree: that one is about HTTP headers and this one holds on every
// transport, so a later change to either must not silently move the other.
const resultTypeRequiredFrom = "2026-07-28"

// requestProtocolVersion is the revision one request declared, from the two
// places a request carries it, or "" when it carried neither.
//
// The body first, because the specification names _meta as where every request
// supplies this metadata; the MCP-Protocol-Version header second, since it is
// request scoped too and required on every Streamable HTTP request from
// 2026-07-28. There is deliberately no fall back to the session: that is the
// state the protocol forbids inferring from, and reintroducing it here would
// undo the point.
func requestProtocolVersion(params json.RawMessage, header string) string {
	if version := metaProtocolVersion(params); version != "" {
		return version
	}
	return header
}

// missingResultTypeWarning reports a result that omits the resultType field on a
// request made under a revision that requires one.
//
// Judged by the request's own declared revision, never the session's. In the
// stateless model there is no negotiated session version to speak of, and a
// server that answered a 2026-07-28 request successfully accepted that revision,
// so its result has to carry the field. An unmatched response is not judged at
// all: without its request there is nothing that says which revision it belongs
// to, and guessing is what this whole change removes.
//
// initialize stays exempt. Its version is a proposal, not an accepted revision,
// and a client may put its preferred one in the MCP-Protocol-Version header of
// that first request; a 2025-11-25 server answering it correctly would otherwise
// be told it omitted a required field, and warnings fail a default check run.
// 2026-07-28 removed the handshake outright, so a server answering one is
// speaking an earlier revision whatever the request claimed.
//
// Presence, not validity. The spec also requires that an unrecognised resultType
// be treated as invalid, which this does not check: the valid set grows with
// whatever extensions the pair negotiated, so it is its own problem with its own
// false positives. Tracked separately rather than half-done here.
func missingResultTypeWarning(msg proxy.RPCMessage, c *call) string {
	if len(msg.Result) == 0 {
		return "" // an error response carries no result to check
	}
	if c == nil || c.method == "initialize" {
		return ""
	}
	if !atLeastRevision(c.protocolVersion, resultTypeRequiredFrom) {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(msg.Result, &fields) != nil {
		return "" // not an object: validationWarning already has something to say
	}
	raw, ok := fields["resultType"]
	if !ok {
		return "result is missing required resultType"
	}
	// A key alone is not the field. resultType is a string, so null, a number and
	// an empty string are not values it can hold, and the first two are the worst
	// kind of wrong: they read as "present, fine" to a check that only looks for
	// the key. Rejecting them costs nothing, because no extension can make a
	// number valid. What is deliberately not checked is whether a non-empty
	// string is one the pair recognises, since that set grows with whatever
	// extensions they negotiated and guessing at it would invent false alarms.
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "result has an invalid resultType"
	}
	return ""
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
