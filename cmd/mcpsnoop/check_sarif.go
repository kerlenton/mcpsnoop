package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

const (
	// sarifSchema is the URI code scanning documents for a 2.1.0 log. The OASIS
	// repository's own copy has moved twice and the path most producers still
	// emit is a 404, which leaves a schema-aware reader with nothing to resolve.
	sarifSchema  = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVersion = "2.1.0"
	// sarifDriverName and sarifInformationURI name the tool a code-scanning alert
	// is attributed to, so both stay fixed regardless of how the binary was built.
	sarifDriverName     = "mcpsnoop"
	sarifInformationURI = "https://github.com/kerlenton/mcpsnoop"
	// sarifAssertionRuleID covers the --max-duration, --expect-tool and
	// --forbid-tool failures, which are not signals and so have no signal name.
	sarifAssertionRuleID = "mcpsnoop/assertion"
	// sarifTruncationRuleID reports the report's own limit having been reached.
	// It is not a signal either, and it describes this file rather than the
	// traffic, but a reader who is not told the list was cut reads it as the
	// whole story.
	sarifTruncationRuleID = "mcpsnoop/report-truncated"
	// sarifFingerprintKey is versioned, so a later change to how a finding is
	// identified starts a fresh alert rather than silently rewriting the history
	// of the alerts raised under the old scheme.
	sarifFingerprintKey = "mcpsnoopFinding/v1"

	sarifLevelError   = "error"
	sarifLevelWarning = "warning"
	sarifLevelNote    = "note"

	// sarifMaxResults caps the results one run emits. Code scanning rejects a
	// file outright above 25,000 results in a run and displays only the top 5,000
	// of what it does accept, so an unbounded report either never reaches the
	// Security tab or arrives silently cut. A capture can pass both thresholds
	// without being pathological: one systematically misconfigured gateway yields
	// two findings per request.
	sarifMaxResults = 5000
)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

// sarifRule carries help as well as the two descriptions, because code scanning
// marks help.text required and renders it as the alert's "how to fix" panel. A
// rule without one leaves that panel empty on every alert mcpsnoop raises.
type sarifRule struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	ShortDescription sarifMessage `json:"shortDescription"`
	FullDescription  sarifMessage `json:"fullDescription"`
	Help             sarifMessage `json:"help"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	RuleIndex int             `json:"ruleIndex"`
	Level     string          `json:"level"`
	Message   sarifMessage    `json:"message"`
	Locations []sarifLocation `json:"locations,omitempty"`
	// PartialFingerprints identifies the finding rather than where it landed, so
	// a consumer that reads the key can recognise the same finding in a later
	// report over the same capture. Code scanning is not that consumer: it
	// computes its own primaryLocationLineHash and ignores tool-defined keys, so
	// this is written for everything else that reads SARIF.
	PartialFingerprints map[string]string `json:"partialFingerprints"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

func writeCheckSARIF(w io.Writer, log checkLog, summaries []checkSummary, selected map[checkSignal]bool, assertionFailures [][]string) error {
	payload := buildCheckSARIF(log, summaries, selected, assertionFailures, appVersion())
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

// buildCheckSARIF renders every finding of the run as one result. It emits a
// single run for the whole file rather than one per session: code scanning caps
// a SARIF file at 20 runs and a concatenated capture can hold more sessions than
// that, so the session id rides on each result instead.
func buildCheckSARIF(log checkLog, summaries []checkSummary, selected map[checkSignal]bool, assertionFailures [][]string, version string) sarifLog {
	rules := checkSARIFRules()
	ruleIndex := make(map[string]int, len(rules))
	for i, rule := range rules {
		ruleIndex[rule.ID] = i
	}
	b := &sarifBuilder{
		uri:       sarifArtifactURI(log.path),
		lines:     log.lines,
		selected:  selected,
		ruleIndex: ruleIndex,
		// Non-nil, so a clean run marshals "results": [] and not null. The empty
		// array is what tells code scanning that the alerts it still holds are fixed.
		results: []sarifResult{},
	}
	for i, summary := range summaries {
		b.session(log.store, summary)
		if i >= len(assertionFailures) {
			continue
		}
		for _, msg := range assertionFailures[i] {
			// An assertion runs only because the user asked for it and it fails the
			// run outright, so it is never a note. It also has no frame: it judges the
			// session as a whole.
			b.add(summary.sessionID, sarifAssertionRuleID, sarifLevelError,
				fmt.Sprintf("session %s: %s", summary.sessionID, msg), msg, 0)
		}
	}
	if found := len(b.results); found > sarifMaxResults {
		dropped := b.truncate()
		// A warning, not an error: the gate has already decided whether this run
		// passes, and a report that turned a green run red on its own would be the
		// reporter overruling it. A note would be ranked below every finding if code
		// scanning ever had to cut the list further, which is exactly when it must
		// survive.
		b.add("", sarifTruncationRuleID, sarifLevelWarning,
			fmt.Sprintf("the capture holds %d findings, more than a code-scanning upload accepts in one run, so %d of them were left out of this report; the text and junit formats report all of them",
				found, dropped),
			"report-truncated", 0)
	}
	return sarifLog{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           sarifDriverName,
				Version:        version,
				InformationURI: sarifInformationURI,
				Rules:          rules,
			}},
			Results: b.results,
		}},
	}
}

// sarifBuilder accumulates the results of one run.
type sarifBuilder struct {
	uri       string
	lines     exporter.FrameLines
	selected  map[checkSignal]bool
	ruleIndex map[string]int
	results   []sarifResult
}

// session appends every finding of one session, in a fixed order so two runs
// over the same log produce byte-identical output.
func (b *sarifBuilder) session(st *store.Store, summary checkSummary) {
	sessionID := summary.sessionID
	if summary.baselineCreated {
		// A first-seen baseline is recorded, not verified, which the text and junit
		// formats both say out loud. SARIF has no equivalent of junit's <skipped>,
		// so it is a note: the run is not failing, but it verified nothing either,
		// and a log carrying no result at all would read as though it had.
		b.add(sessionID, sarifRuleID(checkDrift), sarifLevelNote,
			fmt.Sprintf("session %s: recorded first-seen tool baseline (trusted, not verified)", sessionID),
			"baseline-recorded", 0)
	}

	var toolListSeq uint64
	pending := make(map[string]bool)
	for _, event := range st.Timeline(sessionID) {
		if event.Kind == store.EventResponse && event.Call != nil && event.Call.Method == "tools/list" {
			// The last page, not the first: a paginated listing is only complete after
			// it, and drift compares the complete list.
			toolListSeq = event.Seq
		}
		if event.Errored {
			// Keyed off the frames the store counted the errors on, so the report
			// names them rather than re-deriving the conditions and drifting from the
			// gate. Deriving them from Call.Errored instead reported neither a
			// transport failure nor an unmatched error response, both of which carry
			// no call, and anchored an async task failure at the call's first response
			// rather than at the frame that failed it.
			b.frame(sessionID, checkError, sarifErrorMessage(sessionID, event), event.Seq)
		}
		if event.Kind == store.EventInvalid {
			b.frame(sessionID, checkInvalid,
				fmt.Sprintf("session %s frame %d: frame on the protocol channel is not valid JSON-RPC", sessionID, event.Seq), event.Seq)
		}
		if event.Warning != "" {
			b.frame(sessionID, checkWarn,
				fmt.Sprintf("session %s frame %d: %s", sessionID, event.Seq, event.Warning), event.Seq)
		}
		if event.RoutingMismatch {
			// A mismatched frame usually carries the warning naming it, and then it is
			// two findings under two rules, exactly as the gate counts it under two
			// signals. Reporting fewer results than the gate counts would leave a
			// failing run with nothing to open.
			b.frame(sessionID, checkMismatch, sarifMismatchMessage(sessionID, event.Seq, event.Warning), event.Seq)
		}
		if event.Deprecated != "" {
			b.frame(sessionID, checkDeprecated,
				fmt.Sprintf("session %s frame %d: %s", sessionID, event.Seq, event.Deprecated), event.Seq)
		}
		if event.LateResult {
			// Keyed off the frame the store counted it on, like the error axis above.
			// Call.LateResult is true from the request frame onwards once the answer
			// lands, so reading it here would report the same finding twice.
			b.frame(sessionID, checkLateResult, sarifLateResultMessage(sessionID, event), event.Seq)
		}
		if event.Kind == store.EventRequest && event.Call != nil && event.Call.State == store.Pending {
			// A multi round-trip retry continues the same call, so keying on the call
			// reports one hung operation rather than one per round trip.
			if key := sarifCallKey(*event.Call); !pending[key] {
				pending[key] = true
				b.frame(sessionID, checkPending, sarifPendingMessage(sessionID, event.Seq, *event.Call), event.Seq)
			}
		}
	}

	// Drift and a baseline error both concern the advertised tool list, so both
	// anchor at the response that carried it. A stdin run, or one whose log holds
	// no tools/list response, simply gets no region.
	driftLevel := b.level(checkDrift)
	if summary.drift.BaselineError != "" {
		// A baseline that could not be read is not a tool change. Saying otherwise
		// sends the reader after something that never happened.
		b.add(sessionID, sarifRuleID(checkDrift), driftLevel,
			fmt.Sprintf("session %s: tool baseline error: %s", sessionID, summary.drift.BaselineError),
			"baseline-error", toolListSeq)
	}
	// Looped over store.ToolDriftKinds so a kind the gate counts cannot go
	// unreported here, leaving a failing run with no result to act on.
	for _, kind := range store.ToolDriftKinds {
		for _, name := range summary.drift.Names(kind) {
			b.add(sessionID, sarifRuleID(checkDrift), driftLevel,
				fmt.Sprintf("session %s: tool %q %s", sessionID, name, driftLabel(kind)),
				string(kind)+"/"+name, toolListSeq)
		}
	}

	if summary.missingFrames > 0 {
		// The dropped frames never reached the log, so there is no line to point at.
		b.add(sessionID, sarifRuleID(checkIncomplete), b.level(checkIncomplete),
			checkSignalFailureReason(sessionID, checkIncomplete, int(summary.missingFrames)),
			"missing-frames", 0)
	}
}

// frame appends a finding anchored at one captured frame.
func (b *sarifBuilder) frame(sessionID string, signal checkSignal, message string, seq uint64) {
	b.add(sessionID, sarifRuleID(signal), b.level(signal), message, strconv.FormatUint(seq, 10), seq)
}

// add appends one result. anchor identifies the finding for its fingerprint and
// is deliberately never the line number: a line moves whenever anything above it
// changes, and a fingerprint that moved with it would close the alert and reopen
// the same finding as a new one. A seq of zero means the finding has no frame,
// which both transports leave free by numbering from one.
func (b *sarifBuilder) add(sessionID, ruleID, level, message, anchor string, seq uint64) {
	result := sarifResult{
		RuleID:    ruleID,
		RuleIndex: b.ruleIndex[ruleID],
		Level:     level,
		Message:   sarifMessage{Text: message},
		PartialFingerprints: map[string]string{
			sarifFingerprintKey: sarifFingerprint(sessionID, ruleID, anchor),
		},
	}
	if b.uri != "" {
		physical := sarifPhysicalLocation{ArtifactLocation: sarifArtifactLocation{URI: b.uri}}
		if seq != 0 {
			if line, ok := b.lines.Line(sessionID, seq); ok {
				physical.Region = &sarifRegion{StartLine: line}
			}
		}
		result.Locations = []sarifLocation{{PhysicalLocation: physical}}
	}
	b.results = append(b.results, result)
}

// truncate cuts the report down to what a code-scanning upload will carry and
// returns how many results were left out. The findings the gate failed on go
// first, since a note dropped in favour of an error costs a reader nothing they
// were going to act on, and one slot is held back for the result that says the
// list was cut.
func (b *sarifBuilder) truncate() int {
	kept := make([]sarifResult, 0, sarifMaxResults)
	// Two ordered passes rather than a sort, so results keep the order they were
	// found in within a level and two runs over one log stay byte-identical.
	for _, level := range []string{sarifLevelError, sarifLevelWarning, sarifLevelNote} {
		for _, result := range b.results {
			if len(kept) == sarifMaxResults-1 {
				break
			}
			if result.Level == level {
				kept = append(kept, result)
			}
		}
	}
	dropped := len(b.results) - len(kept)
	b.results = kept
	return dropped
}

// level ties the report to the gate: a signal named in --fail-on is an error,
// one outside it a note, which surfaces the observation without asserting that
// anyone must act on it.
func (b *sarifBuilder) level(signal checkSignal) string {
	if b.selected[signal] {
		return sarifLevelError
	}
	return sarifLevelNote
}

func sarifRuleID(signal checkSignal) string { return "mcpsnoop/" + string(signal) }

// checkSARIFRules is one rule per signal in checkSignalOrder plus one for the
// assertions, so a signal added to that list reaches the report. Every rule is
// declared on every run, clean or not: a consumer closes an alert only when it
// sees the rule again without its result, so a rule that vanished from a green
// run would leave its fixed finding open forever.
func checkSARIFRules() []sarifRule {
	rules := make([]sarifRule, 0, len(checkSignalOrder)+2)
	for _, signal := range checkSignalOrder {
		name, short, full, help := checkSARIFSignalRule(signal)
		rules = append(rules, sarifRule{
			ID:               sarifRuleID(signal),
			Name:             name,
			ShortDescription: sarifMessage{Text: short},
			FullDescription:  sarifMessage{Text: full},
			Help:             sarifMessage{Text: help},
		})
	}
	return append(rules, sarifRule{
		ID:               sarifAssertionRuleID,
		Name:             "AssertionFailure",
		ShortDescription: sarifMessage{Text: "The session violates an asserted contract"},
		FullDescription:  sarifMessage{Text: "A --max-duration, --expect-tool or --forbid-tool assertion failed: a completed tool call exceeded the latency budget, a tool that had to be called never was, or a forbidden tool was."},
		Help:             sarifMessage{Text: "The result names which assertion failed and what the run actually did. Either the run stopped meeting the contract, or the contract has moved on and the flag on the check step needs updating."},
	}, sarifRule{
		ID:               sarifTruncationRuleID,
		Name:             "ReportTruncated",
		ShortDescription: sarifMessage{Text: "The capture held more findings than this report carries"},
		FullDescription:  sarifMessage{Text: "Code scanning refuses a run holding more than 25,000 results and displays only the top 5,000 of what it accepts, so mcpsnoop caps the report and says how many findings it left out. The findings the gate failed on are kept first."},
		Help:             sarifMessage{Text: "Run mcpsnoop check over the same capture with the default text format, or with --format junit, to see every finding. A capture this large usually means one systematic fault repeating, so fixing the first few findings tends to clear most of the rest."},
	})
}

func checkSARIFSignalRule(signal checkSignal) (name, short, full, help string) {
	switch signal {
	case checkError:
		return "CallError", "A call ended in an error",
			"A request was answered with a JSON-RPC error, or with a result marked isError, or its task ended in a failure. It is the same axis the session error count and the CI gate read.",
			"Open the frame the result points at and read what the server actually said, which the message carries verbatim. A run that exercises failure paths on purpose can drop error from --fail-on, or capture only the traffic under test."
	case checkInvalid:
		return "InvalidFrame", "A frame is not valid JSON-RPC",
			"A frame on the protocol channel could not be parsed as JSON-RPC, which usually means something wrote to the transport that is not part of the protocol, or the stream is corrupted.",
			"On stdio this is almost always a server logging to stdout, which the transport reserves for the protocol. Send that output to stderr instead. If the frame is JSON-RPC that mcpsnoop could not parse, the line the result points at is the whole evidence."
	case checkWarn:
		return "ProtocolWarning", "A frame violates a protocol expectation",
			"A frame breaks an expectation the MCP or JSON-RPC specification sets, such as reusing an id already in flight, answering one id twice, or replying with neither result nor error.",
			"The message names the expectation and the frame that broke it. Fix the side that emitted the frame; mcpsnoop only observes, so nothing on the proxy changes this. A warning fails the gate by default, so a warning you accept has to leave --fail-on explicitly."
	case checkMismatch:
		return "RoutingMismatch", "A routing header disagrees with the body",
			"An Mcp-Method, Mcp-Name, Mcp-Param-* or MCP-Protocol-Version header disagrees with the request body, rides a batch, or is missing where the negotiated revision requires it. A server's own -32020 rejection counts as the same condition.",
			"A gateway routing on the headers and a server reading the body are now looking at two different requests, so this is worth treating as a routing fault rather than cosmetics. The message names which header disagrees and with what."
	case checkPending:
		return "PendingCall", "A call never got a response",
			"A request was captured with no response before the capture ended, so the caller was left waiting. Only a request that was still open at the end of the session counts.",
			"Either the server never answered, or the capture stopped first. Check the end of the log before treating it as a hang, since a capture cut short leaves every in-flight call pending."
	case checkLateResult:
		return "LateResult", "A response arrived after its request was cancelled",
			"The client cancelled a request and the server answered it anyway. The spec says a server receiving a cancellation SHOULD not send a response for it and that a client SHOULD ignore one that arrives, so this is wasted work on both sides rather than a protocol violation, which is why it is not in the default gate.",
			"The message carries how long after the cancellation the answer arrived. A short delay is the race the specification allows for, since a cancellation can reach a server that has already replied. A long one means the server carried on working after being told to stop."
	case checkDrift:
		return "ToolDefinitionDrift", "An advertised tool definition changed after approval",
			"An advertised tool differs from the trusted first-seen baseline for its server label in its description, title, input or output schema, annotations or icons, or a tool was added or removed. A baseline that could not be read is reported here too, since drift could not be verified.",
			"Read the change before accepting it: a description is a prompt an agent obeys, so rewriting one silently is how a trusted tool is repurposed. If the change is intended, re-record the baseline with mcpsnoop baseline. If the baseline could not be read at all, nothing was verified this run."
	case checkDeprecated:
		return "DeprecatedFeature", "A frame uses a deprecated protocol feature",
			"A frame uses a feature the MCP specification has deprecated. Nothing is broken today, but the feature is on its way out and the traffic will need changing.",
			"The message names the feature and what replaced it. Nothing fails until the feature is removed, so this is a note unless deprecated is named in --fail-on."
	case checkIncomplete:
		return "IncompleteCapture", "Frames were dropped, so the capture understates the session",
			"Envelopes were dropped upstream, inferred from gaps in the per-session sequence numbers. Every other signal is then a floor rather than a total, because the dropped frames were never judged.",
			"Every other count in this report is a floor, not a total, so a green run over an incomplete capture proves less than it looks. Frames are dropped when a sink cannot keep up, so a slow or blocked sink is the first place to look."
	}
	// Deliberately generic, and deliberately covered by a test that walks
	// checkSignalOrder, because a signal reaching a code-scanning alert under a
	// placeholder description is worse than one that never reaches it at all.
	return string(signal), "An mcpsnoop check signal", "A signal reported by mcpsnoop check.", "See the mcpsnoop documentation."
}

// sarifErrorMessage names what went wrong on the frame the error was counted on,
// rather than leaving a reader with a count and a frame number. The store counts
// an error on four kinds of frame, and each one reads differently: a settled
// call, a task that ended in a failed state, an HTTP failure carrying no
// JSON-RPC body, and an error response matching no request it could name.
func sarifErrorMessage(sessionID string, event store.EventView) string {
	switch {
	// The failed call is read before the task it was polling: a tasks/get that
	// errored is the poll failing, not the task, and a task lifecycle frame
	// carries both calls either way.
	case event.Kind == store.EventResponse && event.Call != nil && event.Call.Errored:
		return fmt.Sprintf("session %s frame %d: %s ended in an error: %s",
			sessionID, event.Seq, sarifCallSubject(*event.Call), sarifErrorDetail(*event.Call))
	case event.TaskCall != nil:
		return fmt.Sprintf("session %s frame %d: %s failed as task %s: %s",
			sessionID, event.Seq, sarifCallSubject(*event.TaskCall), event.TaskID, sarifErrorDetail(*event.TaskCall))
	case event.Kind == store.EventTransport:
		return fmt.Sprintf("session %s frame %d: the transport failed with HTTP %d",
			sessionID, event.Seq, event.HTTPStatus)
	}
	// An error response whose id matches no request the capture saw. There is no
	// call to name, so the frame and its own warning are all a reader gets.
	if event.Warning != "" {
		return fmt.Sprintf("session %s frame %d: error response could not be matched to a call: %s",
			sessionID, event.Seq, event.Warning)
	}
	return fmt.Sprintf("session %s frame %d: error response could not be matched to a call", sessionID, event.Seq)
}

// sarifErrorDetail says why a call is counted as failed, preferring the server's
// own words to a restatement of the flag.
func sarifErrorDetail(call store.CallView) string {
	switch {
	case call.Err != nil:
		return fmt.Sprintf("%s (code %d)", call.Err.Message, call.Err.Code)
	case call.ToolErr:
		return "the result is marked isError"
	}
	return "the call failed"
}

// sarifLateResultMessage names the call that was answered after its cancellation
// and how late the answer was, since the delay is what separates the race the
// spec allows for from a server that kept working after being told to stop. The
// store writes that sentence as the frame's observation, so the two formats read
// alike rather than paraphrasing each other.
func sarifLateResultMessage(sessionID string, event store.EventView) string {
	subject := "a cancelled call"
	if event.Call != nil {
		subject = sarifCallSubject(*event.Call)
	}
	if event.Observation == "" {
		return fmt.Sprintf("session %s frame %d: %s was answered after it was cancelled", sessionID, event.Seq, subject)
	}
	return fmt.Sprintf("session %s frame %d: %s was answered after it was cancelled: %s",
		sessionID, event.Seq, subject, event.Observation)
}

func sarifPendingMessage(sessionID string, seq uint64, call store.CallView) string {
	return fmt.Sprintf("session %s frame %d: %s never got a response", sessionID, seq, sarifCallSubject(call))
}

func sarifMismatchMessage(sessionID string, seq uint64, warning string) string {
	if warning == "" {
		return fmt.Sprintf("session %s frame %d: a routing header disagrees with the body", sessionID, seq)
	}
	return fmt.Sprintf("session %s frame %d: routing header mismatch: %s", sessionID, seq, warning)
}

// sarifCallSubject names a call the way the reader saw it on the wire, tool name
// included when there is one, since "tools/call" alone identifies nothing.
func sarifCallSubject(call store.CallView) string {
	method := call.Method
	if method == "" {
		method = "call"
	}
	if call.IsTool && call.ToolName != "" {
		method = fmt.Sprintf("%s %s", method, call.ToolName)
	}
	if call.ID == "" {
		return method
	}
	return fmt.Sprintf("%s (id %s)", method, call.ID)
}

// sarifCallKey identifies one correlated call. The id alone would not: a client
// may reuse an id once the earlier call is settled, and the two are separate
// findings.
func sarifCallKey(call store.CallView) string {
	return string(call.ReqDir) + "\x00" + call.ID + "\x00" + strconv.FormatInt(call.Start.UnixNano(), 10)
}

// sarifFingerprint identifies a finding by what it is, never by where it landed
// in the file, so re-running check over a capture whose lines have moved
// produces the same value.
func sarifFingerprint(sessionID, ruleID, anchor string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + ruleID + "\x00" + anchor))
	return hex.EncodeToString(sum[:8])
}

// sarifArtifactURI turns the session log path into the URI a result points at.
// Code scanning resolves a relative URI against the root of the repository it
// analysed, so a log under the working directory is reported relative to it,
// which is right whenever check runs at the root of the checkout as a workflow
// step does by default. A log outside cannot honestly be given a relative path,
// so it gets an absolute file: URI; code scanning converts that back against the
// checkout when it can and otherwise leaves the alert without source context
// rather than dropping it.
func sarifArtifactURI(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, abs); err == nil && !sarifEscapes(rel) {
			// Through net/url so a space or a '#' in a directory name is escaped
			// rather than silently truncating the URI.
			return (&url.URL{Path: filepath.ToSlash(rel)}).String()
		}
	}
	slash := filepath.ToSlash(abs)
	if !strings.HasPrefix(slash, "/") {
		// A Windows path (C:/logs/session.jsonl) needs the leading slash that makes
		// it file:///C:/logs/session.jsonl.
		slash = "/" + slash
	}
	return (&url.URL{Scheme: "file", Path: slash}).String()
}

// sarifEscapes reports whether a relative path climbs out of the directory it
// was computed against. Matching on a ".." prefix alone would also catch a
// legitimate "..data" directory.
func sarifEscapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
