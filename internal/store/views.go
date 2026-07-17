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
	ID       string
	Method   string
	ReqDir   proxy.Direction
	IsTool   bool
	ToolName string
	Params   json.RawMessage
	Result   json.RawMessage
	Err      *proxy.RPCError
	ToolErr  bool // result.isError == true
	Start    time.Time
	End      time.Time
	State    CallState
}

// Failed reports a protocol error OR a tool-level error (result.isError).
func (c CallView) Failed() bool { return c.Err != nil || c.ToolErr }

// Done reports whether a response has arrived.
func (c CallView) Done() bool { return c.State != Pending }

// Duration is the request→response latency, or elapsed-so-far if still pending.
func (c CallView) Duration() time.Duration {
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
	// RoutingMismatch is true when a routing header disagrees with the body (or is
	// present on a batch). It is a structured handle for the same condition the
	// warning describes, so filters and exporters need not match warning text.
	RoutingMismatch bool
	Call            *CallView // set for request/response events
}

// SessionHeader is a lightweight per-session summary for the left panel.
type SessionHeader struct {
	ID            string
	Label         string
	First         time.Time
	Last          time.Time
	Requests      int
	Responses     int
	Notifications int
	Errors        int
	Pending       int
	HasCaps       bool
}

// CapsView is an immutable snapshot of the negotiated capabilities.
type CapsView struct {
	ProtocolVersion string
	ClientInfo      json.RawMessage
	ServerInfo      json.RawMessage
	Client          json.RawMessage
	Server          json.RawMessage
	Instructions    string
}

// ToolStats summarizes calls to one MCP tool within a session.
type ToolStats struct {
	Name    string
	Calls   int
	Errors  int
	Pending int
	P50     time.Duration
	P95     time.Duration
	P99     time.Duration
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
		Seq:             e.seq,
		TS:              e.ts,
		Dir:             e.dir,
		Kind:            e.kind,
		Method:          e.method,
		ID:              e.id,
		Raw:             e.raw,
		Text:            e.text,
		Warning:         e.warning,
		MCPMethod:       e.mcpMethod,
		MCPName:         e.mcpName,
		RoutingMismatch: e.mismatch,
	}
	if e.call != nil {
		cv := e.call.view()
		v.Call = &cv
	}
	return v
}

func (c *call) view() CallView {
	return CallView{
		ID:       c.id,
		Method:   c.method,
		ReqDir:   c.reqDir,
		IsTool:   c.isTool,
		ToolName: c.toolName,
		Params:   c.params,
		Result:   c.result,
		Err:      c.err,
		ToolErr:  c.toolErr,
		Start:    c.start,
		End:      c.end,
		State:    c.state,
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
			ID:            sess.id,
			Label:         sess.label,
			First:         sess.first,
			Last:          sess.last,
			Requests:      sess.requests,
			Responses:     sess.responses,
			Notifications: sess.notifications,
			Errors:        sess.errors,
			Pending:       sess.pending,
			HasCaps:       sess.caps.set,
		})
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
	for _, ev := range sess.events {
		if ev.ts.Before(start) {
			continue
		}
		i := int(ev.ts.Sub(start) / step)
		if i >= n {
			i = n - 1
		}
		buckets[i]++
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
		if ev.kind == EventRequest && ev.call != nil {
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

	type aggregate struct {
		stats     ToolStats
		durations []time.Duration
	}
	byName := make(map[string]*aggregate)
	var slowest []SlowToolCall
	callIndex := 0
	for _, ev := range sess.events {
		if ev.kind != EventRequest || ev.call == nil {
			continue
		}
		c := ev.call
		index := callIndex
		callIndex++
		if !c.isTool {
			continue
		}
		agg := byName[c.toolName]
		if agg == nil {
			agg = &aggregate{stats: ToolStats{Name: c.toolName}}
			byName[c.toolName] = agg
		}
		agg.stats.Calls++
		if c.state == Pending {
			agg.stats.Pending++
			continue
		}
		if c.err != nil || c.toolErr {
			agg.stats.Errors++
		}
		duration := c.end.Sub(c.start)
		agg.durations = append(agg.durations, duration)
		slowest = append(slowest, SlowToolCall{
			CallIndex: index, ID: c.id, ToolName: c.toolName,
			Duration: duration, Failed: c.err != nil || c.toolErr, Start: c.start,
		})
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

func nearestRank(sorted []time.Duration, percentile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	return sorted[max(index, 0)]
}
