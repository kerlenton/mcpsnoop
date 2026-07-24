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
	checkDrift      checkSignal = "drift"
	checkDeprecated checkSignal = "deprecated"
)

var checkSignalOrder = []checkSignal{checkError, checkInvalid, checkWarn, checkMismatch, checkPending, checkDrift, checkDeprecated}

type checkOutputFormat string

const (
	checkFormatText  checkOutputFormat = "text"
	checkFormatJUnit checkOutputFormat = "junit"
)

type checkSummary struct {
	sessionID       string
	errors          int
	invalid         int
	warnings        int
	mismatches      int
	pending         int
	deprecated      int
	drift           store.ToolDrift
	baselineCreated bool
}

func newCheckCmd() *cobra.Command {
	var failOn, formatFlag string
	var baselineDir string
	var assertions checkAssertions
	cmd := &cobra.Command{
		Use:   "check [session-id|log.jsonl|-]",
		Short: "Fail when a captured session violates a signal or an assertion",
		Long:  "Check a captured session against signals (errors, invalid frames, warnings, routing-header mismatches, calls that never got a response, tool-definition drift) and assertions (a tool-call latency budget, and tools that must or must not have been called). With no session, the newest session log is checked. Use - to read from stdin.",
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
			st, _, err := loadCheckSession(cmd, arg)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
				return exitCode(1)
			}

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

			if format == checkFormatJUnit {
				if err := writeCheckJUnit(cmd.OutOrStdout(), summaries, signals, assertionFailures); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
					return exitCode(1)
				}
				if checkFailed(summaries, signals) {
					anyFailed = true
				}
			} else {
				for i, summary := range summaries {
					fmt.Fprintf(cmd.OutOrStdout(), "session %s: errors=%d invalid=%d warnings=%d mismatches=%d pending=%d deprecated=%d\n",
						summary.sessionID, summary.errors, summary.invalid, summary.warnings, summary.mismatches, summary.pending, summary.deprecated)
					if summary.baselineCreated {
						// No baseline existed, so this run trusted the current definitions
						// rather than verifying them. Say so, or an ephemeral CI reads green
						// while having checked nothing.
						fmt.Fprintln(cmd.OutOrStdout(), "recorded first-seen tool baseline (trusted, not verified)")
					}
					if summary.drift.BaselineError != "" {
						// A baseline problem is not itself drift, so report it plainly and let
						// it fail the run only when drift is the selected signal (see count).
						fmt.Fprintln(cmd.OutOrStdout(), "tool baseline error:", summary.drift.BaselineError)
					} else if !summary.drift.Empty() {
						writeToolDrift(cmd.OutOrStdout(), summary.drift)
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
	cmd.Flags().StringVar(&failOn, "fail-on", "error,invalid,warn", "comma-separated signals to fail on, any of error, invalid, warn, mismatch, pending, drift, deprecated")
	cmd.Flags().StringVar(&formatFlag, "format", string(checkFormatText), "output format, one of text or junit")
	cmd.Flags().StringVar(&baselineDir, "baseline", "", "tool-baseline directory to compare against (default: the mcpsnoop state dir); point CI at a persisted or checked-in directory")
	cmd.Flags().DurationVar(&assertions.maxDuration, "max-duration", 0, "fail if any completed tool call exceeds this duration (e.g. 500ms), disabled when zero")
	cmd.Flags().StringArrayVar(&assertions.expectTools, "expect-tool", nil, "fail if this tool was never called, repeatable")
	cmd.Flags().StringArrayVar(&assertions.forbidTools, "forbid-tool", nil, "fail if this tool was called, repeatable")
	return cmd
}

// checkAssertions holds the first-class check assertions, evaluated per session
// on top of the signal counts.
type checkAssertions struct {
	maxDuration time.Duration
	expectTools []string
	forbidTools []string
}

// eval returns one message per assertion the session violates, empty when all
// pass. A tool counts as called on any tools/call for it, whatever the outcome.
// The latency budget only judges calls that got a response, since a call that
// never did has no real latency and is the pending signal's job.
func (a checkAssertions) eval(st *store.Store, sessionID string) []string {
	if a.maxDuration <= 0 && len(a.expectTools) == 0 && len(a.forbidTools) == 0 {
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
			// Only a call that actually got a response has a real latency. Pending and
			// superseded calls never did, so they are not judged here.
			if !c.IsTool || !c.Done() || c.State == store.Superseded {
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
			callWord := "calls"
			if exceeded == 1 {
				callWord = "call"
			}
			failures = append(failures, fmt.Sprintf("%d tool %s exceeded the %s budget (worst: tool %q took %s)",
				exceeded, callWord, a.maxDuration, worstTool, worstDuration.Round(time.Millisecond)))
		}
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

func parseCheckSignals(value string) (map[checkSignal]bool, error) {
	signals := make(map[checkSignal]bool)
	for _, part := range strings.Split(value, ",") {
		signal := checkSignal(strings.TrimSpace(part))
		switch signal {
		case checkError, checkInvalid, checkWarn, checkMismatch, checkPending, checkDrift, checkDeprecated:
			signals[signal] = true
		default:
			return nil, fmt.Errorf("--fail-on must contain error, invalid, warn, mismatch, pending, drift, or deprecated, got %q", part)
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
	default:
		return "", fmt.Errorf("--format must be text or junit, got %q", value)
	}
}

func loadCheckSession(cmd *cobra.Command, arg string) (*store.Store, string, error) {
	if arg == "-" {
		return exporter.Load(cmd.InOrStdin(), "stdin")
	}
	path, err := exporter.ResolveSessionPath(arg)
	if err != nil {
		return nil, "", err
	}
	return exporter.LoadFile(path)
}

// summarizeCheck counts each signal for every session in the store, so a
// concatenated multi-session capture is gated as a whole rather than only its
// first session.
func summarizeCheck(st *store.Store, baselines *toolbaseline.Manager) []checkSummary {
	var summaries []checkSummary
	for _, header := range st.Sessions() {
		summary := checkSummary{sessionID: header.ID, errors: header.Errors, pending: header.Pending}
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
	case checkDrift:
		// A baseline error is not drift, but it means drift could not be verified,
		// so count it as a drift failure for a run that selected the drift signal.
		n := s.drift.Count()
		if s.drift.BaselineError != "" {
			n++
		}
		return n
	case checkDeprecated:
		return s.deprecated
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
