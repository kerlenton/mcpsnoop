package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
)

// DuplicateCall is a tool call the client sent again, with the same arguments,
// while it still could not know what had become of the first attempt. MCP
// defines nothing for that case. A server that ran the first attempt and lost
// the answer, or carried on past a cancellation it was entitled to ignore, runs
// the second one too, and a tool that is not idempotent then does its work twice.
type DuplicateCall struct {
	Tool string
	// Seq and ID are the retry's request frame and id, EarlierSeq and EarlierID
	// the attempt it repeated.
	Seq        uint64
	ID         string
	EarlierSeq uint64
	EarlierID  string
	// AfterCancel says the client cancelled the earlier attempt before sending
	// this one, which is how a timeout shows on stdio.
	AfterCancel bool
	// RanTwice says the server answered both with success, so as far as the wire
	// shows the work happened twice. Otherwise the earlier attempt's outcome is
	// unknown, and it may have.
	RanTwice bool
}

// callFate is what the wire says became of one call.
type callFate int

const (
	// fateUnknown is a call with no answer, a cancellation the server never
	// answered, or a failure in front of the server that says nothing about
	// whether the server ran it.
	fateUnknown callFate = iota
	fateSucceeded
	// fateFailed is an answer saying the work did not happen or did not finish,
	// which includes a request refused before it reached the tool.
	fateFailed
)

func (c *call) fate() callFate {
	switch {
	case c.undelivered:
		return fateFailed
	case c.transportStatus >= 500:
		return fateUnknown
	case c.transportStatus != 0:
		return fateFailed
	case c.state == Completed:
		return fateSucceeded
	case c.state == Cancelled && c.lateResult:
		// An error after a cancellation is usually the cancellation reported back,
		// the Go SDK turns a cancelled handler into an isError result, and it says
		// nothing about whether the work was done before the handler noticed.
		if c.errored {
			return fateUnknown
		}
		return fateSucceeded
	case c.state == Failed:
		return fateFailed
	}
	return fateUnknown
}

// outcomeUnknown reports whether, right now, the client cannot know what became
// of a call still held as an attempt. An answer, a task handle, a refusal and a
// request mcpsnoop never delivered all tell it, and each of those lets go of the
// attempt where it settles, so what is left to judge here is an input request,
// which is the server saying it has not done the work yet, and the states with
// no answer at all. A cancelled call stays unknown even once a late answer
// arrives, since the spec tells the client to ignore that answer, so a client
// that backs off before sending the call again has learned nothing from it.
func (c *call) outcomeUnknown() bool {
	switch c.state {
	case Pending:
		return !c.mrtrHasState && c.mrtrKeys == ""
	case Cancelled, Superseded:
		return true
	case Failed:
		return c.transportStatus >= 500
	}
	return false
}

// answered reports whether the server itself answered the call, late or not.
// A status from something in front of it is not the server answering.
func (c *call) answered() bool {
	if c.lateResult {
		return true
	}
	return (c.state == Completed || c.state == Failed) && c.transportStatus == 0
}

// noteAttempt links a new tool call to an identical one still waiting on its
// answer, and becomes the attempt the next identical call is compared with.
func (sess *session) noteAttempt(c *call) {
	if c.signature == "" {
		return
	}
	if earlier := sess.openAttempts[c.signature]; earlier != nil && earlier.outcomeUnknown() {
		c.retryOf = earlier
		c.retryAfterCancel = earlier.state == Cancelled
		sess.retries = append(sess.retries, c)
	}
	sess.openAttempts[c.signature] = c
}

// forgetAttempt drops a call that now has an answer from the attempts a later
// call could repeat. Held calls are the ones nobody has answered yet, so the map
// stays as small as what is still in flight rather than growing with every
// distinct call a long session makes.
func (sess *session) forgetAttempt(c *call) {
	if c.signature != "" && sess.openAttempts[c.signature] == c {
		delete(sess.openAttempts, c.signature)
	}
}

// callSignature identifies a tool call by what it asks the server to do, the
// tool and its arguments, so two requests differing only in their id, their
// _meta or how their JSON was spelled compare equal. Absent arguments and an
// empty object ask for the same thing. Hashed, since arguments can be large and
// the key is kept for as long as the call goes unanswered. Empty when the
// arguments cannot be read, which never matches anything.
func callSignature(tool string, params json.RawMessage) string {
	if tool == "" {
		return ""
	}
	var p struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) != nil {
		return ""
	}
	args := "{}"
	if trimmed := bytes.TrimSpace(p.Arguments); len(trimmed) > 0 && string(trimmed) != "null" {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			return ""
		}
		// Marshalling sorts object keys, and json.Number keeps each number as it
		// was written, so 1 and 1.0 stay different arguments, as they may be to the
		// tool that reads them.
		canonical, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		args = string(canonical)
	}
	sum := sha256.Sum256([]byte(tool + "\x00" + args))
	return string(sum[:])
}

// mayRepeatEffects reports whether running a tool twice may do its work twice,
// which is every tool that does not declare itself read-only or idempotent,
// read through the defaults as clients read them. A tool never listed takes the
// defaults, which say neither.
func (sess *session) mayRepeatEffects(tool string) bool {
	hints := ParseToolHints(sess.toolDefinitions[tool].Annotations)
	return !hints.Value(HintReadOnly) && !hints.Value(HintIdempotent)
}

// Duplicates reports the tool calls a session sent again before it could know
// what became of an identical earlier one, in the order they were sent.
//
// A repeat is left out when the wire shows it did no extra work. The tool says
// it is read-only or idempotent, the earlier call was refused or answered with a
// failure, or the repeat itself failed. So is a repeat whose earlier call was
// answered and never cancelled, which is what two identical calls sent in
// parallel on purpose look like. A client that gave up without cancelling looks
// the same, and the wire cannot tell the two apart.
func (s *Store) Duplicates(sessionID string) []DuplicateCall {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	var out []DuplicateCall
	for _, retry := range sess.retries {
		if d, ok := sess.duplicateOf(retry); ok {
			out = append(out, d)
		}
	}
	return out
}

// duplicateOf judges one call that repeated an earlier one, by the rules
// Duplicates documents. Shared with the stream, so a frame and the report about
// it never disagree.
func (sess *session) duplicateOf(retry *call) (DuplicateCall, bool) {
	earlier := retry.retryOf
	if earlier == nil || !sess.mayRepeatEffects(retry.toolName) {
		return DuplicateCall{}, false
	}
	// Answered and never cancelled is two calls in flight on purpose, or a client
	// that gave up without saying so, and the wire cannot tell the two apart. A
	// first call cancelled after the second went out, the way a hedged request
	// works, is neither, and if it was answered anyway the work ran twice.
	if earlier.state != Cancelled && earlier.answered() {
		return DuplicateCall{}, false
	}
	earlierFate, retryFate := earlier.fate(), retry.fate()
	if earlierFate == fateFailed || retryFate == fateFailed {
		return DuplicateCall{}, false
	}
	return DuplicateCall{
		Tool:        retry.toolName,
		Seq:         retry.requestSeq,
		ID:          retry.id,
		EarlierSeq:  earlier.requestSeq,
		EarlierID:   earlier.id,
		AfterCancel: retry.retryAfterCancel,
		RanTwice:    earlierFate == fateSucceeded && retryFate == fateSucceeded,
	}, true
}

// Observation is how the stream marks the frame that repeated a call, shorter
// than Phrase since the frame already names the tool.
func (d DuplicateCall) Observation() string {
	out := "repeats the call on frame " + strconv.FormatUint(d.EarlierSeq, 10)
	if d.AfterCancel {
		out += ", which the client had cancelled"
	} else {
		out += ", which was still unanswered"
	}
	if d.RanTwice {
		return out + ", and the server answered both, so it ran twice"
	}
	return out + ", so it may have run twice"
}

// Phrase is how a report says what one duplicate amounts to, written to follow
// the tool's name, so check and the export word it the same way.
func (d DuplicateCall) Phrase() string {
	var b strings.Builder
	b.WriteString("was called again with the same arguments")
	if d.AfterCancel {
		b.WriteString(" after the client cancelled the call on frame ")
	} else {
		b.WriteString(" while the call on frame ")
	}
	b.WriteString(strconv.FormatUint(d.EarlierSeq, 10))
	if !d.AfterCancel {
		b.WriteString(" was still unanswered")
	}
	if d.RanTwice {
		b.WriteString(", and the server answered both, so it ran twice")
	} else {
		b.WriteString(", so it may have run twice")
	}
	return b.String()
}
