package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func executeInventory(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	cmd := newInventoryCmd()
	cmd.SetArgs(args)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var code exitCode
	if !errors.As(err, &code) {
		t.Fatalf("unexpected command error: %v", err)
	}
	return int(code), stdout.String(), stderr.String()
}

// writeSessionLog writes one log whose lines are the given envelopes, the way the
// proxy's sink does, and returns its path.
func writeSessionLog(t *testing.T, name string, envs ...proxy.Envelope) string {
	t.Helper()
	path := filepath.Join(paths.SessionsDir(), name)
	var buf bytes.Buffer
	for _, env := range envs {
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stdioSession builds the frames of one stdio run, meta frame first.
func stdioSession(t *testing.T, id, label string, at time.Time, cwd string, command []string, redacted bool) []proxy.Envelope {
	t.Helper()
	meta, err := json.Marshal(proxy.SessionMeta{Command: command, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return []proxy.Envelope{{
		SessionID: id, ServerLabel: label, Seq: 1, TS: at,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio,
		Raw: meta, Redacted: redacted,
	}}
}

// TestInventoryFoldsSessionsIntoServers is the whole point of the command: one
// row per server across many runs, not one per session.
func TestInventoryFoldsSessionsIntoServers(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "a-1.jsonl", stdioSession(t, "s1", "server.py", t0, "/proj/a", []string{"python3", "server.py"}, false)...)
	writeSessionLog(t, "a-2.jsonl", stdioSession(t, "s2", "server.py", t0.Add(2*time.Hour), "/proj/a", []string{"python3", "server.py"}, false)...)

	inv, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 1 {
		t.Fatalf("servers = %d, want 1; two runs of one server are one row", len(inv.Servers))
	}
	row := inv.Servers[0]
	if row.Runs != 2 {
		t.Fatalf("runs = %d, want 2", row.Runs)
	}
	if !row.FirstSeen.Equal(t0) || !row.LastSeen.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("seen %v to %v, want %v to %v", row.FirstSeen, row.LastSeen, t0, t0.Add(2*time.Hour))
	}
	if inv.Scanned != 2 || inv.Skipped != 0 {
		t.Fatalf("scanned %d skipped %d, want 2 and 0", inv.Scanned, inv.Skipped)
	}
}

// TestInventoryKeysOnCommandAndCWDNotLabel is the reason the row key is what it
// is. labelFor derives the label from the command's last path element, so two
// unrelated servers routinely arrive under one name.
func TestInventoryKeysOnCommandAndCWDNotLabel(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// Same derived label three times over: different arguments, and the same
	// arguments from a different directory.
	writeSessionLog(t, "a.jsonl", stdioSession(t, "s1", "server.py", t0, "/proj/a", []string{"python3", "server.py", "alpha"}, false)...)
	writeSessionLog(t, "b.jsonl", stdioSession(t, "s2", "server.py", t0, "/proj/a", []string{"python3", "server.py", "beta"}, false)...)
	writeSessionLog(t, "c.jsonl", stdioSession(t, "s3", "server.py", t0, "/proj/b", []string{"python3", "server.py", "alpha"}, false)...)

	inv, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 3 {
		t.Fatalf("servers = %d, want 3; the label is one name for three servers", len(inv.Servers))
	}
	for _, row := range inv.Servers {
		if row.Label != "server.py" {
			t.Fatalf("label = %q, want the derived one shown as a name", row.Label)
		}
	}
}

// TestInventoryCoversHTTPSessions covers the population the issue is about, plus
// the log shape that predates the HTTP meta frame entirely.
func TestInventoryCoversHTTPSessions(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Target: "https://api.example.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	writeSessionLog(t, "http-new.jsonl", proxy.Envelope{
		SessionID: "h1", ServerLabel: "api.example.com", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: meta,
	})
	// An HTTP log captured before mcpsnoop recorded an endpoint: its first frame
	// is traffic, and it must still yield a row rather than vanishing.
	writeSessionLog(t, "http-old.jsonl", proxy.Envelope{
		SessionID: "h2", ServerLabel: "legacy-http", Seq: 1, TS: t0.Add(time.Hour),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})

	inv, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(inv.Servers))
	}
	byLabel := map[string]serverRow{}
	for _, row := range inv.Servers {
		byLabel[row.Label] = row
	}
	if got := byLabel["api.example.com"].Endpoint; got != "https://api.example.com/mcp" {
		t.Fatalf("endpoint = %q, want the recorded one", got)
	}
	old := byLabel["legacy-http"]
	if old.Runs != 1 || old.Transport != proxy.TransportHTTP {
		t.Fatalf("a pre-meta HTTP log did not produce a usable row: %+v", old)
	}
	if old.Endpoint != "" || len(old.Command) != 0 {
		t.Fatalf("a pre-meta log invented an endpoint or command: %+v", old)
	}
}

// TestInventorySurvivesUnreadableLogs covers the routine case, not a
// hypothetical: a run whose exec fails leaves a zero-byte log behind, and a shim
// still writing leaves a torn final line.
func TestInventorySurvivesUnreadableLogs(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "good.jsonl", stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "index.js"}, false)...)

	dir := paths.SessionsDir()
	for name, body := range map[string]string{
		"empty.jsonl":   "",
		"torn.jsonl":    `{"session_id":"x","seq":1,"dire`,
		"garbage.jsonl": "not json at all\n",
		"noid.jsonl":    "{}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file that is not a log at all must not even be looked at.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := takeInventory(dir, false)
	if err != nil {
		t.Fatalf("one unreadable log must not fail the run: %v", err)
	}
	if len(inv.Servers) != 1 || inv.Scanned != 1 {
		t.Fatalf("servers %d scanned %d, want the one good log read", len(inv.Servers), inv.Scanned)
	}
	// The empty one is counted apart from the damaged three: a zero-byte log is
	// what a failed exec and an uncalled HTTP proxy both leave, so calling it
	// unreadable would report routine housekeeping as damage.
	if inv.Empty != 1 || inv.Skipped != 3 {
		t.Fatalf("empty %d skipped %d, want 1 and 3, all counted rather than dropped in silence", inv.Empty, inv.Skipped)
	}

	code, stdout, stderr := executeInventory(t, nil)
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "3 logs skipped as unreadable") || !strings.Contains(stdout, "1 empty log") {
		t.Fatalf("the output does not say what it could not read:\n%s", stdout)
	}
}

// countingLog counts the bytes a reader actually pulls out of a log.
type countingLog struct {
	inner io.ReadCloser
	read  *int64
}

func (c countingLog) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	*c.read += int64(n)
	return n, err
}

func (c countingLog) Close() error { return c.inner.Close() }

// TestInventoryReadsOneEnvelopePerLog is the criterion the whole design rests
// on. It is measured rather than inferred: nothing about the returned rows can
// see how much of a file was pulled, because json.Decoder stops at the end of
// the first value however many bytes it was handed, so a reader that slurped the
// file whole first would answer identically and pass any assertion about output.
func TestInventoryReadsOneEnvelopePerLog(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "index.js"}, CWD: "/proj"})
	if err != nil {
		t.Fatal(err)
	}
	head, err := json.Marshal(proxy.Envelope{
		SessionID: "s1", ServerLabel: "index.js", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	const tail = 8 << 20 // eight megabytes of frames the command has no business reading
	var buf bytes.Buffer
	buf.Write(append(head, '\n'))
	filler, err := json.Marshal(proxy.Envelope{
		SessionID: "s1", ServerLabel: "index.js", Seq: 2, TS: t0,
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for buf.Len() < tail {
		buf.Write(append(filler, '\n'))
	}
	if err := os.WriteFile(filepath.Join(paths.SessionsDir(), "big.jsonl"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var read int64
	prev := openLog
	openLog = func(path string) (io.ReadCloser, error) {
		f, err := prev(path)
		if err != nil {
			return nil, err
		}
		return countingLog{inner: f, read: &read}, nil
	}
	t.Cleanup(func() { openLog = prev })

	inv, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(inv.Servers))
	}
	if got := strings.Join(inv.Servers[0].Command, " "); got != "node index.js" {
		t.Fatalf("command = %q, want the meta frame's own", got)
	}
	// json.Decoder buffers ahead of the value it is parsing, so this is a bound on
	// the buffer rather than on the envelope. What it rules out is reading the log.
	const bound = 64 << 10
	if read > bound {
		t.Fatalf("read %d bytes of an %d-byte log, want at most %d; the walk is reading past the first envelope", read, buf.Len(), bound)
	}
	if read == 0 {
		t.Fatal("the counting reader saw nothing, so this test is measuring the wrong thing")
	}
}

// TestInventoryDoesNotReadPastTheHeadWithoutTools pins the other half of the opt
// in. The count being absent from the rows does not prove the read never
// happened, only that its result was not published, so the call itself is what
// is observed.
func TestInventoryDoesNotReadPastTheHeadWithoutTools(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "a.jsonl", stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "i.js"}, false)...)

	calls := 0
	prev := countTools
	countTools = func(path string) (int, toolCountStatus) {
		calls++
		return prev(path)
	}
	t.Cleanup(func() { countTools = prev })

	if _, err := takeInventory(paths.SessionsDir(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("a plain run read past the head %d times; --tools is what pays for that", calls)
	}
	if _, err := takeInventory(paths.SessionsDir(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("--tools read past the head %d times, want exactly one log per server", calls)
	}
}

// TestInventoryShowsARedactedCommandAsRecorded keeps the row honest. A scrubbed
// command line is not the command that ran, and printing it without saying so
// presents mcpsnoop's own placeholder as the server's argument.
func TestInventoryShowsARedactedCommandAsRecorded(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "r.jsonl", stdioSession(t, "s1", "secretsrv", t0, "/proj",
		[]string{"python3", "server.py", "--api-key", "[REDACTED]"}, true)...)

	code, stdout, stderr := executeInventory(t, nil)
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "--api-key [REDACTED]") {
		t.Fatalf("the command is not shown as recorded:\n%s", stdout)
	}
	if !strings.Contains(stdout, "redacted") {
		t.Fatalf("a scrubbed command is presented as the full one:\n%s", stdout)
	}
}

// TestInventoryOutputIsStableAcrossRuns is what makes the command usable as a
// governance baseline. Two runs over one directory have to produce one answer.
func TestInventoryOutputIsStableAcrossRuns(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for i := range 12 {
		writeSessionLog(t, fmt.Sprintf("s%02d.jsonl", i),
			stdioSession(t, fmt.Sprintf("s%d", i), fmt.Sprintf("srv%d", i%4), t0.Add(time.Duration(i)*time.Minute),
				fmt.Sprintf("/proj/%d", i%3), []string{"node", fmt.Sprintf("index%d.js", i%4)}, false)...)
	}
	_, first, _ := executeInventory(t, nil)
	for range 3 {
		_, again, _ := executeInventory(t, nil)
		if again != first {
			t.Fatalf("two runs over one directory disagree:\n--- first\n%s\n--- again\n%s", first, again)
		}
	}
	if strings.Count(first, "\n  command") == 0 {
		t.Fatalf("the run produced no rows:\n%s", first)
	}
}

// TestInventoryCountsToolsOnlyWhenAsked pins both halves of the opt in: the
// count is right when asked for, and nothing reads past the first envelope when
// it is not.
func TestInventoryCountsToolsOnlyWhenAsked(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	session := func(id string, at time.Time, tools []string) []proxy.Envelope {
		envs := stdioSession(t, id, "srv", at, "/proj", []string{"node", "index.js"}, false)
		list := make([]string, 0, len(tools))
		for _, name := range tools {
			list = append(list, fmt.Sprintf(`{"name":%q,"description":"x","inputSchema":{"type":"object"}}`, name))
		}
		return append(envs,
			proxy.Envelope{SessionID: id, ServerLabel: "srv", Seq: 2, TS: at.Add(time.Millisecond),
				Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
				Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)},
			proxy.Envelope{SessionID: id, ServerLabel: "srv", Seq: 3, TS: at.Add(2 * time.Millisecond),
				Direction: proxy.ServerToClient, Transport: proxy.TransportStdio,
				Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"tools":[%s]}}`, strings.Join(list, ",")))},
		)
	}
	// The older run advertised two tools, the newer one three. The count is what
	// the server advertised the last time it ran. The file names put the older run
	// first in directory order on purpose: named the other way round the test
	// passes on whichever log ReadDir happened to reach first rather than on the
	// timestamps, and a mutation that ignores them entirely goes unnoticed.
	writeSessionLog(t, "a-older-run.jsonl", session("s1", t0, []string{"a", "b"})...)
	writeSessionLog(t, "b-newer-run.jsonl", session("s2", t0.Add(time.Hour), []string{"a", "b", "c"})...)

	without, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(without.Servers) != 1 || without.Servers[0].Tools != nil {
		t.Fatalf("a plain run counted tools anyway: %+v", without.Servers)
	}

	with, err := takeInventory(paths.SessionsDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(with.Servers) != 1 || with.Servers[0].Tools == nil {
		t.Fatalf("--tools produced no count: %+v", with.Servers)
	}
	if got := *with.Servers[0].Tools; got != 3 {
		t.Fatalf("tools = %d, want the 3 the newest run advertised", got)
	}
}

// TestInventoryRefusesAnIncompleteToolListing keeps a paginated listing from
// reading as a small server. A partial page is not a count.
func TestInventoryRefusesAnIncompleteToolListing(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	envs := stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "index.js"}, false)
	envs = append(envs,
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Millisecond),
			Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)},
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0.Add(2 * time.Millisecond),
			Direction: proxy.ServerToClient, Transport: proxy.TransportStdio,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a","inputSchema":{"type":"object"}}],"nextCursor":"more"}}`)},
	)
	writeSessionLog(t, "partial.jsonl", envs...)

	inv, err := takeInventory(paths.SessionsDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(inv.Servers))
	}
	if inv.Servers[0].Tools != nil {
		t.Fatalf("tools = %d, want no count at all for a listing that never finished", *inv.Servers[0].Tools)
	}
	_, stdout, _ := executeInventory(t, []string{"--tools"})
	if !strings.Contains(stdout, "no complete tools/list") {
		t.Fatalf("the output does not say why there is no count:\n%s", stdout)
	}
}

// TestInventoryJSONKeepsAmpersandsReadable pins the encoder choice. An endpoint
// with a query is the ordinary case here, and the standard library escapes & for
// HTML embedding, which turns a correct URL into one that reads as mangled.
func TestInventoryJSONKeepsAmpersandsReadable(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Target: "https://h/mcp?tenant=[stripped]&api_key=[stripped]"})
	if err != nil {
		t.Fatal(err)
	}
	writeSessionLog(t, "h.jsonl", proxy.Envelope{
		SessionID: "h1", ServerLabel: "h", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: meta,
	})

	code, stdout, stderr := executeInventory(t, []string{"--format", "json"})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if strings.Contains(stdout, `\u0026`) {
		t.Fatalf("the endpoint is HTML-escaped in the JSON output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "tenant=[stripped]&api_key=[stripped]") {
		t.Fatalf("the endpoint is not carried verbatim:\n%s", stdout)
	}
	var parsed inventory
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("the JSON output does not parse: %v", err)
	}
	if len(parsed.Servers) != 1 || parsed.Servers[0].Endpoint == "" {
		t.Fatalf("round trip lost the row: %+v", parsed)
	}
}

// TestInventoryRejectsAnUnknownFormat keeps a typo from silently producing the
// default rather than the format the caller asked for.
func TestInventoryRejectsAnUnknownFormat(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	code, stdout, stderr := executeInventory(t, []string{"--format", "yaml"})
	if code != 2 {
		t.Fatalf("code = %d, want 2 for a usage error", code)
	}
	if stdout != "" || !strings.Contains(stderr, "invalid --format") {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}
}

// TestInventoryOnAnEmptyDirectorySaysSo keeps a fresh machine from printing
// nothing at all, which reads as a broken command.
func TestInventoryOnAnEmptyDirectorySaysSo(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	code, stdout, stderr := executeInventory(t, nil)
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "no servers found") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestInventoryCountsToolsWhileAShimIsStillWriting is why --tools does not use
// exporter.LoadFile. A shim appending to its log right now leaves a torn final
// line, and LoadFile treats that as corruption, which is correct when the log is
// the subject and wrong here: it would make the whole inventory unusable exactly
// while something is running.
func TestInventoryCountsToolsWhileAShimIsStillWriting(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	envs := stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "index.js"}, false)
	envs = append(envs,
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Millisecond),
			Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)},
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0.Add(2 * time.Millisecond),
			Direction: proxy.ServerToClient, Transport: proxy.TransportStdio,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a","inputSchema":{"type":"object"}},{"name":"b","inputSchema":{"type":"object"}}]}}`)},
	)
	path := writeSessionLog(t, "live.jsonl", envs...)
	// The half-written frame a live shim leaves behind.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"session_id":"s1","seq":4,"ts":"2026-08-01T12:00:03Z","dire`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	inv, err := takeInventory(paths.SessionsDir(), true)
	if err != nil {
		t.Fatalf("a log a shim is still writing must not fail the run: %v", err)
	}
	if len(inv.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(inv.Servers))
	}
	if inv.Servers[0].Tools == nil {
		t.Fatal("no tool count for a log whose only fault is a torn final line")
	}
	if got := *inv.Servers[0].Tools; got != 2 {
		t.Fatalf("tools = %d, want the 2 that arrived before the tear", got)
	}
}

// TestInventoryCannotBeMadeToForgeRows is the injection the text renderer used
// to allow. None of the values in a row is written by mcpsnoop: a command comes
// from whoever installed the server, a working directory comes off the
// filesystem, and labelFor never runs the control-character check paths.CheckLabel
// applies to an explicit --label. A newline in any of them closed the field it
// was printed in and made every following line read as another server block, so
// a baseline somebody diffs against named servers that never ran.
func TestInventoryCannotBeMadeToForgeRows(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	forge := "\n  cwd       /opt/approved\n\nFORGED  stdio  99 runs"

	for _, tc := range []struct {
		name string
		envs []proxy.Envelope
	}{
		{"in an argument", stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "index.js" + forge}, false)},
		{"in the working directory", stdioSession(t, "s2", "srv2", t0, "/proj"+forge, []string{"node", "b.js"}, false)},
		{"in the label", stdioSession(t, "s3", "srv3"+forge, t0, "/proj", []string{"node", "c.js"}, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MCPSNOOP_HOME", t.TempDir())
			writeSessionLog(t, "x.jsonl", tc.envs...)
			code, stdout, stderr := executeInventory(t, nil)
			if code != 0 || stderr != "" {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if strings.Contains(stdout, "FORGED") && !strings.Contains(stdout, `\n`) {
				t.Fatalf("a forged block reached the output unescaped:\n%s", stdout)
			}
			// The header counts one server, so exactly one block may follow it.
			if blocks := strings.Count(stdout, "  command   ") + strings.Count(stdout, "  endpoint  "); blocks != 1 {
				t.Fatalf("output holds %d blocks for one server:\n%s", blocks, stdout)
			}
			if !strings.HasPrefix(stdout, "1 server ") {
				t.Fatalf("header does not agree with the body:\n%s", stdout)
			}
		})
	}
}

// TestInventoryDoesNotLetAnEscapeSequenceDriveTheTerminal is the same hazard in
// its nastier form. This output is meant to be read in a terminal and piped to a
// file, and neither should be able to carry control codes out of a log.
func TestInventoryDoesNotLetAnEscapeSequenceDriveTheTerminal(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "esc.jsonl", stdioSession(t, "s1", "srv", t0, "/proj",
		[]string{"node", "index.js\x1b[2J\x1b[1;31m"}, false)...)

	_, stdout, _ := executeInventory(t, nil)
	if strings.ContainsFunc(stdout, func(r rune) bool { return r == 0x1b }) {
		t.Fatalf("an escape sequence reached the output:\n%q", stdout)
	}
	if !strings.Contains(stdout, `\x1b`) {
		t.Fatalf("the value was dropped rather than quoted, so it is not recoverable:\n%s", stdout)
	}
}

// TestInventoryDistinguishesArgumentBoundaries keeps two different commands from
// rendering as one. An argument holding a space is the everyday shape of
// node "~/My Project/build/index.js".
func TestInventoryDistinguishesArgumentBoundaries(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "one.jsonl", stdioSession(t, "s1", "srv", t0, "/proj", []string{"python3", "s.py", "--name", "a b"}, false)...)
	writeSessionLog(t, "two.jsonl", stdioSession(t, "s2", "srv", t0, "/proj", []string{"python3", "s.py", "--name", "a", "b"}, false)...)

	inv, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 2 {
		t.Fatalf("servers = %d, want 2 distinct commands", len(inv.Servers))
	}
	_, stdout, _ := executeInventory(t, nil)
	if strings.Count(stdout, `--name "a b"`) != 1 || strings.Count(stdout, "--name a b\n") != 1 {
		t.Fatalf("the two commands do not render distinguishably:\n%s", stdout)
	}
}

// TestInventoryDoesNotHangOnAFifo covers a directory anything can write to. Open
// on a fifo blocks until a writer appears, and the command prints nothing until
// the whole walk is done, so one of them stopped the answer entirely.
func TestInventoryDoesNotHangOnAFifo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no fifos")
	}
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "good.jsonl", stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "i.js"}, false)...)
	fifo := filepath.Join(paths.SessionsDir(), "pipe.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}

	done := make(chan inventory, 1)
	go func() {
		inv, err := takeInventory(paths.SessionsDir(), false)
		if err == nil {
			done <- inv
		}
		close(done)
	}()
	select {
	case inv, ok := <-done:
		if !ok {
			t.Fatal("takeInventory failed on a directory holding a fifo")
		}
		if len(inv.Servers) != 1 {
			t.Fatalf("servers = %d, want the one real log still reported", len(inv.Servers))
		}
		if inv.Skipped != 1 {
			t.Fatalf("skipped = %d, want the fifo counted", inv.Skipped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a fifo in the sessions directory hung the whole command")
	}
}

// TestInventorySeparatesEmptyLogsFromDamagedOnes keeps ordinary housekeeping
// from being reported as damage. A zero-byte log is what a failed exec leaves,
// and what an HTTP proxy nobody called leaves on purpose.
func TestInventorySeparatesEmptyLogsFromDamagedOnes(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "good.jsonl", stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "i.js"}, false)...)
	dir := paths.SessionsDir()
	for _, name := range []string{"a.jsonl", "b.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.jsonl"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := takeInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Empty != 2 || inv.Skipped != 1 {
		t.Fatalf("empty %d skipped %d, want 2 and 1", inv.Empty, inv.Skipped)
	}
	_, stdout, _ := executeInventory(t, nil)
	if !strings.Contains(stdout, "2 empty logs left by a run that recorded nothing") {
		t.Fatalf("empty logs are reported as damage:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 log skipped as unreadable") {
		t.Fatalf("the damaged log is not named:\n%s", stdout)
	}
}

// TestInventorySaysWhyThereIsNoToolCount covers the difference between a server
// that advertised nothing, one whose listing never finished, and a log that
// could not be read. One sentence for all three made mcpsnoop state something
// false about the third.
func TestInventorySaysWhyThereIsNoToolCount(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	listing := func(id, cwd, result string) []proxy.Envelope {
		return append(stdioSession(t, id, "srv-"+id, t0, cwd, []string{"node", cwd + ".js"}, false),
			proxy.Envelope{SessionID: id, ServerLabel: "srv-" + id, Seq: 2, TS: t0.Add(time.Millisecond),
				Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
				Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)},
			proxy.Envelope{SessionID: id, ServerLabel: "srv-" + id, Seq: 3, TS: t0.Add(2 * time.Millisecond),
				Direction: proxy.ServerToClient, Transport: proxy.TransportStdio, Raw: json.RawMessage(result)},
		)
	}
	writeSessionLog(t, "none.jsonl", listing("n", "/none", `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)...)
	writeSessionLog(t, "partial.jsonl", listing("p", "/partial", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a","inputSchema":{"type":"object"}}],"nextCursor":"more"}}`)...)
	// A complete listing followed by a line the decoder cannot finish. The count
	// is already in the store when the tear is reached, so the reason printed has
	// to be about the read rather than about the listing.
	broken := filepath.Join(paths.SessionsDir(), "broken.jsonl")
	writeSessionLog(t, "broken.jsonl", listing("b", "/broken", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a","inputSchema":{"type":"object"}},{"name":"b","inputSchema":{"type":"object"}}]}}`)...)
	f, err := os.OpenFile(broken, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"session_id\":\"b\",,,BROKEN}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	inv, err := takeInventory(paths.SessionsDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	byCWD := map[string]serverRow{}
	for _, row := range inv.Servers {
		byCWD[row.CWD] = row
	}
	if row := byCWD["/none"]; row.Tools == nil || *row.Tools != 0 {
		t.Fatalf("a server that advertised none should count zero, got %+v", row.Tools)
	}
	if row := byCWD["/partial"]; row.Tools != nil || !strings.Contains(row.ToolsNote, "no complete tools/list") {
		t.Fatalf("a paginating listing is misreported: tools=%v note=%q", row.Tools, row.ToolsNote)
	}
	if row := byCWD["/broken"]; row.Tools != nil || !strings.Contains(row.ToolsNote, "could not be read") {
		t.Fatalf("a log that could not be read is reported as a server that advertised nothing: tools=%v note=%q", row.Tools, row.ToolsNote)
	}

	// The JSON has to carry the same three answers, or a consumer cannot tell an
	// unasked-for count from one that was asked for and failed.
	_, stdout, _ := executeInventory(t, []string{"--tools", "--format", "json"})
	var parsed inventory
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, row := range parsed.Servers {
		if row.ToolsNote != "" {
			notes++
		}
	}
	if notes != 2 {
		t.Fatalf("the JSON carries %d reasons, want one for the partial listing and one for the unreadable log:\n%s", notes, stdout)
	}
}

// TestInventoryKeepsARangeInOneZone stops a range printing backwards. Timestamps
// are recorded with the offset that was in force, so two runs either side of a
// daylight-saving change carry different ones.
func TestInventoryKeepsARangeInOneZone(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	plus3 := time.FixedZone("+03", 3*60*60)
	plus2 := time.FixedZone("+02", 2*60*60)
	earlier := time.Date(2026, 10, 25, 3, 30, 0, 0, plus3) // 00:30 UTC
	later := time.Date(2026, 10, 25, 3, 10, 0, 0, plus2)   // 01:10 UTC
	writeSessionLog(t, "a.jsonl", stdioSession(t, "s1", "dst", earlier, "/proj", []string{"python3", "s.py"}, false)...)
	writeSessionLog(t, "b.jsonl", stdioSession(t, "s2", "dst", later, "/proj", []string{"python3", "s.py"}, false)...)

	inv, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(inv.Servers))
	}
	rendered := seenRange(inv.Servers[0])
	first, last, ok := strings.Cut(rendered, " to ")
	if !ok {
		t.Fatalf("seen = %q, want a range", rendered)
	}
	// Read as clock faces, not as instants. Parsing keeps the offsets and orders
	// them correctly whatever they are, which is exactly what a person reading two
	// wall-clock times side by side cannot do.
	zone := func(stamp string) string { return stamp[len(stamp)-6:] }
	if zone(first) != zone(last) {
		t.Fatalf("the two ends are rendered in different zones, so the range reads backwards: %q", rendered)
	}
	if first >= last {
		t.Fatalf("the range reads backwards: %q", rendered)
	}
}

// TestInventoryIgnoresAZeroTimestamp keeps one envelope with no ts from dating
// every run of a server to year one.
func TestInventoryIgnoresAZeroTimestamp(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "real.jsonl", stdioSession(t, "s1", "zt", t0, "/w", []string{"node", "z.js"}, false)...)
	writeSessionLog(t, "nots.jsonl", stdioSession(t, "s2", "zt", time.Time{}, "/w", []string{"node", "z.js"}, false)...)

	inv, err := takeInventory(paths.SessionsDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(inv.Servers))
	}
	if got := inv.Servers[0].FirstSeen; !got.Equal(t0) {
		t.Fatalf("first seen = %v, want the only real timestamp %v", got, t0)
	}
}

// TestInventoryNamesEveryLabelOfOneServer keeps an explicit --label from being
// dropped. The key is the command and directory, so one server run as prod and
// again as staging is one row, and showing only the alphabetically first name
// silently loses the other.
func TestInventoryNamesEveryLabelOfOneServer(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "a.jsonl", stdioSession(t, "s1", "prod", t0, "/proj", []string{"node", "server.js"}, false)...)
	writeSessionLog(t, "b.jsonl", stdioSession(t, "s2", "staging", t0.Add(time.Hour), "/proj", []string{"node", "server.js"}, false)...)

	_, stdout, _ := executeInventory(t, nil)
	if !strings.Contains(stdout, "prod, staging") {
		t.Fatalf("one of the two labels was dropped:\n%s", stdout)
	}
	if !strings.HasPrefix(stdout, "1 server ") {
		t.Fatalf("two labels of one command should still be one server:\n%s", stdout)
	}
}

// TestInventoryDoesNotClaimARedactionItCannotSee keeps the note off a row that
// shows no command. The flag rides individual frames, so a pre-meta log's first
// frame can carry it while there is nothing recorded for it to qualify.
func TestInventoryDoesNotClaimARedactionItCannotSee(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "old.jsonl", proxy.Envelope{
		SessionID: "h1", ServerLabel: "legacy", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP, Redacted: true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"[REDACTED]"}}`),
	})

	_, stdout, _ := executeInventory(t, nil)
	if strings.Contains(stdout, "redacted") {
		t.Fatalf("a row with no command claims its command was rewritten:\n%s", stdout)
	}
	if !strings.Contains(stdout, "no meta frame") {
		t.Fatalf("the row does not say why it has no command:\n%s", stdout)
	}
}

// TestInventoryTellsAMissingMetaFrameFromAnEmptyOne keeps the reason accurate.
// A target with no host records an empty endpoint, so the meta frame is present
// and names nothing, which is a different thing from having no meta frame.
func TestInventoryTellsAMissingMetaFrameFromAnEmptyOne(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{})
	if err != nil {
		t.Fatal(err)
	}
	writeSessionLog(t, "hollow.jsonl", proxy.Envelope{
		SessionID: "h1", ServerLabel: "http", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: meta,
	})

	_, stdout, _ := executeInventory(t, nil)
	if strings.Contains(stdout, "this log has no meta frame") {
		t.Fatalf("a log with a meta frame is reported as having none:\n%s", stdout)
	}
	if !strings.Contains(stdout, "names neither a command nor an endpoint") {
		t.Fatalf("the row does not say what it actually found:\n%s", stdout)
	}
}
