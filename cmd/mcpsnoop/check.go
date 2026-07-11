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
	checkError   checkSignal = "error"
	checkInvalid checkSignal = "invalid"
	checkWarn    checkSignal = "warn"
)

var checkSignalOrder = []checkSignal{checkError, checkInvalid, checkWarn}

type checkSummary struct {
	sessionID string
	errors    int
	invalid   int
	warnings  int
}

func newCheckCmd() *cobra.Command {
	var failOn string
	cmd := &cobra.Command{
		Use:   "check [session-id|log.jsonl|-]",
		Short: "Fail when a captured session contains selected signals",
		Long:  "Check a captured session for errors, invalid frames, or warnings. With no session, the newest session log is checked. Use - to read from stdin.",
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
			st, sessionID, err := loadCheckSession(cmd, arg)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop check:", err)
				return exitCode(1)
			}

			summary := summarizeCheck(st, sessionID)
			fmt.Fprintf(cmd.OutOrStdout(), "session %s: errors=%d invalid=%d warnings=%d\n",
				summary.sessionID, summary.errors, summary.invalid, summary.warnings)

			failed := summary.failed(signals)
			if len(failed) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "check failed: %s\n", strings.Join(failed, ","))
				return exitCode(1)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "check passed")
			return nil
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&failOn, "fail-on", "error,invalid,warn", "comma-separated signals to fail on: error, invalid, warn")
	return cmd
}

func parseCheckSignals(value string) (map[checkSignal]bool, error) {
	signals := make(map[checkSignal]bool)
	for _, part := range strings.Split(value, ",") {
		signal := checkSignal(strings.TrimSpace(part))
		switch signal {
		case checkError, checkInvalid, checkWarn:
			signals[signal] = true
		default:
			return nil, fmt.Errorf("--fail-on must contain error, invalid, or warn, got %q", part)
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

func summarizeCheck(st *store.Store, sessionID string) checkSummary {
	summary := checkSummary{sessionID: sessionID}
	for _, header := range st.Sessions() {
		if header.ID == sessionID {
			summary.errors = header.Errors
			break
		}
	}
	for _, event := range st.Timeline(sessionID) {
		if event.Kind == store.EventInvalid {
			summary.invalid++
		}
		if event.Warning != "" {
			summary.warnings++
		}
	}
	return summary
}

func (s checkSummary) failed(selected map[checkSignal]bool) []string {
	counts := map[checkSignal]int{
		checkError:   s.errors,
		checkInvalid: s.invalid,
		checkWarn:    s.warnings,
	}
	var failed []string
	for _, signal := range checkSignalOrder {
		if selected[signal] && counts[signal] > 0 {
			failed = append(failed, string(signal))
		}
	}
	return failed
}
