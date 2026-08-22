package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func newStatsCmd() *cobra.Command {
	var (
		since  string
		labels []string
		limit  int
		format string
	)
	cmd := &cobra.Command{
		Use:   "stats [--since 7d] [--label name] [--limit N] [--format text|json]",
		Short: "Fold every stored capture into one row per server and tool",
		Long: "check reads one session and diff reads exactly two, so a tool that fails one run in four stays invisible until somebody opens the captures by hand. stats walks the sessions directory and pools the per-tool numbers the store already computes.\n\n" +
			"Tool execution errors and protocol errors are counted apart, because the specification makes them different findings: a tool answering isError is reporting something a model can act on, and a JSON-RPC error is the request or the server being wrong.\n\n" +
			"Percentiles are computed over the pooled raw durations. A median of medians is a median of nothing. One multi round-trip operation counts as one call with one duration, however many requests it took.\n\n" +
			"Rows are keyed on the server, which is the recorded command and working directory for stdio and the endpoint for HTTP, not on the label. Two servers routinely derive one label, and pooling them produces a figure that is true of neither.\n\n" +
			"It reports and does not gate. Nothing is written, no baseline is touched, and the exit code is 0 whenever the walk succeeded.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				fmt.Fprintf(cmd.ErrOrStderr(), "mcpsnoop stats: invalid --format %q, want text or json\n", format)
				return exitCode(2)
			}
			var cutoff time.Time
			if strings.TrimSpace(since) != "" {
				age, err := parseAge(since, "--since")
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop stats:", err)
					return exitCode(2)
				}
				cutoff = time.Now().Add(-age)
			}
			if limit < 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop stats: --limit must not be negative")
				return exitCode(2)
			}
			// Asked of the flag rather than of its value. pflag parses --label "" into
			// no values at all, which is indistinguishable from never passing the flag,
			// so a filter that names nothing would otherwise report the whole directory
			// under a flag that says it narrowed the answer.
			if cmd.Flags().Changed("label") && len(nonBlank(labels)) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop stats:", errBlankLabel)
				return exitCode(2)
			}

			roll, err := rollUp(paths.SessionsDir(), cutoff, labels, limit)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop stats:", err)
				// A flag that names nothing is the caller's mistake, not the directory's,
				// so it exits like every other usage error rather than like a read failure.
				if errors.Is(err, errBlankLabel) {
					return exitCode(2)
				}
				return exitCode(1)
			}
			if format == "json" {
				return writeStatsJSON(cmd.OutOrStdout(), roll)
			}
			return writeStatsText(cmd.OutOrStdout(), roll)
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&since, "since", "", "only read logs modified within this window, e.g. 7d or 72h")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only read sessions carrying this label, repeat or comma-separated")
	cmd.Flags().IntVar(&limit, "limit", hub.DefaultBackfillLimit, "read at most this many of the newest logs, 0 for no bound")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	return cmd
}

// errBlankLabel is --label given with nothing in it, which must not fall through
// to meaning no filter at all.
var errBlankLabel = errors.New("--label was given but names nothing")

// nonBlank drops the empty and whitespace-only entries of a repeated flag.
func nonBlank(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// toolRow is one server and tool pair as the rollup reports it.
type toolRow struct {
	Key string `json:"-"`
	// Server is the name to show. Labels holds every distinct one the sessions of
	// this server carried, which differ only when someone passed --label, since
	// the key is the command rather than the name.
	Server string   `json:"server"`
	Labels []string `json:"labels,omitempty"`
	// Command, CWD and Endpoint are the identity the row is actually keyed on,
	// spelled the way inventory spells them. Without them two servers that derive
	// one label print as two identical lines and marshal to two identical objects,
	// so a consumer indexing on server and tool keeps one and drops the other, and
	// the reason the rows are separate is invisible in the place it matters most.
	Command  []string `json:"command,omitempty"`
	CWD      string   `json:"cwd,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
	Tool     string   `json:"tool"`
	Calls    int      `json:"calls"`
	// ToolErrors is result.isError, the tool reporting a failure a model can act
	// on. ProtocolErrors is a JSON-RPC error, the request or the server being
	// wrong. FailureRate is both over Calls.
	ToolErrors     int     `json:"tool_errors"`
	ProtocolErrors int     `json:"protocol_errors"`
	FailureRate    float64 `json:"failure_rate"`
	// Sessions counted this tool, FailedSessions saw it fail at least once. The
	// pair answers "one run in ten", which a rate over calls cannot.
	Sessions       int     `json:"sessions"`
	FailedSessions int     `json:"failed_sessions"`
	P50MS          float64 `json:"p50_ms"`
	P95MS          float64 `json:"p95_ms"`
	P99MS          float64 `json:"p99_ms"`
	// DefinitionBytes is what advertising this tool costs, from the most recent
	// session that listed it. Zero when no session in the window did.
	DefinitionBytes int `json:"definition_bytes"`

	// serverKey is the identity half of Key, without the tool name, so the writer
	// can ask how many servers share a label rather than how many rows do.
	serverKey string
	labels    map[string]struct{}
	durations []time.Duration
	sessions  map[string]struct{}
	failed    map[string]struct{}
}

// rollup is the whole answer, including how much of the directory it read.
type rollup struct {
	Dir string `json:"sessions_dir"`
	// Read is how many logs were folded in and Total is how many the directory
	// held, so a bounded answer never passes for a complete one. Empty counts the
	// zero-byte logs a failed exec or an uncalled HTTP proxy leaves, and Skipped
	// the ones that could not be read, kept apart for the same reason inventory
	// keeps them apart: one is housekeeping and the other is damage.
	Read    int       `json:"read"`
	Total   int       `json:"total"`
	Empty   int       `json:"empty"`
	Skipped int       `json:"skipped"`
	Rows    []toolRow `json:"rows"`
}

// afterFold runs once per folded capture. It is a variable so a test can sample
// how much the walk is holding while it is still walking: the claim here is
// about peak, and peak is invisible from outside a call that has already
// returned, where every store it dropped is collectable anyway.
var afterFold = func() {}

// rollUp folds every selected log into one row per server and tool.
//
// One store is resident at a time. A log is loaded, its calls are folded into
// the running aggregate, and the store is dropped before the next one opens, so
// peak memory is the largest single capture plus the counters rather than the
// whole directory.
func rollUp(dir string, since time.Time, labels []string, limit int) (rollup, error) {
	logs, counts, err := sessionLogs(dir, since, 0)
	if err != nil {
		return rollup{}, err
	}
	// A --label of nothing but blanks is not "no filter". The command catches the
	// case pflag can express as an empty slice; this catches a caller handing in a
	// slice of blanks directly.
	if len(labels) > 0 && len(nonBlank(labels)) == 0 {
		return rollup{}, errBlankLabel
	}
	want := make(map[string]struct{}, len(labels))
	for _, l := range nonBlank(labels) {
		want[l] = struct{}{}
	}

	// A label is on the first envelope, so it is answered from the head rather
	// than by loading a whole capture and discarding it. That also makes --limit
	// mean the newest logs of the selected server rather than the newest logs
	// overall, which is what somebody asking for both is after: --label x --limit 5
	// should be five of x, not however many of x happen to be in the newest five.
	unreadable := 0
	if len(want) > 0 {
		kept := logs[:0]
		for _, log := range logs {
			head, err := readSessionHead(log.path)
			if err != nil {
				// Counted here, because a log dropped in the pre-pass never reaches the
				// fold that would otherwise count it. Saying nothing made --label report
				// a clean directory that a plain run reported as holding a damaged log.
				unreadable++
				continue
			}
			if _, hit := want[head.label]; hit {
				kept = append(kept, log)
			}
		}
		logs = kept
	}
	if limit > 0 && len(logs) > limit {
		logs = logs[:limit]
	}

	roll := rollup{Dir: dir, Total: counts.total, Empty: counts.empty, Skipped: counts.skipped + unreadable}
	rows := make(map[string]*toolRow)
	// Definition costs live beside the rows rather than on them, because a session
	// can advertise a tool it never calls and the newest description of a tool is
	// the one worth reporting whichever session made it.
	costs := make(map[string]int)
	// Oldest first, so the definition cost a row reports is the newest one seen
	// rather than whichever log the listing reached last.
	for _, log := range slices.Backward(logs) {
		st, _, err := exporter.LoadFileTolerant(log.path)
		if err != nil {
			roll.Skipped++
			continue
		}
		// Every session in the file, not only the one its first envelope names. A
		// log holding several is not exotic: concatenating captures is what the
		// issue's own reproduction does, and the store already separates them, so
		// folding one and dropping the rest loses calls without saying a word.
		headers := st.Sessions()
		if len(headers) == 0 {
			roll.Skipped++
			continue
		}
		roll.Read++
		for _, header := range headers {
			foldSession(rows, costs, st, header.ID, header)
		}
		afterFold()
	}

	roll.Rows = make([]toolRow, 0, len(rows))
	for key, row := range rows {
		row.DefinitionBytes = costs[key]
		row.Labels = slices.Sorted(maps.Keys(row.labels))
		row.Server = strings.Join(row.Labels, ", ")
		if row.Server == "" {
			row.Server = "(unlabelled)"
		}
		row.Sessions = len(row.sessions)
		row.FailedSessions = len(row.failed)
		if row.Calls > 0 {
			row.FailureRate = float64(row.ToolErrors+row.ProtocolErrors) / float64(row.Calls) * 100
		}
		// Pooled and sorted once, here. Combining per-session percentiles would be
		// an average of medians, which describes no distribution.
		slices.Sort(row.durations)
		row.P50MS = percentileMS(row.durations, 0.50)
		row.P95MS = percentileMS(row.durations, 0.95)
		row.P99MS = percentileMS(row.durations, 0.99)
		roll.Rows = append(roll.Rows, *row)
	}
	slices.SortFunc(roll.Rows, func(a, b toolRow) int {
		// Worst first, and a broken server outranks a tool reporting a domain
		// failure rather than sharing one bucket with it.
		if c := cmp.Compare(b.ProtocolErrors, a.ProtocolErrors); c != 0 {
			return c
		}
		if c := cmp.Compare(b.ToolErrors, a.ToolErrors); c != 0 {
			return c
		}
		if c := cmp.Compare(b.P50MS, a.P50MS); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Server, b.Server); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Tool, b.Tool); c != 0 {
			return c
		}
		// Last resort, and it has to be total. Two servers deriving one label with
		// one tool and identical figures tie on every column above, and map order
		// then decided the output, so two runs over an unchanged directory printed
		// the rows in different orders.
		return cmp.Compare(a.Key, b.Key)
	})
	return roll, nil
}

// foldSession adds one capture's tool calls to the running aggregate.
//
// The calls come from the store rather than from the raw JSONL, because the
// store is what knows a multi round-trip retry continues an operation rather
// than starting one. Keying on the JSON-RPC id instead would count one logical
// operation as several and feed its wall clock in more than once, which is the
// failure docs/2026-07-28-mrtr-breaks-latency.md exists about.
func foldSession(rows map[string]*toolRow, costs map[string]int, st *store.Store, sessionID string, header store.SessionHeader) {
	command, cwd, _ := st.Command(sessionID)
	id := serverIdentity{
		label: header.Label, transport: header.Transport,
		command: command, cwd: cwd, endpoint: header.Endpoint,
	}
	// The label joins the identity rather than replacing it. Alone it pools two
	// servers that derive one name, which is what the identity is for. Left out, it
	// pools one command deliberately run as prod and again as staging, and smears
	// two deployments into one distribution. Both are the same mistake, so both
	// halves key.
	server := id.key() + "\x00" + header.Label

	// Recorded per advertised tool rather than inside the call loop. A session
	// that listed a tool without calling it still knows what advertising it costs,
	// and gating the write on a call meant the figure came from the newest session
	// that used the tool rather than the newest that described it. Sessions arrive
	// oldest first, so the last write wins and that is the newest description.
	if cost, ok := st.ToolCosts(sessionID); ok {
		for _, per := range cost.PerTool {
			costs[server+"\x00"+per.Name] = per.Bytes
		}
	}

	for _, call := range st.Calls(sessionID) {
		if !call.IsTool || call.ToolName == "" {
			continue
		}
		key := server + "\x00" + call.ToolName
		row := rows[key]
		if row == nil {
			row = &toolRow{
				Key: key, serverKey: server, Tool: call.ToolName,
				Command: command, CWD: cwd, Endpoint: header.Endpoint,
				labels:   make(map[string]struct{}),
				sessions: make(map[string]struct{}),
				failed:   make(map[string]struct{}),
			}
			rows[key] = row
		}
		if header.Label != "" {
			row.labels[header.Label] = struct{}{}
		}
		row.sessions[sessionID] = struct{}{}
		row.Calls++
		switch {
		case call.ToolErr:
			row.ToolErrors++
			row.failed[sessionID] = struct{}{}
		case call.Errored:
			row.ProtocolErrors++
			row.failed[sessionID] = struct{}{}
		}
		// A call still open, superseded, or cancelled without a late result has no
		// round trip to measure. It counts as a call, since it was made, and
		// contributes no latency, which is the rule the per-session summary uses.
		//
		// Superseded needs naming separately, because it passes both other tests.
		// A client reusing an id while the earlier request is in flight makes the
		// store stamp the older call's end with the newer request's timestamp, so
		// Done() is true and Duration() is the gap between two unrelated requests.
		// Folding that in reported an hour-long round trip on a server whose slowest
		// real answer was twenty milliseconds.
		if d := call.Duration(); call.Done() && call.State != store.Superseded && d > 0 {
			row.durations = append(row.durations, d)
		}
	}
}

// percentileMS is the nearest-rank percentile of a sorted duration slice, in
// milliseconds, matching what the per-session summary reports.
func percentileMS(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted))*p+0.999999) - 1
	return float64(sorted[min(max(index, 0), len(sorted)-1)]) / float64(time.Millisecond)
}

func writeStatsJSON(w io.Writer, roll rollup) error {
	enc := jsonwire.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(roll)
}

func writeStatsText(w io.Writer, roll rollup) error {
	if len(roll.Rows) == 0 {
		_, err := fmt.Fprintf(w, "no tool calls found: %s\n", readTail(roll))
		return err
	}

	names := serverNames(roll.Rows)
	tools := make([]string, len(roll.Rows))
	serverW := len("SERVER")
	toolW := len("TOOL")
	for i, row := range roll.Rows {
		tools[i] = oneLine(row.Tool)
		serverW = max(serverW, width(names[i]))
		toolW = max(toolW, width(tools[i]))
	}
	// Capped, because neither is mcpsnoop's to choose. A tool name is whatever the
	// server advertised and a command is whatever installed it, so one absurd
	// value would otherwise pad every row in the table to its width.
	serverW = min(serverW, maxNameW)
	toolW = min(toolW, maxNameW)

	if _, err := fmt.Fprintf(w, "%s\n\n", readTail(roll)); err != nil {
		return err
	}
	header := fmt.Sprintf("%s  %s  %6s %5s %6s  %7s  %9s  %9s %9s %9s  %7s",
		pad("SERVER", serverW), pad("TOOL", toolW), "CALLS", "ERR", "PROTO", "FAIL%", "SESS", "p50", "p95", "p99", "DEF")
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	for i, row := range roll.Rows {
		def := "-"
		if row.DefinitionBytes > 0 {
			def = fmt.Sprintf("%dB", row.DefinitionBytes)
		}
		line := fmt.Sprintf("%s  %s  %6d %5d %6d  %6.1f%%  %9s  %9s %9s %9s  %7s",
			pad(elide(names[i], serverW), serverW), pad(elide(tools[i], toolW), toolW),
			row.Calls, row.ToolErrors, row.ProtocolErrors, row.FailureRate,
			fmt.Sprintf("%d/%d", row.FailedSessions, row.Sessions),
			msLabel(row.P50MS), msLabel(row.P95MS), msLabel(row.P99MS), def)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	// The two error columns only teach themselves once, and the difference is the
	// whole reason they are two columns.
	_, err := fmt.Fprint(w, "\nERR is result.isError, the tool reporting a failure a model can act on."+
		"\nPROTO is a JSON-RPC error, the request or the server being wrong."+
		"\nSESS is sessions that saw a failure over sessions that called the tool.\n")
	return err
}

// maxNameW bounds the two free-text columns. Eight bytes short of forty leaves
// the fixed columns aligned on an eighty-column terminal for ordinary names.
const maxNameW = 32

// serverNames renders the SERVER cell of each row, qualified only where it has
// to be.
//
// A row is keyed on the recorded command and working directory, or on the
// endpoint, and shows the label. When two rows share a label that is the whole
// point of keying on something else, and printing the label alone made them two
// identical lines, which reads as one row printed twice rather than as the two
// servers it is. Those rows carry their working directory or endpoint; every
// other row is untouched, so the common table looks as it did.
func serverNames(rows []toolRow) []string {
	byLabel := make(map[string]map[string]struct{}, len(rows))
	for _, row := range rows {
		if byLabel[row.Server] == nil {
			byLabel[row.Server] = make(map[string]struct{})
		}
		// Servers, not rows. Keying on the row counted one server's two tools as two
		// servers and qualified a name that was never ambiguous.
		byLabel[row.Server][row.serverKey] = struct{}{}
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		name := oneLine(row.Server)
		if len(byLabel[row.Server]) > 1 {
			switch {
			case row.CWD != "":
				name += " (" + oneLine(row.CWD) + ")"
			case row.Endpoint != "":
				name += " (" + oneLine(row.Endpoint) + ")"
			case len(row.Command) > 0:
				name += " (" + oneLine(strings.Join(row.Command, " ")) + ")"
			}
		}
		out[i] = name
	}
	return out
}

// elide trims a value to the column it has to fit in, keeping the tail, because
// what distinguishes two long paths or two long tool names is at the end of them.
func elide(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w < 2 {
		return s
	}
	return "…" + string(r[len(r)-(w-1):])
}

// width and pad measure and align in runes rather than bytes.
//
// fmt's %-*s counts bytes, so a name holding anything outside ASCII, including
// the ellipsis elide adds, padded its cell short and pushed every column to its
// right out of line. Runes are not display cells either, and a table of CJK will
// still sit a little wide, but a server name or a tool name is overwhelmingly
// ASCII with the occasional accent, and for those this is exact.
func width(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, w int) string {
	if n := w - width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// readTail states how much of the directory the answer covers, so a window never
// passes for the whole record.
func readTail(roll rollup) string {
	out := fmt.Sprintf("read %s of %d in %s", plural(roll.Read, "log"), roll.Total, roll.Dir)
	if roll.Empty > 0 {
		out += fmt.Sprintf(", %s left by a run that recorded nothing", plural(roll.Empty, "empty log"))
	}
	if roll.Skipped > 0 {
		out += fmt.Sprintf(", %s skipped as unreadable", plural(roll.Skipped, "log"))
	}
	return out
}

// msLabel renders a duration column. A tool whose calls never completed has no
// latency rather than a zero one, and printing 0ms would read as instant.
func msLabel(ms float64) string {
	switch {
	case ms == 0:
		return "-"
	case ms < 10:
		return fmt.Sprintf("%.1fms", ms)
	case ms < 10_000:
		return fmt.Sprintf("%.0fms", ms)
	// Past ten seconds the millisecond is noise and the digits overflow a column
	// sized for them, which pushed every cell to its right out of line. A half-hour
	// reindex is a real answer and has to fit beside a five-millisecond ping.
	case ms < 600_000:
		return fmt.Sprintf("%.1fs", ms/1000)
	default:
		return fmt.Sprintf("%.1fmin", ms/60_000)
	}
}
