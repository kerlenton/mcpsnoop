package main

import (
	"fmt"
	"math"
	"time"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/sessiondiff"
)

func newDiffCmd() *cobra.Command {
	var durationThreshold time.Duration
	var durationRatio float64
	cmd := &cobra.Command{
		Use:   "diff <session-id|log.jsonl> <session-id|log.jsonl>",
		Short: "Compare tools and calls across two captured sessions",
		Long:  "Compare two captured sessions for added or removed tools, input schema changes, call status changes, and notable duration shifts.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if durationThreshold < 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop diff: --duration-threshold must not be negative")
				return exitCode(2)
			}
			if durationRatio < 1 || math.IsNaN(durationRatio) || math.IsInf(durationRatio, 0) {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop diff: --duration-ratio must be finite and at least 1")
				return exitCode(2)
			}

			before, err := loadDiffSession(args[0])
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop diff:", err)
				return exitCode(1)
			}
			after, err := loadDiffSession(args[1])
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop diff:", err)
				return exitCode(1)
			}

			report := sessiondiff.Compare(before, after, sessiondiff.Options{
				DurationThreshold: durationThreshold,
				DurationRatio:     durationRatio,
			})
			if err := sessiondiff.WriteText(cmd.OutOrStdout(), report); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop diff:", err)
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().DurationVar(&durationThreshold, "duration-threshold", sessiondiff.DefaultDurationThreshold, "minimum absolute duration change to report")
	cmd.Flags().Float64Var(&durationRatio, "duration-ratio", sessiondiff.DefaultDurationRatio, "minimum slowdown or speedup ratio to report")
	return cmd
}

func loadDiffSession(arg string) (exporter.SessionExport, error) {
	path, err := exporter.ResolveSessionPath(arg)
	if err != nil {
		return exporter.SessionExport{}, err
	}
	st, sessionID, err := exporter.LoadFile(path)
	if err != nil {
		return exporter.SessionExport{}, err
	}
	return exporter.Build(st, sessionID)
}
