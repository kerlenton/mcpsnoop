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
	sarifSchema  = "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/master/Schemata/sarif-schema-2.1.0.json"
	sarifVersion = "2.1.0"
	// sarifDriverName and sarifInformationURI name the tool a code-scanning alert
	// is attributed to, so both stay fixed regardless of how the binary was built.
	sarifDriverName     = "mcpsnoop"
	sarifInformationURI = "https://github.com/kerlenton/mcpsnoop"
	// sarifAssertionRuleID covers the --max-duration, --expect-tool and
	// --forbid-tool failures, which are not signals and so have no signal name.
	sarifAssertionRuleID = "mcpsnoop/assertion"
	// sarifFingerprintKey is versioned, so a later change to how a finding is
	// identified starts a fresh alert rather than silently rewriting the history
	// of the alerts raised under the old scheme.
	sarifFingerprintKey = "mcpsnoopFinding/v1"

	sarifLevelError = "error"
	sarifLevelNote  = "note"
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

type sarifRule struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	ShortDescription sarifMessage `json:"shortDescription"`
	FullDescription  sarifMessage `json:"fullDescription"`
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
	// PartialFingerprints is what lets a consumer recognise a finding it has seen
	// before across runs, so an unchanged one is not reported twice.
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
				fmt.Sprintf("session %s: %s", summary.sessionID, msg), msg, 0, false)
		}
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
			"baseline-recorded", 0, false)
	}

	var toolListSeq uint64
	var toolListSeen bool
	pending := make(map[string]bool)
	for _, event := range st.Timeline(sessionID) {
		if event.Kind == store.EventResponse && event.Call != nil && event.Call.Method == "tools/list" {
			// The last page, not the first: a paginated listing is only complete after
			// it, and drift compares the complete list.
			toolListSeq, toolListSeen = event.Seq, true
		}
		if event.Errored {
			// Keyed off the frame the store counted the error on, so the results are
			// one per counted error. Deriving them from Call.Errored instead reported
			// neither a transport failure nor an unmatched error response, both of
			// which carry no call, and anchored an async task failure at the call's
			// first response rather than at the frame that failed it.
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
			"baseline-error", toolListSeq, toolListSeen)
	}
	// Looped over store.ToolDriftKinds so a kind the gate counts cannot go
	// unreported here, leaving a failing run with no result to act on.
	for _, kind := range store.ToolDriftKinds {
		for _, name := range summary.drift.Names(kind) {
			b.add(sessionID, sarifRuleID(checkDrift), driftLevel,
				fmt.Sprintf("session %s: tool %q %s", sessionID, name, driftLabel(kind)),
				string(kind)+"/"+name, toolListSeq, toolListSeen)
		}
	}

	if summary.missingFrames > 0 {
		// The dropped frames never reached the log, so there is no line to point at.
		b.add(sessionID, sarifRuleID(checkIncomplete), b.level(checkIncomplete),
			checkSignalFailureReason(sessionID, checkIncomplete, int(summary.missingFrames)),
			"missing-frames", 0, false)
	}
}

// frame appends a finding anchored at one captured frame.
func (b *sarifBuilder) frame(sessionID string, signal checkSignal, message string, seq uint64) {
	b.add(sessionID, sarifRuleID(signal), b.level(signal), message, strconv.FormatUint(seq, 10), seq, true)
}

// add appends one result. anchor identifies the finding for its fingerprint and
// is deliberately never the line number: a line moves whenever anything above it
// changes, and a fingerprint that moved with it would close the alert and reopen
// the same finding as a new one.
func (b *sarifBuilder) add(sessionID, ruleID, level, message, anchor string, seq uint64, framed bool) {
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
		if framed {
			if line, ok := b.lines.Line(sessionID, seq); ok {
				physical.Region = &sarifRegion{StartLine: line}
			}
		}
		result.Locations = []sarifLocation{{PhysicalLocation: physical}}
	}
	b.results = append(b.results, result)
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
	rules := make([]sarifRule, 0, len(checkSignalOrder)+1)
	for _, signal := range checkSignalOrder {
		name, short, full := checkSARIFSignalRule(signal)
		rules = append(rules, sarifRule{
			ID:               sarifRuleID(signal),
			Name:             name,
			ShortDescription: sarifMessage{Text: short},
			FullDescription:  sarifMessage{Text: full},
		})
	}
	return append(rules, sarifRule{
		ID:               sarifAssertionRuleID,
		Name:             "AssertionFailure",
		ShortDescription: sarifMessage{Text: "The session violates an asserted contract"},
		FullDescription:  sarifMessage{Text: "A --max-duration, --expect-tool or --forbid-tool assertion failed: a completed tool call exceeded the latency budget, a tool that had to be called never was, or a forbidden tool was."},
	})
}

func checkSARIFSignalRule(signal checkSignal) (name, short, full string) {
	switch signal {
	case checkError:
		return "CallError", "A call ended in an error",
			"A request was answered with a JSON-RPC error, or with a result marked isError, or its task ended in a failure. It is the same axis the session error count and the CI gate read."
	case checkInvalid:
		return "InvalidFrame", "A frame is not valid JSON-RPC",
			"A frame on the protocol channel could not be parsed as JSON-RPC, which usually means something wrote to the transport that is not part of the protocol, or the stream is corrupted."
	case checkWarn:
		return "ProtocolWarning", "A frame violates a protocol expectation",
			"A frame breaks an expectation the MCP or JSON-RPC specification sets, such as reusing an id already in flight, answering one id twice, or replying with neither result nor error."
	case checkMismatch:
		return "RoutingMismatch", "A routing header disagrees with the body",
			"An Mcp-Method, Mcp-Name, Mcp-Param-* or MCP-Protocol-Version header disagrees with the request body, rides a batch, or is missing where the negotiated revision requires it. A server's own -32020 rejection counts as the same condition."
	case checkPending:
		return "PendingCall", "A call never got a response",
			"A request was captured with no response before the capture ended, so the caller was left waiting. Only a request that was still open at the end of the session counts."
	case checkDrift:
		return "ToolDefinitionDrift", "An advertised tool definition changed after approval",
			"An advertised tool differs from the trusted first-seen baseline for its server label in its description, title, input or output schema, annotations or icons, or a tool was added or removed. A baseline that could not be read is reported here too, since drift could not be verified."
	case checkDeprecated:
		return "DeprecatedFeature", "A frame uses a deprecated protocol feature",
			"A frame uses a feature the MCP specification has deprecated. Nothing is broken today, but the feature is on its way out and the traffic will need changing."
	case checkIncomplete:
		return "IncompleteCapture", "Frames were dropped, so the capture understates the session",
			"Envelopes were dropped upstream, inferred from gaps in the per-session sequence numbers. Every other signal is then a floor rather than a total, because the dropped frames were never judged."
	}
	return string(signal), "An mcpsnoop check signal", "A signal reported by mcpsnoop check."
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

// sarifFingerprint is what lets a consumer recognise a finding across runs. It
// is built from what the finding is, never from where it landed in the file.
func sarifFingerprint(sessionID, ruleID, anchor string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + ruleID + "\x00" + anchor))
	return hex.EncodeToString(sum[:8])
}

// sarifArtifactURI turns the session log path into the URI a result points at.
// Code scanning resolves a relative URI against the root of the repository it
// analysed and refuses a result whose artifact it cannot locate, so a log under
// the working directory is reported relative to it. A log outside cannot
// honestly be given a relative path, so it gets an absolute file: URI, which
// will not render as an alert but also will not point at the wrong file.
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
