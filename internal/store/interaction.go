package store

import (
	"bytes"
	"slices"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// InteractionView is one logical operation, however many requests it took on the
// wire.
//
// Under multi round-trip requests a tool call can be several requests, each with
// its own JSON-RPC id, because the specification says the id "MUST be different
// between the initial request and the retry, as they are independent requests".
// A call with no retry is a one hop interaction, so this describes every
// operation uniformly rather than only the chained ones.
type InteractionView struct {
	// CallID is the id of the request that opened the operation, and CallSeq the
	// sequence of that frame, which is what the exporter indexes calls by.
	CallSeq  uint64
	CallID   string
	Method   string
	ToolName string
	// RoundTrips is how many requests the operation took. One means no retry.
	RoundTrips int
	// ServerTime is how long the server held the operation across every hop, and
	// ClientTurnaround the rest, which is the time between an answer arriving and
	// the client coming back, mostly a person deciding. They sum to Duration by
	// construction rather than by arithmetic anyone has to trust.
	ServerTime       time.Duration
	ClientTurnaround time.Duration
	Duration         time.Duration
	Start            time.Time
	State            CallState
	Errored          bool
	// LateResult marks a cancelled operation whose answer arrived anyway, which is
	// what makes its timing real rather than a stub.
	LateResult bool
	// Hops is the per-hop breakdown, newest last, and is built only for an
	// operation that took more than one request. A single hop's breakdown is the
	// totals above it word for word, so producing one would restate them and, on a
	// session of twenty thousand ordinary calls, allocate twenty thousand slices to
	// do it, under the read lock a live panel holds.
	//
	// It is derived from the frames still held, so a live store that has released
	// old ones reports fewer hops than RoundTrips counts. HopsComplete says which
	// case this is, because a partial breakdown that looked whole would understate
	// an operation rather than declining to describe it.
	Hops         []HopView
	HopsComplete bool
	// swapped records that this operation's request came from the server, so the
	// walk knows to swap each hop the way the totals already are.
	swapped bool
}

// Done reports whether the operation is no longer open, the same question
// CallView.Done answers about a single request.
func (i InteractionView) Done() bool { return i.State != Pending && i.State != Streaming }

// Measurable reports whether this operation has a latency worth judging, which
// is the rule --max-duration already applies to a call. An operation still open
// has none, a superseded one was never answered, and a cancelled one without a
// late result delivered nothing.
func (i InteractionView) Measurable() bool {
	return i.Done() && i.State != Superseded && (i.State != Cancelled || i.LateResult)
}

// HopView is one request and its answer inside an interaction.
type HopView struct {
	// RequestID is the JSON-RPC id of this hop, which differs from the others by
	// specification.
	RequestID  string
	RequestAt  time.Time
	ResponseAt time.Time
	// ServerTime is this hop alone, and ClientTurnaround is the wait before it,
	// zero on the first hop since nothing preceded it.
	ServerTime       time.Duration
	ClientTurnaround time.Duration
	// Asked names the server-to-client requests this hop's answer carried, sorted,
	// and is empty on a hop that finished the operation. AskedUnknown separates
	// that from a hop whose answer is no longer readable, which a live store
	// produces by releasing frame bodies to stay inside its byte budget: the frame
	// is still there and its timing is still exact, so the breakdown is complete
	// and only this one field is not.
	Asked        []string
	AskedUnknown bool
	// Pending marks a hop whose answer never arrived.
	Pending bool
}

// Interactions returns one entry per operation in the session, oldest first.
//
// The counts and the two totals come from the call, which accumulates them as
// frames arrive, so they are exact whatever the store has since released. The
// per-hop breakdown is read from the frames still held, which is the whole log
// on a batch read and a window on a live one.
func (s *Store) Interactions(sessionID string) []InteractionView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}

	// One pass, one map. Grouping the frames first and then walking each group
	// allocated a slice per call, and this runs under the read lock while the live
	// panel rebuilds on a timer, so every allocation here is one the ingest path
	// waits behind.
	out := make([]InteractionView, 0, 16)
	at := make(map[*call]int, 16)
	// lastResponse is where each entry's previous hop ended, which is what the
	// next hop's wait is measured from.
	lastResponse := make([]time.Time, 0, 16)
	for _, ev := range sess.events {
		if ev.call == nil {
			continue
		}
		i, known := at[ev.call]
		switch ev.kind {
		case EventRequest:
			if !known {
				i = len(out)
				at[ev.call] = i
				out = append(out, interactionOf(ev.call))
				lastResponse = append(lastResponse, time.Time{})
			}
			if out[i].RoundTrips < 2 {
				continue // one hop, and its breakdown is the totals already recorded
			}
			hop := HopView{RequestID: ev.id, RequestAt: ev.ts, Pending: true}
			if prev := lastResponse[i]; !prev.IsZero() && ev.ts.After(prev) {
				hop.ClientTurnaround = ev.ts.Sub(prev)
			}
			out[i].Hops = append(out[i].Hops, hop)
		case EventResponse:
			if !known || out[i].RoundTrips < 2 || len(out[i].Hops) == 0 {
				continue // the request that opened this hop is no longer held
			}
			hop := &out[i].Hops[len(out[i].Hops)-1]
			if !hop.Pending {
				continue // already answered, a duplicate or late response
			}
			hop.ResponseAt, hop.Pending = ev.ts, false
			if ev.ts.After(hop.RequestAt) {
				hop.ServerTime = ev.ts.Sub(hop.RequestAt)
			}
			hop.Asked, hop.AskedUnknown = askedOf(ev)
			lastResponse[i] = ev.ts
		}
	}

	for i := range out {
		if out[i].swapped {
			for h := range out[i].Hops {
				hop := &out[i].Hops[h]
				hop.ServerTime, hop.ClientTurnaround = hop.ClientTurnaround, hop.ServerTime
			}
		}
		// Complete means the breakdown accounts for the totals, not merely that the
		// hop count matches. Work can settle off the request and response pair a hop
		// is made of, a task handle being the plain case, and a breakdown adding up
		// to a hundred milliseconds must not describe itself as the whole of a
		// thirty second operation.
		var hopServer, hopClient time.Duration
		for _, h := range out[i].Hops {
			hopServer += h.ServerTime
			hopClient += h.ClientTurnaround
		}
		if out[i].RoundTrips < 2 {
			// Nothing to be incomplete about. The totals are the whole description of
			// an operation that took one request.
			out[i].HopsComplete = true
			continue
		}
		out[i].HopsComplete = len(out[i].Hops) == out[i].RoundTrips &&
			hopServer == out[i].ServerTime && hopClient == out[i].ClientTurnaround
	}
	return out
}

// interactionOf builds the totals of one operation from the call that carries
// them. The hops are filled in by the walk, since they come from frames.
func interactionOf(c *call) InteractionView {
	server, client := c.serverTime, c.clientTime
	swapped := c.reqDir == proxy.ServerToClient
	if swapped {
		// A request the server sent is answered by the client, so the interval
		// between them is the client's and the gap between hops is the server's.
		// Every revision from 2026-07-28 routes these through MRTR instead, so this
		// is an older capture or a deprecated shape, and reporting the client's work
		// under a field named for the server would be wrong in the one place it is
		// still possible.
		server, client = client, server
	}
	return InteractionView{
		CallSeq: c.requestSeq, CallID: c.id, Method: c.method, ToolName: c.toolName,
		RoundTrips:       c.hops,
		ServerTime:       server,
		ClientTurnaround: client,
		// The span the two intervals cover, not the clock since the request. A chain
		// nobody finished has an open interval that grows with the current time, and
		// reporting that would make the total disagree with its own parts.
		Duration: server + client,
		Start:    c.start, State: c.state, Errored: c.errored, LateResult: c.lateResult,
		swapped: swapped,
	}
}

// askedOf names what an answer asked the client for, sorted so two captures
// compare. unknown is true when the frame's body is no longer held, which a live
// store does to stay inside its byte budget, so a hop whose answer cannot be read
// is told apart from one that asked for nothing.
func askedOf(ev *event) (asked []string, unknown bool) {
	if ev.bodyReleased {
		return nil, true
	}
	if len(ev.raw) == 0 {
		return nil, false
	}
	// A substring scan before a decode. Only an InputRequiredResult can name
	// anything here, and its resultType is that literal, so a frame without those
	// bytes cannot be one. Decoding every response instead cost a full unmarshal of
	// each body, which on a live session of forty thousand frames took longer than
	// the interval the panel refreshes on, so the panel could never keep up and
	// held the store's read lock while it failed to. A false positive costs one
	// decode that correctly answers nothing.
	if !bytes.Contains(ev.raw, []byte(`"input_required"`)) {
		return nil, false
	}
	msg, ok := proxy.ParseRPC(ev.raw)
	if !ok {
		return nil, false
	}
	state, ok := parseInputRequired(msg.Result)
	if !ok || len(state.methods) == 0 {
		return nil, false
	}
	return slices.Clone(state.methods), false
}
