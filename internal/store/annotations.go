package store

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// The behaviour hints a tool's annotations can carry. Every revision that has
// annotations, 2025-03-26 through 2026-07-28, defines the same four with the
// same defaults, so one reading serves every capture.
const (
	HintReadOnly    = "readOnlyHint"
	HintDestructive = "destructiveHint"
	HintIdempotent  = "idempotentHint"
	HintOpenWorld   = "openWorldHint"
)

// BehaviourHints is every hint in the order a report names them.
var BehaviourHints = []string{HintReadOnly, HintDestructive, HintIdempotent, HintOpenWorld}

// hintDefault is what the specification assumes for a hint a tool leaves out.
// Every default is the cautious reading, so a tool that says nothing is taken to
// write, to destroy, to be unsafe to retry, and to reach the open world.
func hintDefault(hint string) bool {
	return hint == HintDestructive || hint == HintOpenWorld
}

// AnnotationFindingKind names something worth knowing about how a tool declares
// its behaviour. All but one are observations rather than verdicts. Every hint
// is optional, so leaving one out breaks no rule, but each changes what a client
// does with the tool. AnnotationsMalformed is the exception and says so on its
// own doc.
type AnnotationFindingKind string

const (
	// AnnotationsMalformed is the one kind here that is a violation rather than an
	// observation. Every revision that defines annotations types the block as an
	// object, each hint as a boolean and title as a string, and the official SDK
	// clients reject the whole listing over a value they cannot read as its type.
	// One wrong value then costs a client built on them every tool the server
	// offers rather than the one that sent it, so it is reported as a warning on
	// the tools/list frame as well as here.
	AnnotationsMalformed AnnotationFindingKind = "malformedAnnotations"
	// AnnotationsMissing is a tool that declares no behaviour hint at all. Every
	// client then falls back on the defaults, so VS Code asks before each call,
	// Codex in its default approval mode asks too, and Claude Code runs it alone
	// rather than beside other calls.
	AnnotationsMissing AnnotationFindingKind = "unannotated"
	// AnnotationsImplicit is a tool that declares some hints but leaves out one of
	// readOnlyHint, destructiveHint and openWorldHint, so the default fills it in.
	// ChatGPT's app submission requirements ask for all three as explicit booleans.
	AnnotationsImplicit AnnotationFindingKind = "implicitHints"
	// AnnotationsContradictory is readOnlyHint and destructiveHint both true. The
	// specification makes destructiveHint meaningful only when readOnlyHint is
	// false, and clients split on it, since Codex asks for approval and VS Code
	// does not.
	AnnotationsContradictory AnnotationFindingKind = "contradictoryHints"
)

// AnnotationFindingKinds is every kind in report order, exported for the reason
// SchemaFindingKinds is. A renderer walking its own list would miss a kind added
// here.
var AnnotationFindingKinds = []AnnotationFindingKind{
	AnnotationsMalformed,
	AnnotationsMissing,
	AnnotationsImplicit,
	AnnotationsContradictory,
}

// ObservationalAnnotationKinds are the kinds that never fail check unless the
// annotations signal is selected. It is AnnotationFindingKinds minus the one
// violation, which reaches the default gate as a warning on the frame that
// advertised the tool, the way ObservationalSchemaKinds leaves out its own.
var ObservationalAnnotationKinds = AnnotationFindingKinds[1:]

// IsObservationalAnnotationKind reports whether a kind is an observation rather
// than the violation.
func IsObservationalAnnotationKind(kind AnnotationFindingKind) bool {
	return kind != AnnotationsMalformed
}

// annotationObject decodes a tool's annotations. absent is true for a missing
// or null field, and ok is false when the field holds something other than an
// object.
func annotationObject(raw json.RawMessage) (obj map[string]json.RawMessage, absent, ok bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true, true
	}
	if json.Unmarshal(trimmed, &obj) != nil || obj == nil {
		return nil, false, false
	}
	return obj, false, true
}

// hintBool reads one hint. null is rejected explicitly, because json.Unmarshal
// accepts it into a bool without error and would report the hint as false.
func hintBool(value json.RawMessage) (flag, ok bool) {
	trimmed := bytes.TrimSpace(value)
	if string(trimmed) == "null" {
		return false, false
	}
	return flag, json.Unmarshal(trimmed, &flag) == nil
}

// annotationViolations names each value in a tool's annotations whose type is
// not the one every revision since 2025-03-26 gives it, in the words a warning
// uses after "sends", or nil for none. null is a type like any other here, since
// a hint is optional by being left out, not by being null. A value mcpsnoop's
// own redaction rewrote is unreadable rather than wrong, so it is never reported.
func annotationViolations(raw json.RawMessage, redacted bool) []string {
	obj, absent, ok := annotationObject(raw)
	if absent {
		return nil
	}
	if !ok {
		if partlyRedacted(redacted, string(raw)) {
			return nil
		}
		return []string{"annotations as " + jsonType(raw) + " instead of an object"}
	}
	var wrong []string
	if value, has := obj["title"]; has && jsonKind(value) != '"' && !partlyRedacted(redacted, string(value)) {
		wrong = append(wrong, "annotations.title as "+jsonType(value)+" instead of a string")
	}
	for _, hint := range BehaviourHints {
		value, has := obj[hint]
		if !has {
			continue
		}
		if _, valid := hintBool(value); !valid && !partlyRedacted(redacted, string(value)) {
			wrong = append(wrong, "annotations."+hint+" as "+jsonType(value)+" instead of a boolean")
		}
	}
	return wrong
}

// analyzeAnnotations reports what is worth knowing about one tool's annotations.
// redacted says the frame passed through mcpsnoop's own redaction, so a value
// holding the redaction placeholder is mcpsnoop's rewrite rather than the
// server's mistake and is not reported as malformed.
func analyzeAnnotations(raw json.RawMessage, redacted bool) []AnnotationFindingKind {
	var findings []AnnotationFindingKind
	// First, so the violation leads the list the way the root rule leads a
	// schema's. The rest is still read, since a block with one bad value can
	// still leave hints out or contradict itself.
	if len(annotationViolations(raw, redacted)) > 0 {
		findings = append(findings, AnnotationsMalformed)
	}
	obj, absent, ok := annotationObject(raw)
	if absent {
		return []AnnotationFindingKind{AnnotationsMissing}
	}
	if !ok {
		return findings
	}
	present := 0
	declared := make(map[string]bool, len(BehaviourHints))
	values := make(map[string]bool, len(BehaviourHints))
	for _, hint := range BehaviourHints {
		value, has := obj[hint]
		if !has {
			continue
		}
		present++
		if flag, valid := hintBool(value); valid {
			declared[hint], values[hint] = true, flag
		}
	}
	if present == 0 {
		// An empty object, or one holding only title or keys the specification
		// does not define, declares exactly as much as no annotations at all.
		return append(findings, AnnotationsMissing)
	}
	for _, hint := range []string{HintReadOnly, HintDestructive, HintOpenWorld} {
		if _, has := obj[hint]; !has {
			findings = append(findings, AnnotationsImplicit)
			break
		}
	}
	if declared[HintReadOnly] && values[HintReadOnly] && declared[HintDestructive] && values[HintDestructive] {
		findings = append(findings, AnnotationsContradictory)
	}
	return findings
}

// AnnotationReport groups the observational annotation findings of one session's
// advertised tools by kind.
type AnnotationReport struct {
	// ByKind maps a finding kind to the tools that carry it.
	ByKind map[AnnotationFindingKind][]string
}

func (r AnnotationReport) Empty() bool { return r.Count() == 0 }

// Count is the number of tool and kind pairs, which is what a gate fails on.
func (r AnnotationReport) Count() int {
	n := 0
	for _, names := range r.ByKind {
		n += len(names)
	}
	return n
}

// Names returns the tools carrying one kind, or nil for none.
func (r AnnotationReport) Names(kind AnnotationFindingKind) []string {
	if r.ByKind == nil {
		return nil
	}
	return slices.Clone(r.ByKind[kind])
}

// AnnotationFindings reports the observational annotation findings of every tool
// a session has advertised so far, the way SchemaFindings does, so a listing
// still paginating reports what it has. ok is false when the session never
// carried a tools/list response. The violation is left out, as SchemaFindings
// leaves out its own, since it already reached the default gate as a warning.
func (s *Store) AnnotationFindings(sessionID string) (AnnotationReport, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok || !sess.toolListSeen {
		return AnnotationReport{}, false
	}
	report := AnnotationReport{ByKind: make(map[AnnotationFindingKind][]string)}
	for _, name := range sess.advertisedTools {
		for _, kind := range sess.toolDefinitions[name].AnnotationFindings {
			if !IsObservationalAnnotationKind(kind) {
				continue
			}
			report.ByKind[kind] = append(report.ByKind[kind], name)
		}
	}
	return report, true
}

// ToolHints is what one tool's annotations say about its behaviour, with the
// specification's default standing in for every hint it leaves out.
type ToolHints struct {
	values   map[string]bool
	explicit map[string]bool
	// malformed holds the raw value of each hint that was present but not a
	// boolean, keyed by hint. notObject is true when annotations itself was sent
	// as something other than an object.
	malformed map[string]string
	notObject bool
	rawObject string
}

// ParseToolHints reads a tool's annotations.
func ParseToolHints(raw json.RawMessage) ToolHints {
	h := ToolHints{
		values:    make(map[string]bool, len(BehaviourHints)),
		explicit:  make(map[string]bool, len(BehaviourHints)),
		malformed: make(map[string]string),
	}
	for _, hint := range BehaviourHints {
		h.values[hint] = hintDefault(hint)
	}
	obj, absent, ok := annotationObject(raw)
	if absent {
		return h
	}
	if !ok {
		h.notObject = true
		h.rawObject = strings.TrimSpace(string(raw))
		return h
	}
	for _, hint := range BehaviourHints {
		value, has := obj[hint]
		if !has {
			continue
		}
		flag, valid := hintBool(value)
		if !valid {
			h.malformed[hint] = strings.TrimSpace(string(value))
			continue
		}
		h.values[hint], h.explicit[hint] = flag, true
	}
	return h
}

// Value is the hint as a client following the specification reads it, the
// declared boolean or the default.
func (h ToolHints) Value(hint string) bool { return h.values[hint] }

// Declares reports whether the server sent the hint as a boolean.
func (h ToolHints) Declares(hint string) bool { return h.explicit[hint] }

// describe renders a hint's value for a report, saying when it is the default
// rather than something the server said.
func (h ToolHints) describe(hint string) string {
	if h.notObject {
		return "malformed annotations " + h.rawObject
	}
	if raw, bad := h.malformed[hint]; bad {
		return "malformed " + raw
	}
	value := "false"
	if h.values[hint] {
		value = "true"
	}
	if !h.explicit[hint] {
		value += " (default)"
	}
	return value
}

// HintChange is one hint of one tool moving between the trusted baseline and
// the current listing.
type HintChange struct {
	Hint string
	From string
	To   string
}

// AnnotationShift says which way a tool's declared behaviour moved. Loosened
// holds the changes that let a client trust the tool more, which is what makes
// one skip a confirmation or run calls side by side, and what would let one
// retry a call on its own. Tightened holds the rest.
type AnnotationShift struct {
	Loosened  []HintChange
	Tightened []HintChange
}

// CompareToolHints classifies the difference between two annotation blocks.
//
// destructiveHint and idempotentHint count only while the tool is not
// read-only on both sides, which is when the specification says they mean
// anything. Counting them on a read-only tool would call it loosening when a
// server adds destructiveHint false to satisfy ChatGPT's review, which changes
// what no client does. The one exception is an explicit destructiveHint true on
// a read-only tool, since Codex reads that as asking for approval anyway, so
// dropping it does loosen what one client does. A readOnlyHint flip is judged
// on its own and the hints it activates or silences are part of that flip.
//
// A hint that turns malformed is loosening. A strict client then drops the whole
// listing, which the warning on that frame reports, while a lenient one such as
// the Python SDK reads "true", "yes" or 1 as true, so nothing the server declares
// can be relied on.
func CompareToolHints(before, after json.RawMessage) AnnotationShift {
	b, a := ParseToolHints(before), ParseToolHints(after)
	var shift AnnotationShift
	note := func(hint string, loosened bool) {
		change := HintChange{Hint: hint, From: b.describe(hint), To: a.describe(hint)}
		if loosened {
			shift.Loosened = append(shift.Loosened, change)
			return
		}
		shift.Tightened = append(shift.Tightened, change)
	}

	if a.notObject {
		if !b.notObject || b.rawObject != a.rawObject {
			shift.Loosened = append(shift.Loosened, HintChange{Hint: "annotations", From: annotationsSummary(b), To: annotationsSummary(a)})
		}
		return shift
	}
	settled := make(map[string]bool, len(BehaviourHints))
	for _, hint := range BehaviourHints {
		raw, bad := a.malformed[hint]
		if !bad {
			continue
		}
		settled[hint] = true
		if before, was := b.malformed[hint]; !was || before != raw {
			note(hint, true)
		}
	}

	if !settled[HintReadOnly] {
		switch before, now := b.Value(HintReadOnly), a.Value(HintReadOnly); {
		case !before && now:
			note(HintReadOnly, true)
		case before && !now:
			note(HintReadOnly, false)
		}
	}
	readOnlyBoth := b.Value(HintReadOnly) && a.Value(HintReadOnly)
	writableBoth := !b.Value(HintReadOnly) && !a.Value(HintReadOnly)

	if !settled[HintDestructive] {
		before, now := b.Value(HintDestructive), a.Value(HintDestructive)
		switch {
		case writableBoth && before && !now:
			note(HintDestructive, true)
		case writableBoth && !before && now:
			note(HintDestructive, false)
		case readOnlyBoth:
			wasExplicitTrue := b.Declares(HintDestructive) && before
			isExplicitTrue := a.Declares(HintDestructive) && now
			if wasExplicitTrue && !isExplicitTrue {
				note(HintDestructive, true)
			} else if !wasExplicitTrue && isExplicitTrue {
				note(HintDestructive, false)
			}
		}
	}
	if !settled[HintIdempotent] && writableBoth {
		switch before, now := b.Value(HintIdempotent), a.Value(HintIdempotent); {
		case !before && now:
			note(HintIdempotent, true)
		case before && !now:
			note(HintIdempotent, false)
		}
	}
	if !settled[HintOpenWorld] {
		switch before, now := b.Value(HintOpenWorld), a.Value(HintOpenWorld); {
		case before && !now:
			note(HintOpenWorld, true)
		case !before && now:
			note(HintOpenWorld, false)
		}
	}
	return shift
}

// annotationsSummary is how a whole annotations block reads in a change line.
func annotationsSummary(h ToolHints) string {
	if h.notObject {
		return "malformed " + h.rawObject
	}
	return "an object"
}
