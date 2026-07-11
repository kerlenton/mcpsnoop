package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

type checkSignal string

const (
	checkError   checkSignal = "error"
	checkInvalid checkSignal = "invalid"
	checkWarn    checkSignal = "warn"
	checkSlow    checkSignal = "slow"
	checkPending checkSignal = "pending"
)

var checkSignalOrder = []checkSignal{checkError, checkInvalid, checkWarn, checkSlow, checkPending}

type checkSummary struct {
	sessionID string
	errors    int
	invalid   int
	warnings  int
	slow      int
	pending   int
}

func newCheckCmd() *cobra.Command {
	var failOn string
	var slowThreshold time.Duration
	cmd := &cobra.Command{
		Use:   "check [session-id|log.jsonl|-]",
		Short: "Fail when a captured session contains selected signals",
		Long:  "Check a captured session for errors, invalid frames, warnings, slow calls, or calls that never got a response. With no session, the newest session log is checked. Use - to read from stdin.",
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

			anyFailed := false
			for _, summary := range summarizeCheck(st, slowThreshold) {
				fmt.Fprintf(cmd.OutOrStdout(), "session %s: errors=%d invalid=%d warnings=%d slow=%d pending=%d\n",
					summary.sessionID, summary.errors, summary.invalid, summary.warnings, summary.slow, summary.pending)
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
	cmd.Flags().StringVar(&failOn, "fail-on", "error,invalid,warn", "comma-separated signals to fail on, any of error, invalid, warn, slow, pending")
	cmd.Flags().DurationVar(&slowThreshold, "slow-threshold", store.DefaultSlowThreshold, "a completed call longer than this counts as slow")
	return cmd
}

func parseCheckSignals(value string) (map[checkSignal]bool, error) {
	signals := make(map[checkSignal]bool)
	for _, part := range strings.Split(value, ",") {
		signal := checkSignal(strings.TrimSpace(part))
		switch signal {
		case checkError, checkInvalid, checkWarn, checkSlow, checkPending:
			signals[signal] = true
		default:
			return nil, fmt.Errorf("--fail-on must contain error, invalid, warn, slow, or pending, got %q", part)
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
// first session. A non-positive threshold falls back to the default, matching
// how the store treats it.
func summarizeCheck(st *store.Store, slowThreshold time.Duration) []checkSummary {
	if slowThreshold <= 0 {
		slowThreshold = store.DefaultSlowThreshold
	}
	var summaries []checkSummary
	for _, header := range st.Sessions() {
		summary := checkSummary{sessionID: header.ID, errors: header.Errors, pending: header.Pending}
		for _, event := range st.Timeline(header.ID) {
			if event.Kind == store.EventInvalid {
				summary.invalid++
			}
			if event.Warning != "" {
				summary.warnings++
			}
			if event.Kind == store.EventResponse && event.Call != nil && event.Call.Slow(slowThreshold) {
				summary.slow++
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func (s checkSummary) failed(selected map[checkSignal]bool) []string {
	counts := map[checkSignal]int{
		checkError:   s.errors,
		checkInvalid: s.invalid,
		checkWarn:    s.warnings,
		checkSlow:    s.slow,
		checkPending: s.pending,
	}
	var failed []string
	for _, signal := range checkSignalOrder {
		if selected[signal] && counts[signal] > 0 {
			failed = append(failed, string(signal))
		}
	}
	return failed
}
