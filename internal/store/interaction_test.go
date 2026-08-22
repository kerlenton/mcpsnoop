package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// mrtrFixture is the capture from issue #201: one book_flight chain of three
// hops where the server works 0.4s, 0.3s and 0.5s while the user takes 12s and
// then 25s, followed by an ordinary fast call for contrast.
func mrtrFixture(t *testing.T, s *Store, upTo int) {
	t.Helper()
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight","arguments":{"to":"JFK"}}}`},
		{proxy.ServerToClient, 400, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"confirm":{"method":"elicitation/create","params":{"message":"Confirm $840?"}}}}}`},
		{proxy.ClientToServer, 12400, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","arguments":{"to":"JFK"},"requestState":"st-1","inputResponses":{"confirm":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 12700, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"input_required","requestState":"st-2","inputRequests":{"seat":{"method":"elicitation/create","params":{"message":"Window or aisle?"}}}}}`},
		{proxy.ClientToServer, 37700, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"book_flight","arguments":{"to":"JFK"},"requestState":"st-2","inputResponses":{"seat":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 38200, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"booked"}]}}`},
		{proxy.ClientToServer, 39000, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lookup_price"}}`},
		{proxy.ServerToClient, 39200, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`},
	}
	if upTo <= 0 || upTo > len(frames) {
		upTo = len(frames)
	}
	for i, f := range frames[:upTo] {
		s.Ingest(proxy.Envelope{SessionID: "demo", ServerLabel: "booking", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f.raw)})
	}
}

func interactionFor(t *testing.T, s *Store, tool string) InteractionView {
	t.Helper()
	for _, in := range s.Interactions("demo") {
		if in.ToolName == tool {
			return in
		}
	}
	t.Fatalf("no interaction for %q in %+v", tool, s.Interactions("demo"))
	return InteractionView{}
}

// TestInteractionsSplitServerTimeFromHumanTime is the whole feature, on the
// capture the issue is written around. The server spent 1.2s and the user spent
// 37s, and until now both landed in one 38.2s scalar.
func TestInteractionsSplitServerTimeFromHumanTime(t *testing.T) {
	s := New()
	mrtrFixture(t, s, 0)

	book := interactionFor(t, s, "book_flight")
	if book.RoundTrips != 3 {
		t.Fatalf("round trips = %d, want 3", book.RoundTrips)
	}
	if book.ServerTime != 1200*time.Millisecond {
		t.Fatalf("server time = %v, want 1.2s", book.ServerTime)
	}
	if book.ClientTurnaround != 37*time.Second {
		t.Fatalf("client turnaround = %v, want 37s", book.ClientTurnaround)
	}

	quick := interactionFor(t, s, "lookup_price")
	if quick.RoundTrips != 1 || quick.ServerTime != 200*time.Millisecond || quick.ClientTurnaround != 0 {
		t.Fatalf("an ordinary call = %d trips, %v server, %v client; want 1, 200ms, 0",
			quick.RoundTrips, quick.ServerTime, quick.ClientTurnaround)
	}
}

// TestTheTwoSharesAlwaysAddUp is the invariant that makes the split trustworthy.
// It holds by construction rather than by arithmetic anyone has to check, since
// the intervals telescope from the first request to the last response.
func TestTheTwoSharesAlwaysAddUp(t *testing.T) {
	for _, upTo := range []int{2, 4, 6, 8} {
		t.Run(fmt.Sprintf("through frame %d", upTo), func(t *testing.T) {
			s := New()
			mrtrFixture(t, s, upTo)
			for _, in := range s.Interactions("demo") {
				if got := in.ServerTime + in.ClientTurnaround; got != in.Duration {
					t.Fatalf("%s: %v server + %v client = %v, want the %v duration",
						in.ToolName, in.ServerTime, in.ClientTurnaround, got, in.Duration)
				}
			}
		})
	}
}

// TestEveryHopIsMeasuredSeparately covers the breakdown underneath the totals.
func TestEveryHopIsMeasuredSeparately(t *testing.T) {
	s := New()
	mrtrFixture(t, s, 0)
	book := interactionFor(t, s, "book_flight")
	if !book.HopsComplete || len(book.Hops) != 3 {
		t.Fatalf("hops = %d complete=%v, want all three", len(book.Hops), book.HopsComplete)
	}
	for i, want := range []struct {
		id     string
		server time.Duration
		wait   time.Duration
		asked  int
	}{
		{"1", 400 * time.Millisecond, 0, 1},
		{"2", 300 * time.Millisecond, 12 * time.Second, 1},
		{"3", 500 * time.Millisecond, 25 * time.Second, 0},
	} {
		got := book.Hops[i]
		if got.RequestID != want.id || got.ServerTime != want.server || got.ClientTurnaround != want.wait {
			t.Fatalf("hop %d = id %s, %v server, %v wait; want id %s, %v, %v",
				i+1, got.RequestID, got.ServerTime, got.ClientTurnaround, want.id, want.server, want.wait)
		}
		if len(got.Asked) != want.asked {
			t.Fatalf("hop %d asked %v, want %d entries", i+1, got.Asked, want.asked)
		}
		if got.Pending {
			t.Fatalf("hop %d reads as unanswered", i+1)
		}
	}
	// The per-hop shares are the totals, taken apart.
	var server, client time.Duration
	for _, h := range book.Hops {
		server += h.ServerTime
		client += h.ClientTurnaround
	}
	if server != book.ServerTime || client != book.ClientTurnaround {
		t.Fatalf("hops sum to %v/%v against totals %v/%v", server, client, book.ServerTime, book.ClientTurnaround)
	}
}

// TestAnAbandonedChainReportsWhatItSaw covers the fixture cut after the second
// answer. MRTR makes an abandoned chain ordinary, since a server must not assume
// a client will retry, so the view reports the hops observed and invents none.
func TestAnAbandonedChainReportsWhatItSaw(t *testing.T) {
	s := New()
	mrtrFixture(t, s, 4)
	book := interactionFor(t, s, "book_flight")
	if book.RoundTrips != 2 {
		t.Fatalf("round trips = %d, want the 2 that happened", book.RoundTrips)
	}
	if book.State != Pending {
		t.Fatalf("state = %v, want pending", book.State)
	}
	if len(book.Hops) != 2 {
		t.Fatalf("hops = %d, want 2 with none invented", len(book.Hops))
	}
	if book.Hops[1].Pending {
		t.Fatalf("the second hop was answered, it is the third that never happened")
	}
	// 400ms plus 300ms of server, 12s of client, and nothing beyond what was seen.
	if book.ServerTime != 700*time.Millisecond || book.ClientTurnaround != 12*time.Second {
		t.Fatalf("server %v client %v, want 700ms and 12s", book.ServerTime, book.ClientTurnaround)
	}
}

// TestAnUnlinkedRetryIsItsOwnInteraction keeps the view from filling in a link
// matchRetry refused. A wrong link silently attributes one operation's timing to
// another and leaves no symptom, which is why the refusal exists.
func TestAnUnlinkedRetryIsItsOwnInteraction(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	// Two identical stateless questions in flight at once, so the retry fits both.
	for i, id := range []string{"1", "2"} {
		s.Ingest(req(uint64(2*i+1), t0, proxy.ClientToServer, id, "tools/call", `{"name":"book"}`))
		s.Ingest(resp(uint64(2*i+2), t0.Add(time.Second), proxy.ServerToClient, id,
			`"result":{"resultType":"input_required","inputRequests":{"who":{"method":"elicitation/create"}}}`))
	}
	s.Ingest(req(9, t0.Add(2*time.Second), proxy.ClientToServer, "3", "tools/call",
		`{"name":"book","inputResponses":{"who":{"action":"accept"}}}`))

	var single, chained int
	for _, in := range s.Interactions("s1") {
		if in.RoundTrips == 1 {
			single++
		} else {
			chained++
		}
	}
	if chained != 0 {
		t.Fatalf("an ambiguous retry was folded into a chain: %+v", s.Interactions("s1"))
	}
	if single != 3 {
		t.Fatalf("interactions = %d, want three separate one hop ones", single)
	}
}

// TestAStateViolationStillReportsItsHops keeps a requestState violation from
// also destroying the timing view. The link was still made, so the hops are
// still known.
func TestAStateViolationStillReportsItsHops(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"book"}`))
	s.Ingest(resp(2, t0.Add(time.Second), proxy.ServerToClient, "1",
		`"result":{"resultType":"input_required","requestState":"issued","inputRequests":{"who":{"method":"elicitation/create"}}}`))
	// The retry echoes something else, which is the violation.
	ev := s.Ingest(req(3, t0.Add(5*time.Second), proxy.ClientToServer, "2", "tools/call",
		`{"name":"book","requestState":"tampered","inputResponses":{"who":{"action":"accept"}}}`))
	if ev.MRTRStateIssue != MRTRStateChanged {
		t.Fatalf("the violation was not detected: %q", ev.MRTRStateIssue)
	}
	s.Ingest(resp(4, t0.Add(6*time.Second), proxy.ServerToClient, "2", `"result":{"content":[]}`))

	in := interactionForSession(t, s, "s1", "book")
	if in.RoundTrips != 2 || len(in.Hops) != 2 {
		t.Fatalf("a chain carrying a state violation lost its hops: %+v", in)
	}
	if in.ServerTime != 2*time.Second || in.ClientTurnaround != 4*time.Second {
		t.Fatalf("server %v client %v, want 2s and 4s", in.ServerTime, in.ClientTurnaround)
	}
}

func interactionForSession(t *testing.T, s *Store, session, tool string) InteractionView {
	t.Helper()
	for _, in := range s.Interactions(session) {
		if in.ToolName == tool {
			return in
		}
	}
	t.Fatalf("no interaction for %q", tool)
	return InteractionView{}
}

// TestTheChainStillCountsOnce keeps this a view rather than a re-split of the
// operation, which is what correlating MRTR was written to prevent.
func TestTheChainStillCountsOnce(t *testing.T) {
	s := New()
	mrtrFixture(t, s, 0)
	if n := len(s.Calls("demo")); n != 2 {
		t.Fatalf("calls = %d, want one per operation", n)
	}
	summary, ok := s.ToolSummary("demo")
	if !ok {
		t.Fatal("no tool summary")
	}
	for _, tool := range summary.Tools {
		if tool.Calls != 1 {
			t.Fatalf("%s counted %d calls, want 1", tool.Name, tool.Calls)
		}
		want := 1
		if tool.Name == "book_flight" {
			want = 3
		}
		if tool.MaxRoundTrips != want {
			t.Fatalf("%s round trips = %d, want %d", tool.Name, tool.MaxRoundTrips, want)
		}
	}
}

// TestTheTotalsSurviveEviction is why the counts are accumulated as frames
// arrive rather than derived from the timeline. A live store releases old frames
// to stay inside its budget, so a derived answer would silently be a window.
func TestTheTotalsSurviveEviction(t *testing.T) {
	s := NewBounded(64<<10, 5) // a window smaller than the chain
	mrtrFixture(t, s, 6)       // the chain alone, no trailing call to evict it
	book := interactionForSession(t, s, "demo", "book_flight")
	if book.RoundTrips != 3 {
		t.Fatalf("round trips = %d, want the 3 that happened even though the frames are gone", book.RoundTrips)
	}
	if book.ServerTime != 1200*time.Millisecond || book.ClientTurnaround != 37*time.Second {
		t.Fatalf("server %v client %v, want 1.2s and 37s", book.ServerTime, book.ClientTurnaround)
	}
	if book.HopsComplete {
		t.Fatalf("a partial breakdown claims to be whole: %d hops of %d", len(book.Hops), book.RoundTrips)
	}
	if len(book.Hops) >= book.RoundTrips {
		t.Fatalf("hops = %d, want fewer than the %d round trips once frames are released", len(book.Hops), book.RoundTrips)
	}
}

// ingestFrames feeds a session one capture, given as direction, offset in
// milliseconds and raw body.
func ingestFrames(t *testing.T, s *Store, frames [][3]any) {
	t.Helper()
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for i, f := range frames {
		s.Ingest(proxy.Envelope{SessionID: "demo", ServerLabel: "srv", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f[1].(int)) * time.Millisecond), Direction: f[0].(proxy.Direction),
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f[2].(string))})
	}
}

// TestServerTimeIsClosedOnEverySettlement covers the paths that settle a call
// outside the ordinary terminal branch. Each of them left the server's share at
// zero while the call itself reported a duration, so the interaction contradicted
// the call it describes.
func TestServerTimeIsClosedOnEverySettlement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames [][3]any
		server time.Duration
	}{
		{
			// A tools/call answered with a task handle, finished through the task.
			name: "task handle",
			frames: [][3]any{
				{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"long_job"}}`},
				{proxy.ServerToClient, 100, `{"jsonrpc":"2.0","id":"1","result":{"resultType":"task","taskId":"t-1","status":"working"}}`},
				{proxy.ClientToServer, 29000, `{"jsonrpc":"2.0","id":"2","method":"tasks/get","params":{"taskId":"t-1"}}`},
				{proxy.ServerToClient, 30050, `{"jsonrpc":"2.0","id":"2","result":{"taskId":"t-1","status":"completed","result":{"content":[]}}}`},
			},
			server: 30050 * time.Millisecond,
		},
		{
			// Cancelled, then answered anyway.
			name: "late result after a cancel",
			frames: [][3]any{
				{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"slow"}}`},
				{proxy.ClientToServer, 2000, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"1"}}`},
				{proxy.ServerToClient, 9000, `{"jsonrpc":"2.0","id":"1","result":{"content":[]}}`},
			},
			server: 9 * time.Second,
		},
		{
			// A long-lived stream closed gracefully.
			name: "graceful stream close",
			frames: [][3]any{
				{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"1","method":"subscriptions/listen","params":{}}`},
				{proxy.ServerToClient, 4000, `{"jsonrpc":"2.0","id":"1","result":{}}`},
			},
			server: 4 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			ingestFrames(t, s, tc.frames)
			all := s.Interactions("demo")
			if len(all) == 0 {
				t.Fatal("no interaction")
			}
			in := all[0]
			if in.ServerTime != tc.server {
				t.Fatalf("server time = %v, want %v; the interval was never closed", in.ServerTime, tc.server)
			}
			if in.Duration != tc.server {
				t.Fatalf("duration = %v, want %v", in.Duration, tc.server)
			}
			// And the view agrees with the call it describes.
			for _, c := range s.Calls("demo") {
				if c.RequestSeq == in.CallSeq && c.Done() && c.Duration() != in.Duration {
					t.Fatalf("the interaction says %v and the call says %v", in.Duration, c.Duration())
				}
			}
		})
	}
}

// TestAResentResultIsNotAHop keeps the gap between two copies of one answer off
// the server's account. Parking leaves the call Pending, so the duplicate guard
// cannot catch a server that repeats itself.
func TestAResentResultIsNotAHop(t *testing.T) {
	const asked = `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"st","inputRequests":{"k":{"method":"elicitation/create"}}}}`
	s := New()
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t"}}`},
		{proxy.ServerToClient, 400, asked},
		{proxy.ServerToClient, 10000, asked}, // the same answer again, nine and a half seconds later
		{proxy.ClientToServer, 20000, `{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"t","requestState":"st","inputResponses":{"k":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 20500, `{"jsonrpc":"2.0","id":"2","result":{"content":[]}}`},
	})
	in := s.Interactions("demo")[0]
	if in.RoundTrips != 2 {
		t.Fatalf("round trips = %d, want 2; a resend is not a hop", in.RoundTrips)
	}
	if in.ServerTime != 900*time.Millisecond {
		t.Fatalf("server time = %v, want 900ms; the idle gap between two copies is not server work", in.ServerTime)
	}
	if in.ClientTurnaround != 19600*time.Millisecond {
		t.Fatalf("client turnaround = %v, want 19.6s", in.ClientTurnaround)
	}
	if in.ServerTime+in.ClientTurnaround != in.Duration {
		t.Fatalf("the shares do not add up: %+v", in)
	}
}

// TestAFrameOutOfOrderIsDroppedNotDeferred covers a log that was hand-edited or
// two captures concatenated. Skipping the interval but moving the mark charged
// the skew to whichever hop closed next, which is worse than declining to
// measure the one that was wrong.
func TestAFrameOutOfOrderIsDroppedNotDeferred(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t"}}`},
		{proxy.ServerToClient, 5000, `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"st","inputRequests":{"k":{"method":"elicitation/create"}}}}`},
		// The retry claims to have happened before the answer it answers.
		{proxy.ClientToServer, 3000, `{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"t","requestState":"st","inputResponses":{"k":{"action":"accept"}}}}`},
		// And the operation then finishes normally, which is where a mark left on the
		// backwards frame would surface: the last hop would be measured from 3000
		// rather than from 5000 and report seven seconds of server work that never
		// happened.
		{proxy.ServerToClient, 10000, `{"jsonrpc":"2.0","id":"2","result":{"content":[]}}`},
	})
	in := s.Interactions("demo")[0]
	if in.ClientTurnaround != 0 {
		t.Fatalf("client turnaround = %v, want none; the retry went backwards", in.ClientTurnaround)
	}
	// Five seconds to the answer, then five more to the final result, and nothing
	// from the frame that went backwards.
	if in.ServerTime != 10*time.Second {
		t.Fatalf("server time = %v, want 10s, with the skew dropped rather than charged to the next hop", in.ServerTime)
	}
	if in.ServerTime+in.ClientTurnaround != in.Duration {
		t.Fatalf("the shares do not add up: %+v", in)
	}
}

// TestAServerInitiatedRequestDoesNotBlameTheServer covers the deprecated shape
// that 2026-07-28 replaced with MRTR. The peer that answers a server's request
// is the client, so the interval between them is not the server's time.
func TestAServerInitiatedRequestDoesNotBlameTheServer(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{
		{proxy.ServerToClient, 0, `{"jsonrpc":"2.0","id":"1","method":"sampling/createMessage","params":{"messages":[]}}`},
		{proxy.ClientToServer, 3000, `{"jsonrpc":"2.0","id":"1","result":{"role":"assistant"}}`},
	})
	in := s.Interactions("demo")[0]
	if in.ServerTime != 0 {
		t.Fatalf("server time = %v, want none; the client is what answered", in.ServerTime)
	}
	if in.ClientTurnaround != 3*time.Second {
		t.Fatalf("client turnaround = %v, want 3s", in.ClientTurnaround)
	}
	if in.ServerTime+in.ClientTurnaround != in.Duration {
		t.Fatalf("the shares do not add up: %+v", in)
	}
}

// TestAOneHopInteractionCarriesNoRedundantBreakdown keeps the common case cheap.
// One hop's breakdown is the totals word for word, and producing one allocated a
// slice per call under the read lock a live panel holds.
func TestAOneHopInteractionCarriesNoRedundantBreakdown(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t"}}`},
		{proxy.ServerToClient, 200, `{"jsonrpc":"2.0","id":"1","result":{"content":[]}}`},
	})
	in := s.Interactions("demo")[0]
	if len(in.Hops) != 0 {
		t.Fatalf("hops = %+v, want none for a single request", in.Hops)
	}
	if !in.HopsComplete {
		t.Fatal("a one hop interaction has nothing to be incomplete about")
	}
	if in.RoundTrips != 1 || in.ServerTime != 200*time.Millisecond {
		t.Fatalf("the totals are still the whole description: %+v", in)
	}
}

// TestAnUnreadableAnswerIsNotAnEmptyOne keeps the two apart. A live store
// releases frame bodies to stay inside its byte budget, and a hop whose answer is
// gone did not ask for nothing.
func TestAnUnreadableAnswerIsNotAnEmptyOne(t *testing.T) {
	s := NewBounded(1, 0) // one byte of bodies, so everything is released at once
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t"}}`},
		{proxy.ServerToClient, 400, `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"st","inputRequests":{"k":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 5000, `{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"t","requestState":"st","inputResponses":{"k":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 5100, `{"jsonrpc":"2.0","id":"2","result":{"content":[]}}`},
	})
	all := s.Interactions("demo")
	if len(all) == 0 {
		t.Fatal("no interaction")
	}
	in := all[0]
	if in.RoundTrips != 2 {
		t.Fatalf("round trips = %d, want 2", in.RoundTrips)
	}
	var unknown int
	for _, h := range in.Hops {
		if h.AskedUnknown {
			unknown++
		}
		if h.AskedUnknown && len(h.Asked) > 0 {
			t.Fatalf("a hop is both unreadable and readable: %+v", h)
		}
	}
	if unknown == 0 {
		t.Fatalf("a released body reads as a hop that asked for nothing: %+v", in.Hops)
	}
	// The timings are exact regardless, so the breakdown is not incomplete.
	if in.ServerTime != 500*time.Millisecond || in.ClientTurnaround != 4600*time.Millisecond {
		t.Fatalf("releasing a body changed a timing: %+v", in)
	}
}
