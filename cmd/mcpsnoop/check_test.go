package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/toolbaseline"
)

func TestCheckFailsOnSelectedSessionSignals(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writeCheckLog(t, paths.SessionLogPath("s1"), checkSignalEnvelopes()...)

	code, stdout, stderr := executeCheck(t, []string{"s1"}, "")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "session s1: errors=2 invalid=1 warnings=1 mismatches=0 pending=1\ncheck failed: error,invalid,warn\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckFailsOnlyOnSelectedSignals(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkSignalEnvelopes()...)

	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "invalid", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 because the fixture contains an invalid frame", code)
	}
	if stdout != "session s1: errors=2 invalid=1 warnings=1 mismatches=0 pending=1\ncheck failed: invalid\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckIgnoresUnselectedSignals(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	errorOnly := encodeCheckLog(t, checkErrorEnvelopes()...)
	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "invalid,warn", "-"}, errorOnly)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "session s1: errors=2 invalid=0 warnings=0 mismatches=0 pending=0\ncheck passed\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckPassesCleanSessionFromStdin(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	)

	code, stdout, stderr := executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "session s1: errors=0 invalid=0 warnings=0 mismatches=0 pending=0\nrecorded first-seen tool baseline (trusted, not verified)\ncheck passed\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckWritesJUnitGolden(t *testing.T) {
	// checkSignalEnvelopes carries a tools/list, so the run observes a baseline.
	// Isolate it the way the drift tests do, or it writes into the real state dir.
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkSignalEnvelopes()...)

	code, stdout, stderr := executeCheck(t, []string{"--format", "junit", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	const want = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="mcpsnoop check" tests="7" failures="3" errors="0" skipped="0" time="0">
  <testsuite name="s1" tests="7" failures="3" errors="0" skipped="0" time="0">
    <testcase classname="mcpsnoop.check" name="s1/error" time="0">
      <failure message="session s1 has 2 errors" type="mcpsnoop.check.error">session s1 has 2 errors</failure>
    </testcase>
    <testcase classname="mcpsnoop.check" name="s1/invalid" time="0">
      <failure message="session s1 has 1 invalid frame" type="mcpsnoop.check.invalid">session s1 has 1 invalid frame</failure>
    </testcase>
    <testcase classname="mcpsnoop.check" name="s1/warn" time="0">
      <failure message="session s1 has 1 warning" type="mcpsnoop.check.warn">session s1 has 1 warning</failure>
    </testcase>
    <testcase classname="mcpsnoop.check" name="s1/mismatch" time="0"></testcase>
    <testcase classname="mcpsnoop.check" name="s1/pending" time="0"></testcase>
    <testcase classname="mcpsnoop.check" name="s1/drift" time="0"></testcase>
    <testcase classname="mcpsnoop.check" name="s1/assertions" time="0"></testcase>
  </testsuite>
</testsuites>
`
	if stdout != want {
		t.Fatalf("stdout mismatch\nwant:\n%s\ngot:\n%s", want, stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckJUnitHonorsFailOn(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkErrorEnvelopes()...)

	code, stdout, stderr := executeCheck(t, []string{"--format", "junit", "--fail-on", "invalid,warn", "-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 because errors are not selected", code)
	}
	for _, want := range []string{`tests="7"`, `failures="0"`, `name="s1/error"`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "<failure") {
		t.Fatalf("stdout should not contain failures:\n%s", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckRejectsUnknownFormat(t *testing.T) {
	code, stdout, stderr := executeCheck(t, []string{"--format", "json", "-"}, "{}\n")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "mcpsnoop check: --format must be text or junit") {
		t.Fatalf("stderr = %q, want --format error", stderr)
	}
}

func TestCheckRejectsUnknownOrEmptyFailOnValues(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	for _, value := range []string{"error,bogus", ""} {
		t.Run(value, func(t *testing.T) {
			code, stdout, stderr := executeCheck(t, []string{"--fail-on", value, "-"}, "{}\n")
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "mcpsnoop check: --fail-on") {
				t.Fatalf("stderr = %q, want --fail-on error", stderr)
			}
		})
	}
}

func TestCheckReportsMalformedInput(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	code, stdout, stderr := executeCheck(t, []string{"-"}, "not-json\n")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "mcpsnoop check: stdin: invalid JSONL envelope") {
		t.Fatalf("stderr = %q, want malformed-input error", stderr)
	}
}

func TestCheckGatesEverySessionNotJustTheFirst(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	env := func(session string, seq uint64, dir proxy.Direction, raw string) proxy.Envelope {
		return proxy.Envelope{SessionID: session, ServerLabel: "srv", Seq: seq, TS: time.Unix(int64(seq), 0), Direction: dir, Raw: json.RawMessage(raw)}
	}
	// The first session is clean, the second carries an error.
	log := encodeCheckLog(t,
		env("s1", 1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		env("s1", 2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
		env("s2", 3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`),
		env("s2", 4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"boom"}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--fail-on", "error", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 because the second session has an error", code)
	}
	for _, want := range []string{"session s1:", "session s2:", "check failed: error"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q\n%s", want, stdout)
		}
	}
}

func TestCheckFailsOnHungCall(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	// A request with no response is a hung call that only the pending signal sees.
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hang"}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--fail-on", "pending", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a request that never got a response", code)
	}
	if !strings.Contains(stdout, "pending=1") || !strings.Contains(stdout, "check failed: pending") {
		t.Fatalf("stdout = %q", stdout)
	}

	// The default signals do not gate on pending, so the same log passes.
	code, stdout, _ = executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 since pending is opt-in", code)
	}
	if !strings.Contains(stdout, "check passed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCheckFailsOnRoutingMismatch(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	// The Mcp-Name header claims a safe tool while the body calls another one: a
	// routing mismatch (tool shadowing) that a compliant gateway would reject.
	shadow := checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dangerous"}}`)
	shadow.Transport, shadow.MCPMethod, shadow.MCPName = "http", "tools/call", "safe"
	log := encodeCheckLog(t, shadow,
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
	)

	// The dedicated signal gates on it specifically.
	code, stdout, _ := executeCheck(t, []string{"--fail-on", "mismatch", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a routing mismatch", code)
	}
	if !strings.Contains(stdout, "mismatches=1") || !strings.Contains(stdout, "check failed: mismatch") {
		t.Fatalf("stdout = %q", stdout)
	}

	// A clean session leaves the mismatch signal quiet.
	clean := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	)
	code, stdout, _ = executeCheck(t, []string{"--fail-on", "mismatch", "-"}, clean)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 for a clean session", code)
	}
	if !strings.Contains(stdout, "mismatches=0") || !strings.Contains(stdout, "check passed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCheckFailsOnToolDefinitionDriftWhenSelected(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	baseline := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search docs","inputSchema":{"type":"object"}}]}}`),
	)
	code, _, stderr := executeCheck(t, []string{"--fail-on", "drift", "-"}, baseline)
	if code != 0 || stderr != "" {
		t.Fatalf("baseline check = code %d, stderr %q", code, stderr)
	}

	changed := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search private docs","inputSchema":{"type":"object"}}]}}`),
	)
	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "drift", "-"}, changed)
	if code != 1 || stderr != "" {
		t.Fatalf("drift check = code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"definition drift:", "description changed: search", "check failed: drift"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q\n%s", want, stdout)
		}
	}

	code, stdout, stderr = executeCheck(t, []string{"-"}, changed)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "description changed: search") {
		t.Fatalf("default check = code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestCheckBaselineFlagRecordsThenDetectsDrift(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir()) // exercise --baseline, not the state dir
	dir := t.TempDir()
	baseline := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search docs","inputSchema":{"type":"object"}}]}}`),
	)

	// First run against an empty baseline dir records; it does not verify.
	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "drift", "--baseline", dir, "-"}, baseline)
	if code != 0 || stderr != "" {
		t.Fatalf("first run = code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "recorded first-seen tool baseline") {
		t.Fatalf("first run should announce it only recorded a baseline, got %q", stdout)
	}

	// The persisted directory lets the second run actually verify, and catch drift.
	changed := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search private docs","inputSchema":{"type":"object"}}]}}`),
	)
	code, stdout, stderr = executeCheck(t, []string{"--fail-on", "drift", "--baseline", dir, "-"}, changed)
	if code != 1 || stderr != "" {
		t.Fatalf("second run = code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"description changed: search", "check failed: drift"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("second run missing %q\n%s", want, stdout)
		}
	}
}

func TestCheckReportsCorruptBaselineWithoutFailingUnlessDriftSelected(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	// A corrupt baseline for the session's server label, as a crash once left behind.
	if err := os.WriteFile(toolbaseline.New(dir).Path("srv"), []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}}`),
	)

	// The default gate does not select drift, so the baseline problem is reported
	// but does not fail the run.
	code, stdout, stderr := executeCheck(t, []string{"--baseline", dir, "-"}, log)
	if code != 0 || stderr != "" {
		t.Fatalf("default gate = code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "tool baseline error:") || !strings.Contains(stdout, "check passed") {
		t.Fatalf("expected a reported baseline error and a pass, got %q", stdout)
	}

	// Selecting drift makes the same unverifiable baseline fail.
	code, stdout, stderr = executeCheck(t, []string{"--fail-on", "drift", "--baseline", dir, "-"}, log)
	if code != 1 || stderr != "" {
		t.Fatalf("drift gate = code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "tool baseline error:") || !strings.Contains(stdout, "check failed: drift") {
		t.Fatalf("drift should fail on a baseline error, got %q", stdout)
	}
}

func TestCheckReportsMissingLabelBaselineWithoutFailingUnlessDriftSelected(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	// A session with no server label cannot key a baseline, but that must not fail
	// a run that did not ask for drift.
	noLabel := func(seq uint64, dir proxy.Direction, raw string) proxy.Envelope {
		return proxy.Envelope{SessionID: "s1", ServerLabel: "", Seq: seq, TS: time.Unix(int64(seq), 0), Direction: dir, Raw: json.RawMessage(raw)}
	}
	log := encodeCheckLog(t,
		noLabel(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		noLabel(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("a missing label should not fail the default gate, code %d", code)
	}
	if !strings.Contains(stdout, "tool baseline error:") || !strings.Contains(stdout, "check passed") {
		t.Fatalf("expected a reported baseline error and a pass, got %q", stdout)
	}

	code, stdout, _ = executeCheck(t, []string{"--fail-on", "drift", "-"}, log)
	if code != 1 || !strings.Contains(stdout, "check failed: drift") {
		t.Fatalf("drift should fail on a missing-label baseline, got code %d stdout %q", code, stdout)
	}
}

func TestCheckMaxDurationAssertion(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	// echo takes one second: request at t=1s, response at t=2s.
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--max-duration", "500ms", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a call over the budget", code)
	}
	if !strings.Contains(stdout, `assertion failed: tool "echo" took 1s, over the 500ms budget`) {
		t.Fatalf("stdout = %q", stdout)
	}

	code, stdout, _ = executeCheck(t, []string{"--max-duration", "2s", "-"}, log)
	if code != 0 || !strings.Contains(stdout, "check passed") {
		t.Fatalf("a call within budget should pass, code %d stdout %q", code, stdout)
	}
}

func TestCheckExpectAndForbidToolAssertions(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
	)

	// expect-tool: satisfied when the tool was called, fails when it was not.
	if code, stdout, _ := executeCheck(t, []string{"--expect-tool", "echo", "-"}, log); code != 0 || !strings.Contains(stdout, "check passed") {
		t.Fatalf("echo should satisfy --expect-tool echo, code %d stdout %q", code, stdout)
	}
	code, stdout, _ := executeCheck(t, []string{"--expect-tool", "search", "-"}, log)
	if code != 1 || !strings.Contains(stdout, `assertion failed: expected tool "search" was never called`) {
		t.Fatalf("--expect-tool search should fail, code %d stdout %q", code, stdout)
	}

	// forbid-tool: passes when the tool was not called, fails when it was.
	if code, stdout, _ := executeCheck(t, []string{"--forbid-tool", "delete", "-"}, log); code != 0 || !strings.Contains(stdout, "check passed") {
		t.Fatalf("--forbid-tool delete should pass when delete was not called, code %d stdout %q", code, stdout)
	}
	code, stdout, _ = executeCheck(t, []string{"--forbid-tool", "echo", "-"}, log)
	if code != 1 || !strings.Contains(stdout, `assertion failed: forbidden tool "echo" was called`) {
		t.Fatalf("--forbid-tool echo should fail, code %d stdout %q", code, stdout)
	}
}

func TestCheckAssertionsCompose(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	// echo takes a second, and search is never called, so both assertions fail.
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
	)
	code, stdout, _ := executeCheck(t, []string{"--max-duration", "500ms", "--expect-tool", "search", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when either assertion fails", code)
	}
	for _, want := range []string{`tool "echo" took`, `expected tool "search" was never called`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q\n%s", want, stdout)
		}
	}
}

func TestCheckPassesForTruncatedBody(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	// A perfectly valid response whose observed copy was capped at maxFrameBytes. It
	// must not turn the default check red over an observation limit.
	trunc := checkEnvelope(1, proxy.ServerToClient, `{"jsonrpc":"2.0","result":{}}`)
	trunc.Truncated = true

	code, stdout, stderr := executeCheck(t, []string{"-"}, encodeCheckLog(t, trunc))
	if code != 0 || stderr != "" {
		t.Fatalf("a truncated body must not fail the default check, code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "warnings=0") || !strings.Contains(stdout, "check passed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCheckPassesForDeprecatedFeature(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	deprecated := checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"roots/list"}`)

	code, stdout, stderr := executeCheck(t, []string{"-"}, encodeCheckLog(t, deprecated))
	if code != 0 || stderr != "" {
		t.Fatalf("a deprecated feature must not fail the default check, code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "warnings=0") || !strings.Contains(stdout, "check passed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func executeCheck(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	cmd := newCheckCmd()
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(stdin))
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

func writeCheckLog(t *testing.T, path string, envelopes ...proxy.Envelope) {
	t.Helper()
	data := encodeCheckLog(t, envelopes...)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func encodeCheckLog(t *testing.T, envelopes ...proxy.Envelope) string {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, env := range envelopes {
		if err := enc.Encode(env); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func checkSignalEnvelopes() []proxy.Envelope {
	envelopes := checkErrorEnvelopes()
	return append(envelopes,
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 5, TS: time.Unix(5, 0), Direction: proxy.ServerToClient, Text: "not json-rpc"},
		checkEnvelope(6, proxy.ClientToServer, `{"id":3,"method":"tools/list"}`),
	)
}

func checkErrorEnvelopes() []proxy.Envelope {
	return []proxy.Envelope{
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"missing"}}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"missing"}}`),
		checkEnvelope(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"broken"}}`),
		checkEnvelope(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[]}}`),
	}
}

func checkEnvelope(seq uint64, direction proxy.Direction, raw string) proxy.Envelope {
	return proxy.Envelope{
		SessionID:   "s1",
		ServerLabel: "srv",
		Seq:         seq,
		TS:          time.Unix(int64(seq), 0),
		Direction:   direction,
		Raw:         json.RawMessage(raw),
	}
}
