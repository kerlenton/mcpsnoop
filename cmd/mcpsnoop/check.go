package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/toolbaseline"
)

type checkSignal string

const (
	checkError    checkSignal = "error"
	checkInvalid  checkSignal = "invalid"
	checkWarn     checkSignal = "warn"
	checkMismatch checkSignal = "mismatch"
	checkPending  checkSignal = "pending"
	checkDrift    checkSignal = "drift"
)

var checkSignalOrder = []checkSignal{checkError, checkInvalid, checkWarn, checkMismatch, checkPending, checkDrift}

type checkSummary struct {
	sessionID  string
	errors     int
	invalid    int
	warnings   int
	mismatches int
	pending    int
	drift      store.ToolDrift
}

func newCheckCmd() *cobra.Command {
	var failOn string
	cmd := &cobra.Command{
		Use:   "check [session-id|log.jsonl|-]",
		Short: "Fail when a captured session contains selected signals",
		Long:  "Check a captured session for errors, invalid frames, warnings, routing-header mismatches, calls that never got a response, or tool definitions that drifted from their trust-on-first-use baseline. With no session, the newest session log is checked. Use - to read from stdin.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			signals, err := parseCheckSignals(failOn)
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

			summaries, err := summarizeCheck(st, toolbaseline.New(paths.ToolBaselinesDir()))
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
				return exitCode(1)
			}
			anyFailed := false
			for _, summary := range summaries {
				fmt.Fprintf(cmd.OutOrStdout(), "session %s: errors=%d invalid=%d warnings=%d mismatches=%d pending=%d\n",
					summary.sessionID, summary.errors, summary.invalid, summary.warnings, summary.mismatches, summary.pending)
				if !summary.drift.Empty() {
					writeToolDrift(cmd.OutOrStdout(), summary.drift)
				}
				if failed := summary.failed(signals); len(failed) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "check failed: %s\n", strings.Join(failed, ","))
					anyFailed = true
				}
			}
			if anyFailed {
				return exitCode(1)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "check passed")
			return nil
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&failOn, "fail-on", "error,invalid,warn", "comma-separated signals to fail on, any of error, invalid, warn, mismatch, pending, drift")
	return cmd
}

func parseCheckSignals(value string) (map[checkSignal]bool, error) {
	signals := make(map[checkSignal]bool)
	for _, part := range strings.Split(value, ",") {
		signal := checkSignal(strings.TrimSpace(part))
		switch signal {
		case checkError, checkInvalid, checkWarn, checkMismatch, checkPending, checkDrift:
			signals[signal] = true
		default:
			return nil, fmt.Errorf("--fail-on must contain error, invalid, warn, mismatch, pending, or drift, got %q", part)
		}
	}
	return signals, nil
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
func summarizeCheck(st *store.Store, baselines *toolbaseline.Manager) ([]checkSummary, error) {
	var summaries []checkSummary
	for _, header := range st.Sessions() {
		summary := checkSummary{sessionID: header.ID, errors: header.Errors, pending: header.Pending}
		if _, ok := st.ToolDefinitions(header.ID); ok {
			report, _, err := toolbaseline.ObserveSession(baselines, st, header.ID)
			if err != nil {
				return nil, err
			}
			summary.drift = report
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
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func (s checkSummary) failed(selected map[checkSignal]bool) []string {
	counts := map[checkSignal]int{
		checkError:    s.errors,
		checkInvalid:  s.invalid,
		checkWarn:     s.warnings,
		checkMismatch: s.mismatches,
		checkPending:  s.pending,
		checkDrift:    s.drift.Count(),
	}
	var failed []string
	for _, signal := range checkSignalOrder {
		if selected[signal] && counts[signal] > 0 {
			failed = append(failed, string(signal))
		}
	}
	return failed
}
