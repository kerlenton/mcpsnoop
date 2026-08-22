package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	if stdout != "session s1: errors=2 invalid=1 warnings=1 mismatches=0 pending=1 late_results=0 deprecated=0 missing_frames=0 schema_findings=0\ncheck failed: error,invalid,warn\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckReportsLateResultsAndGatesOnlyWhenSelected(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow"}}`),
		checkEnvelope(2, proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`),
		checkEnvelope(3, proxy.ServerToClient, `{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`),
	)

	code, stdout, stderr := executeCheck(t, []string{"-"}, log)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "late_results=1") || !strings.Contains(stdout, "check passed") {
		t.Fatalf("default late-result check = code %d stdout %q stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = executeCheck(t, []string{"--fail-on", "late-result", "-"}, log)
	if code != 1 || stderr != "" || !strings.Contains(stdout, "check failed: late-result") {
		t.Fatalf("selected late-result check = code %d stdout %q stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = executeCheck(t, []string{"--format", "junit", "--fail-on", "late-result", "-"}, log)
	if code != 1 || stderr != "" {
		t.Fatalf("late-result junit = code %d stderr %q", code, stderr)
	}
	for _, want := range []string{`name="s1/late-result"`, `type="mcpsnoop.check.late-result"`, "1 late result"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("junit stdout missing %q\n%s", want, stdout)
		}
	}
}

func TestCheckFailsOnlyOnSelectedSignals(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkSignalEnvelopes()...)

	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "invalid", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 because the fixture contains an invalid frame", code)
	}
	if stdout != "session s1: errors=2 invalid=1 warnings=1 mismatches=0 pending=1 late_results=0 deprecated=0 missing_frames=0 schema_findings=0\ncheck failed: invalid\n" {
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
	if stdout != "session s1: errors=2 invalid=0 warnings=0 mismatches=0 pending=0 late_results=0 deprecated=0 missing_frames=0 schema_findings=0\ncheck passed\n" {
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
	if stdout != "session s1: errors=0 invalid=0 warnings=0 mismatches=0 pending=0 late_results=0 deprecated=0 missing_frames=0 schema_findings=0\nrecorded first-seen tool baseline (trusted, not verified)\ncheck passed\n" {
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
<testsuites name="mcpsnoop check" tests="11" failures="3" errors="0" skipped="0" time="0">
  <testsuite name="s1" tests="11" failures="3" errors="0" skipped="0" time="0">
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
    <testcase classname="mcpsnoop.check" name="s1/late-result" time="0"></testcase>
    <testcase classname="mcpsnoop.check" name="s1/drift" time="0"></testcase>
    <testcase classname="mcpsnoop.check" name="s1/deprecated" time="0"></testcase>
    <testcase classname="mcpsnoop.check" name="s1/incomplete" time="0"></testcase>
    <testcase classname="mcpsnoop.check" name="s1/schema" time="0"></testcase>
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
	for _, want := range []string{`tests="11"`, `failures="0"`, `name="s1/error"`} {
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
	if !strings.Contains(stderr, "mcpsnoop check: --format must be text, junit, or sarif") {
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

func TestCheckReportsMissingFramesWithoutFailingByDefault(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`),
		// Seq 3 and 4 were dropped upstream.
		checkEnvelope(5, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	)

	code, stdout, stderr := executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 because incomplete is opt-in", code)
	}
	if !strings.Contains(stdout, "missing_frames=2") {
		t.Fatalf("stdout = %q, want missing_frames=2", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckFailsOnIncompleteCapture(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(5, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	)

	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "incomplete", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a lossy capture", code)
	}
	if !strings.Contains(stdout, "missing_frames=3") || !strings.Contains(stdout, "check failed: incomplete") {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
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
	if !strings.Contains(stdout, `assertion failed: 1 tool call exceeded the 500ms budget (worst: tool "echo" took 1s)`) {
		t.Fatalf("stdout = %q", stdout)
	}

	code, stdout, _ = executeCheck(t, []string{"--max-duration", "2s", "-"}, log)
	if code != 0 || !strings.Contains(stdout, "check passed") {
		t.Fatalf("a call within budget should pass, code %d stdout %q", code, stdout)
	}
}

func TestCheckMaxDurationSummarizesSlowCalls(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
		checkEnvelope(3, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
		checkEnvelope(4, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`),
		checkEnvelope(9, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`),
		checkEnvelope(10, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lookup"}}`),
		checkEnvelope(13, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`),
		checkEnvelope(14, proxy.ClientToServer, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fast"}}`),
		checkEnvelope(15, proxy.ServerToClient, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`),
		checkEnvelope(16, proxy.ClientToServer, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"pending"}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--max-duration", "1s", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for calls over the budget", code)
	}
	if strings.Count(stdout, "assertion failed:") != 1 {
		t.Fatalf("stdout should contain one bounded assertion failure, got %q", stdout)
	}
	if !strings.Contains(stdout, `assertion failed: 3 tool calls exceeded the 1s budget (worst: tool "search" took 5s)`) {
		t.Fatalf("stdout = %q", stdout)
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
	for _, want := range []string{`worst: tool "echo" took 1s`, `expected tool "search" was never called`} {
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

func TestCheckFailsForSelectedDeprecatedFeature(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	deprecated := checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"roots/list"}`)

	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "deprecated", "-"}, encodeCheckLog(t, deprecated))
	if code != 1 || stderr != "" {
		t.Fatalf("deprecated gate = code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "check failed: deprecated") {
		t.Fatalf("stdout = %q, want deprecated failure", stdout)
	}
}

// The text output has to say how many deprecated calls there were, not just that
// there were some. The JUnit path already reports a count, and for a signal whose
// whole purpose is tracking a migration, "how many are left" is the question.
func TestCheckTextOutputReportsDeprecatedCount(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"roots/list"}`),
		checkEnvelope(2, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"sampling/createMessage"}`),
		checkEnvelope(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"logging/setLevel"}`),
	)

	// The count is reported whether or not the signal is selected, the same way
	// errors and warnings are always counted and only gate when chosen.
	_, stdout, _ := executeCheck(t, []string{"-"}, log)
	if !strings.Contains(stdout, "deprecated=3") {
		t.Fatalf("a default run should still report the count, got %q", stdout)
	}

	code, stdout, _ := executeCheck(t, []string{"--fail-on", "deprecated", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when the signal is selected", code)
	}
	if !strings.Contains(stdout, "deprecated=3") {
		t.Fatalf("stdout = %q, want the count alongside the failure", stdout)
	}
}

func TestCheckJUnitReportsSelectedDeprecatedFeature(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	deprecated := checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"roots/list"}`)

	code, stdout, stderr := executeCheck(t, []string{"--format", "junit", "--fail-on", "deprecated", "-"}, encodeCheckLog(t, deprecated))
	if code != 1 || stderr != "" {
		t.Fatalf("deprecated junit gate = code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{`name="s1/deprecated"`, `type="mcpsnoop.check.deprecated"`, `1 deprecated protocol feature`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("junit stdout missing %q\n%s", want, stdout)
		}
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

// TestCheckJUnitSurfacesTheBaselineState. junit rendered only the signal counts,
// so a run that recorded a first-seen baseline and verified nothing came out a
// fully green suite, which is the state the baseline mechanism exists to make
// visible, and a corrupt baseline was reported as a tool definition change,
// sending the reader after something that never happened.
func TestCheckJUnitSurfacesTheBaselineState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}}`),
	)

	_, stdout, _ := executeCheck(t, []string{"--format", "junit", "--baseline", dir, "-"}, log)
	if !strings.Contains(stdout, `<skipped message="recorded first-seen tool baseline`) {
		t.Fatalf("junit must say the run only recorded a baseline:\n%s", stdout)
	}

	// A baseline that cannot be read is not a tool change.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the first run should have written a baseline: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stdout, _ = executeCheck(t, []string{"--format", "junit", "--fail-on", "drift", "--baseline", dir, "-"}, log)
	if !strings.Contains(stdout, "tool baseline error:") {
		t.Fatalf("junit must name the baseline error rather than report drift:\n%s", stdout)
	}
	if strings.Contains(stdout, "tool definition change") {
		t.Fatalf("a baseline error must not be reported as a tool change:\n%s", stdout)
	}
}

// TestCheckFailsOnSchemaFindings. schema is the one member of checkSignalOrder
// with no gate test, and five separate mutations of its path survived the whole
// suite: the count returning zero, the selector being rejected, the section not
// being printed, the section being empty, and the junit reason falling through
// to "signal". This walks the flag end to end, from parsing it to failing on it
// to naming what failed.
func TestCheckFailsOnSchemaFindings(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
			`{"name":"search","inputSchema":{"type":"object","properties":{"q":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}},`+
			`{"name":"lookup","inputSchema":{"type":"object","$schema":"http://json-schema.org/draft-07/schema#"}}`+
			`]}}`),
	)

	// The observations never fail a default run, which is what makes them
	// observations rather than the violation.
	code, stdout, _ := executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d on the default gate, want 0\n%s", code, stdout)
	}
	if got := checkTextSignalCount(t, stdout, "schema_findings"); got != 2 {
		t.Fatalf("schema_findings = %d, want 2", got)
	}

	code, stdout, _ = executeCheck(t, []string{"--fail-on", "schema", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d under --fail-on schema, want 1\n%s", code, stdout)
	}
	// The section names the tool under its kind, or a failing run says only that
	// two findings exist and leaves the reader to find them.
	for _, want := range []string{"schema findings:", "oneOf: search", "nonDefaultDialect: lookup", "check failed: schema"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}

	_, junit, _ := executeCheck(t, []string{"--format", "junit", "--fail-on", "schema", "-"}, log)
	if !strings.Contains(junit, `<failure message="session s1 has 2 schema findings"`) {
		t.Fatalf("junit does not name the schema failure:\n%s", junit)
	}
}

// TestCheckRejectsAnUnknownSignalAndAcceptsEveryKnownOne. parseCheckSignals is a
// hand-kept switch beside checkSignalOrder, so a signal added to the list and
// forgotten there is rejected as a typo the moment somebody selects it.
func TestCheckRejectsAnUnknownSignalAndAcceptsEveryKnownOne(t *testing.T) {
	names := make([]string, 0, len(checkSignalOrder))
	for _, signal := range checkSignalOrder {
		names = append(names, string(signal))
	}
	if _, err := parseCheckSignals(strings.Join(names, ",")); err != nil {
		t.Fatalf("every signal in checkSignalOrder must be selectable: %v", err)
	}
	if _, err := parseCheckSignals("schemas"); err == nil {
		t.Fatal("an unknown signal must be rejected rather than silently ignored")
	}
}

// mrtrCheckLog is the capture from issue #201, encoded for check -. The server
// works 1.2s in total while the user takes 37s, so the wall clock is 38.2s.
func mrtrCheckLog(t *testing.T) string {
	t.Helper()
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight"}}`},
		{proxy.ServerToClient, 400, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"confirm":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 12400, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","requestState":"st-1","inputResponses":{"confirm":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 12700, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"input_required","requestState":"st-2","inputRequests":{"seat":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 37700, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"book_flight","requestState":"st-2","inputResponses":{"seat":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 38200, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`},
		{proxy.ClientToServer, 39000, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lookup_price"}}`},
		{proxy.ServerToClient, 39200, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`},
	}
	envs := make([]proxy.Envelope, 0, len(frames))
	for i, f := range frames {
		envs = append(envs, proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir,
			Raw: json.RawMessage(f.raw)})
	}
	return encodeCheckLog(t, envs...)
}

// TestCheckServerDurationBlamesTheServerAlone is the reason the flag exists.
// --max-duration blames a tool for 38.2s of which it is responsible for 1.2s,
// because the seconds a person spent answering are inside the span. That figure
// stays what it is, and this one names what it measures.
func TestCheckServerDurationBlamesTheServerAlone(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := mrtrCheckLog(t)

	code, stdout, _ := executeCheck(t, []string{"--max-server-duration", "1s", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a server over the budget", code)
	}
	if !strings.Contains(stdout, `1 tool call exceeded the 1s server budget (worst: tool "book_flight" held for 1.2s)`) {
		t.Fatalf("stdout = %q", stdout)
	}
	if strings.Contains(stdout, "38.2s") {
		t.Fatalf("the server budget reported the wall clock:\n%s", stdout)
	}

	code, stdout, _ = executeCheck(t, []string{"--max-server-duration", "2s", "-"}, log)
	if code != 0 || !strings.Contains(stdout, "check passed") {
		t.Fatalf("a server within budget should pass, code %d stdout %q", code, stdout)
	}
}

// TestCheckRoundTripsAssertion covers the second sibling, which multi round-trip
// requests make possible: the specification puts no bound on how many times a
// server may ask again.
func TestCheckRoundTripsAssertion(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := mrtrCheckLog(t)

	code, stdout, _ := executeCheck(t, []string{"--max-round-trips", "2", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a chain over the budget", code)
	}
	if !strings.Contains(stdout, `1 tool call exceeded the 2 round trip budget (worst: tool "book_flight" took 3)`) {
		t.Fatalf("stdout = %q", stdout)
	}

	code, stdout, _ = executeCheck(t, []string{"--max-round-trips", "3", "-"}, log)
	if code != 0 || !strings.Contains(stdout, "check passed") {
		t.Fatalf("a chain within budget should pass, code %d stdout %q", code, stdout)
	}
}

// TestCheckWallClockIsUnchangedByTheSiblings is the promise the whole flag
// surface rests on. Nobody's pipeline changes behaviour on upgrade, so
// --max-duration still means the whole interaction including the human.
func TestCheckWallClockIsUnchangedByTheSiblings(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := mrtrCheckLog(t)

	code, stdout, _ := executeCheck(t, []string{"--max-duration", "5s", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, `1 tool call exceeded the 5s budget (worst: tool "book_flight" took 38.2s)`) {
		t.Fatalf("--max-duration no longer means wall clock: %q", stdout)
	}

	// And the new ones are opt in, so a default run over the same capture passes.
	code, stdout, _ = executeCheck(t, []string{"-"}, log)
	if code != 0 || !strings.Contains(stdout, "check passed") {
		t.Fatalf("a default run should be unaffected, code %d stdout %q", code, stdout)
	}
}

// TestCheckSiblingsSkipAnOperationWithNoLatency keeps the two new assertions to
// the same rule --max-duration already applies. An operation still open has no
// latency to judge, and reporting one would blame a server for a chain the
// client walked away from.
func TestCheckSiblingsSkipAnOperationWithNoLatency(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	// A chain the client abandoned after two answers, over an hour of wall clock.
	log := encodeCheckLog(t,
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0, Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book"}}`)},
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Hour), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st","inputRequests":{"q":{"method":"elicitation/create"}}}}`)},
	)
	for _, args := range [][]string{
		{"--max-server-duration", "1ms", "-"},
		{"--max-round-trips", "1", "-"},
	} {
		code, stdout, _ := executeCheck(t, args, log)
		if code != 0 {
			t.Fatalf("%v exited %d on an operation that never completed: %s", args, code, stdout)
		}
	}
}

// TestCheckRoundTripsCountsAnUnfinishedChain is the case the assertion exists
// for. A server asking again and again produces exactly the operation nobody
// finishes, so gating the count on completion made the budget silent on its own
// headline case. A latency still needs an ending; a count of requests already
// made does not.
func TestCheckRoundTripsCountsAnUnfinishedChain(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight"}}`},
		{proxy.ServerToClient, 400, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"a":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 5000, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","requestState":"st-1","inputResponses":{"a":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 5400, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"input_required","requestState":"st-2","inputRequests":{"b":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 9000, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"book_flight","requestState":"st-2","inputResponses":{"b":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 9400, `{"jsonrpc":"2.0","id":3,"result":{"resultType":"input_required","requestState":"st-3","inputRequests":{"c":{"method":"elicitation/create"}}}}`},
	}
	envs := make([]proxy.Envelope, 0, len(frames))
	for i, f := range frames {
		envs = append(envs, proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir, Raw: json.RawMessage(f.raw)})
	}
	log := encodeCheckLog(t, envs...)

	code, stdout, _ := executeCheck(t, []string{"--max-round-trips", "2", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; the chain took three requests and nobody finished it\n%s", code, stdout)
	}
	if !strings.Contains(stdout, `exceeded the 2 round trip budget (worst: tool "book_flight" took 3)`) {
		t.Fatalf("stdout = %q", stdout)
	}

	// A latency, by contrast, still needs an ending, so the server budget stays
	// quiet on the same capture.
	code, stdout, _ = executeCheck(t, []string{"--max-server-duration", "1ms", "-"}, log)
	if code != 0 {
		t.Fatalf("the server budget judged an operation that never completed, code %d: %s", code, stdout)
	}
}
