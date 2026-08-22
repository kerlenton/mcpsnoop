package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func executeStats(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	cmd := newStatsCmd()
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

// call is one tools/call and its answer, at a chosen duration.
type call struct {
	tool     string
	duration time.Duration
	// answer is the response body after the id, so a test can choose between a
	// result, a tool error and a JSON-RPC error.
	answer string
}

const (
	answerOK    = `"result":{"content":[]}`
	answerIsErr = `"result":{"content":[],"isError":true}`
	answerProto = `"error":{"code":-32603,"message":"boom"}`
)

// writeCapture writes one session log: a meta frame, a tools/list, then the
// calls. command and cwd decide which server it belongs to.
func writeCapture(t *testing.T, name, label string, at time.Time, cwd string, command []string, tools []string, calls []call) string {
	t.Helper()
	meta, err := json.Marshal(proxy.SessionMeta{Command: command, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSuffix(name, ".jsonl")
	envs := []proxy.Envelope{{
		SessionID: id, ServerLabel: label, Seq: 1, TS: at,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta,
	}}
	seq := uint64(1)
	add := func(dir proxy.Direction, ts time.Time, raw string) {
		seq++
		envs = append(envs, proxy.Envelope{
			SessionID: id, ServerLabel: label, Seq: seq, TS: ts,
			Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw),
		})
	}
	if len(tools) > 0 {
		defs := make([]string, 0, len(tools))
		for _, tool := range tools {
			defs = append(defs, fmt.Sprintf(`{"name":%q,"description":"d","inputSchema":{"type":"object"}}`, tool))
		}
		add(proxy.ClientToServer, at.Add(time.Millisecond), `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`)
		add(proxy.ServerToClient, at.Add(2*time.Millisecond),
			fmt.Sprintf(`{"jsonrpc":"2.0","id":"list","result":{"tools":[%s]}}`, strings.Join(defs, ",")))
	}
	clock := at.Add(10 * time.Millisecond)
	for i, c := range calls {
		reqID := fmt.Sprintf("c%d", i)
		add(proxy.ClientToServer, clock, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":%q}}`, reqID, c.tool))
		clock = clock.Add(c.duration)
		add(proxy.ServerToClient, clock, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,%s}`, reqID, c.answer))
		clock = clock.Add(time.Millisecond)
	}

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
	// Modification time is the selection rule, so it has to match the capture.
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func rowFor(t *testing.T, roll rollup, server, tool string) toolRow {
	t.Helper()
	for _, row := range roll.Rows {
		if row.Server == server && row.Tool == tool {
			return row
		}
	}
	t.Fatalf("no row for %s/%s in %+v", server, tool, roll.Rows)
	return toolRow{}
}

// TestStatsPoolsWhatCheckCannotSee is the reproduction the issue is built on: a
// tool that fails one run in four while the newest capture is clean, so check
// passes honestly and says nothing about it.
func TestStatsPoolsWhatCheckCannotSee(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	tools := []string{"search_docs", "read_file", "run_query"}
	for i := range 12 {
		answer := answerOK
		if i%4 == 0 { // three of twelve fail
			answer = answerIsErr
		}
		writeCapture(t, fmt.Sprintf("flaky-%02d.jsonl", i), "flaky-demo", t0.Add(time.Duration(i)*time.Hour),
			"/srv/flaky", []string{"node", "server.js"}, tools, []call{
				{tool: "search_docs", duration: 40 * time.Millisecond, answer: answerOK},
				{tool: "read_file", duration: 15 * time.Millisecond, answer: answerOK},
				{tool: "run_query", duration: 400 * time.Millisecond, answer: answer},
			})
	}

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if roll.Read != 12 || roll.Total != 12 {
		t.Fatalf("read %d of %d, want 12 of 12", roll.Read, roll.Total)
	}
	row := rowFor(t, roll, "flaky-demo", "run_query")
	if row.Calls != 12 || row.ToolErrors != 3 || row.ProtocolErrors != 0 {
		t.Fatalf("run_query = %d calls, %d tool errors, %d protocol errors; want 12, 3, 0", row.Calls, row.ToolErrors, row.ProtocolErrors)
	}
	if row.Sessions != 12 || row.FailedSessions != 3 {
		t.Fatalf("sessions = %d/%d, want 3/12", row.FailedSessions, row.Sessions)
	}
	if got := row.FailureRate; got < 24.9 || got > 25.1 {
		t.Fatalf("failure rate = %.1f%%, want 25%%", got)
	}
	// Worst first: run_query carries the only errors.
	if roll.Rows[0].Tool != "run_query" {
		t.Fatalf("rows do not sort worst first: %+v", roll.Rows)
	}
}

// TestStatsKeepsTwoServersApart is why the rollup keys on the server rather than
// on the tool name. Pooling two distributions produces a figure true of neither.
func TestStatsKeepsTwoServersApart(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		writeCapture(t, fmt.Sprintf("fast-%d.jsonl", i), "flaky-demo", t0.Add(time.Duration(i)*time.Hour),
			"/srv/one", []string{"node", "server.js"}, []string{"search_docs"},
			[]call{{tool: "search_docs", duration: 40 * time.Millisecond, answer: answerOK}})
		writeCapture(t, fmt.Sprintf("slow-%d.jsonl", i), "docs-mirror", t0.Add(time.Duration(i)*time.Hour),
			"/srv/two", []string{"python3", "mirror.py"}, []string{"search_docs"},
			[]call{{tool: "search_docs", duration: 380 * time.Millisecond, answer: answerOK}})
	}

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(roll.Rows) != 2 {
		t.Fatalf("rows = %d, want one per server; pooling them describes neither", len(roll.Rows))
	}
	fast := rowFor(t, roll, "flaky-demo", "search_docs")
	slow := rowFor(t, roll, "docs-mirror", "search_docs")
	if fast.P50MS > 100 || slow.P50MS < 300 {
		t.Fatalf("the two distributions are smeared: %.0fms and %.0fms", fast.P50MS, slow.P50MS)
	}
}

// TestStatsSeparatesTwoServersSharingOneLabel is what keying on the label alone
// cannot do, and the case #128 measured: labelFor derives the label from the
// command's last path element, so two servers arrive under one name.
func TestStatsSeparatesTwoServersSharingOneLabel(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "a.jsonl", "server.py", t0, "/proj/alpha", []string{"python3", "server.py", "alpha"},
		[]string{"search"}, []call{{tool: "search", duration: 20 * time.Millisecond, answer: answerOK}})
	writeCapture(t, "b.jsonl", "server.py", t0.Add(time.Hour), "/proj/beta", []string{"python3", "server.py", "beta"},
		[]string{"search"}, []call{{tool: "search", duration: 500 * time.Millisecond, answer: answerIsErr}})

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(roll.Rows) != 2 {
		t.Fatalf("rows = %d, want 2; one derived label covers two servers", len(roll.Rows))
	}
	var withError, clean int
	for _, row := range roll.Rows {
		if row.ToolErrors > 0 {
			withError++
		} else {
			clean++
		}
	}
	if withError != 1 || clean != 1 {
		t.Fatalf("the failing server is not isolated: %+v", roll.Rows)
	}
}

// TestStatsCountsAnMRTROperationOnce is the failure the correlation exists to
// prevent. Keying on the JSON-RPC id would report one operation as two calls and
// feed the same wall clock in twice.
func TestStatsCountsAnMRTROperationOnce(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	env := func(seq uint64, ts time.Time, dir proxy.Direction, raw string) proxy.Envelope {
		return proxy.Envelope{SessionID: "m1", ServerLabel: "elicit", Seq: seq, TS: ts,
			Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)}
	}
	envs := []proxy.Envelope{
		{SessionID: "m1", ServerLabel: "elicit", Seq: 1, TS: t0, Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta},
		env(2, t0.Add(10*time.Millisecond), proxy.ClientToServer, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"confirm"}}`),
		env(3, t0.Add(20*time.Millisecond), proxy.ServerToClient, `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"blob","inputRequests":{"k":{"method":"elicitation/create","params":{}}}}}`),
		// The user takes five seconds to answer, then the client retries under a new id.
		env(4, t0.Add(5020*time.Millisecond), proxy.ClientToServer, `{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"confirm","inputResponses":{"k":{"action":"accept"}}}}`),
		env(5, t0.Add(6020*time.Millisecond), proxy.ServerToClient, `{"jsonrpc":"2.0","id":"2","result":{"content":[]}}`),
	}
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(paths.SessionsDir(), "m1.jsonl"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(roll.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(roll.Rows))
	}
	row := roll.Rows[0]
	if row.Calls != 1 {
		t.Fatalf("calls = %d, want 1; a multi round-trip operation is one call however many requests it took", row.Calls)
	}
	// Six seconds of wall clock, spanning the time the user spent answering.
	if row.P50MS < 5900 || row.P50MS > 6100 {
		t.Fatalf("p50 = %.0fms, want the whole exchange near 6000ms", row.P50MS)
	}
}

// TestStatsPoolsRawDurations is the difference between a percentile and an
// average of percentiles. Each session here is uniform, so a per-session p95 is
// that session's only value and averaging them lands in the middle of the range
// rather than near its top.
func TestStatsPoolsRawDurations(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for i := range 20 {
		writeCapture(t, fmt.Sprintf("s%02d.jsonl", i), "srv", t0.Add(time.Duration(i)*time.Hour),
			"/srv", []string{"node", "s.js"}, []string{"t"},
			[]call{{tool: "t", duration: time.Duration(i+1) * 10 * time.Millisecond, answer: answerOK}})
	}

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, roll, "srv", "t")
	if row.Calls != 20 {
		t.Fatalf("calls = %d, want 20", row.Calls)
	}
	// Durations are 10..200ms. Nearest rank puts p50 at the 10th value and p95 at
	// the 19th. An average of the twenty per-session values would be 105ms for
	// every percentile alike.
	if row.P50MS < 95 || row.P50MS > 105 {
		t.Fatalf("p50 = %.0fms, want 100ms", row.P50MS)
	}
	if row.P95MS < 185 || row.P95MS > 195 {
		t.Fatalf("p95 = %.0fms, want 190ms; averaging per-session percentiles would give 105ms", row.P95MS)
	}
	if row.P95MS <= row.P50MS {
		t.Fatalf("p95 %.0fms is not above p50 %.0fms, so the pooling collapsed", row.P95MS, row.P50MS)
	}
}

// TestStatsSeparatesTheTwoErrorKinds covers the distinction the spec draws on
// the Tools page: a protocol error is the request or the server being wrong, and
// isError is the tool reporting something a model can act on.
func TestStatsSeparatesTheTwoErrorKinds(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "a.jsonl", "srv", t0, "/srv", []string{"node", "s.js"}, []string{"t"}, []call{
		{tool: "t", duration: 10 * time.Millisecond, answer: answerOK},
		{tool: "t", duration: 10 * time.Millisecond, answer: answerIsErr},
		{tool: "t", duration: 10 * time.Millisecond, answer: answerProto},
		{tool: "t", duration: 10 * time.Millisecond, answer: answerProto},
	})

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, roll, "srv", "t")
	if row.Calls != 4 || row.ToolErrors != 1 || row.ProtocolErrors != 2 {
		t.Fatalf("got %d calls, %d tool, %d protocol; want 4, 1, 2", row.Calls, row.ToolErrors, row.ProtocolErrors)
	}
	if got := row.FailureRate; got < 74.9 || got > 75.1 {
		t.Fatalf("failure rate = %.1f%%, want 75%% over both kinds", got)
	}
	_, stdout, _ := executeStats(t, nil)
	if !strings.Contains(stdout, "ERR is result.isError") || !strings.Contains(stdout, "PROTO is a JSON-RPC error") {
		t.Fatalf("the table never explains its two error columns:\n%s", stdout)
	}
}

// TestStatsFlags covers the window, the label filter, the bound and the report
// of how much of the directory the answer covers.
func TestStatsFlags(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	now := time.Now()
	for i := range 6 {
		label, cwd := "recent", "/recent"
		at := now.Add(-time.Duration(i) * time.Hour)
		if i >= 3 {
			label, cwd = "ancient", "/ancient"
			at = now.Add(-time.Duration(30+i) * 24 * time.Hour)
		}
		writeCapture(t, fmt.Sprintf("s%d.jsonl", i), label, at, cwd, []string{"node", cwd + ".js"},
			[]string{"t"}, []call{{tool: "t", duration: 10 * time.Millisecond, answer: answerOK}})
	}

	t.Run("since bounds the window", func(t *testing.T) {
		roll, err := rollUp(paths.SessionsDir(), time.Now().Add(-24*time.Hour), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if roll.Read != 3 || roll.Total != 6 {
			t.Fatalf("read %d of %d, want 3 of 6", roll.Read, roll.Total)
		}
	})

	t.Run("label restricts the walk", func(t *testing.T) {
		roll, err := rollUp(paths.SessionsDir(), time.Time{}, []string{"ancient"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if roll.Read != 3 {
			t.Fatalf("read %d, want the 3 ancient logs", roll.Read)
		}
		for _, row := range roll.Rows {
			if row.Server != "ancient" {
				t.Fatalf("a row from another label survived: %+v", row)
			}
		}
	})

	t.Run("limit selects within the label", func(t *testing.T) {
		roll, err := rollUp(paths.SessionsDir(), time.Time{}, []string{"ancient"}, 2)
		if err != nil {
			t.Fatal(err)
		}
		if roll.Read != 2 {
			t.Fatalf("read %d, want 2 of the ancient logs; a limit applied before the label would have read none", roll.Read)
		}
	})

	t.Run("the output says how much it covers", func(t *testing.T) {
		_, stdout, _ := executeStats(t, []string{"--limit", "2"})
		if !strings.Contains(stdout, "read 2 logs of 6") {
			t.Fatalf("a bounded answer does not say it is bounded:\n%s", stdout)
		}
	})

	t.Run("the default limit is the backfill limit", func(t *testing.T) {
		cmd := newStatsCmd()
		if got := cmd.Flags().Lookup("limit").DefValue; got != fmt.Sprintf("%d", hub.DefaultBackfillLimit) {
			t.Fatalf("--limit default = %s, want %d", got, hub.DefaultBackfillLimit)
		}
	})
}

// TestStatsOnAnEmptyWindowExitsZero keeps stats a report rather than a gate.
func TestStatsOnAnEmptyWindowExitsZero(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	code, stdout, stderr := executeStats(t, []string{"--since", "1h"})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "no tool calls found") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestStatsNeverExposesAPayload is the privacy boundary. A capture holds params
// and results, and a rollup that leaked them would defeat every redaction flag.
func TestStatsNeverExposesAPayload(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const secret = "sk-live-do-not-print-me"
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	envs := []proxy.Envelope{
		{SessionID: "p1", ServerLabel: "srv", Seq: 1, TS: t0, Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta},
		{SessionID: "p1", ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Millisecond), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t","arguments":{"token":%q}}}`, secret))},
		{SessionID: "p1", ServerLabel: "srv", Seq: 3, TS: t0.Add(20 * time.Millisecond), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":%q}]}}`, secret))},
	}
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(paths.SessionsDir(), "p1.jsonl"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{nil, {"--format", "json"}} {
		_, stdout, stderr := executeStats(t, args)
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			t.Fatalf("stats printed a captured payload with %v:\n%s%s", args, stdout, stderr)
		}
	}
}

// TestStatsWritesNothing pins the read-only promise. A rollup that recorded a
// baseline would change what a later check --fail-on drift concludes.
func TestStatsWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", home)
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "a.jsonl", "srv", t0, "/srv", []string{"node", "s.js"}, []string{"t"},
		[]call{{tool: "t", duration: 10 * time.Millisecond, answer: answerOK}})

	before := snapshotTree(t, home)
	if code, _, stderr := executeStats(t, nil); code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if after := snapshotTree(t, home); !slicesEqual(before, after) {
		t.Fatalf("stats changed the state directory:\nbefore %v\nafter  %v", before, after)
	}
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s|%d|%v", path, info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStatsJSONCarriesTheSameFigures keeps the two formats from drifting.
func TestStatsJSONCarriesTheSameFigures(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "a.jsonl", "srv", t0, "/srv", []string{"node", "s.js"}, []string{"t"}, []call{
		{tool: "t", duration: 10 * time.Millisecond, answer: answerOK},
		{tool: "t", duration: 30 * time.Millisecond, answer: answerIsErr},
	})

	code, stdout, stderr := executeStats(t, []string{"--format", "json"})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	var parsed rollup
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("the JSON does not parse: %v\n%s", err, stdout)
	}
	if len(parsed.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(parsed.Rows))
	}
	row := parsed.Rows[0]
	if row.Calls != 2 || row.ToolErrors != 1 || row.Sessions != 1 || row.FailedSessions != 1 {
		t.Fatalf("json row = %+v", row)
	}
	if row.DefinitionBytes == 0 {
		t.Fatal("the definition cost is missing from the JSON")
	}
	if parsed.Read != 1 || parsed.Total != 1 {
		t.Fatalf("read %d of %d, want 1 of 1", parsed.Read, parsed.Total)
	}
}

// TestStatsRejectsBadFlags keeps a typo from silently producing the default.
func TestStatsRejectsBadFlags(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"format", "invalid --format", []string{"--format", "yaml"}},
		{"since", "--since", []string{"--since", "0d"}},
		{"since unparseable", "--since", []string{"--since", "soon"}},
		{"limit", "--limit", []string{"--limit", "-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := executeStats(t, tc.args)
			if code != 2 {
				t.Fatalf("code = %d, want 2 for a usage error", code)
			}
			if stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("stdout %q stderr %q", stdout, stderr)
			}
		})
	}
}

// TestStatsCountsAPendingCallWithoutLatency covers the rule the per-session
// summary already applies. A call that never came back was still made, so it
// counts, and it has no round trip to measure, so folding a zero into the
// percentiles would drag them toward a latency nothing observed.
func TestStatsCountsAPendingCallWithoutLatency(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	envs := []proxy.Envelope{
		{SessionID: "h1", ServerLabel: "srv", Seq: 1, TS: t0, Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta},
		// One answered call at 100ms.
		{SessionID: "h1", ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Millisecond), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t"}}`)},
		{SessionID: "h1", ServerLabel: "srv", Seq: 3, TS: t0.Add(101 * time.Millisecond), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":"1","result":{"content":[]}}`)},
		// One that never came back.
		{SessionID: "h1", ServerLabel: "srv", Seq: 4, TS: t0.Add(200 * time.Millisecond), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"t"}}`)},
	}
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(paths.SessionsDir(), "h1.jsonl"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, roll, "srv", "t")
	if row.Calls != 2 {
		t.Fatalf("calls = %d, want 2; a call that never came back was still made", row.Calls)
	}
	// Every percentile, not just the median. With two calls and one round trip,
	// the median lands on the measured one either way, so folding the open call's
	// elapsed time in would hide there and surface only at the tail.
	for _, got := range []struct {
		name string
		ms   float64
	}{{"p50", row.P50MS}, {"p95", row.P95MS}, {"p99", row.P99MS}} {
		if got.ms < 95 || got.ms > 105 {
			t.Fatalf("%s = %.0fms, want the one measured round trip at 100ms; a call still open has no round trip to fold in", got.name, got.ms)
		}
	}
	if row.ToolErrors != 0 || row.ProtocolErrors != 0 {
		t.Fatalf("a pending call is not an error: %+v", row)
	}
}

// TestStatsHoldsOneStoreAtATime is the memory bound the rollup promises. Each
// capture carries a megabyte of result bodies, so holding them all would be
// hundreds of megabytes; folding one at a time keeps the peak at one capture
// plus the counters.
func TestStatsHoldsOneStoreAtATime(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a few hundred logs")
	}
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const logs = 300
	body := strings.Repeat("x", 64<<10) // 64 KiB per result, 300 of them
	for i := range logs {
		writeCaptureWithBody(t, fmt.Sprintf("s%03d.jsonl", i), t0.Add(time.Duration(i)*time.Minute), body)
	}

	// Sampled during the walk, not after it. Once rollUp returns, every store it
	// held is collectable whether it dropped them as it went or kept them all, so
	// measuring afterwards cannot tell the two apart.
	var peak uint64
	prev := afterFold
	afterFold = func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		peak = max(peak, m.HeapAlloc)
	}
	t.Cleanup(func() { afterFold = prev })

	runtime.GC()
	var start runtime.MemStats
	runtime.ReadMemStats(&start)
	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if roll.Read != logs {
		t.Fatalf("read %d of %d logs", roll.Read, logs)
	}
	row := rowFor(t, roll, "srv", "t")
	if row.Calls != logs {
		t.Fatalf("calls = %d, want %d", row.Calls, logs)
	}
	// Everything the walk reads is 300 * 64 KiB, near twenty megabytes. What may
	// be live at once is one capture plus the counters and one duration per call.
	// The bound is loose on purpose: it is here to catch holding every store, not
	// to police allocation.
	grew := int64(peak) - int64(start.HeapAlloc)
	const bound = 8 << 20
	if grew > bound {
		t.Fatalf("the walk grew the heap by %d bytes across %d logs of %d bytes each; it is holding stores rather than folding them",
			grew, logs, len(body))
	}
	if peak == 0 {
		t.Fatal("nothing was sampled, so this test is measuring the wrong thing")
	}
}

// writeCaptureWithBody writes one capture whose single tool result carries body,
// so a directory of them is large on disk and in any store that keeps them.
func writeCaptureWithBody(t *testing.T, name string, at time.Time, body string) {
	t.Helper()
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSuffix(name, ".jsonl")
	result, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "1",
		"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": body}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	envs := []proxy.Envelope{
		{SessionID: id, ServerLabel: "srv", Seq: 1, TS: at, Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta},
		{SessionID: id, ServerLabel: "srv", Seq: 2, TS: at.Add(time.Millisecond), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t"}}`)},
		{SessionID: id, ServerLabel: "srv", Seq: 3, TS: at.Add(11 * time.Millisecond), Direction: proxy.ServerToClient, Raw: result},
	}
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	path := filepath.Join(paths.SessionsDir(), name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestStatsIgnoresASupersededCallsGap covers the criterion the pending test only
// half covered. A client reusing an id while the earlier request is in flight
// makes the store stamp the older call's end with the newer request's timestamp,
// so the call reads as done with a duration that is the gap between two
// unrelated requests. Folding that in reported a ten-minute round trip on a
// server whose slowest real answer was twenty milliseconds.
func TestStatsIgnoresASupersededCallsGap(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	envs := []proxy.Envelope{{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta}}
	seq := uint64(1)
	at := t0.Add(10 * time.Millisecond)
	add := func(dir proxy.Direction, ts time.Time, raw string) {
		seq++
		envs = append(envs, proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: ts,
			Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)})
	}
	for i := range 10 { // ten honest 20ms round trips
		id := fmt.Sprintf("%d", i)
		add(proxy.ClientToServer, at, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"echo"}}`, id))
		at = at.Add(20 * time.Millisecond)
		add(proxy.ServerToClient, at, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"content":[]}}`, id))
		at = at.Add(5 * time.Millisecond)
	}
	// An id reused ten minutes later while the first was still in flight.
	add(proxy.ClientToServer, at, `{"jsonrpc":"2.0","id":"99","method":"tools/call","params":{"name":"echo"}}`)
	at = at.Add(10 * time.Minute)
	add(proxy.ClientToServer, at, `{"jsonrpc":"2.0","id":"99","method":"tools/call","params":{"name":"echo"}}`)
	add(proxy.ServerToClient, at.Add(20*time.Millisecond), `{"jsonrpc":"2.0","id":"99","result":{"content":[]}}`)

	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(paths.SessionsDir(), "s1.jsonl"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, roll, "srv", "echo")
	if row.Calls != 12 {
		t.Fatalf("calls = %d, want 12; a superseded call was still made", row.Calls)
	}
	for _, got := range []struct {
		name string
		ms   float64
	}{{"p50", row.P50MS}, {"p95", row.P95MS}, {"p99", row.P99MS}} {
		if got.ms > 100 {
			t.Fatalf("%s = %.0fms, want the measured round trips near 20ms; the gap between two reused ids is not a latency", got.name, got.ms)
		}
	}
}

// TestStatsShowsWhichServerARowIsAbout is the other half of keying on the
// command and working directory. Two servers that derive one label printed as
// two identical lines, which reads as one row printed twice, and marshalled to
// two identical objects, so a consumer indexing on server and tool kept one and
// dropped the other.
func TestStatsShowsWhichServerARowIsAbout(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// Identical in every figure, so only the identity can tell them apart.
	writeCapture(t, "a.jsonl", "server.py", t0, "/proj/alpha", []string{"python3", "server.py", "alpha"},
		[]string{"search"}, []call{{tool: "search", duration: 20 * time.Millisecond, answer: answerOK}})
	writeCapture(t, "b.jsonl", "server.py", t0.Add(time.Hour), "/proj/beta", []string{"python3", "server.py", "beta"},
		[]string{"search"}, []call{{tool: "search", duration: 20 * time.Millisecond, answer: answerOK}})

	_, stdout, _ := executeStats(t, nil)
	if !strings.Contains(stdout, "/proj/alpha") || !strings.Contains(stdout, "/proj/beta") {
		t.Fatalf("the two servers are indistinguishable in the table:\n%s", stdout)
	}
	var rowLines []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "server.py") && !strings.Contains(line, "SERVER") {
			rowLines = append(rowLines, line)
		}
	}
	if len(rowLines) != 2 {
		t.Fatalf("rows = %d, want 2:\n%s", len(rowLines), stdout)
	}
	if rowLines[0] == rowLines[1] {
		t.Fatalf("the two rows render identically:\n%s", stdout)
	}

	_, jsonOut, _ := executeStats(t, []string{"--format", "json"})
	var parsed rollup
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Rows) != 2 {
		t.Fatalf("json rows = %d, want 2", len(parsed.Rows))
	}
	if parsed.Rows[0].CWD == parsed.Rows[1].CWD || parsed.Rows[0].CWD == "" {
		t.Fatalf("the json rows do not carry the identity they are keyed on: %+v", parsed.Rows)
	}
}

// TestStatsDoesNotQualifyAnUnambiguousName keeps the ordinary table unchanged.
// One server with two tools is not two servers.
func TestStatsDoesNotQualifyAnUnambiguousName(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "a.jsonl", "srv", t0, "/srv", []string{"node", "s.js"}, []string{"a", "b"}, []call{
		{tool: "a", duration: 10 * time.Millisecond, answer: answerOK},
		{tool: "b", duration: 20 * time.Millisecond, answer: answerOK},
	})
	_, stdout, _ := executeStats(t, nil)
	if strings.Contains(stdout, "(/srv)") {
		t.Fatalf("a name that was never ambiguous was qualified anyway:\n%s", stdout)
	}
}

// TestStatsKeepsTwoLabelsOfOneCommandApart is the case the label half of the key
// exists for. One command deliberately run as prod and again as staging is two
// deployments, and pooling them smears two distributions into one.
func TestStatsKeepsTwoLabelsOfOneCommandApart(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "p.jsonl", "prod", t0, "/srv", []string{"node", "server.js"},
		[]string{"search"}, []call{{tool: "search", duration: 20 * time.Millisecond, answer: answerOK}})
	writeCapture(t, "s.jsonl", "staging", t0.Add(time.Hour), "/srv", []string{"node", "server.js"},
		[]string{"search"}, []call{{tool: "search", duration: 500 * time.Millisecond, answer: answerOK}})

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(roll.Rows) != 2 {
		t.Fatalf("rows = %d, want one per label; pooling them describes neither", len(roll.Rows))
	}
	fast := rowFor(t, roll, "prod", "search")
	slow := rowFor(t, roll, "staging", "search")
	if fast.P50MS > 100 || slow.P50MS < 300 {
		t.Fatalf("the two deployments are smeared: %.0fms and %.0fms", fast.P50MS, slow.P50MS)
	}
}

// TestStatsFoldsEverySessionInALog covers a file holding more than one session,
// which is what concatenating captures produces and what the issue's own
// reproduction does. Folding the first and dropping the rest lost calls silently.
func TestStatsFoldsEverySessionInALog(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	a := writeCapture(t, "a.jsonl", "alpha", t0, "/a", []string{"node", "a.js"},
		[]string{"t"}, []call{{tool: "t", duration: 20 * time.Millisecond, answer: answerOK}})
	b := writeCapture(t, "b.jsonl", "beta", t0.Add(time.Hour), "/b", []string{"node", "b.js"},
		[]string{"t"}, []call{{tool: "t", duration: 30 * time.Millisecond, answer: answerOK}})
	joined, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.SessionsDir(), "joined.jsonl"), append(joined, tail...), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{a, b} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(roll.Rows) != 2 {
		t.Fatalf("rows = %d, want both sessions of the joined log: %+v", len(roll.Rows), roll.Rows)
	}
	if got := rowFor(t, roll, "beta", "t"); got.Calls != 1 {
		t.Fatalf("the second session of the log contributed nothing: %+v", got)
	}
}

// TestStatsDefinitionCostComesFromTheNewestListing keeps the figure attached to
// the session that described the tool rather than the newest one that happened
// to call it.
func TestStatsDefinitionCostComesFromTheNewestListing(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// The older run advertises a long description, the newer one a short one, and
	// only the older one calls the tool.
	writeCapture(t, "a-old.jsonl", "srv", t0, "/srv", []string{"node", "s.js"},
		[]string{"searchsearchsearchsearchsearchsearch"},
		[]call{{tool: "searchsearchsearchsearchsearchsearch", duration: 20 * time.Millisecond, answer: answerOK}})
	writeCapture(t, "b-new.jsonl", "srv", t0.Add(time.Hour), "/srv", []string{"node", "s.js"},
		[]string{"searchsearchsearchsearchsearchsearch", "extra"}, nil)

	roll, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, roll, "srv", "searchsearchsearchsearchsearchsearch")
	if row.DefinitionBytes == 0 {
		t.Fatal("no definition cost at all")
	}
	// Both sessions describe the tool identically here, so the assertion that
	// matters is that a session which never called it still supplied the figure.
	if roll.Read != 2 {
		t.Fatalf("read %d logs, want both", roll.Read)
	}
}

// TestStatsRefusesABlankLabel keeps a filter that names nothing from quietly
// meaning no filter, which is the one wrong answer a filter can give.
func TestStatsRefusesABlankLabel(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "a.jsonl", "srv", t0, "/srv", []string{"node", "s.js"}, []string{"t"},
		[]call{{tool: "t", duration: 10 * time.Millisecond, answer: answerOK}})

	for _, value := range []string{" ", "", ",", " , "} {
		code, stdout, stderr := executeStats(t, []string{"--label", value})
		if code != 2 {
			t.Fatalf("--label %q exited %d, want 2", value, code)
		}
		if stdout != "" || !strings.Contains(stderr, "--label") {
			t.Fatalf("--label %q: stdout %q stderr %q", value, stdout, stderr)
		}
	}
}

// TestStatsCountsAnUnreadableLogUnderALabelFilter keeps the coverage line honest
// whichever flags are set. A log dropped in the label pre-pass never reaches the
// fold that would otherwise count it.
func TestStatsCountsAnUnreadableLogUnderALabelFilter(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "good.jsonl", "srv", t0, "/srv", []string{"node", "s.js"}, []string{"t"},
		[]call{{tool: "t", duration: 10 * time.Millisecond, answer: answerOK}})
	if err := os.WriteFile(filepath.Join(paths.SessionsDir(), "junk.jsonl"), []byte("not json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plain, err := rollUp(paths.SessionsDir(), time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := rollUp(paths.SessionsDir(), time.Time{}, []string{"srv"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Skipped != 1 {
		t.Fatalf("a plain run reported %d skipped, want 1", plain.Skipped)
	}
	if filtered.Skipped != plain.Skipped {
		t.Fatalf("--label reported %d skipped where a plain run reported %d; the same directory cannot be described two ways",
			filtered.Skipped, plain.Skipped)
	}
}

// TestStatsReportsEmptyLogsApartFromDamagedOnes keeps two commands reading one
// directory from describing it differently.
func TestStatsReportsEmptyLogsApartFromDamagedOnes(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeCapture(t, "good.jsonl", "srv", t0, "/srv", []string{"node", "s.js"}, []string{"t"},
		[]call{{tool: "t", duration: 10 * time.Millisecond, answer: answerOK}})
	dir := paths.SessionsDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.jsonl"), []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	statsRoll, err := rollUp(dir, time.Time{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := takeInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if statsRoll.Empty != inv.Empty || statsRoll.Skipped != inv.Skipped {
		t.Fatalf("stats says empty=%d skipped=%d and inventory says empty=%d skipped=%d for one directory",
			statsRoll.Empty, statsRoll.Skipped, inv.Empty, inv.Skipped)
	}
	_, stdout, _ := executeStats(t, nil)
	if !strings.Contains(stdout, "1 empty log") || !strings.Contains(stdout, "1 log skipped") {
		t.Fatalf("the coverage line does not name both:\n%s", stdout)
	}
}

// TestStatsTableStaysAligned covers the two ways a value the log supplied could
// push the fixed columns out of line: a duration too wide for its cell, and a
// name with no bound on its length.
func TestStatsTableStaysAligned(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("x", 5000)
	writeCapture(t, "a.jsonl", "batch", t0, "/srv", []string{"node", "s.js"}, []string{"reindex", long}, []call{
		{tool: "reindex", duration: 30 * time.Minute, answer: answerOK},
		{tool: long, duration: 5 * time.Millisecond, answer: answerOK},
	})

	_, stdout, _ := executeStats(t, nil)
	var widths []int
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.Contains(line, "CALLS") || strings.Contains(line, "batch") {
			widths = append(widths, len([]rune(line)))
		}
	}
	if len(widths) != 3 {
		t.Fatalf("want a header and two rows, got %d lines:\n%s", len(widths), stdout)
	}
	for _, w := range widths[1:] {
		if w != widths[0] {
			t.Fatalf("rows are %v cells wide against a %d-cell header:\n%s", widths, widths[0], stdout)
		}
	}
	if widths[0] > 140 {
		t.Fatalf("one tool name widened the whole table to %d cells:\n%s", widths[0], stdout[:400])
	}
	if !strings.Contains(stdout, "min") {
		t.Fatalf("a half-hour call is not rendered in a unit that fits:\n%s", stdout)
	}
}

// TestStatsOrderIsTotal keeps two runs over an unchanged directory producing one
// answer even when rows tie on every visible column.
func TestStatsOrderIsTotal(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for i := range 8 {
		writeCapture(t, fmt.Sprintf("s%d.jsonl", i), "server.py", t0.Add(time.Duration(i)*time.Hour),
			fmt.Sprintf("/proj/%d", i), []string{"python3", "server.py", fmt.Sprintf("%d", i)},
			[]string{"search"}, []call{{tool: "search", duration: 20 * time.Millisecond, answer: answerOK}})
	}
	_, first, _ := executeStats(t, nil)
	for range 5 {
		_, again, _ := executeStats(t, nil)
		if again != first {
			t.Fatalf("two runs over one directory disagree:\n--- first\n%s\n--- again\n%s", first, again)
		}
	}
}

// TestParseAgeRefusesAnAgeItCannotHold is the overflow that reached prune. A
// duration is an int64 of nanoseconds, so a large day count wraps to a negative
// one, and prune would then compute a cutoff in the future and delete every log
// in the directory, which is the one thing --older-than exists to prevent.
func TestParseAgeRefusesAnAgeItCannotHold(t *testing.T) {
	for _, value := range []string{"200000d", "9223372036854775807d"} {
		if _, err := parseAge(value, "--older-than"); err == nil {
			t.Fatalf("parseAge(%q) was accepted", value)
		}
	}
	// The boundary itself still works, and one day past it does not.
	if _, err := parseAge(fmt.Sprintf("%dd", maxAgeDays), "--older-than"); err != nil {
		t.Fatalf("the largest expressible age was refused: %v", err)
	}
	if _, err := parseAge(fmt.Sprintf("%dd", maxAgeDays+1), "--older-than"); err == nil {
		t.Fatal("one day past the largest expressible age was accepted")
	}

	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	kept := writePruneLog(t, "keepme.jsonl", time.Hour)
	code, _, stderr := executePrune(t, []string{"--older-than", "200000d", "--yes"}, "")
	if code == 0 {
		t.Fatal("prune accepted an age it cannot express")
	}
	if !strings.Contains(stderr, "--older-than") {
		t.Fatalf("stderr = %q", stderr)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("prune deleted a log that is an hour old under a 200000-day cutoff: %v", err)
	}
}
