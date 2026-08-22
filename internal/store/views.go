package store

import (
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// CallView is an immutable snapshot of a correlated request/response pair.
type CallView struct {
	RequestSeq uint64
	ID         string
	Method     string
	ReqDir     proxy.Direction
	IsTool     bool
	ToolName   string
	Params     json.RawMessage
	Result     json.RawMessage
	Err        *proxy.RPCError
	ToolErr    bool // result.isError == true
	// Errored is the "something went wrong" axis, distinct from Failed(): a
	// cancelled task is Failed() (it delivered nothing) but not Errored (the user
	// stopped it on purpose). The session error counter and the CI gate read this.
	Errored      bool
	Start        time.Time
	End          time.Time
	State        CallState
	CancelledAt  time.Time
	CancelReason string
	LateResult   bool
	TaskID       string
	TaskStatus   string
}

// Failed reports a protocol error, a tool-level error (result.isError), or any
// call the store already settled as Failed. The last clause matters for a task
// that ends in a terminal failure carrying no error object, where the state would
// otherwise say Failed while this predicate said otherwise, and everything that
// judges an outcome through here (the exporter, the stream, status:err) reads it.
// On the synchronous path it changes nothing, since Failed is only ever set there
// alongside an error or a tool error.
func (c CallView) Failed() bool { return c.Err != nil || c.ToolErr || c.State == Failed }

// Done reports whether the call is no longer open.
func (c CallView) Done() bool { return c.State != Pending && c.State != Streaming }

// Duration is the request-to-response latency, zero for a settled call without a
// response, or elapsed time for a call that is still open.
func (c CallView) Duration() time.Duration {
	if c.State == Cancelled && !c.LateResult {
		return 0
	}
	if c.Done() {
		return c.End.Sub(c.Start)
	}
	return time.Since(c.Start)
}

// EventView is an immutable snapshot of one timeline entry.
type EventView struct {
	Seq     uint64
	TS      time.Time
	Dir     proxy.Direction
	Kind    EventKind
	Method  string
	ID      string
	Raw     json.RawMessage
	Text    string
	Warning string
	// MCPMethod and MCPName are the HTTP routing headers (Mcp-Method, Mcp-Name),
	// set on request frames captured over the streamable-HTTP transport.
	MCPMethod string
	MCPName   string
	// MCPProtocolVersion is the MCP-Protocol-Version request header.
	MCPProtocolVersion string
	// MCPParamHeaders are the captured Mcp-Param-* request headers in stable,
	// case-insensitive field-name order.
	MCPParamHeaders []proxy.MCPParamHeader
	// HTTPStatus is the status of the response this frame arrived on, zero on
	// stdio and on client frames. AuthChallenge is that response's
	// WWW-Authenticate header.
	HTTPStatus    int
	AuthChallenge string
	// RoutingMismatch is true when a routing header (Mcp-Method/Mcp-Name or an
	// Mcp-Param-* value) or the MCP-Protocol-Version header disagrees with the body,
	// or a routing header rides a batch, or a required one is missing. It is also
	// true on a server's -32020
	// response, which is that same condition as the server saw it and remains
	// useful when no matching tool definition was observed. A structured handle
	// for the condition the warning describes, so
	// filters and exporters need not match warning text.
	RoutingMismatch bool
	// Truncated is true when mcpsnoop capped its own observed copy of a large body.
	// It is a structured marker, not a protocol warning, so it never fails check.
	Truncated bool
	// Deprecated carries a heads-up when a frame uses a feature deprecated in the
	// 2026-07-28 MCP release. It is structured like Truncated and never fails check.
	Deprecated string
	// CacheHint surfaces ttlMs and cacheScope from a cacheable result (SEP-2549).
	CacheHint CacheHint
	// CacheStaleRefetch is set when a client re-fetches inside a declared ttlMs
	// window. Structured like Deprecated and never fails check.
	CacheStaleRefetch string
	Observation       string
	Call              *CallView // set for request/response events
	TaskCall          *CallView // originating call for a tasks/* lifecycle frame
	TaskID            string
	// Errored marks a frame this session counted an error on, so a reporter can
	// name the frames the Errors total came from. It is the frame to point a
	// reader at: a transport failure and an unmatched error response have no
	// Call, and a task failure lands on its terminal frame while Call.Errored,
	// read off a live call, would retroactively mark that call's first response
	// instead. Every counted error marks a frame; one frame that raises the count
	// twice still marks once, so the marked frames are where the count came from
	// rather than a running tally of it.
	Errored bool
	// LateResult marks the frame this session counted a late result on, one per
	// LateResults. Call.LateResult says the call ever got one, which stays true
	// for the rest of the capture and is visible from the request frame too.
	LateResult bool
	// BodyReleased is true when a bounded store dropped this frame's observed
	// body to stay inside its memory budget. Everything else about the frame is
	// still here, so it keeps its row and its verdict; only Raw and Text are gone
	// and the log on disk still holds them.
	BodyReleased bool
	// MRTRRoot is the id of the request this one continues, set when a multi
	// round-trip retry was recognised (SEP-2322). Empty on an ordinary request.
	MRTRRoot string
	// MRTRStateIssue classifies a changed, missing, or invented requestState on
	// this retry. It never contains the opaque state itself.
	MRTRStateIssue MRTRStateIssue
}

// SessionHeader is a lightweight per-session summary for the left panel.
type SessionHeader struct {
	ID    string
	Label string
	// Transport is the channel the session was captured on, "stdio" or "http",
	// and is empty only for a log whose frames named none. Endpoint is the MCP
	// endpoint of an HTTP session as EndpointForLog wrote it down, which is
	// stripped of userinfo, query values and the fragment, so it identifies the
	// server without being an address to dial. Empty on stdio, and empty on an
	// HTTP log captured before mcpsnoop recorded it.
	Transport            string
	Endpoint             string
	First                time.Time
	Last                 time.Time
	Requests             int
	Responses            int
	Notifications        int
	Errors               int
	Pending              int
	HasCaps              bool
	HasToolDrift         bool
	HasToolBaselineError bool
	MissingFrames        uint64 // envelopes dropped upstream, inferred from Seq gaps
	// DroppedFromMemory counts frames a bounded live store removed from the
	// timeline to stay inside its budget. Unlike MissingFrames these were
	// observed and are still in the log on disk, so the capture is complete and
	// only this view of it is not.
	DroppedFromMemory int
	// RetiredExchanges counts multi round-trip operations dropped at the parking
	// cap while still open. MRTR tells servers not to assume a client will ever
	// retry, so abandoned exchanges accumulate with no frame to settle them, and
	// the cap is what stops the list growing for the life of the session. A
	// non-zero count means a retry arriving for one of those would no longer link,
	// so the operation would read as two calls with two durations.
	RetiredExchanges int
	LateResults      int
}

// ToolDefinition is the contract a server advertised for one MCP tool.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// Title is the display name the spec gives precedence over annotations.title
	// and over Name, so a change here changes what the user is shown. OutputSchema
	// is the contract for structuredContent, Annotations carries the behaviour
	// hints a client is told to treat as untrusted, and Icons is what a UI renders
	// beside the tool. All three stay raw, since drift compares them as JSON and
	// re-encoding a decoded form cannot reproduce what the server sent.
	Title        string
	OutputSchema json.RawMessage
	Annotations  json.RawMessage
	Icons        json.RawMessage
	Findings     []SchemaFinding
	paramHeaders []paramHeaderBinding
	// Cost is what advertising this tool weighs, measured once at ingest.
	Cost ToolCost
}

// ToolCost is the measured weight of one advertised tool definition, in bytes.
// Bytes and not tokens: a token count is model specific, so measuring one would
// mean shipping a tokeniser and choosing whose. Bytes are exact, stable across
// captures, and let a reader apply whatever ratio their own model implies. Every
// figure is measured on one basis, the JSON with insignificant whitespace
// removed, so two servers stay comparable regardless of how either formats its
// tools/list.
type ToolCost struct {
	// Name duplicates ToolDefinition.Name so a cost can travel on its own, into
	// a sorted breakdown or an export, without carrying the whole definition.
	Name string
	// Bytes is the definition object with insignificant whitespace removed, so it
	// covers the name, description, schema, any annotations, and the JSON around
	// them, and stays comparable between servers. This is the number that sums to
	// the fixed cost of the tool list.
	Bytes int
	// DescriptionBytes is the description JSON encoded, quotes and escaping
	// included, and SchemaBytes is the inputSchema with whitespace removed. Both
	// are measured on the same basis as Bytes and point at where it went, but
	// deliberately do not sum to it: the remainder is the name, the annotations
	// and the structural punctuation, and pretending otherwise would invite
	// reading the gap as a bug.
	DescriptionBytes int
	SchemaBytes      int
	// FindingKinds lists the schema finding kinds detected on this tool, keyed
	// structurally for export and check rather than by display text.
	FindingKinds []SchemaFindingKind
}

// ToolListCost is the fixed context cost of a session's advertised tool list,
// the price paid on every conversation with that server before a single call is
// made. PerTool is sorted heaviest first, since the question a reader has is
// which two or three tools account for most of it.
type ToolListCost struct {
	Tools   int
	Bytes   int
	PerTool []ToolCost
	// Complete is false while tools/list is still paginating, or never finished.
	// Bytes then covers only the pages seen, which makes it a floor rather than
	// the total, and it must not be presented as one.
	Complete bool
}

// ToolDriftKind names one way an advertised tool definition can differ from the
// trusted baseline. Every field the spec lets a server change after approval
// gets its own kind, because "something changed" is not actionable and the kinds
// carry very different weight: a description edit is routine, a readOnlyHint
// flipping to destructive is the rug-pull this feature exists for.
type ToolDriftKind string

const (
	DriftToolAdded    ToolDriftKind = "added"
	DriftToolRemoved  ToolDriftKind = "removed"
	DriftDescription  ToolDriftKind = "description"
	DriftInputSchema  ToolDriftKind = "input_schema"
	DriftTitle        ToolDriftKind = "title"
	DriftOutputSchema ToolDriftKind = "output_schema"
	DriftAnnotations  ToolDriftKind = "annotations"
	DriftIcons        ToolDriftKind = "icons"
)

// ToolDriftKinds is every kind in report order, so a renderer never has to
// enumerate them by hand. Adding a kind here reaches Count, the clone, both
// renderers and the CI gate at once; a hand-written list that missed one would
// leave real drift detected and never shown.
var ToolDriftKinds = []ToolDriftKind{
	DriftToolAdded,
	DriftToolRemoved,
	DriftDescription,
	DriftInputSchema,
	DriftTitle,
	DriftOutputSchema,
	DriftAnnotations,
	DriftIcons,
}

// Label is the phrase a report uses for this kind, written so it reads after a
// count ("2 tools removed").
func (k ToolDriftKind) Label() string {
	switch k {
	case DriftToolAdded:
		return "tools added"
	case DriftToolRemoved:
		return "tools removed"
	case DriftDescription:
		return "descriptions changed"
	case DriftInputSchema:
		return "input schemas changed"
	case DriftTitle:
		return "titles changed"
	case DriftOutputSchema:
		return "output schemas changed"
	case DriftAnnotations:
		return "annotations changed"
	case DriftIcons:
		return "icons changed"
	}
	return string(k)
}

// ToolDrift is the difference between the current complete tool list and the
// persisted trust-on-first-use baseline for the session's server label.
type ToolDrift struct {
	// Changes maps a kind to the tool names it applies to, sorted.
	Changes map[ToolDriftKind][]string
	// Unverified names the kinds this baseline could not answer, because it was
	// recorded before mcpsnoop tracked them. It is deliberately not drift and
	// deliberately not an error: the tools may be fine, and we simply have no
	// record to compare against. Empty() ignores it, so an older baseline does
	// not fail a drift-gated run over our own upgrade.
	Unverified    []ToolDriftKind
	BaselineError string
}

func (d ToolDrift) Empty() bool { return d.Count() == 0 && d.BaselineError == "" }

func (d ToolDrift) Count() int {
	total := 0
	for _, names := range d.Changes {
		total += len(names)
	}
	return total
}

// Names returns the tools affected by one kind, sorted, or nil for none.
func (d ToolDrift) Names(kind ToolDriftKind) []string { return d.Changes[kind] }

// Add records that a tool differs in one respect. It is the only writer, so the
// map is never nil for a caller that used it.
func (d *ToolDrift) Add(kind ToolDriftKind, tool string) {
	if d.Changes == nil {
		d.Changes = make(map[ToolDriftKind][]string, len(ToolDriftKinds))
	}
	d.Changes[kind] = append(d.Changes[kind], tool)
}

// CapsView is an immutable snapshot of the negotiated capabilities.
type CapsView struct {
	ProtocolVersion string
	ClientInfo      json.RawMessage
	ServerInfo      json.RawMessage
	Client          json.RawMessage
	Server          json.RawMessage
	Instructions    string
	// Extensions is the union of the extension ids each side advertised in its
	// capabilities `extensions` map (SEP-2133), sorted by id and carrying which
	// sides advertised each. Nil when neither side advertised any, so a session
	// without extensions is indistinguishable from one before the field existed.
	Extensions []ExtensionView
}

// ExtensionView is one extension id and which sides advertised it. Extensions
// are negotiated through a map on both ClientCapabilities and
// ServerCapabilities, so presence on one side alone means nothing is in play.
type ExtensionView struct {
	ID     string
	Client bool
	Server bool
}

// Agreed reports whether both sides advertised the extension, which is the only
// case in which it is actually in play.
func (e ExtensionView) Agreed() bool { return e.Client && e.Server }

// ToolStats summarizes calls to one MCP tool within a session.
type ToolStats struct {
	Name  string
	Calls int
	// Errors is every call that went wrong, and ProtocolErrors and ToolErrors
	// split it into the two things that word covers. They are very different
	// findings. A tool that answers isError is doing its job, reporting that the
	// search found nothing or the input failed validation, and a server returning
	// a JSON-RPC error is broken. Reading one number for both means a well-behaved
	// tool that reports domain failures looks exactly like a broken server, and in
	// the summary it also sorts above one.
	//
	// ToolErrors counts result.isError. ProtocolErrors counts everything else that
	// was an error, which is a JSON-RPC error object and a task that ended failed
	// without one. It is deliberately the complement rather than its own tally, so
	// the two always add up to Errors and a failure mode added later lands in it
	// instead of disappearing from both.
	Errors         int
	ProtocolErrors int
	ToolErrors     int
	Pending        int
	P50            time.Duration
	P95            time.Duration
	P99            time.Duration
	// ResultBytes totals this tool's results and MaxResultBytes is the largest
	// single one, both as observed on the wire. Unlike the definition cost this
	// half is paid per call, so a tool that is cheap to advertise can still be
	// the expensive one. A call answered with a JSON-RPC error carries no result
	// and so contributes nothing; a tool-level failure (result.isError) does.
	//
	// These are the bytes as they arrived, not normalised the way ToolCost is.
	// A definition is a stable contract worth measuring canonically so two
	// captures compare; a result is a one-off payload, and compacting every one
	// of them to count it would allocate a second copy of a megabyte answer on
	// a path that already holds the write lock.
	ResultBytes    int64
	MaxResultBytes int
}

// SlowToolCall identifies one of a session's slowest completed tool calls.
type SlowToolCall struct {
	CallIndex int
	ID        string
	ToolName  string
	Duration  time.Duration
	Failed    bool
	Start     time.Time
}

// SessionToolSummary is the aggregate tool activity for one session.
type SessionToolSummary struct {
	Tools   []ToolStats
	Slowest []SlowToolCall
}

// view builds the snapshot for an event. Caller holds at least the read lock.
func (e *event) view(_ *session) EventView {
	v := EventView{
		Seq:                e.seq,
		TS:                 e.ts,
		Dir:                e.dir,
		Kind:               e.kind,
		Method:             e.method,
		ID:                 e.id,
		Raw:                e.raw,
		Text:               e.text,
		Warning:            e.warning,
		MCPMethod:          e.mcpMethod,
		MCPName:            e.mcpName,
		MCPProtocolVersion: e.mcpProtocolVersion,
		MCPParamHeaders:    slices.Clone(e.mcpParamHeaders),
		HTTPStatus:         e.status,
		AuthChallenge:      e.authChallenge,
		RoutingMismatch:    e.mismatch,
		Truncated:          e.truncated,
		Deprecated:         e.deprecated,
		CacheHint:          e.cacheHint,
		CacheStaleRefetch:  e.cacheStaleRefetch,
		Observation:        e.observation,
		TaskID:             e.taskID,
		MRTRRoot:           e.mrtrRoot,
		MRTRStateIssue:     e.mrtrStateIssue,
		Errored:            e.errored,
		LateResult:         e.lateResult,
		BodyReleased:       e.bodyReleased,
	}
	if e.call != nil {
		cv := e.call.view()
		v.Call = &cv
	}
	if e.taskCall != nil {
		cv := e.taskCall.view()
		v.TaskCall = &cv
	}
	return v
}

func (c *call) view() CallView {
	return CallView{
		RequestSeq:   c.requestSeq,
		ID:           c.id,
		Method:       c.method,
		ReqDir:       c.reqDir,
		IsTool:       c.isTool,
		ToolName:     c.toolName,
		Params:       c.params,
		Result:       c.result,
		Err:          c.err,
		ToolErr:      c.toolErr,
		Errored:      c.errored,
		Start:        c.start,
		End:          c.end,
		State:        c.state,
		CancelledAt:  c.cancelledAt,
		CancelReason: c.cancelReason,
		LateResult:   c.lateResult,
		TaskID:       c.taskID,
		TaskStatus:   c.taskStatus,
	}
}

// Sessions returns per-session headers in first-seen order.
func (s *Store) Sessions() []SessionHeader {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SessionHeader, 0, len(s.order))
	for _, id := range s.order {
		sess := s.sessions[id]
		out = append(out, SessionHeader{
			ID:                   sess.id,
			Label:                sess.label,
			Transport:            sess.transport,
			Endpoint:             sess.target,
			First:                sess.first,
			Last:                 sess.last,
			Requests:             sess.requests,
			Responses:            sess.responses,
			Notifications:        sess.notifications,
			Errors:               sess.errors,
			Pending:              sess.pending,
			HasCaps:              sess.caps.set,
			HasToolDrift:         sess.toolDrift.Count() > 0,
			HasToolBaselineError: sess.toolDrift.BaselineError != "",
			MissingFrames:        sess.missing,
			DroppedFromMemory:    sess.dropped,
			RetiredExchanges:     sess.retiredExchanges,
			LateResults:          sess.lateResults,
		})
	}
	return out
}

// ToolDefinitions returns the current complete tools/list definition set in
// server order. ok is false until the final page of a listing has arrived.
func (s *Store) ToolDefinitions(sessionID string) ([]ToolDefinition, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok || !sess.toolListComplete {
		return nil, false
	}
	definitions := make([]ToolDefinition, 0, len(sess.advertisedTools))
	for _, name := range sess.advertisedTools {
		definition := sess.toolDefinitions[name]
		definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
		definition.OutputSchema = append(json.RawMessage(nil), definition.OutputSchema...)
		definition.Annotations = append(json.RawMessage(nil), definition.Annotations...)
		definition.Icons = append(json.RawMessage(nil), definition.Icons...)
		definition.Findings = slices.Clone(definition.Findings)
		definitions = append(definitions, definition)
	}
	return definitions, true
}

// ToolCosts reports the fixed context cost of a session's advertised tool list.
//
// Unlike ToolDefinitions this does not require a complete list. A baseline may
// only be trusted once the whole list has been seen, but a partial list still
// has a real per-tool cost worth showing, and the caller is told through
// Complete that the total is a floor. ok is false only when the session never
// carried a tools/list response at all, which is a different thing from a
// server that advertises no tools.
func (s *Store) ToolCosts(sessionID string) (ToolListCost, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok || !sess.toolListSeen {
		return ToolListCost{}, false
	}
	cost := ToolListCost{
		Tools:    len(sess.advertisedTools),
		Complete: sess.toolListComplete,
		PerTool:  make([]ToolCost, 0, len(sess.advertisedTools)),
	}
	for _, name := range sess.advertisedTools {
		definition := sess.toolDefinitions[name]
		cost.Bytes += definition.Cost.Bytes
		cost.PerTool = append(cost.PerTool, definition.Cost)
	}
	// Heaviest first, then by name for a stable order among equals. Alphabetical
	// would bury the answer, which is which tools the cost is actually in.
	slices.SortFunc(cost.PerTool, func(a, b ToolCost) int {
		if c := cmp.Compare(b.Bytes, a.Bytes); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return cost, true
}

// SetToolDrift attaches the current baseline comparison to a session.
func (s *Store) SetToolDrift(sessionID string, drift ToolDrift) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.toolDrift = drift
		sess.toolDriftSet = true
	}
}

// ToolDrift returns the current baseline comparison for a session.
func (s *Store) ToolDrift(sessionID string) (ToolDrift, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok || !sess.toolDriftSet {
		return ToolDrift{}, false
	}
	return cloneToolDrift(sess.toolDrift), true
}

// SchemaReport groups tool names by observational schema finding kind. It
// includes every tool advertised so far, even when tools/list is still
// paginating, so a violation on an early page is not lost.
type SchemaReport struct {
	// ByKind maps an observational finding kind to the tools that carry it.
	ByKind map[SchemaFindingKind][]string
}

func (r SchemaReport) Empty() bool {
	return len(r.ByKind) == 0
}

func (r SchemaReport) Count() int {
	n := 0
	for _, names := range r.ByKind {
		n += len(names)
	}
	return n
}

func (r SchemaReport) Names(kind SchemaFindingKind) []string {
	if r.ByKind == nil {
		return nil
	}
	return slices.Clone(r.ByKind[kind])
}

// SchemaFindings returns observational schema findings for every tool seen in
// the session so far. ok is false when the session never carried a tools/list
// response.
func (s *Store) SchemaFindings(sessionID string) (SchemaReport, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok || !sess.toolListSeen {
		return SchemaReport{}, false
	}
	report := SchemaReport{ByKind: make(map[SchemaFindingKind][]string)}
	for _, name := range sess.advertisedTools {
		definition := sess.toolDefinitions[name]
		for _, f := range definition.Findings {
			if !IsObservationalSchemaKind(f.Kind) {
				continue
			}
			report.ByKind[f.Kind] = append(report.ByKind[f.Kind], name)
		}
	}
	return report, true
}

// cloneToolDrift deep-copies a report for a reader. It walks the map rather than
// naming kinds, so a kind added later cannot be silently dropped here while
// Count still sees it, which would light the drift marker over a panel saying
// nothing is wrong.
func cloneToolDrift(drift ToolDrift) ToolDrift {
	out := ToolDrift{
		Unverified:    slices.Clone(drift.Unverified),
		BaselineError: drift.BaselineError,
	}
	if drift.Changes != nil {
		out.Changes = make(map[ToolDriftKind][]string, len(drift.Changes))
		for kind, names := range drift.Changes {
			out.Changes[kind] = slices.Clone(names)
		}
	}
	return out
}

// Timeline returns a snapshot of a session's events, oldest first. A nil slice
// is returned for an unknown session.
func (s *Store) Timeline(sessionID string) []EventView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	out := make([]EventView, 0, len(sess.events))
	for _, ev := range sess.events {
		out = append(out, ev.view(sess))
	}
	return out
}

// Capabilities returns the negotiated capabilities for a session.
func (s *Store) Capabilities(sessionID string) (CapsView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok || !sess.caps.set {
		return CapsView{}, false
	}
	return CapsView{
		ProtocolVersion: sess.caps.protocolVersion,
		ClientInfo:      sess.caps.clientInfo,
		ServerInfo:      sess.caps.serverInfo,
		Client:          sess.caps.client,
		Server:          sess.caps.server,
		Instructions:    sess.caps.instructions,
		Extensions: mergeExtensions(
			extensionIDs(sess.caps.client),
			extensionIDs(sess.caps.server),
		),
	}, true
}

// ToolUsage reports which advertised tools were called during the session.
func (s *Store) ToolUsage(sessionID string) (used, unused, unadvertised []string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, found := s.sessions[sessionID]
	if !found {
		return nil, nil, nil, false
	}

	called := make(map[string]bool)
	for _, c := range sess.calls {
		if c.isTool && c.toolName != "" {
			called[c.toolName] = true
		}
	}

	for _, name := range sess.advertisedTools {
		if called[name] {
			used = append(used, name)
		} else {
			unused = append(unused, name)
		}
	}

	for name := range called {
		if _, ok := sess.advertisedSet[name]; !ok {
			unadvertised = append(unadvertised, name)
		}
	}
	sort.Strings(unadvertised)

	if len(sess.advertisedTools) == 0 && len(unadvertised) == 0 {
		return nil, nil, nil, false
	}
	return used, unused, unadvertised, true
}

// Command returns the wrapped server command for a session (from the meta
// frame), used to replay against an isolated copy. ok is false if unknown.
func (s *Store) Command(sessionID string) (command []string, cwd string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, found := s.sessions[sessionID]
	if !found || len(sess.command) == 0 {
		return nil, "", false
	}
	return append([]string(nil), sess.command...), sess.cwd, true
}

// Activity buckets the session's frame timestamps into n equal windows over the
// last span, oldest window first, for the sessions activity sparkline.
func (s *Store) Activity(sessionID string, n int, span time.Duration) []int {
	buckets := make([]int, max(n, 0))
	if n <= 0 {
		return buckets
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return buckets
	}
	start := time.Now().Add(-span)
	step := span / time.Duration(n)
	// Events are in arrival (ascending time) order, so walk back from the newest and
	// stop at the first one older than the window, instead of scanning the whole
	// session on every refresh.
	for i := len(sess.events) - 1; i >= 0; i-- {
		ev := sess.events[i]
		if ev.ts.Before(start) {
			break
		}
		b := int(ev.ts.Sub(start) / step)
		if b >= n {
			b = n - 1
		}
		buckets[b]++
	}
	return buckets
}

// Calls returns all correlated calls for a session in request order.
func (s *Store) Calls(sessionID string) []CallView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	out := make([]CallView, 0, len(sess.events))
	for _, ev := range sess.events {
		// A multi round-trip retry points at the call it continues, so counting it
		// again here would report one logical operation as several and feed its
		// duration into the statistics twice.
		if ev.kind == EventRequest && ev.call != nil && ev.mrtrRoot == "" {
			out = append(out, ev.call.view())
		}
	}
	return out
}

// ToolSummary returns per-tool latency and error statistics plus the five
// slowest completed tool calls. Pending calls are counted but have no latency.
func (s *Store) ToolSummary(sessionID string) (SessionToolSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return SessionToolSummary{}, false
	}

	// Seeded from whatever a bounded store already released, so the statistics
	// describe every tool call the session made rather than only the calls whose
	// frames are still held.
	byName, slowest, callIndex := sess.toolStats.clone()
	for _, ev := range sess.events {
		foldToolCall(byName, &slowest, &callIndex, ev)
	}
	tools := make([]ToolStats, 0, len(byName))
	for _, agg := range byName {
		slices.Sort(agg.durations)
		agg.stats.P50 = nearestRank(agg.durations, 0.50)
		agg.stats.P95 = nearestRank(agg.durations, 0.95)
		agg.stats.P99 = nearestRank(agg.durations, 0.99)
		tools = append(tools, agg.stats)
	}
	slices.SortFunc(tools, func(a, b ToolStats) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortStableFunc(slowest, func(a, b SlowToolCall) int {
		if c := cmp.Compare(b.Duration, a.Duration); c != 0 {
			return c
		}
		return a.Start.Compare(b.Start)
	})
	if len(slowest) > 5 {
		slowest = slowest[:5]
	}
	return SessionToolSummary{Tools: tools, Slowest: slowest}, true
}

// toolAggregate is one tool's running statistics. durations are kept whole
// rather than summarised, because the percentiles are exact and eight bytes a
// call is nothing beside the frame it came from.
type toolAggregate struct {
	stats     ToolStats
	durations []time.Duration
}

// toolStats accumulates the calls a bounded store has already dropped the frames
// for. An unbounded store never writes to it, so its summary is built exactly as
// it always was.
type toolStats struct {
	byName    map[string]*toolAggregate
	slowest   []SlowToolCall
	callIndex int
}

func (t toolStats) clone() (map[string]*toolAggregate, []SlowToolCall, int) {
	byName := make(map[string]*toolAggregate, len(t.byName))
	for name, agg := range t.byName {
		byName[name] = &toolAggregate{stats: agg.stats, durations: slices.Clone(agg.durations)}
	}
	return byName, slices.Clone(t.slowest), t.callIndex
}

// fold adds one call to the accumulator, through the same arithmetic the summary
// uses, so a released call counts exactly as it would have while it was held.
func (t *toolStats) fold(ev *event) {
	if t.byName == nil {
		t.byName = make(map[string]*toolAggregate)
	}
	foldToolCall(t.byName, &t.slowest, &t.callIndex, ev)
	if len(t.slowest) > slowestKept {
		slices.SortStableFunc(t.slowest, func(a, b SlowToolCall) int {
			if c := cmp.Compare(b.Duration, a.Duration); c != 0 {
				return c
			}
			return a.Start.Compare(b.Start)
		})
		t.slowest = t.slowest[:slowestKept]
	}
}

// slowestKept is how many slow calls survive in the accumulator. The summary
// shows five, and keeping only those five would be wrong the moment a sixth
// released call was slower than one already dropped, so a margin is held.
const slowestKept = 64

// foldToolCall is the one place a call turns into statistics. ToolSummary walks
// the frames it still holds through it and a bounded store walks the frames it
// is about to drop through it, so the two can never drift.
func foldToolCall(byName map[string]*toolAggregate, slowest *[]SlowToolCall, callIndex *int, ev *event) {
	// Skipping a continuation matters twice over: it would count one logical
	// operation as several calls, and feed the same duration into the percentiles
	// once per round trip.
	if ev.kind != EventRequest || ev.call == nil || ev.mrtrRoot != "" {
		return
	}
	c := ev.call
	index := *callIndex
	*callIndex++
	if !c.isTool {
		return
	}
	agg := byName[c.toolName]
	if agg == nil {
		agg = &toolAggregate{stats: ToolStats{Name: c.toolName}}
		byName[c.toolName] = agg
	}
	agg.stats.Calls++
	if c.state == Pending {
		agg.stats.Pending++
		return
	}
	if c.state == Superseded || (c.state == Cancelled && !c.lateResult) {
		return
	}
	if c.errored {
		agg.stats.Errors++
		if c.toolErr {
			agg.stats.ToolErrors++
		} else {
			agg.stats.ProtocolErrors++
		}
	}
	// max, not len, so a result whose bytes a bounded store released still counts
	// what it cost. This figure is the whole point of the cost panel.
	if n := max(len(c.result), c.releasedResultBytes); n > 0 {
		agg.stats.ResultBytes += int64(n)
		agg.stats.MaxResultBytes = max(agg.stats.MaxResultBytes, n)
	}
	duration := c.end.Sub(c.start)
	agg.durations = append(agg.durations, duration)
	*slowest = append(*slowest, SlowToolCall{
		CallIndex: index, ID: c.id, ToolName: c.toolName,
		Duration: duration, Failed: c.errored, Start: c.start,
	})
}

func nearestRank(sorted []time.Duration, percentile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	return sorted[max(index, 0)]
}

// Elicitations returns every question this session's servers put to the user,
// oldest first, each paired with the answer that came back.
//
// Rounds are read from the calls rather than from the timeline, because a chain
// folds into one call and the timeline no longer holds the interim results: the
// retry overwrites them. Ordering is by when the question was asked, which is
// the order a reader watched them happen.
func (s *Store) Elicitations(sessionID string) []ElicitationView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	var out []ElicitationView
	seen := make(map[*call]struct{})
	for _, ev := range sess.events {
		// Walked through the timeline so the calls arrive in the order they opened,
		// and deduplicated because a chain maps several request frames onto one call.
		if ev.kind != EventRequest || ev.call == nil {
			continue
		}
		if _, done := seen[ev.call]; done {
			continue
		}
		seen[ev.call] = struct{}{}
		for _, el := range ev.call.elicitations {
			out = append(out, el.view(ev.call))
		}
	}
	slices.SortStableFunc(out, func(a, b ElicitationView) int { return a.Asked.Compare(b.Asked) })
	return out
}
