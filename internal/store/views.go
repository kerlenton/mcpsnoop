package store

import (
	"encoding/json"
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

// Slow reports whether a completed call exceeded threshold.
func (c CallView) Slow(threshold time.Duration) bool {
	return c.Done() && c.End.Sub(c.Start) > threshold
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
	Call    *CallView // set for request/response events
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
}

// view builds the snapshot for an event. Caller holds at least the read lock.
func (e *event) view(_ *session) EventView {
	v := EventView{
		Seq:     e.seq,
		TS:      e.ts,
		Dir:     e.dir,
		Kind:    e.kind,
		Method:  e.method,
		ID:      e.id,
		Raw:     e.raw,
		Text:    e.text,
		Warning: e.warning,
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
	}, true
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
