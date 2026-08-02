package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// cancelNotif is a notifications/cancelled naming a request id, in the shape a
// host sends when it gives up: the id it issued plus the sentence it showed the
// user.
func cancelNotif(seq uint64, ts time.Time, dir proxy.Direction, requestID, reason string) proxy.Envelope {
	params := `{"requestId":` + requestID
	if reason != "" {
		params += `,"reason":` + jsonString(reason)
	}
	params += `}`
	return notif(seq, ts, dir, "notifications/cancelled", params)
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestCancelSettlesAnUnansweredCall is the "server obeyed the cancel" half of
// the bug: before this the call stayed Pending forever, so a conforming
// cancellation was indistinguishable from a hang and --fail-on pending could
// never be switched on.
func TestCancelSettlesAnUnansweredCall(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s.Ingest(req(1, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"slow_job"}`))
	cancel := s.Ingest(cancelNotif(2, t0.Add(160*time.Millisecond), proxy.ClientToServer, "3", "Request timed out"))

	events := s.Timeline("s1")
	call := events[0].Call
	if call == nil || call.State != Cancelled {
		t.Fatalf("call state = %v, want cancelled", call)
	}
	if call.LateResult {
		t.Fatal("no response arrived, so late_result must stay false")
	}
	if !call.Done() {
		t.Fatal("a cancelled call is settled, so Done() must be true")
	}
	if got := call.CancelledAt; !got.Equal(t0.Add(160 * time.Millisecond)) {
		t.Fatalf("cancelled_at = %v, want the cancel frame's timestamp", got)
	}
	if call.CancelReason != "Request timed out" {
		t.Fatalf("cancel reason = %q, want the host's own words", call.CancelReason)
	}
	if got := call.CancelStatus(); got != "cancelled_request" {
		t.Fatalf("cancel status = %q, want cancelled_request", got)
	}
	// The observation must not fail a default check run: the spec asks the server
	// to do exactly this.
	if cancel.Warning != "" {
		t.Fatalf("the cancel frame must carry no warning, got %q", cancel.Warning)
	}
	header := s.Sessions()[0]
	if header.Pending != 0 {
		t.Fatalf("pending = %d, want 0 once the call is settled", header.Pending)
	}
	if header.Errors != 0 {
		t.Fatalf("errors = %d, want 0: a cancel is a decision, not an error", header.Errors)
	}
}

// TestCancelKeepsALateResultAsEvidence is the "server ignored the cancel" half:
// the answer arrived and the host had already walked away, which used to read as
// a clean success with no trace of the cancel anywhere.
func TestCancelKeepsALateResultAsEvidence(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s.Ingest(req(1, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"slow_job"}`))
	s.Ingest(cancelNotif(2, t0.Add(160*time.Millisecond), proxy.ClientToServer, "3", "Request timed out"))
	late := s.Ingest(resp(3, t0.Add(610*time.Millisecond), proxy.ServerToClient, "3", `"result":{"content":[{"type":"text","text":"done"}]}`))

	call := late.Call
	if call == nil || call.State != Cancelled {
		t.Fatalf("call state = %v, want the call to stay cancelled", call)
	}
	if !call.LateResult {
		t.Fatal("the late response must be recorded on the call")
	}
	if len(call.Result) == 0 {
		t.Fatal("the result bytes are the evidence and must be kept")
	}
	if got := call.CancelStatus(); got != "cancelled_late_result" {
		t.Fatalf("cancel status = %q, want cancelled_late_result", got)
	}
	// The note carries the ordering and the gap mcpsnoop observed, and nothing else.
	if !strings.Contains(late.LateResult, "450ms") {
		t.Fatalf("late-result note = %q, want the 450ms gap", late.LateResult)
	}
	// It must be an observation, not a protocol warning, or a default check run
	// starts failing on a race the spec explicitly blesses.
	if late.Warning != "" {
		t.Fatalf("late result must not ride the warning field, got %q", late.Warning)
	}
	header := s.Sessions()[0]
	if header.Pending != 0 || header.Errors != 0 {
		t.Fatalf("pending/errors = %d/%d, want 0/0", header.Pending, header.Errors)
	}
}

// TestLateErrorAfterACancelStaysAnError. A server acknowledging the cancel with
// an error response is a common shape. The call still reads cancelled, because
// that is what happened to it, but the error axis is untouched: an error frame on
// the wire counted before this change and must count the same after it.
func TestLateErrorAfterACancelStaysAnError(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s.Ingest(req(1, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"slow_job"}`))
	s.Ingest(cancelNotif(2, t0.Add(100*time.Millisecond), proxy.ClientToServer, "3", ""))
	late := s.Ingest(resp(3, t0.Add(200*time.Millisecond), proxy.ServerToClient, "3",
		`"error":{"code":-32800,"message":"request cancelled"}`))

	if late.Call.State != Cancelled || !late.Call.LateResult {
		t.Fatalf("call = state %v late %v, want cancelled with a late result", late.Call.State, late.Call.LateResult)
	}
	if !late.Call.Errored || !late.Call.Failed() {
		t.Fatal("an error frame on the wire is still an error")
	}
	if late.Call.Err == nil || late.Call.Err.Code != -32800 {
		t.Fatalf("the error object must be kept, got %+v", late.Call.Err)
	}
	if h := s.Sessions()[0]; h.Errors != 1 {
		t.Fatalf("errors = %d, want 1, unchanged from before a cancel was settled", h.Errors)
	}
}

// TestCancelIgnoresIdsItCannotClaim keeps the store from inventing anything on
// evidence it does not have, the discipline serverCancelWarning is written to.
func TestCancelIgnoresIdsItCannotClaim(t *testing.T) {
	t.Run("never observed id", func(t *testing.T) {
		s := New()
		t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		s.Ingest(req(1, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"slow_job"}`))
		s.Ingest(cancelNotif(2, t0.Add(time.Millisecond), proxy.ClientToServer, "404", ""))

		if got := s.Timeline("s1")[0].Call.State; got != Pending {
			t.Fatalf("state = %v, want the unrelated call left pending", got)
		}
		if h := s.Sessions()[0]; h.Pending != 1 {
			t.Fatalf("pending = %d, want 1", h.Pending)
		}
	})

	t.Run("already answered", func(t *testing.T) {
		s := New()
		t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		s.Ingest(req(1, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"fast"}`))
		s.Ingest(resp(2, t0.Add(5*time.Millisecond), proxy.ServerToClient, "3", `"result":{"content":[]}`))
		s.Ingest(cancelNotif(3, t0.Add(10*time.Millisecond), proxy.ClientToServer, "3", "too slow"))

		call := s.Timeline("s1")[0].Call
		if call.State != Completed {
			t.Fatalf("state = %v, want the completed call untouched", call.State)
		}
		if call.CancelReason != "" || !call.CancelledAt.IsZero() {
			t.Fatal("a cancel arriving after the answer must not rewrite the call")
		}
	})

	t.Run("opposite direction", func(t *testing.T) {
		s := New()
		t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		// A server-initiated request with id 1. JSON-RPC scopes id uniqueness per
		// sender, so a client cancelling its own id 1 says nothing about this one.
		s.Ingest(req(1, t0, proxy.ServerToClient, "1", "sampling/createMessage", `{}`))
		s.Ingest(cancelNotif(2, t0.Add(time.Millisecond), proxy.ClientToServer, "1", "not mine"))

		if got := s.Timeline("s1")[0].Call.State; got != Pending {
			t.Fatalf("state = %v, want the server's own request untouched", got)
		}
	})
}

// TestCancelLeavesAStreamTeardownCompleted is the regression on the one shape
// notifications/cancelled already had: a server tearing down a
// subscriptions/listen stream ends that call Completed, not Cancelled.
func TestCancelLeavesAStreamTeardownCompleted(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s.Ingest(req(1, t0, proxy.ClientToServer, "5", "subscriptions/listen", `{}`))
	s.Ingest(cancelNotif(2, t0.Add(time.Second), proxy.ServerToClient, "5", "shutting down"))

	call := s.Timeline("s1")[0].Call
	if call.State != Completed {
		t.Fatalf("state = %v, want completed", call.State)
	}
	if h := s.Sessions()[0]; h.Pending != 0 {
		t.Fatalf("pending = %d, want 0", h.Pending)
	}
}

// TestSecondResponseAfterALateResultStillWarns. The first answer after a cancel
// is evidence; a second one for the same id is an ordinary duplicate again, and
// neither may move the pending counter.
func TestSecondResponseAfterALateResultStillWarns(t *testing.T) {
	s := New()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s.Ingest(req(1, t0, proxy.ClientToServer, "3", "tools/call", `{"name":"slow_job"}`))
	s.Ingest(cancelNotif(2, t0.Add(100*time.Millisecond), proxy.ClientToServer, "3", ""))
	first := s.Ingest(resp(3, t0.Add(200*time.Millisecond), proxy.ServerToClient, "3", `"result":{"content":[]}`))
	second := s.Ingest(resp(4, t0.Add(300*time.Millisecond), proxy.ServerToClient, "3", `"result":{"content":[]}`))

	if strings.Contains(first.Warning, "duplicate response") {
		t.Fatalf("the first late result is not a duplicate, got %q", first.Warning)
	}
	if !strings.Contains(second.Warning, "duplicate response for the same id") {
		t.Fatalf("second response warning = %q, want the duplicate warning", second.Warning)
	}
	if h := s.Sessions()[0]; h.Pending != 0 {
		t.Fatalf("pending = %d, want 0", h.Pending)
	}
}

// TestToolSummarySkipsCancelledCallLatency mirrors the superseded case: the end
// stored on a cancelled call is when the cancel landed, not a latency, so it must
// not reach the percentiles. A cancelled call that did get a late answer still
// feeds them, because that duration is a real server latency.
func TestToolSummarySkipsCancelledCallLatency(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	s := New()
	// One ordinary echo call at 25ms.
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo"}`))
	s.Ingest(resp(2, t0.Add(25*time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	// id 2 is cancelled a full second in and never answered.
	s.Ingest(req(3, t0, proxy.ClientToServer, "2", "tools/call", `{"name":"echo"}`))
	s.Ingest(cancelNotif(4, t0.Add(time.Second), proxy.ClientToServer, "2", "gave up"))

	sum, ok := s.ToolSummary("s1")
	if !ok || len(sum.Tools) != 1 {
		t.Fatalf("ToolSummary = %+v ok %v", sum, ok)
	}
	echo := sum.Tools[0]
	if echo.Calls != 2 || echo.Errors != 0 {
		t.Fatalf("echo calls/errors = %d/%d, want 2/0", echo.Calls, echo.Errors)
	}
	if echo.P50 != 25*time.Millisecond || echo.P95 != 25*time.Millisecond {
		t.Fatalf("echo percentiles = %s/%s, want 25ms from the answered call only", echo.P50, echo.P95)
	}
	for _, sc := range sum.Slowest {
		if sc.Duration >= time.Second {
			t.Fatalf("a cancel timestamp leaked into slowest as a latency: %+v", sc)
		}
	}

	// The late-result shape is a real round trip, so it does feed the statistics.
	withLate := New()
	withLate.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"echo"}`))
	withLate.Ingest(cancelNotif(2, t0.Add(160*time.Millisecond), proxy.ClientToServer, "1", ""))
	withLate.Ingest(resp(3, t0.Add(610*time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	lateSum, _ := withLate.ToolSummary("s1")
	if lateSum.Tools[0].P50 != 610*time.Millisecond {
		t.Fatalf("late-result p50 = %s, want the real 610ms round trip", lateSum.Tools[0].P50)
	}
}
