package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

type checkSignal string

const (
	checkError    checkSignal = "error"
	checkInvalid  checkSignal = "invalid"
	checkWarn     checkSignal = "warn"
	checkMismatch checkSignal = "mismatch"
	checkPending  checkSignal = "pending"
)

var checkSignalOrder = []checkSignal{checkError, checkInvalid, checkWarn, checkMismatch, checkPending}

type checkOutputFormat string

const (
	checkFormatText  checkOutputFormat = "text"
	checkFormatJUnit checkOutputFormat = "junit"
)

type checkSummary struct {
	sessionID  string
	errors     int
	invalid    int
	warnings   int
	mismatches int
	pending    int
}

func newCheckCmd() *cobra.Command {
	var failOn, formatFlag string
	cmd := &cobra.Command{
		Use:   "check [session-id|log.jsonl|-]",
		Short: "Fail when a captured session contains selected signals",
		Long:  "Check a captured session for errors, invalid frames, warnings, routing-header mismatches, or calls that never got a response. With no session, the newest session log is checked. Use - to read from stdin.",
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

			anyFailed := false
			summaries := summarizeCheck(st)
			if format == checkFormatJUnit {
				if err := writeCheckJUnit(cmd.OutOrStdout(), summaries, signals); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
					return exitCode(1)
				}
				anyFailed = checkFailed(summaries, signals)
			} else {
				for _, summary := range summaries {
					fmt.Fprintf(cmd.OutOrStdout(), "session %s: errors=%d invalid=%d warnings=%d mismatches=%d pending=%d\n",
						summary.sessionID, summary.errors, summary.invalid, summary.warnings, summary.mismatches, summary.pending)
					if failed := summary.failed(signals); len(failed) > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "check failed: %s\n", strings.Join(failed, ","))
						anyFailed = true
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
	cmd.Flags().StringVar(&failOn, "fail-on", "error,invalid,warn", "comma-separated signals to fail on, any of error, invalid, warn, mismatch, pending")
	cmd.Flags().StringVar(&formatFlag, "format", string(checkFormatText), "output format, one of text or junit")
	return cmd
}

func parseCheckSignals(value string) (map[checkSignal]bool, error) {
	signals := make(map[checkSignal]bool)
	for _, part := range strings.Split(value, ",") {
		signal := checkSignal(strings.TrimSpace(part))
		switch signal {
		case checkError, checkInvalid, checkWarn, checkMismatch, checkPending:
			signals[signal] = true
		default:
			return nil, fmt.Errorf("--fail-on must contain error, invalid, warn, mismatch, or pending, got %q", part)
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
func summarizeCheck(st *store.Store) []checkSummary {
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
			if event.RoutingMismatch {
				summary.mismatches++
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
