package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func newInventoryCmd() *cobra.Command {
	var format string
	var withTools bool
	cmd := &cobra.Command{
		Use:   "inventory [--tools] [--format text|json]",
		Short: "List every MCP server that has run through mcpsnoop on this machine",
		Long: "Fold the saved session logs into one row per server rather than one per session, reporting what launched it, from where, when it first and last ran, and how many times.\n\n" +
			"The row key is the recorded command and working directory, not the label, because the label is derived from the command's last path element and two different servers routinely produce the same one. An HTTP session is keyed on the endpoint it proxied instead, since mcpsnoop launched nothing.\n\n" +
			"Reading is one envelope per log, the meta frame the proxy writes first, so this stays cheap over a large sessions directory. --tools is the exception and reads each log in full to count what the server last advertised.\n\n" +
			"Local and read only. It reports what ran on this machine through mcpsnoop, and knows nothing about servers it never wrapped. A run with --trace-file wrote outside the sessions directory and will not appear, and prune deletes logs, so first seen is only ever as old as what is still on disk.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				fmt.Fprintf(cmd.ErrOrStderr(), "mcpsnoop inventory: invalid --format %q, want text or json\n", format)
				return exitCode(2)
			}
			dir := paths.SessionsDir()
			inv, err := takeInventory(dir, withTools)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop inventory:", err)
				return exitCode(1)
			}
			if format == "json" {
				return writeInventoryJSON(cmd.OutOrStdout(), inv)
			}
			return writeInventoryText(cmd.OutOrStdout(), inv, withTools)
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().BoolVar(&withTools, "tools", false, "also count the tools each server last advertised, which reads every log in full")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	return cmd
}

// serverRow is one server as the inventory reports it, folded from every session
// that shares its key.
type serverRow struct {
	// Key is what makes two sessions the same server. It is never printed.
	Key string `json:"-"`
	// Label is the name to show, and Labels holds every distinct one the runs of
	// this server carried. They differ when someone passed --label, since the key
	// is the command and directory: one server run once as prod and once as
	// staging is one server, and showing only the first name would quietly drop
	// the other.
	Label     string   `json:"label"`
	Labels    []string `json:"labels,omitempty"`
	Transport string   `json:"transport,omitempty"`
	Command   []string `json:"command,omitempty"`
	CWD       string   `json:"cwd,omitempty"`
	// Endpoint is the MCP endpoint of an HTTP server, already stripped of
	// userinfo, query values and the fragment by the proxy that recorded it.
	Endpoint string `json:"endpoint,omitempty"`
	Runs     int    `json:"runs"`
	// Redacted marks a row whose command is not the command that ran, because a
	// --redact rule rewrote part of it before it was written down.
	Redacted  bool      `json:"redacted,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Tools is what the server advertised the last time it ran, and is filled in
	// only under --tools. A nil pointer means there is no count, and ToolsNote
	// then says why, so "not asked for", "asked and the server advertised none",
	// "asked and the listing never finished" and "asked and the log could not be
	// read" are four answers rather than one absent field.
	Tools     *int   `json:"tools,omitempty"`
	ToolsNote string `json:"tools_note,omitempty"`
	// newestLog is the log of the most recent run, which is the only one --tools
	// reads. Held here so the count happens once per server after the fold rather
	// than once per session that was briefly the newest.
	newestLog string
	// sawMeta records that at least one of this server's logs opened with a meta
	// frame, so a row with neither a command nor an endpoint can say whether the
	// frame was missing or was there and carried nothing usable.
	sawMeta bool
	labels  map[string]struct{}
}

// inventory is the whole answer, including what it could not read. A skipped log
// is reported rather than dropped: a zero-byte file is what a run whose exec
// failed leaves behind, and a reader comparing session counts deserves to know
// the difference between "nothing ran" and "something ran and left nothing
// readable".
type inventory struct {
	Dir     string `json:"sessions_dir"`
	Scanned int    `json:"scanned"`
	// Empty counts zero-byte logs and Skipped counts the rest. They are separate
	// because an empty log is what correct operation leaves behind, twice over: a
	// run whose exec failed, and an HTTP proxy started and never called, which
	// holds its meta frame back on purpose so it does not hijack a bare check or
	// export. Calling those unreadable would report ordinary housekeeping as
	// damage.
	Empty   int         `json:"empty"`
	Skipped int         `json:"skipped"`
	Servers []serverRow `json:"servers"`
}

// sessionHead is what the first envelope of a log says about the run it opens.
type sessionHead struct {
	label     string
	transport string
	command   []string
	cwd       string
	endpoint  string
	// redacted comes from the meta frame alone, never from a traffic frame. The
	// note it drives is about the command, and a log whose head is traffic has no
	// command to qualify, so carrying the flag across would print "part of this
	// was rewritten" under a row that shows nothing to have rewritten.
	redacted bool
	meta     bool
	started  time.Time
}

// takeInventory folds every readable log in dir into one row per server. A log it
// cannot read is counted and stepped over, never fatal, because an unreadable one
// among many says nothing about the rest.
func takeInventory(dir string, withTools bool) (inventory, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return inventory{}, err
	}
	inv := inventory{Dir: dir}
	rows := make(map[string]*serverRow)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// Stat before opening. The sessions directory is an ordinary directory that
		// anything can write to, and os.Open on a fifo blocks in open(2) until a
		// writer appears, which hung the whole command before it printed a line. A
		// stat follows a symlink, so a link to a real log still works and a link to
		// a fifo is skipped along with the fifo itself.
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			inv.Skipped++
			continue
		}
		if info.Size() == 0 {
			inv.Empty++
			continue
		}
		head, err := readSessionHead(path)
		if err != nil {
			inv.Skipped++
			continue
		}
		inv.Scanned++
		row := rows[head.key()]
		if row == nil {
			row = &serverRow{
				Key: head.key(), Label: head.label, Transport: head.transport,
				Command: head.command, CWD: head.cwd, Endpoint: head.endpoint,
				FirstSeen: head.started, LastSeen: head.started,
				labels: make(map[string]struct{}),
			}
			rows[head.key()] = row
		}
		row.Runs++
		row.sawMeta = row.sawMeta || head.meta
		if head.label != "" {
			row.labels[head.label] = struct{}{}
		}
		// Redaction is sticky across runs. One scrubbed capture is enough to make
		// the command shown here not the command that ran, and saying so on only
		// some of a server's runs would be saying it nowhere a reader looks.
		row.Redacted = row.Redacted || head.redacted
		// A zero timestamp is not an early one. An envelope with no ts decodes to
		// the zero instant, which is before everything, so taking it as the minimum
		// would date every run of the server to year one.
		if !head.started.IsZero() && (row.FirstSeen.IsZero() || head.started.Before(row.FirstSeen)) {
			row.FirstSeen = head.started
		}
		if head.started.After(row.LastSeen) || row.Runs == 1 {
			row.LastSeen = head.started
			row.newestLog = path
		}
	}

	inv.Servers = make([]serverRow, 0, len(rows))
	for _, row := range rows {
		row.Labels = slices.Sorted(maps.Keys(row.labels))
		if len(row.Labels) > 0 {
			row.Label = row.Labels[0]
		}
		// The count is what the server advertised the last time it ran, so exactly
		// one log per server is read, after the fold has settled which one that is.
		// Doing it during the fold would read every log that was briefly the newest
		// and throw all but one away.
		if withTools {
			n, status := countTools(row.newestLog)
			switch status {
			case toolsCounted:
				row.Tools = &n
			case toolsIncomplete:
				row.ToolsNote = "the newest run has no complete tools/list"
			case toolsUnreadable:
				row.ToolsNote = "the newest run's log could not be read past its head"
			}
		}
		inv.Servers = append(inv.Servers, *row)
	}
	// Sorted by name and then by key rather than by recency, so running this twice
	// with nothing new produces the same bytes and a diff between two machines or
	// two dates shows what actually differs rather than what merely ran again.
	slices.SortFunc(inv.Servers, func(a, b serverRow) int {
		if c := cmp.Compare(a.Label, b.Label); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return inv, nil
}

// openLog opens one session log. It is a variable so a test can observe how much
// of a file the walk actually pulls, which is the one thing the whole design
// rests on and which no assertion about the returned rows can see: json.Decoder
// stops at the end of the first value however many bytes it was handed, so a
// reader that slurped the file whole first would answer identically.
var openLog = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// countTools is the only read that goes past a log's head, and is a variable for
// the same reason: nothing about the rows says whether it ran, so a test that
// needs to know the plain path left it alone has to be told.
var countTools = countAdvertisedTools

// readSessionHead reads the first envelope of a log and nothing else. The proxy
// writes the meta frame first on both transports, so one decode answers what a
// run was, and json.Decoder stops at the end of that value rather than reading
// the file. A log large enough to matter is exactly the log worth not reading.
func readSessionHead(path string) (sessionHead, error) {
	f, err := openLog(path)
	if err != nil {
		return sessionHead{}, err
	}
	defer f.Close()

	var env proxy.Envelope
	if err := json.NewDecoder(f).Decode(&env); err != nil {
		return sessionHead{}, err
	}
	if env.SessionID == "" {
		return sessionHead{}, fmt.Errorf("%s: first envelope names no session", path)
	}
	head := sessionHead{
		label:     env.ServerLabel,
		transport: env.Transport,
		started:   env.TS,
	}
	if env.Direction != proxy.DirectionMeta {
		// A log whose first frame is traffic predates the meta frame on its
		// transport, or had its head trimmed. It still names a server that ran, so
		// it gets a row from what it does carry rather than vanishing, which for
		// HTTP is every log captured before mcpsnoop recorded an endpoint at all.
		return head, nil
	}
	var meta proxy.SessionMeta
	if err := json.Unmarshal(env.Raw, &meta); err != nil {
		return sessionHead{}, err
	}
	head.command, head.cwd, head.endpoint = meta.Command, meta.CWD, meta.Target
	head.redacted, head.meta = env.Redacted, true
	return head, nil
}

// key folds the head into the identity every command in this binary shares, so
// what counts as one server is decided once. See serverIdentity in sessionscan.go.
func (h sessionHead) key() string {
	return serverIdentity{
		label: h.label, transport: h.transport,
		command: h.command, cwd: h.cwd, endpoint: h.endpoint,
	}.key()
}

// toolCountStatus distinguishes the three ways --tools can fail to produce a
// number, because one sentence for all of them makes mcpsnoop state something
// false. A log that could not be read is not a server that advertised nothing,
// and printing the second when the first happened is a wrong answer rather than
// a missing one.
type toolCountStatus int

const (
	toolsCounted toolCountStatus = iota
	toolsIncomplete
	toolsUnreadable
)

// countAdvertisedTools reports how many tools a session's server advertised, and
// is the only part of the inventory that reads past the first envelope, which is
// why it is behind a flag.
//
// A partial tools/list is not a count: the server was still paginating, so
// reporting what arrived would understate it, and there is no honest number to
// print. That is toolsIncomplete. A log the decoder could not finish is
// toolsUnreadable, which is a different thing and says so.
//
// The store is bounded because the answer does not need the frames. A tool
// inventory and whether its listing completed are session state the store folds
// in at ingest and eviction does not touch, so a hundred-megabyte log is read
// through a fixed window rather than held whole to extract one integer. The
// bound is generous enough that nothing here is even close to it, and exists so
// that walking every server on a machine cannot be sunk by whichever log happens
// to be the largest.
func countAdvertisedTools(path string) (int, toolCountStatus) {
	const (
		toolCountBodyBytes = 8 << 20
		toolCountFrames    = 50_000
	)
	st, sessionID, err := exporter.LoadFileTolerantBounded(path, toolCountBodyBytes, toolCountFrames)
	if err != nil {
		return 0, toolsUnreadable
	}
	definitions, complete := st.ToolDefinitions(sessionID)
	if !complete {
		return 0, toolsIncomplete
	}
	return len(definitions), toolsCounted
}

func writeInventoryJSON(w io.Writer, inv inventory) error {
	enc := jsonwire.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(inv)
}

func writeInventoryText(w io.Writer, inv inventory, withTools bool) error {
	if len(inv.Servers) == 0 {
		_, err := fmt.Fprintf(w, "no servers found in %s%s\n", inv.Dir, unreadTail(inv))
		return err
	}

	header := fmt.Sprintf("%s across %s in %s%s",
		plural(len(inv.Servers), "server"), plural(inv.Scanned, "session"), inv.Dir, unreadTail(inv))
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}

	for _, row := range inv.Servers {
		name := oneLine(row.Label)
		if name == "" {
			name = "(unlabelled)"
		}
		if len(row.Labels) > 1 {
			names := make([]string, 0, len(row.Labels))
			for _, l := range row.Labels {
				names = append(names, oneLine(l))
			}
			name = strings.Join(names, ", ")
		}
		transport := oneLine(row.Transport)
		if transport == "" {
			transport = "unknown"
		}
		if _, err := fmt.Fprintf(w, "\n%s  %s  %s\n", name, transport, plural(row.Runs, "run")); err != nil {
			return err
		}
		switch {
		case len(row.Command) > 0:
			if err := field(w, "command", renderCommand(row.Command)); err != nil {
				return err
			}
			if row.CWD != "" {
				if err := field(w, "cwd", row.CWD); err != nil {
					return err
				}
			}
		case row.Endpoint != "":
			if err := field(w, "endpoint", row.Endpoint); err != nil {
				return err
			}
		case row.sawMeta:
			if err := field(w, "command", "not recorded, the meta frame of this log names neither a command nor an endpoint"); err != nil {
				return err
			}
		default:
			if err := field(w, "command", "not recorded, this log has no meta frame"); err != nil {
				return err
			}
		}
		if row.Redacted {
			if err := field(w, "redacted", "a --redact rule rewrote part of this, so it is not the command that ran"); err != nil {
				return err
			}
		}
		if withTools {
			count := row.ToolsNote
			if row.Tools != nil {
				count = plural(*row.Tools, "tool")
			}
			if err := field(w, "tools", count); err != nil {
				return err
			}
		}
		if err := field(w, "seen", seenRange(row)); err != nil {
			return err
		}
	}
	return nil
}

// unreadTail names what the walk could not fold in, so a reader comparing
// session counts is never left to guess. Empty logs are named separately from
// unreadable ones because an empty one is the ordinary residue of a failed exec
// or of an HTTP proxy nobody called, not damage.
func unreadTail(inv inventory) string {
	var parts []string
	if inv.Empty > 0 {
		parts = append(parts, plural(inv.Empty, "empty log")+" left by a run that recorded nothing")
	}
	if inv.Skipped > 0 {
		parts = append(parts, plural(inv.Skipped, "log")+" skipped as unreadable")
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

// seenRange renders when a server first and last ran, both ends in the reader's
// own zone. Rendering each end in the offset its own log recorded made a range
// print backwards across a daylight-saving change, since 03:10+02:00 is later
// than 03:30+03:00 and reads earlier.
func seenRange(row serverRow) string {
	first := row.FirstSeen.Local().Format(time.RFC3339)
	if last := row.LastSeen.Local().Format(time.RFC3339); last != first {
		return first + " to " + last
	}
	return first
}

// renderCommand joins argv for a reader without losing where one argument ends
// and the next begins. An element holding a space is ordinary, since
// `node "~/My Project/build/index.js"` is how half the world installs a server,
// and joined plainly it is indistinguishable from two arguments.
func renderCommand(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		if arg == "" || strings.ContainsAny(arg, " \t\"'\\") || hasControl(arg) {
			parts = append(parts, strconv.Quote(arg))
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

// field renders one labelled line of a server block, padded so the values line
// up under each other.
func field(w io.Writer, name, value string) error {
	_, err := fmt.Fprintf(w, "  %-9s %s\n", name, oneLine(value))
	return err
}

// oneLine keeps a value the log supplied from ending the line it is printed on.
//
// A command argument, a working directory and a derived label are written by
// whatever installed the server rather than by mcpsnoop, and none of them is
// checked for control characters on the way in: labelFor only trims at the last
// separator, and a working directory comes off the filesystem, so a directory
// whose name holds a newline is enough. Printed raw, that newline closes the
// field and every following line reads as a fresh server block, so a baseline
// somebody diffs against names servers that never ran while the header above it
// still counts one. An escape sequence in the same position drives the terminal.
//
// paths.CheckLabel refuses a control character for this reason and the TUI drops
// them for it too. Quoting rather than dropping keeps the value recoverable.
func oneLine(s string) string {
	if hasControl(s) {
		return strconv.Quote(s)
	}
	return s
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// plural renders a count with its noun, so a summary reads as a sentence rather
// than as a number and a noun that disagree with it.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
