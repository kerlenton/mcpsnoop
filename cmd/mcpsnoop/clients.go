package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/clientprofile"
	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/wiretext"
)

func newClientsCmd() *cobra.Command {
	var (
		since  string
		labels []string
		limit  int
		format string
	)
	cmd := &cobra.Command{
		Use:   "clients [--since 7d] [--label name] [--limit N] [--format text|json|markdown] [log.jsonl ...]",
		Short: "Show what each MCP client actually did on the wire",
		Long: "Client compatibility tables are written from documentation. clients is read from traffic. For every client that talked through mcpsnoop it reports the protocol revision it spoke, how it opened the conversation, what it declared it supports, and how it behaved when the server needed something back, through multi round-trip requests or the older server-initiated requests.\n\n" +
			"Sessions are grouped by the clientInfo each client sent, name and version, so two versions of one client stay apart. A client that never named itself is reported as unidentified rather than guessed at.\n\n" +
			"With no arguments it walks the sessions directory the way stats does. Name logs to read exactly those, which is how a published comparison is built from captures kept for the purpose. --format markdown prints one table with a column per client.\n\n" +
			"It reports and does not gate. Nothing is written, and the exit code is 0 whenever the read succeeded.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" && format != "markdown" {
				fmt.Fprintf(cmd.ErrOrStderr(), "mcpsnoop clients: invalid --format %q, want text, json or markdown\n", format)
				return exitCode(2)
			}
			// Named logs are the whole selection, so the flags that choose from the
			// directory would either be ignored in silence or mean something new.
			if len(args) > 0 && (cmd.Flags().Changed("since") || cmd.Flags().Changed("limit")) {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop clients: --since and --limit choose logs from the sessions directory and do not apply to named logs")
				return exitCode(2)
			}
			var cutoff time.Time
			if strings.TrimSpace(since) != "" {
				age, err := parseAge(since, "--since")
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop clients:", err)
					return exitCode(2)
				}
				cutoff = time.Now().Add(-age)
			}
			if limit < 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop clients: --limit must not be negative")
				return exitCode(2)
			}
			if cmd.Flags().Changed("label") && len(nonBlank(labels)) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop clients:", errBlankLabel)
				return exitCode(2)
			}

			var (
				rep clientsReport
				err error
			)
			if len(args) > 0 {
				rep, err = readNamedClients(args, labels)
			} else {
				rep, err = readClients(paths.SessionsDir(), cutoff, labels, limit)
			}
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop clients:", err)
				if errors.Is(err, errBlankLabel) {
					return exitCode(2)
				}
				return exitCode(1)
			}
			switch format {
			case "json":
				enc := jsonwire.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			case "markdown":
				return writeClientsMarkdown(cmd.OutOrStdout(), rep)
			default:
				return writeClientsText(cmd.OutOrStdout(), rep)
			}
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&since, "since", "", "only read logs modified within this window, e.g. 7d or 72h")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only read sessions carrying this label, repeat or comma-separated")
	cmd.Flags().IntVar(&limit, "limit", hub.DefaultBackfillLimit, "read at most this many of the newest logs, 0 for no bound")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text, json or markdown")
	return cmd
}

// clientsReport is the whole answer and how much was read to reach it.
type clientsReport struct {
	// Dir is set on a walk of the sessions directory and Logs when logs were
	// named, so the answer says which of the two it is.
	Dir     string   `json:"sessions_dir,omitempty"`
	Logs    []string `json:"logs,omitempty"`
	Read    int      `json:"read"`
	Total   int      `json:"total"`
	Empty   int      `json:"empty"`
	Skipped int      `json:"skipped"`
	// Sessions counts the sessions that had client traffic to report on.
	Sessions int                     `json:"sessions"`
	Clients  []clientprofile.Profile `json:"clients"`
}

// readClients walks the sessions directory with the same selection stats uses.
// One store is resident at a time, for the same reason.
func readClients(dir string, since time.Time, labels []string, limit int) (clientsReport, error) {
	logs, counts, unreadable, err := selectLogs(dir, since, labels, limit)
	if err != nil {
		return clientsReport{}, err
	}
	rep := clientsReport{Dir: dir, Total: counts.total, Empty: counts.empty, Skipped: counts.skipped + unreadable}
	var profiles []clientprofile.Profile
	for _, log := range logs {
		st, _, err := exporter.LoadFileTolerant(log.path)
		if err != nil {
			rep.Skipped++
			continue
		}
		headers := st.Sessions()
		if len(headers) == 0 {
			rep.Skipped++
			continue
		}
		rep.Read++
		for _, header := range headers {
			if p, ok := clientprofile.Fold(st, header); ok {
				profiles = append(profiles, p)
			}
		}
	}
	rep.Sessions = len(profiles)
	rep.Clients = clientprofile.Merge(profiles)
	return rep, nil
}

// readNamedClients reads exactly the logs it was given. A named log that cannot
// be read is an error rather than a skip, because the caller asked for it by
// name and a comparison quietly missing a column is worse than no comparison.
func readNamedClients(logs, labels []string) (clientsReport, error) {
	want := make(map[string]struct{}, len(labels))
	for _, l := range nonBlank(labels) {
		want[l] = struct{}{}
	}
	rep := clientsReport{Logs: logs, Total: len(logs)}
	var profiles []clientprofile.Profile
	for _, path := range logs {
		st, _, err := exporter.LoadFileTolerant(path)
		if err != nil {
			return clientsReport{}, fmt.Errorf("%s: %w", path, err)
		}
		rep.Read++
		for _, header := range st.Sessions() {
			if _, hit := want[header.Label]; len(want) > 0 && !hit {
				continue
			}
			if p, ok := clientprofile.Fold(st, header); ok {
				profiles = append(profiles, p)
			}
		}
	}
	rep.Sessions = len(profiles)
	rep.Clients = clientprofile.Merge(profiles)
	return rep, nil
}

// clientsTail states what the answer covers, in the words stats uses for a walk.
func clientsTail(rep clientsReport) string {
	if rep.Dir == "" {
		return fmt.Sprintf("read %s, %s with client traffic", plural(rep.Read, "named log"), plural(rep.Sessions, "session"))
	}
	return readTail(rollup{Dir: rep.Dir, Read: rep.Read, Total: rep.Total, Empty: rep.Empty, Skipped: rep.Skipped}) +
		fmt.Sprintf(", %s with client traffic", plural(rep.Sessions, "session"))
}

// writeClientsText prints one block per client. A terminal has room for the facts
// of one client on a line each and not for four clients side by side, which is
// what the markdown table is for.
func writeClientsText(w io.Writer, rep clientsReport) error {
	if len(rep.Clients) == 0 {
		_, err := fmt.Fprintf(w, "no client traffic found: %s\n", clientsTail(rep))
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\n", clientsTail(rep)); err != nil {
		return err
	}
	rows := clientprofile.Table(rep.Clients)
	labelW := 0
	for _, r := range rows {
		labelW = max(labelW, width(r.Label))
	}
	for i, p := range rep.Clients {
		// Every value below came off the wire, from a client name to a warning, so
		// each goes through wiretext.OneLine before it reaches a terminal.
		if _, err := fmt.Fprintf(w, "\n%s\n", wiretext.OneLine(p.Name())); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := fmt.Fprintf(w, "  %s  %s\n", pad(r.Label, labelW), wiretext.OneLine(r.Cells[i])); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeClientsMarkdown prints the comparison as one table with a column per
// client, which is the shape a published matrix takes.
func writeClientsMarkdown(w io.Writer, rep clientsReport) error {
	if len(rep.Clients) == 0 {
		_, err := fmt.Fprintf(w, "No client traffic found: %s.\n", clientsTail(rep))
		return err
	}
	var b strings.Builder
	b.WriteString("|  |")
	for _, p := range rep.Clients {
		b.WriteString(" " + markdownCell(p.Name()) + " |")
	}
	b.WriteString("\n|---|")
	for range rep.Clients {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, r := range clientprofile.Table(rep.Clients) {
		b.WriteString("| " + r.Label + " |")
		for _, c := range r.Cells {
			b.WriteString(" " + markdownCell(c) + " |")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nRead by `mcpsnoop clients` from %s.\n", plural(rep.Sessions, "session"))
	_, err := io.WriteString(w, b.String())
	return err
}

// markdownCell makes a wire value safe inside a table cell. A pipe would end the
// cell and a newline the row, and the quoting wiretext.OneLine does keeps an escape
// sequence from reaching whatever renders the page.
func markdownCell(s string) string {
	return strings.ReplaceAll(wiretext.OneLine(s), "|", `\|`)
}
