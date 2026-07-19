package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/paths"
)

func newPruneCmd() *cobra.Command {
	var olderThan string
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "prune --older-than <age>",
		Short: "Delete saved session logs older than a cutoff",
		Long: "Delete session logs under the mcpsnoop state directory whose modification time is older than --older-than, the same recency rule the history limit uses to pick what to load.\n\n" +
			"Deletion is never automatic: --older-than is required, --dry-run shows exactly what would go without removing anything, and a real run asks for confirmation unless --yes is given. Tool baselines are left alone, since a baseline is keyed by server label rather than by session.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cutoff, err := pruneCutoff(olderThan)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop prune:", err)
				return exitCode(2)
			}

			victims, total, err := prunableLogs(paths.SessionsDir(), cutoff)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop prune:", err)
				return exitCode(1)
			}

			out := cmd.OutOrStdout()
			if len(victims) == 0 {
				fmt.Fprintf(out, "nothing to prune: no session logs older than %s\n", olderThan)
				return nil
			}

			if dryRun {
				fmt.Fprintf(out, "would remove %d session log(s), %s:\n", len(victims), humanSize(total))
				for _, v := range victims {
					fmt.Fprintln(out, "  "+v)
				}
				return nil
			}

			if !yes {
				if !isTerminal(os.Stdin) {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop prune: refusing to delete without confirmation, pass --yes or run in a terminal")
					return exitCode(1)
				}
				fmt.Fprintf(out, "remove %d session log(s), %s? [y/N] ", len(victims), humanSize(total))
				if !confirmed(cmd.InOrStdin()) {
					fmt.Fprintln(out, "aborted")
					return nil
				}
			}

			removed := 0
			for _, v := range victims {
				if err := os.Remove(v); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "mcpsnoop prune: %v\n", err)
					continue
				}
				removed++
			}
			fmt.Fprintf(out, "removed %d session log(s), %s\n", removed, humanSize(total))
			return nil
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&olderThan, "older-than", "", "delete logs older than this age, e.g. 30d or 72h (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be removed without deleting anything")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// pruneCutoff turns an --older-than value into the cutoff time, before which a log
// is old enough to remove. time.ParseDuration rejects a day suffix, so a whole
// number of days is parsed here on top of the Go duration forms it does accept.
func pruneCutoff(olderThan string) (time.Time, error) {
	s := strings.TrimSpace(olderThan)
	if s == "" {
		return time.Time{}, errors.New("--older-than is required, e.g. 30d or 72h")
	}
	var age time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("invalid --older-than %q, want a whole number of days like 30d, or a Go duration like 72h", s)
		}
		age = time.Duration(n) * 24 * time.Hour
	} else {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return time.Time{}, fmt.Errorf("invalid --older-than %q, want a day count like 30d, or a Go duration like 72h", s)
		}
		age = d
	}
	return time.Now().Add(-age), nil
}

// prunableLogs returns the .jsonl session logs in dir older than cutoff and their
// combined size. Tool baselines live in a sibling directory, so they are untouched.
func prunableLogs(dir string, cutoff time.Time) (paths []string, total int64, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // the file went away between the listing and the stat
		}
		if info.ModTime().Before(cutoff) {
			paths = append(paths, filepath.Join(dir, e.Name()))
			total += info.Size()
		}
	}
	return paths, total, nil
}

func confirmed(r io.Reader) bool {
	line, _ := bufio.NewReader(r).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// isTerminal reports whether f is a character device, so prune only prompts when a
// user is actually there to answer, without pulling in a terminal dependency.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
