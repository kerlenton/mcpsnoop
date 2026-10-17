package main

import (
	"fmt"
	"io"

	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/wiretext"
)

// frameFinding is one frame a frame-level signal counted. The text report and
// the SARIF report both list them from here, so the two cannot name different
// frames for one verdict or word one finding two ways.
type frameFinding struct {
	signal checkSignal
	seq    uint64
	// text says what the frame broke, without the session and the frame that
	// carry it, which each report names its own way.
	text string
}

// frameSignals are the signals counted on frames, in the order a report lists
// them, with the heading the text report gives each.
var frameSignals = []struct {
	signal  checkSignal
	heading string
}{
	{checkError, "errors"},
	{checkInvalid, "invalid frames"},
	{checkWarn, "warnings"},
	{checkMismatch, "routing mismatches"},
	{checkDeprecated, "deprecated features"},
	{checkLateResult, "late results"},
	{checkPending, "pending calls"},
}

// frameFindings lists every frame a frame-level signal counted in one session,
// in timeline order.
func frameFindings(st *store.Store, sessionID string) []frameFinding {
	var findings []frameFinding
	add := func(signal checkSignal, seq uint64, text string) {
		findings = append(findings, frameFinding{signal: signal, seq: seq, text: text})
	}
	pending := make(map[string]bool)
	for _, event := range st.Timeline(sessionID) {
		if event.Errored {
			// Keyed off the frames the store counted the errors on, so the report
			// names them rather than re-deriving the conditions and drifting from the
			// gate. Deriving them from Call.Errored instead reported neither a
			// transport failure nor an unmatched error response, both of which carry
			// no call, and anchored an async task failure at the call's first response
			// rather than at the frame that failed it.
			add(checkError, event.Seq, errorFinding(event))
		}
		if event.Kind == store.EventInvalid {
			add(checkInvalid, event.Seq, "frame on the protocol channel is not valid JSON-RPC")
		}
		if event.Warning != "" {
			add(checkWarn, event.Seq, event.Warning)
		}
		if event.RoutingMismatch {
			// A mismatched frame usually carries the warning naming it, and then it is
			// two findings under two signals, exactly as the gate counts it. Reporting
			// fewer than the gate counts would leave a failing run with nothing to open.
			add(checkMismatch, event.Seq, mismatchFinding(event.Warning))
		}
		if event.Deprecated != "" {
			add(checkDeprecated, event.Seq, event.Deprecated)
		}
		if event.LateResult {
			// Keyed off the frame the store counted it on, like the error axis above.
			// Call.LateResult is true from the request frame onwards once the answer
			// lands, so reading it here would report the same finding twice.
			add(checkLateResult, event.Seq, lateResultFinding(event))
		}
		if event.Kind == store.EventRequest && event.Call != nil && event.Call.State == store.Pending {
			// A multi round-trip retry continues the same call, so keying on the call
			// reports one hung operation rather than one per round trip.
			if key := sarifCallKey(*event.Call); !pending[key] {
				pending[key] = true
				add(checkPending, event.Seq, sarifCallSubject(*event.Call)+" never got a response")
			}
		}
	}
	return findings
}

// frameFindingsShown is how many frames the text report lists for one signal.
// A session can hold thousands, and a log that scrolls past them helps nobody,
// while the SARIF report keeps every one.
const frameFindingsShown = 20

// writeFrameFindings lists the frames behind each signal that failed the run,
// so a log says what failed rather than only how much.
func writeFrameFindings(w io.Writer, findings []frameFinding, failed []string) {
	failing := make(map[checkSignal]bool, len(failed))
	for _, name := range failed {
		failing[checkSignal(name)] = true
	}
	for _, s := range frameSignals {
		if !failing[s.signal] {
			continue
		}
		shown, more := 0, 0
		for _, f := range findings {
			if f.signal != s.signal {
				continue
			}
			if shown == frameFindingsShown {
				more++
				continue
			}
			if shown == 0 {
				fmt.Fprintln(w, s.heading+":")
			}
			fmt.Fprintf(w, "  frame %d, %s\n", f.seq, wiretext.OneLine(f.text))
			shown++
		}
		if more > 0 {
			fmt.Fprintf(w, "  and %d more, every one listed by --format sarif\n", more)
		}
	}
}

// errorFinding names what went wrong on the frame an error was counted on.
func errorFinding(event store.EventView) string {
	switch {
	// The failed call is read before the task it was polling: a tasks/get that
	// errored is the poll failing, not the task, and a task lifecycle frame
	// carries both calls either way.
	case event.Kind == store.EventResponse && event.Call != nil && event.Call.Errored:
		return fmt.Sprintf("%s ended in an error: %s", sarifCallSubject(*event.Call), sarifErrorDetail(*event.Call))
	case event.TaskCall != nil:
		return fmt.Sprintf("%s failed as task %s: %s", sarifCallSubject(*event.TaskCall), event.TaskID, sarifErrorDetail(*event.TaskCall))
	case event.Kind == store.EventTransport:
		return fmt.Sprintf("the transport failed with HTTP %d", event.HTTPStatus)
	}
	// An error response whose id matches no request the capture saw. There is no
	// call to name, so the frame and its own warning are all a reader gets.
	if event.Warning != "" {
		return "error response could not be matched to a call: " + event.Warning
	}
	return "error response could not be matched to a call"
}

// lateResultFinding names the call that was answered after its cancellation
// and how late the answer was, since the delay is what separates the race the
// spec allows for from a server that kept working after being told to stop. The
// store writes that sentence as the frame's observation, so the reports and the
// stream read alike rather than paraphrasing each other.
func lateResultFinding(event store.EventView) string {
	subject := "a cancelled call"
	if event.Call != nil {
		subject = sarifCallSubject(*event.Call)
	}
	if event.Observation == "" {
		return subject + " was answered after it was cancelled"
	}
	return subject + " was answered after it was cancelled: " + event.Observation
}

func mismatchFinding(warning string) string {
	if warning == "" {
		return "a routing header disagrees with the body"
	}
	return "routing header mismatch: " + warning
}
