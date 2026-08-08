package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/toolbaseline"
)

// resolveBaselineDir returns the tool-baseline directory to use: an explicit
// --baseline override, or the mcpsnoop state directory. CI points it at a
// persisted or checked-in directory so a baseline survives across runs.
func resolveBaselineDir(dir string) string {
	if dir != "" {
		return dir
	}
	return paths.ToolBaselinesDir()
}

func newBaselineCmd() *cobra.Command {
	var accept, reset bool
	var baselineDir string
	cmd := &cobra.Command{
		Use:   "baseline [session-id|log.jsonl|-]",
		Short: "Inspect or update the trusted tool-definition baseline",
		Long:  "Compare a captured session's complete tools/list definition set with its server-label baseline. Use --accept after a legitimate change, or --reset to remove the baseline so the next observation becomes trusted.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if accept && reset {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop baseline: --accept and --reset are mutually exclusive")
				return exitCode(2)
			}
			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			// baseline reports tools, never lines, so it never pays for the index.
			sessionLog, err := loadCheckSession(cmd, arg, false)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop baseline:", err)
				return exitCode(1)
			}
			st, sessionID := sessionLog.store, sessionLog.sessionID
			manager := toolbaseline.New(resolveBaselineDir(baselineDir))
			switch {
			case accept:
				server, err := toolbaseline.AcceptSession(manager, st, sessionID)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop baseline:", err)
					return exitCode(1)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "accepted baseline for %s\n", server)
				return nil
			case reset:
				server, err := toolbaseline.ResetSession(manager, st, sessionID)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop baseline:", err)
					return exitCode(1)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "reset baseline for %s\n", server)
				return nil
			default:
				report, created, err := toolbaseline.ObserveSession(manager, st, sessionID)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop baseline:", err)
					return exitCode(1)
				}
				if created {
					fmt.Fprintln(cmd.OutOrStdout(), "created first-seen tool baseline")
					return nil
				}
				// Before the Empty branch, so a clean run still says which fields it
				// could not check rather than reading as a full all-clear.
				writeUnverifiedCoverage(cmd.OutOrStdout(), report)
				if report.Empty() {
					fmt.Fprintln(cmd.OutOrStdout(), "no tool definition drift")
					return nil
				}
				writeToolDrift(cmd.OutOrStdout(), report)
				return exitCode(1)
			}
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().BoolVar(&accept, "accept", false, "replace the baseline with this session's complete tool definitions")
	cmd.Flags().BoolVar(&reset, "reset", false, "remove the baseline for this session's server label")
	cmd.Flags().StringVar(&baselineDir, "baseline", "", "tool-baseline directory (default: the mcpsnoop state dir)")
	return cmd
}

func writeToolDrift(w io.Writer, report store.ToolDrift) {
	fmt.Fprintln(w, "definition drift:")
	// Looped over store.ToolDriftKinds so a kind cannot be counted by the gate and
	// then go unprinted here, leaving a failing run with nothing to act on.
	for _, kind := range store.ToolDriftKinds {
		if names := report.Names(kind); len(names) > 0 {
			fmt.Fprintf(w, "  %s: %s\n", driftLabel(kind), strings.Join(names, ", "))
		}
	}
}

// driftLabel is the per-line phrasing, singular, since it precedes tool names.
func driftLabel(kind store.ToolDriftKind) string {
	switch kind {
	case store.DriftToolAdded:
		return "added"
	case store.DriftToolRemoved:
		return "removed"
	case store.DriftDescription:
		return "description changed"
	case store.DriftInputSchema:
		return "input schema changed"
	case store.DriftTitle:
		return "title changed"
	case store.DriftOutputSchema:
		return "output schema changed"
	case store.DriftAnnotations:
		return "annotations changed"
	case store.DriftIcons:
		return "icons changed"
	}
	return string(kind)
}

// writeUnverifiedCoverage reports the fields this baseline has no record of, so
// a clean run does not read as "everything checked out" when part of the
// definition was never compared.
func writeUnverifiedCoverage(w io.Writer, report store.ToolDrift) {
	if len(report.Unverified) == 0 {
		return
	}
	var kinds []string
	for _, kind := range report.Unverified {
		kinds = append(kinds, string(kind))
	}
	fmt.Fprintf(w, "note: this baseline predates %s tracking; those fields were not compared. "+
		"Re-record with --accept once you trust the current definitions.\n", strings.Join(kinds, ", "))
}
