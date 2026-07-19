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
			st, sessionID, err := loadCheckSession(cmd, arg)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop baseline:", err)
				return exitCode(1)
			}
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
	for _, change := range []struct {
		label string
		names []string
	}{
		{"added", report.AddedTools},
		{"removed", report.RemovedTools},
		{"description changed", report.ChangedDescriptions},
		{"schema changed", report.ChangedSchemas},
	} {
		if len(change.names) > 0 {
			fmt.Fprintf(w, "  %s: %s\n", change.label, strings.Join(change.names, ", "))
		}
	}
}
