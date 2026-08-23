package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/toolbaseline"
)

type checkSignal string

const (
	checkError      checkSignal = "error"
	checkInvalid    checkSignal = "invalid"
	checkWarn       checkSignal = "warn"
	checkMismatch   checkSignal = "mismatch"
	checkPending    checkSignal = "pending"
	checkLateResult checkSignal = "late-result"
	checkDrift      checkSignal = "drift"
	checkDeprecated checkSignal = "deprecated"
	checkIncomplete checkSignal = "incomplete"
	checkSchema     checkSignal = "schema"
)

var checkSignalOrder = []checkSignal{checkError, checkInvalid, checkWarn, checkMismatch, checkPending, checkLateResult, checkDrift, checkDeprecated, checkIncomplete, checkSchema}

type checkOutputFormat string

const (
	checkFormatText  checkOutputFormat = "text"
	checkFormatJUnit checkOutputFormat = "junit"
	checkFormatSARIF checkOutputFormat = "sarif"
)

type checkSummary struct {
	sessionID       string
	errors          int
	invalid         int
	warnings        int
	mismatches      int
	pending         int
	lateResults     int
	deprecated      int
	missingFrames   uint64
	drift           store.ToolDrift
	schema          store.SchemaReport
	baselineCreated bool
}

func newCheckCmd() *cobra.Command {
	var failOn, formatFlag string
	var baselineDir string
	var assertions checkAssertions
	cmd := &cobra.Command{
		Use:   "check [session-id|log.jsonl|-]",
		Short: "Fail when a captured session violates a signal or an assertion",
		Long:  "Check a captured session against signals (errors, invalid frames, warnings, routing-header mismatches, calls that never got a response, results that arrived after cancellation, dropped frames that leave the capture incomplete, tool-definition drift) and assertions (a tool-call latency budget, and tools that must or must not have been called). With no session, the newest session log is checked. Use - to read from stdin.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			signals, err := parseCheckSignals(failOn)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
				return exitCode(2)
			}
			format, err := parseCheckOutputFormat(formatFlag)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
				return exitCode(2)
			}

			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			sessionLog, err := loadCheckSession(cmd, arg, format.needsLineIndex())
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
				// 2, not 1, and the difference is the whole contract a CI wrapper
				// rests on. 1 means the check ran and something failed the gate, so
				// a wrapper reports the findings and carries on. This is the other
				// thing: a path that is not there, a file that is not a session log.
				// Sharing an exit code with a real finding lets a typo in a workflow
				// read as a checked run, which is the one outcome a gate must never
				// produce.
				return exitCode(2)
			}
			st := sessionLog.store

			summaries := summarizeCheck(st, toolbaseline.New(resolveBaselineDir(baselineDir)))
			anyFailed := false

			// Assertions are format independent, so evaluate them once and let each format
			// render them. Evaluating them inside the text branch only would leave the junit
			// path neither reporting an assertion failure nor failing the run on one.
			assertionFailures := make([][]string, len(summaries))
			for i, summary := range summaries {
				assertionFailures[i] = assertions.eval(st, summary.sessionID)
				if len(assertionFailures[i]) > 0 {
					anyFailed = true
				}
			}

			switch format {
			case checkFormatJUnit:
				if err := writeCheckJUnit(cmd.OutOrStdout(), summaries, signals, assertionFailures); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
					// The report never reached the reader, so no verdict was delivered
					// whatever the traffic held. Same bucket as a log that would not load.
					return exitCode(2)
				}
				if checkFailed(summaries, signals) {
					anyFailed = true
				}
			case checkFormatSARIF:
				if err := writeCheckSARIF(cmd.OutOrStdout(), sessionLog, summaries, signals, assertionFailures); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
					return exitCode(2)
				}
				if checkFailed(summaries, signals) {
					anyFailed = true
				}
			default:
				for i, summary := range summaries {
					fmt.Fprintf(cmd.OutOrStdout(), "session %s: errors=%d invalid=%d warnings=%d mismatches=%d pending=%d late_results=%d deprecated=%d missing_frames=%d schema_findings=%d\n",
						summary.sessionID, summary.errors, summary.invalid, summary.warnings, summary.mismatches, summary.pending, summary.lateResults, summary.deprecated, summary.missingFrames, summary.schema.Count())
					if summary.baselineCreated {
						// No baseline existed, so this run trusted the current definitions
						// rather than verifying them. Say so, or an ephemeral CI reads green
						// while having checked nothing.
						fmt.Fprintln(cmd.OutOrStdout(), "recorded first-seen tool baseline (trusted, not verified)")
					}
					writeUnverifiedCoverage(cmd.OutOrStdout(), summary.drift)
					if summary.drift.BaselineError != "" {
						// A baseline problem is not itself drift, so report it plainly and let
						// it fail the run only when drift is the selected signal (see count).
						fmt.Fprintln(cmd.OutOrStdout(), "tool baseline error:", summary.drift.BaselineError)
					} else if !summary.drift.Empty() {
						writeToolDrift(cmd.OutOrStdout(), summary.drift)
					}
					if !summary.schema.Empty() {
						writeSchemaFindings(cmd.OutOrStdout(), summary.schema)
					}
					if failed := summary.failed(signals); len(failed) > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "check failed: %s\n", strings.Join(failed, ","))
						anyFailed = true
					}
					// Assertions carry their own message, so report each one.
					for _, msg := range assertionFailures[i] {
						fmt.Fprintln(cmd.OutOrStdout(), "assertion failed:", msg)
					}
				}
			}
			if anyFailed {
				return exitCode(1)
			}
			if format == checkFormatText {
				fmt.Fprintln(cmd.OutOrStdout(), "check passed")
			}
			return nil
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&failOn, "fail-on", "error,invalid,warn", "comma-separated signals to fail on, any of error, invalid, warn, mismatch, pending, late-result, drift, deprecated, incomplete, schema")
	cmd.Flags().StringVar(&formatFlag, "format", string(checkFormatText), "output format, one of text, junit, or sarif")
	cmd.Flags().StringVar(&baselineDir, "baseline", "", "tool-baseline directory to compare against (default: the mcpsnoop state dir); point CI at a persisted or checked-in directory")
	cmd.Flags().DurationVar(&assertions.maxDuration, "max-duration", 0, "fail if any completed tool call exceeds this wall-clock duration, including time a user spent answering an elicitation (e.g. 500ms), disabled when zero")
	cmd.Flags().DurationVar(&assertions.maxServerDuration, "max-server-duration", 0, "fail if the server's own share of any completed tool call exceeds this duration, excluding time the client took to answer, disabled when zero")
	cmd.Flags().IntVar(&assertions.maxRoundTrips, "max-round-trips", 0, "fail if any tool call took more than this many requests, which multi round-trip requests make possible, disabled when zero")
	cmd.Flags().StringArrayVar(&assertions.expectTools, "expect-tool", nil, "fail if this tool was never called, repeatable")
	cmd.Flags().StringArrayVar(&assertions.forbidTools, "forbid-tool", nil, "fail if this tool was called, repeatable")
	return cmd
}

// checkAssertions holds the first-class check assertions, evaluated per session
// on top of the signal counts.
type checkAssertions struct {
	// maxDuration is wall clock for the whole interaction and deliberately stays
	// that way. Under multi round-trip requests one tool call is several requests
	// and the seconds a person spent answering are inside the span, which is the
	// point: that interval is the one you most want to see. Redefining this flag
	// to measure only the server would quietly loosen every pipeline that already
	// sets it, so the two figures underneath get flags of their own that say what
	// they measure.
	maxDuration       time.Duration
	maxServerDuration time.Duration
	maxRoundTrips     int
	expectTools       []string
	forbidTools       []string
}

// eval returns one message per assertion the session violates, empty when all
// pass. A tool counts as called on any tools/call for it, whatever the outcome.
// The latency budget only judges calls that got a response, since a call that
// never did has no real latency and is the pending signal's job.
func (a checkAssertions) eval(st *store.Store, sessionID string) []string {
	if a.maxDuration <= 0 && a.maxServerDuration <= 0 && a.maxRoundTrips <= 0 &&
		len(a.expectTools) == 0 && len(a.forbidTools) == 0 {
		return nil
	}
	calls := st.Calls(sessionID)
	called := make(map[string]bool)
	for _, c := range calls {
		if c.IsTool && c.ToolName != "" {
			called[c.ToolName] = true
		}
	}

	var failures []string
	if a.maxDuration > 0 {
		exceeded := 0
		var worstDuration time.Duration
		var worstTool string
		for _, c := range calls {
			if !c.IsTool || !c.Done() || c.State == store.Superseded || (c.State == store.Cancelled && !c.LateResult) {
				continue
			}
			duration := c.Duration()
			if duration <= a.maxDuration {
				continue
			}
			exceeded++
			if duration > worstDuration {
				worstDuration = duration
				worstTool = c.ToolName
			}
		}
		if exceeded > 0 {
			failures = append(failures, fmt.Sprintf("%d tool %s exceeded the %s budget (worst: tool %q took %s)",
				exceeded, callWord(exceeded), a.maxDuration, worstTool, worstDuration.Round(time.Millisecond)))
		}
	}
	if a.maxServerDuration > 0 || a.maxRoundTrips > 0 {
		failures = append(failures, a.evalInteractions(st, sessionID)...)
	}
	for _, name := range a.expectTools {
		if !called[name] {
			failures = append(failures, fmt.Sprintf("expected tool %q was never called", name))
		}
	}
	for _, name := range a.forbidTools {
		if called[name] {
			failures = append(failures, fmt.Sprintf("forbidden tool %q was called", name))
		}
	}
	return failures
}

// evalInteractions judges the two figures underneath a wall-clock duration, the
// share the server held the operation for and how many requests it took.
//
// Both are read off frame timestamps and a link mcpsnoop already inferred, so
// neither guesses at intent, which is what a check has to be able to say. A tool
// call that never completed is skipped for the same reason --max-duration skips
// it: an operation still open has no latency to judge, and the pending signal is
// what reports it.
func (a checkAssertions) evalInteractions(st *store.Store, sessionID string) []string {
	var failures []string
	slowest, slowestTool, slowCount := time.Duration(0), "", 0
	mostTrips, tripsTool, tripsCount := 0, "", 0
	for _, in := range st.Interactions(sessionID) {
		if in.ToolName == "" {
			continue
		}
		// A round trip count needs no ending. Every request of the chain has already
		// happened, so a runaway one is countable while it is still running, and
		// that is the case the budget exists for: a server asking again and again is
		// exactly the operation nobody ever finishes. Skipping it made the assertion
		// silent on its own headline case.
		if a.maxRoundTrips > 0 && in.RoundTrips > a.maxRoundTrips {
			tripsCount++
			if in.RoundTrips > mostTrips {
				mostTrips, tripsTool = in.RoundTrips, in.ToolName
			}
		}
		// A latency does need one, which is the rule --max-duration already applies.
		// An operation still open has no round trip to judge, a superseded one was
		// never answered, and a cancelled one without a late result delivered
		// nothing. The pending signal is what reports those.
		if !in.Measurable() {
			continue
		}
		if a.maxServerDuration > 0 && in.ServerTime > a.maxServerDuration {
			slowCount++
			if in.ServerTime > slowest {
				slowest, slowestTool = in.ServerTime, in.ToolName
			}
		}
	}
	if slowCount > 0 {
		failures = append(failures, fmt.Sprintf("%d tool %s exceeded the %s server budget (worst: tool %q held for %s)",
			slowCount, callWord(slowCount), a.maxServerDuration, slowestTool, slowest.Round(time.Millisecond)))
	}
	if tripsCount > 0 {
		failures = append(failures, fmt.Sprintf("%d tool %s exceeded the %d round trip budget (worst: tool %q took %d)",
			tripsCount, callWord(tripsCount), a.maxRoundTrips, tripsTool, mostTrips))
	}
	return failures
}

// callWord picks the noun for a count, so a failure line reads as a sentence.
func callWord(n int) string {
	if n == 1 {
		return "call"
	}
	return "calls"
}

func parseCheckSignals(value string) (map[checkSignal]bool, error) {
	signals := make(map[checkSignal]bool)
	for _, part := range strings.Split(value, ",") {
		signal := checkSignal(strings.TrimSpace(part))
		switch signal {
		case checkError, checkInvalid, checkWarn, checkMismatch, checkPending, checkLateResult, checkDrift, checkDeprecated, checkIncomplete, checkSchema:
			signals[signal] = true
		default:
			return nil, fmt.Errorf("--fail-on must contain error, invalid, warn, mismatch, pending, late-result, drift, deprecated, incomplete, or schema, got %q", part)
		}
	}
	return signals, nil
}

func parseCheckOutputFormat(value string) (checkOutputFormat, error) {
	switch checkOutputFormat(strings.ToLower(strings.TrimSpace(value))) {
	case checkFormatText:
		return checkFormatText, nil
	case checkFormatJUnit:
		return checkFormatJUnit, nil
	case checkFormatSARIF:
		return checkFormatSARIF, nil
	default:
		return "", fmt.Errorf("--format must be text, junit, or sarif, got %q", value)
	}
}

// needsLineIndex reports whether a format points a reader at one line of the
// log, which is the only thing the per-frame index is for. It costs a map entry
// per captured frame held for the whole run, so a format that reports per
// session rather than per frame must not pay for it.
func (f checkOutputFormat) needsLineIndex() bool { return f == checkFormatSARIF }

// checkLog is a loaded session log plus where it came from. A SARIF result has
// to point at a file and a line, which neither the text nor the junit format
// ever needed, so the path and the per-frame line index travel beside the store.
// Both are empty for a stdin run, which has no artifact to point at.
type checkLog struct {
	store     *store.Store
	sessionID string
	path      string
	lines     exporter.FrameLines
}

// loadCheckSession reads the log a check or baseline run judges. withLines asks
// for the per-frame line index, which only a caller that has to name a line
// needs and which retains one map entry per captured frame until the run ends.
func loadCheckSession(cmd *cobra.Command, arg string, withLines bool) (checkLog, error) {
	if arg == "-" {
		st, sessionID, err := exporter.Load(cmd.InOrStdin(), "stdin")
		if err != nil {
			return checkLog{}, err
		}
		return checkLog{store: st, sessionID: sessionID}, nil
	}
	path, err := exporter.ResolveSessionPath(arg)
	if err != nil {
		return checkLog{}, err
	}
	if !withLines {
		st, sessionID, err := exporter.LoadFile(path)
		if err != nil {
			return checkLog{}, err
		}
		return checkLog{store: st, sessionID: sessionID, path: path}, nil
	}
	st, sessionID, lines, err := exporter.LoadFileLines(path)
	if err != nil {
		return checkLog{}, err
	}
	return checkLog{store: st, sessionID: sessionID, path: path, lines: lines}, nil
}

// summarizeCheck counts each signal for every session in the store, so a
// concatenated multi-session capture is gated as a whole rather than only its
// first session.
func summarizeCheck(st *store.Store, baselines *toolbaseline.Manager) []checkSummary {
	var summaries []checkSummary
	for _, header := range st.Sessions() {
		summary := checkSummary{
			sessionID:     header.ID,
			errors:        header.Errors,
			pending:       header.Pending,
			lateResults:   header.LateResults,
			missingFrames: header.MissingFrames,
		}
		if _, ok := st.ToolDefinitions(header.ID); ok {
			report, created, err := toolbaseline.ObserveSession(baselines, st, header.ID)
			if err != nil {
				// Drift is opt-in, so a corrupt baseline or a missing server label is
				// recorded per session rather than failing the whole command. It gates
				// the run only when drift is the selected signal.
				summary.drift = store.ToolDrift{BaselineError: err.Error()}
			} else {
				summary.drift = report
				summary.baselineCreated = created
			}
		}
		if report, ok := st.SchemaFindings(header.ID); ok {
			summary.schema = report
		}
		for _, event := range st.Timeline(header.ID) {
			if event.Kind == store.EventInvalid {
				summary.invalid++
			}
			if event.Warning != "" {
				summary.warnings++
			}
			if event.RoutingMismatch {
				summary.mismatches++
			}
			if event.Deprecated != "" {
				summary.deprecated++
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func checkFailed(summaries []checkSummary, selected map[checkSignal]bool) bool {
	for _, summary := range summaries {
		if len(summary.failed(selected)) > 0 {
			return true
		}
	}
	return false
}

// driftUnverified says why this run compared nothing against the tool baseline,
// and is empty when it did compare. Two causes, one consequence. A baseline that
// would not load is one. A baseline recorded for the first time is the other, and
// it is the ordinary state of an ephemeral CI, where the state directory starts
// empty on every run and every run is therefore a first run.
//
// The text and junit formats have always said so out loud. Saying so was not
// enough: a run that asked to fail on drift and then verified nothing still
// passed, which is exactly the reading the baseline mechanism exists to prevent.
// So a selected drift signal fails on either cause. Nothing changes for a run
// that did not select it.
func (s checkSummary) driftUnverified() string {
	if s.drift.BaselineError != "" {
		return "tool baseline error: " + s.drift.BaselineError
	}
	if s.baselineCreated {
		return "recorded first-seen tool baseline, so no tool definition was verified; " +
			"point --baseline at a directory that survives between runs"
	}
	return ""
}

func (s checkSummary) count(signal checkSignal) int {
	switch signal {
	case checkError:
		return s.errors
	case checkInvalid:
		return s.invalid
	case checkWarn:
		return s.warnings
	case checkMismatch:
		return s.mismatches
	case checkPending:
		return s.pending
	case checkLateResult:
		return s.lateResults
	case checkDrift:
		// Not being able to verify is not the same as having verified and found
		// nothing, so it counts for a run that selected the drift signal.
		n := s.drift.Count()
		if s.driftUnverified() != "" {
			n++
		}
		return n
	case checkDeprecated:
		return s.deprecated
	case checkIncomplete:
		return int(s.missingFrames)
	case checkSchema:
		return s.schema.Count()
	default:
		return 0
	}
}

func (s checkSummary) failed(selected map[checkSignal]bool) []string {
	var failed []string
	for _, signal := range checkSignalOrder {
		if selected[signal] && s.count(signal) > 0 {
			failed = append(failed, string(signal))
		}
	}
	return failed
}
