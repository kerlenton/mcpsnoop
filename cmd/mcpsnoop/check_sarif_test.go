package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestCheckSARIFCleanSessionDeclaresEveryRuleAndNoResults(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"ping"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{}}`),
	)

	code, stdout, stderr := executeCheck(t, []string{"--format", "sarif", "-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	// An empty array rather than null: that is what tells a consumer the alerts it
	// still holds are fixed, where null would read as "nothing was analysed".
	if !strings.Contains(stdout, `"results": []`) {
		t.Fatalf("a clean run must emit an empty results array:\n%s", stdout)
	}

	report := decodeCheckSARIF(t, stdout)
	if report.Schema == "" || report.Version != sarifVersion {
		t.Fatalf("$schema = %q, version = %q", report.Schema, report.Version)
	}
	if len(report.Runs) != 1 {
		t.Fatalf("runs = %d, want exactly one for the whole file", len(report.Runs))
	}
	driver := report.Runs[0].Tool.Driver
	if driver.Name != "mcpsnoop" || driver.Version == "" || driver.InformationURI == "" {
		t.Fatalf("driver = %+v", driver)
	}
	// Every rule on every run, clean or not, or a fixed finding's alert never
	// closes.
	want := []string{
		"mcpsnoop/error", "mcpsnoop/invalid", "mcpsnoop/warn", "mcpsnoop/mismatch",
		"mcpsnoop/pending", "mcpsnoop/drift", "mcpsnoop/deprecated", "mcpsnoop/incomplete",
		"mcpsnoop/assertion",
	}
	if len(driver.Rules) != len(want) {
		t.Fatalf("rules = %d, want %d", len(driver.Rules), len(want))
	}
	for i, id := range want {
		rule := driver.Rules[i]
		if rule.ID != id {
			t.Fatalf("rule %d = %q, want %q", i, rule.ID, id)
		}
		if rule.Name == "" || rule.ShortDescription.Text == "" || rule.FullDescription.Text == "" {
			t.Fatalf("rule %q is missing a name or a description: %+v", id, rule)
		}
	}
}

// TestCheckSARIFReportsOneResultPerFinding. The junit report collapses every
// finding of a kind into one count, which is the whole reason for this format:
// three warnings have to arrive as three results a reader can open.
func TestCheckSARIFReportsOneResultPerFinding(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkSignalEnvelopes()...)

	code, stdout, _ := executeCheck(t, []string{"--format", "sarif", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	report := decodeCheckSARIF(t, stdout)

	// The counts the text format prints for this fixture, one result each.
	for ruleID, want := range map[string]int{
		"mcpsnoop/error":   2,
		"mcpsnoop/invalid": 1,
		"mcpsnoop/warn":    1,
		"mcpsnoop/pending": 1,
	} {
		if got := len(sarifResultsFor(report, ruleID)); got != want {
			t.Fatalf("%s results = %d, want %d\n%s", ruleID, got, want, stdout)
		}
	}

	// The two errors are two distinct findings on two distinct frames, not one
	// message saying "2 errors".
	messages := sarifMessagesFor(report, "mcpsnoop/error")
	if messages[0] == messages[1] {
		t.Fatalf("the two errors must not share a message: %q", messages[0])
	}
	for _, message := range messages {
		if !strings.Contains(message, "session s1 frame ") {
			t.Fatalf("a result must name its session and frame: %q", message)
		}
		if strings.Contains(message, "has 2 errors") {
			t.Fatalf("a result must be a finding, not a per-session count: %q", message)
		}
	}
	if got := sarifMessagesFor(report, "mcpsnoop/error")[0]; !strings.Contains(got, "-32601") {
		t.Fatalf("the error result must carry what the server actually said: %q", got)
	}

	// Every result names a rule that was declared, and points at the right one.
	rules := report.Runs[0].Tool.Driver.Rules
	for _, result := range report.Runs[0].Results {
		if result.RuleIndex < 0 || result.RuleIndex >= len(rules) {
			t.Fatalf("ruleIndex %d is out of range", result.RuleIndex)
		}
		if rules[result.RuleIndex].ID != result.RuleID {
			t.Fatalf("ruleIndex %d points at %q, not %q", result.RuleIndex, rules[result.RuleIndex].ID, result.RuleID)
		}
		if result.PartialFingerprints[sarifFingerprintKey] == "" {
			t.Fatalf("result %q has no fingerprint", result.Message.Text)
		}
	}
}

// TestCheckSARIFLevelFollowsFailOn. The report follows the gate, never the other
// way round, so flipping the selection moves the levels and nothing else.
func TestCheckSARIFLevelFollowsFailOn(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkSignalEnvelopes()...)

	_, defaults, _ := executeCheck(t, []string{"--format", "sarif", "-"}, log)
	_, flipped, _ := executeCheck(t, []string{"--format", "sarif", "--fail-on", "pending", "-"}, log)

	byDefault := decodeCheckSARIF(t, defaults)
	byPending := decodeCheckSARIF(t, flipped)

	for _, tc := range []struct {
		ruleID              string
		underDefault, under string
	}{
		{"mcpsnoop/error", "error", "note"},
		{"mcpsnoop/warn", "error", "note"},
		{"mcpsnoop/pending", "note", "error"},
	} {
		for _, result := range sarifResultsFor(byDefault, tc.ruleID) {
			if result.Level != tc.underDefault {
				t.Fatalf("%s level = %q under the default selection, want %q", tc.ruleID, result.Level, tc.underDefault)
			}
		}
		for _, result := range sarifResultsFor(byPending, tc.ruleID) {
			if result.Level != tc.under {
				t.Fatalf("%s level = %q under --fail-on pending, want %q", tc.ruleID, result.Level, tc.under)
			}
		}
	}

	// Only the levels move. The findings themselves are what the capture holds,
	// which the selection has no say over.
	if got, want := sarifAllMessages(byPending), sarifAllMessages(byDefault); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the selection changed which findings were reported:\n%v\n%v", want, got)
	}
}

// TestCheckSARIFRegionIsTheTrueLine. Seq is per session and skips dropped
// frames, so writing it into startLine would send a reader to the wrong frame,
// which is worse than sending them nowhere.
func TestCheckSARIFRegionIsTheTrueLine(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	writeCheckLog(t, filepath.Join(dir, "two.jsonl"), gapEnvelopes()...)

	code, stdout, _ := executeCheck(t, []string{"--format", "sarif", "--fail-on", "warn,pending,incomplete", "two.jsonl"}, "")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, stdout)
	}
	report := decodeCheckSARIF(t, stdout)

	// alpha's warning rides Seq 5 on line 3, beta's Seq 1 on line 4: in both the
	// line and the Seq are different numbers.
	lines := map[string]int{}
	for _, result := range sarifResultsFor(report, "mcpsnoop/warn") {
		region := sarifRegionOf(t, result)
		lines[sarifSessionOf(t, result.Message.Text)] = region.StartLine
	}
	if lines["alpha"] != 3 {
		t.Fatalf("alpha's warning is on line %d, want 3", lines["alpha"])
	}
	if lines["beta"] != 4 {
		t.Fatalf("beta's warning is on line %d, want 4", lines["beta"])
	}

	// beta's hung tools/call is Seq 2 on line 5.
	var hung sarifResult
	for _, result := range sarifResultsFor(report, "mcpsnoop/pending") {
		if strings.Contains(result.Message.Text, "tools/call y") {
			hung = result
		}
	}
	if hung.RuleID == "" {
		t.Fatalf("no pending result for the hung call:\n%s", stdout)
	}
	if got := sarifRegionOf(t, hung).StartLine; got != 5 {
		t.Fatalf("beta's pending call is on line %d, want 5", got)
	}

	// A log inside the working directory is pointed at relatively, since code
	// scanning resolves the URI against the repository root.
	if uri := sarifURIOf(t, hung); uri != "two.jsonl" {
		t.Fatalf("artifact uri = %q, want the relative path", uri)
	}
}

// TestCheckSARIFIncompleteHasNoRegion. The dropped frames never reached the log,
// so there is no line to send a reader to.
func TestCheckSARIFIncompleteHasNoRegion(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	writeCheckLog(t, filepath.Join(dir, "two.jsonl"), gapEnvelopes()...)

	_, stdout, _ := executeCheck(t, []string{"--format", "sarif", "--fail-on", "incomplete", "two.jsonl"}, "")
	report := decodeCheckSARIF(t, stdout)

	results := sarifResultsFor(report, "mcpsnoop/incomplete")
	if len(results) != 1 {
		t.Fatalf("incomplete results = %d, want one for the only affected session\n%s", len(results), stdout)
	}
	if !strings.Contains(results[0].Message.Text, "alpha") {
		t.Fatalf("the incomplete result must name its session: %q", results[0].Message.Text)
	}
	if len(results[0].Locations) != 1 {
		t.Fatalf("the incomplete result must still point at the log: %+v", results[0].Locations)
	}
	if region := results[0].Locations[0].PhysicalLocation.Region; region != nil {
		t.Fatalf("a dropped frame has no line, got startLine %d", region.StartLine)
	}
}

// TestCheckSARIFFingerprintsSurviveTheLinesMoving. A fingerprint derived from
// the line would close every alert and reopen it as new whenever anything above
// it changed.
func TestCheckSARIFFingerprintsSurviveTheLinesMoving(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "run.jsonl")
	writeCheckLog(t, path, checkSignalEnvelopes()...)

	_, first, _ := executeCheck(t, []string{"--format", "sarif", "run.jsonl"}, "")
	_, again, _ := executeCheck(t, []string{"--format", "sarif", "run.jsonl"}, "")
	if first != again {
		t.Fatalf("two runs over the same log disagree:\n%s\n%s", first, again)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Every envelope moves down one line, and no finding changes.
	if err := os.WriteFile(path, append([]byte("\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, shifted, _ := executeCheck(t, []string{"--format", "sarif", "run.jsonl"}, "")

	before, after := decodeCheckSARIF(t, first), decodeCheckSARIF(t, shifted)
	if len(before.Runs[0].Results) != len(after.Runs[0].Results) {
		t.Fatalf("shifting the lines changed the findings")
	}
	movedALine := false
	for i, result := range before.Runs[0].Results {
		moved := after.Runs[0].Results[i]
		if got, want := moved.PartialFingerprints[sarifFingerprintKey], result.PartialFingerprints[sarifFingerprintKey]; got != want {
			t.Fatalf("fingerprint for %q changed from %q to %q when the lines moved", result.Message.Text, want, got)
		}
		if a, b := sarifRegionOf(t, result), sarifRegionOf(t, moved); b.StartLine == a.StartLine+1 {
			movedALine = true
		}
	}
	if !movedALine {
		t.Fatal("the fixture must actually move the lines, or the test proves nothing")
	}
}

func TestCheckSARIFStdinEmitsNoLocation(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkSignalEnvelopes()...)

	_, stdout, _ := executeCheck(t, []string{"--format", "sarif", "-"}, log)
	report := decodeCheckSARIF(t, stdout)
	if len(report.Runs[0].Results) == 0 {
		t.Fatalf("the fixture must produce findings:\n%s", stdout)
	}
	for _, result := range report.Runs[0].Results {
		// stdin has no artifact, and inventing a path would have code scanning
		// refuse the whole upload.
		if len(result.Locations) != 0 {
			t.Fatalf("a stdin run must not invent a location: %+v", result.Locations)
		}
	}
}

func TestCheckSARIFReportsAssertionFailures(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t, checkErrorEnvelopes()...)

	code, stdout, _ := executeCheck(t, []string{
		"--format", "sarif", "--fail-on", "invalid",
		"--max-duration", "1ms", "--expect-tool", "absent", "--forbid-tool", "missing", "-",
	}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	report := decodeCheckSARIF(t, stdout)

	results := sarifResultsFor(report, "mcpsnoop/assertion")
	if len(results) != 3 {
		t.Fatalf("assertion results = %d, want one per failed assertion\n%s", len(results), stdout)
	}
	joined := strings.Join(sarifMessagesFor(report, "mcpsnoop/assertion"), "\n")
	for _, want := range []string{"budget", `expected tool "absent"`, `forbidden tool "missing"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("assertion results missing %q:\n%s", want, joined)
		}
	}
	for _, result := range results {
		// An assertion only runs because the user asked for it, and it fails the run
		// outright, so it is never a note.
		if result.Level != "error" {
			t.Fatalf("assertion level = %q, want error", result.Level)
		}
	}
}

// TestCheckSARIFMarksTheFirstSeenBaseline. A run that recorded a baseline
// verified nothing, and a log with no result at all would read as though it had.
func TestCheckSARIFMarksTheFirstSeenBaseline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--format", "sarif", "--baseline", dir, "-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: recording a baseline is not a failure", code)
	}
	results := sarifResultsFor(decodeCheckSARIF(t, stdout), "mcpsnoop/drift")
	if len(results) != 1 {
		t.Fatalf("drift results = %d, want the baseline note\n%s", len(results), stdout)
	}
	if !strings.Contains(results[0].Message.Text, "recorded first-seen tool baseline (trusted, not verified)") {
		t.Fatalf("the baseline note must say what junit's <skipped> says: %q", results[0].Message.Text)
	}
	// A first-seen baseline is not drift, so its level never follows the gate.
	if results[0].Level != "note" {
		t.Fatalf("baseline level = %q, want note", results[0].Level)
	}
}

func TestCheckSARIFReportsDriftPerTool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	before := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"find","inputSchema":{"type":"object"}}]}}`),
	)
	if code, _, stderr := executeCheck(t, []string{"--format", "sarif", "--baseline", dir, "-"}, before); code != 0 {
		t.Fatalf("recording the baseline failed: %d %s", code, stderr)
	}

	after := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"find and delete","inputSchema":{"type":"object"}}]}}`),
	)
	code, stdout, _ := executeCheck(t, []string{"--format", "sarif", "--fail-on", "drift", "--baseline", dir, "-"}, after)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	results := sarifResultsFor(decodeCheckSARIF(t, stdout), "mcpsnoop/drift")
	if len(results) != 1 {
		t.Fatalf("drift results = %d, want one per changed tool\n%s", len(results), stdout)
	}
	if !strings.Contains(results[0].Message.Text, `tool "search" description changed`) {
		t.Fatalf("the drift result must name the tool and what changed: %q", results[0].Message.Text)
	}
	if results[0].Level != "error" {
		t.Fatalf("drift level = %q, want error when drift is selected", results[0].Level)
	}
}

func TestCheckSARIFRejectsAnUnknownFormat(t *testing.T) {
	code, stdout, stderr := executeCheck(t, []string{"--format", "sarif-2", "-"}, "{}\n")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--format must be text, junit, or sarif") {
		t.Fatalf("stderr = %q, want all three formats named", stderr)
	}
}

// TestCheckSARIFArtifactURIStaysAbsoluteOutsideTheCheckout. A log the workflow
// never committed cannot honestly be given a path relative to the repository.
func TestCheckSARIFArtifactURIStaysAbsoluteOutsideTheCheckout(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	logDir := t.TempDir()
	t.Chdir(t.TempDir())
	path := filepath.Join(logDir, "run.jsonl")
	writeCheckLog(t, path, checkErrorEnvelopes()...)

	_, stdout, _ := executeCheck(t, []string{"--format", "sarif", path}, "")
	report := decodeCheckSARIF(t, stdout)
	results := sarifResultsFor(report, "mcpsnoop/error")
	if len(results) == 0 {
		t.Fatalf("the fixture must produce findings:\n%s", stdout)
	}
	if uri := sarifURIOf(t, results[0]); !strings.HasPrefix(uri, "file:///") {
		t.Fatalf("artifact uri = %q, want an absolute file: URI", uri)
	}
}

// gapEnvelopes is a two-session log in which the line and the Seq differ for
// every frame after the first session's gap.
func gapEnvelopes() []proxy.Envelope {
	env := func(session string, seq uint64, dir proxy.Direction, raw string) proxy.Envelope {
		return proxy.Envelope{
			SessionID: session, ServerLabel: "srv", Seq: seq,
			TS: time.Unix(int64(seq), 0), Direction: dir, Raw: json.RawMessage(raw),
		}
	}
	return []proxy.Envelope{
		// line 1 and 2
		env("alpha", 1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`),
		env("alpha", 2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
		// line 3, Seq 5: two frames were dropped upstream
		env("alpha", 5, proxy.ClientToServer, `{"id":3,"method":"tools/list"}`),
		// line 4, Seq 1 of a different session
		env("beta", 1, proxy.ClientToServer, `{"id":9,"method":"tools/list"}`),
		// line 5, a call that never gets an answer
		env("beta", 2, proxy.ClientToServer, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"y"}}`),
	}
}

func decodeCheckSARIF(t *testing.T, stdout string) sarifLog {
	t.Helper()
	var report sarifLog
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if len(report.Runs) != 1 {
		t.Fatalf("runs = %d, want exactly one", len(report.Runs))
	}
	return report
}

func sarifResultsFor(report sarifLog, ruleID string) []sarifResult {
	var out []sarifResult
	for _, result := range report.Runs[0].Results {
		if result.RuleID == ruleID {
			out = append(out, result)
		}
	}
	return out
}

func sarifMessagesFor(report sarifLog, ruleID string) []string {
	var out []string
	for _, result := range sarifResultsFor(report, ruleID) {
		out = append(out, result.Message.Text)
	}
	return out
}

func sarifAllMessages(report sarifLog) []string {
	out := make([]string, 0, len(report.Runs[0].Results))
	for _, result := range report.Runs[0].Results {
		out = append(out, result.Message.Text)
	}
	return out
}

func sarifRegionOf(t *testing.T, result sarifResult) sarifRegion {
	t.Helper()
	if len(result.Locations) != 1 || result.Locations[0].PhysicalLocation.Region == nil {
		t.Fatalf("result %q has no region: %+v", result.Message.Text, result.Locations)
	}
	return *result.Locations[0].PhysicalLocation.Region
}

func sarifURIOf(t *testing.T, result sarifResult) string {
	t.Helper()
	if len(result.Locations) != 1 {
		t.Fatalf("result %q has no location", result.Message.Text)
	}
	return result.Locations[0].PhysicalLocation.ArtifactLocation.URI
}

// sarifSessionOf pulls the session id back out of a message, so a test can group
// results the way a reader would.
func sarifSessionOf(t *testing.T, message string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(message, "session ")
	if !ok {
		t.Fatalf("message does not name a session: %q", message)
	}
	id, _, ok := strings.Cut(rest, " ")
	if !ok {
		t.Fatalf("message does not name a session: %q", message)
	}
	return id
}

// TestCheckSARIFReportsErrorsThatCarryNoCall. `error` is one of the three
// default --fail-on signals, and the store counts it on frames that have no
// correlated call at all: an HTTP failure with no JSON-RPC body, and an error
// response whose id matches no request. Deriving the results from a call's
// Errored flag left both uncounted, so the run failed with an empty results
// array — a red build with nothing for the reader to open.
func TestCheckSARIFReportsErrorsThatCarryNoCall(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		// A bare transport failure: a status, no body of its own.
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Unix(1, 0),
			Direction: proxy.ServerToClient, Status: 503,
		},
		// An error response answering a request this capture never saw.
		checkEnvelope(2, proxy.ServerToClient,
			`{"jsonrpc":"2.0","id":99,"error":{"code":-32601,"message":"no such method"}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--format", "sarif", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1: neither error is a call, but both fail the gate", code)
	}
	results := sarifResultsFor(decodeCheckSARIF(t, stdout), "mcpsnoop/error")
	if len(results) != 2 {
		t.Fatalf("mcpsnoop/error results = %d, want 2 (one per counted error):\n%s", len(results), stdout)
	}
	// Named, not just counted: "frame 1" alone does not tell a reader that the
	// transport answered 503 rather than that a call returned an error.
	if !strings.Contains(results[0].Message.Text, "the transport failed with HTTP 503") {
		t.Fatalf("transport failure is not named: %q", results[0].Message.Text)
	}
	if !strings.Contains(results[1].Message.Text, "could not be matched to a call") {
		t.Fatalf("unmatched error response is not named: %q", results[1].Message.Text)
	}
}

// TestCheckSARIFAnchorsATaskFailureAtItsTerminalFrame. A task's terminal failure
// mutates the originating call in place, so reading Errored off that call marked
// its *first* response — the "still working" acknowledgement — and sent a Code
// Scanning reviewer to a frame where nothing went wrong.
func TestCheckSARIFAnchorsATaskFailureAtItsTerminalFrame(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	// A file rather than stdin, since the region is the point of the test and a
	// piped log has no artifact to point into.
	writeCheckLog(t, filepath.Join(dir, "task.jsonl"),
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"task","taskId":"t-1","status":"working"}}`),
		checkEnvelope(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"taskId":"t-1"}}`),
		checkEnvelope(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"taskId":"t-1","status":"failed"}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--format", "sarif", "task.jsonl"}, "")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	results := sarifResultsFor(decodeCheckSARIF(t, stdout), "mcpsnoop/error")
	if len(results) != 1 {
		t.Fatalf("mcpsnoop/error results = %d, want 1:\n%s", len(results), stdout)
	}
	if !strings.Contains(results[0].Message.Text, "frame 4") {
		t.Fatalf("a task failure must anchor at the frame that failed it, not at the acknowledgement: %q",
			results[0].Message.Text)
	}
	// The region follows the same frame, so the alert opens on the line the
	// failure actually arrived on.
	if got := sarifRegionOf(t, results[0]).StartLine; got != 4 {
		t.Fatalf("region startLine = %d, want 4", got)
	}
}

// TestCheckSARIFErrorResultsMatchTheGateCount. The two bugs above were both the
// same shape: the gate counted an error the report never named. This pins the
// invariant itself over a fixture mixing all four counted kinds, so a new error
// source cannot be added to the store without the report following it.
func TestCheckSARIFErrorResultsMatchTheGateCount(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		// A settled call that failed.
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`),
		// A task that ended failed.
		checkEnvelope(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow"}}`),
		checkEnvelope(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"task","taskId":"t-1","status":"working"}}`),
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 5, TS: time.Unix(5, 0), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"t-1","status":"failed"}}`),
		},
		// A transport failure and an unmatched error response.
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 6, TS: time.Unix(6, 0),
			Direction: proxy.ServerToClient, Status: 500,
		},
		checkEnvelope(7, proxy.ServerToClient,
			`{"jsonrpc":"2.0","id":99,"error":{"code":-32601,"message":"no such method"}}`),
	)

	_, textOut, _ := executeCheck(t, []string{"-"}, log)
	want := checkTextSignalCount(t, textOut, "errors")
	if want != 4 {
		t.Fatalf("fixture counted errors = %d, want 4 (one of each kind):\n%s", want, textOut)
	}

	_, sarifOut, _ := executeCheck(t, []string{"--format", "sarif", "-"}, log)
	results := sarifResultsFor(decodeCheckSARIF(t, sarifOut), "mcpsnoop/error")
	if len(results) != want {
		t.Fatalf("sarif reported %d errors, the gate counted %d:\n%s", len(results), want, sarifOut)
	}
}

// checkTextSignalCount reads one signal's count back out of the text report, so
// a test can compare a format against the gate rather than against a constant.
func checkTextSignalCount(t *testing.T, out, signal string) int {
	t.Helper()
	for field := range strings.FieldsSeq(out) {
		value, ok := strings.CutPrefix(field, signal+"=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("%s= is not a number: %q", signal, value)
		}
		return n
	}
	t.Fatalf("text report names no %s count:\n%s", signal, out)
	return 0
}
