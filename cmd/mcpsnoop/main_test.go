package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	hubpkg "github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func TestLabelFor(t *testing.T) {
	cases := []struct {
		cmd  []string
		want string
	}{
		{[]string{"npx", "-y", "@modelcontextprotocol/server-everything"}, "server-everything"},
		{[]string{"npx", "-y", "@modelcontextprotocol/server-filesystem", "/Users/me/code"}, "server-filesystem"},
		{[]string{"node", "build/index.js"}, "index.js"},
		{[]string{"python3", "-m", "my_mcp_server"}, "my_mcp_server"},
		{[]string{"uvx", "some-mcp"}, "some-mcp"},
		{[]string{"./bin/myserver"}, "myserver"},
		{[]string{"deno", "run", "-A", "server.ts"}, "server.ts"},
	}
	for _, c := range cases {
		if got := labelFor(c.cmd); got != c.want {
			t.Errorf("labelFor(%v) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// TestNewSessionIDIsUniquePerRun is the regression for a reused PID silently
// discarding a whole session. The hub deduplicates on a per-session high-water
// mark of Seq, so two runs sharing an id lose the second one entirely, with no
// gap reported and nothing on screen. Uniqueness therefore has to hold per run,
// which the PID cannot give, since it repeats.
func TestNewSessionIDIsUniquePerRun(t *testing.T) {
	const runs = 1000
	seen := make(map[string]bool, runs)
	for range runs {
		id := newSessionID("server.py")
		if seen[id] {
			t.Fatalf("two runs produced the same session id %q; a repeat here is the bug", id)
		}
		seen[id] = true
	}
}

// TestNewSessionIDGivesEachRunItsOwnLog covers the other half of the collision:
// the id is also the log file name, so two runs sharing one would append into a
// single file and leave one capture holding two sessions whose Seq both start
// at 1.
func TestNewSessionIDGivesEachRunItsOwnLog(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	first, second := newSessionID("server.py"), newSessionID("server.py")
	if paths.SessionLogPath(first) == paths.SessionLogPath(second) {
		t.Fatalf("two runs must not share a log file: %s", paths.SessionLogPath(first))
	}
}

// TestNewSessionIDStaysReadable keeps the id greppable and matchable to a
// process. It is what a person types into `mcpsnoop open` and what the shim
// prints on startup, so the label has to lead and the PID has to survive.
func TestNewSessionIDStaysReadable(t *testing.T) {
	id := newSessionID("server.py")
	if !strings.HasPrefix(id, "server.py-") {
		t.Fatalf("session id %q should start with the label", id)
	}
	if !strings.Contains(id, fmt.Sprintf("-%d-", os.Getpid())) {
		t.Fatalf("session id %q should still carry the pid", id)
	}
	// The id becomes a file name under the sessions directory, so the suffix
	// must not introduce a separator. (A label that does is issue #155.)
	if strings.ContainsAny(id, `/\`) {
		t.Fatalf("session id %q must be usable as a file name", id)
	}
}

// stubShim replaces the shim runner so routing tests can capture the wrapped
// command without spawning a process, and returns a restore func.
func stubShim(capture *[]string) func() {
	orig := runShimFn
	runShimFn = func(command []string, _, _ string, _ bool, _ proxy.RedactConfig, _ traceOptions) int {
		*capture = command
		return 0
	}
	return func() { runShimFn = orig }
}

func TestExecuteStampsHARCreatorVersion(t *testing.T) {
	// har.go defaults exporter.Version to "dev" so `go install` builds still
	// stamp something. execute() must overwrite it with the real binary version,
	// the same source the help overlay uses, or every tagged HAR reads "dev".
	var got []string
	defer stubShim(&got)()

	exporter.Version = "stale"
	if code := execute([]string{"--", "server"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if exporter.Version != appVersion() {
		t.Fatalf("HAR creator version = %q, want appVersion() %q", exporter.Version, appVersion())
	}
}

func TestRootPassesWrappedCommandThroughUntouched(t *testing.T) {
	var got []string
	defer stubShim(&got)()

	if code := execute([]string{"--label", "x", "--", "node", "server.js", "--inspect"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	want := []string{"node", "server.js", "--inspect"}
	if !slices.Equal(got, want) {
		t.Fatalf("wrapped command = %v, want %v", got, want)
	}
}

func TestRootDashDashDoesNotDispatchSubcommand(t *testing.T) {
	// `mcpsnoop -- http` must run a server named http, not the http subcommand.
	var got []string
	defer stubShim(&got)()

	if code := execute([]string{"--", "http"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !slices.Equal(got, []string{"http"}) {
		t.Fatalf("wrapped command = %v, want [http]", got)
	}
}

func TestRootWithoutDashDashStopsAtFirstPositional(t *testing.T) {
	// Without `--`, the wrapped command's own flags must not be parsed by mcpsnoop.
	var got []string
	defer stubShim(&got)()

	if code := execute([]string{"node", "server.js", "--inspect"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	want := []string{"node", "server.js", "--inspect"}
	if !slices.Equal(got, want) {
		t.Fatalf("wrapped command = %v, want %v", got, want)
	}
}

// A label names files: it flows verbatim into the session id and so into the
// trace path under SessionsDir, so `--label ../../evil` would write the trace
// outside the state dir and produce a session `open` cannot find again. It must
// be rejected up front with a clear error, not sanitised into a different label
// that would silently re-key the tool baseline.
func TestRootRejectsPathHostileLabel(t *testing.T) {
	for _, label := range []string{"../../evil", "srv/one", `srv\one`, "nul\x00label"} {
		var got []string
		restore := stubShim(&got)

		code := execute([]string{"--label", label, "--", "node", "server.js"})
		restore()
		if code != 2 {
			t.Fatalf("--label %q exit = %d, want 2", label, code)
		}
		if got != nil {
			t.Fatalf("the shim must not run under a rejected label %q, ran %v", label, got)
		}
	}

	// An ordinary label still wraps the command.
	var got []string
	defer stubShim(&got)()
	if code := execute([]string{"--label", "server-everything", "--", "node", "server.js"}); code != 0 {
		t.Fatalf("exit = %d, want 0 for a plain label", code)
	}
}

// The same rule holds for a label read from .mcpsnoop.toml, which is exactly
// the generated-or-shared file the check exists for.
func TestRootRejectsPathHostileLabelFromConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcpsnoop.toml"), []byte("label = \"../../evil\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var got []string
	defer stubShim(&got)()
	if code := execute([]string{"--", "node", "server.js"}); code != 2 {
		t.Fatalf("exit = %d, want 2 for a hostile config label", code)
	}
	if got != nil {
		t.Fatalf("the shim must not run under a rejected config label, ran %v", got)
	}
}

// TestHTTPRejectsPathHostileLabel checks the http command applies the same rule
// as the shim. It stubs the proxy rather than letting the command reach
// RunHTTP: without the stub, a run where the check fails to fire binds the real
// listen address and blocks until the package timeout, so the test that is meant
// to report one bad label instead takes the whole package down with it.
func TestHTTPRejectsPathHostileLabel(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	ran := false
	defer stubHTTP(&ran)()

	cmd := newHTTPCmd()
	cmd.SetArgs([]string{"--label", "../../evil", "--target", "http://localhost:1/mcp"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	var code exitCode
	if !errors.As(err, &code) || int(code) != 2 {
		t.Fatalf("http --label ../../evil = %v, want exit code 2", err)
	}
	if ran {
		t.Fatal("a rejected label must be refused before the proxy starts")
	}
}

// stubHTTP replaces the proxy runner and returns a function restoring it.
func stubHTTP(ran *bool) func() {
	prev := runHTTPFn
	runHTTPFn = func(context.Context, proxy.HTTPConfig) error {
		*ran = true
		return nil
	}
	return func() { runHTTPFn = prev }
}

func TestRootNoArgsRunsHubNotShim(t *testing.T) {
	hub := false
	gotLimit := -1
	origHub := runHubFn
	runHubFn = func(limit int) int {
		hub = true
		gotLimit = limit
		return 0
	}
	defer func() { runHubFn = origHub }()

	origShim := runShimFn
	runShimFn = func([]string, string, string, bool, proxy.RedactConfig, traceOptions) int {
		t.Fatal("shim ran for bare mcpsnoop, want hub")
		return 0
	}
	defer func() { runShimFn = origShim }()

	if code := execute(nil); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !hub {
		t.Fatal("bare mcpsnoop did not launch the hub")
	}
	if gotLimit != hubpkg.DefaultBackfillLimit {
		t.Fatalf("history limit = %d, want default %d", gotLimit, hubpkg.DefaultBackfillLimit)
	}
}

func TestRootHistoryLimitConfiguresHub(t *testing.T) {
	gotLimit := -1
	origHub := runHubFn
	runHubFn = func(limit int) int {
		gotLimit = limit
		return 0
	}
	defer func() { runHubFn = origHub }()

	if code := execute([]string{"--history-limit", "7"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if gotLimit != 7 {
		t.Fatalf("history limit = %d, want 7", gotLimit)
	}
}

func TestRedactKeysFlagParsesCommaSeparatedAndRepeatedValues(t *testing.T) {
	var flag redactKeysFlag
	if err := flag.Set("token, api_key"); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set("password"); err != nil {
		t.Fatal(err)
	}

	cfg := redactConfig(false, flag, nil, nil)
	if cfg.CommonSecrets {
		t.Fatal("CommonSecrets = true, want false")
	}
	if got, want := cfg.Keys, []string{"token", "api_key", "password"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if got := flag.String(); got != "token,api_key,password" {
		t.Fatalf("String() = %q, want token,api_key,password", got)
	}
}

func TestRedactKeysFlagConfigEnablesCommonSecretsPreset(t *testing.T) {
	var flag redactKeysFlag
	if err := flag.Set("custom_secret"); err != nil {
		t.Fatal(err)
	}

	cfg := redactConfig(true, flag, nil, nil)
	if !cfg.CommonSecrets {
		t.Fatal("CommonSecrets = false, want true")
	}
	if got, want := cfg.Keys, []string{"custom_secret"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestExportRedactsCapturedSessionWithoutModifyingInput(t *testing.T) {
	input, original := writeUnredactedSession(t)
	output := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(output, bytes.Repeat([]byte("stale"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"export", input, "--format", "json"}
	args = append(args, capturedSessionRedactionFlags()...)
	args = append(args, "--output", output)

	if code := execute(args); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(got) {
		t.Fatalf("export did not replace the existing output cleanly:\n%s", got)
	}
	assertCapturedSessionRedacted(t, got)
	assertFileUnchanged(t, input, original)
}

func TestExportRejectsSourceAsOutputWithoutModifyingInput(t *testing.T) {
	t.Run("path", func(t *testing.T) {
		input, original := writeUnredactedSession(t)
		if code := execute([]string{"export", input, "--redact-secrets", "--output", input}); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		assertFileUnchanged(t, input, original)
	})

	t.Run("hard link", func(t *testing.T) {
		input, original := writeUnredactedSession(t)
		output := filepath.Join(t.TempDir(), "same-session.jsonl")
		if err := os.Link(input, output); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if code := execute([]string{"export", input, "--redact-secrets", "--output", output}); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		assertFileUnchanged(t, input, original)
	})

	t.Run("stdin file", func(t *testing.T) {
		input, original := writeUnredactedSession(t)
		in, err := os.Open(input)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()

		cmd := newExportCmd()
		cmd.SetIn(in)
		cmd.SetArgs([]string{"-", "--redact-secrets", "--output", input})
		cmd.SilenceErrors = true
		err = cmd.Execute()
		var code exitCode
		if !errors.As(err, &code) || code != 1 {
			t.Fatalf("error = %v, want exit status 1", err)
		}
		assertFileUnchanged(t, input, original)
	})
}

func TestOpenRedactsCapturedSessionInMemoryWithoutModifyingInput(t *testing.T) {
	input, original := writeUnredactedSession(t)
	var opened *store.Store
	previous := runOpenTUIFn
	runOpenTUIFn = func(_ context.Context, st *store.Store) error {
		opened = st
		return nil
	}
	defer func() { runOpenTUIFn = previous }()

	args := []string{"open", input}
	args = append(args, capturedSessionRedactionFlags()...)
	if code := execute(args); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if opened == nil {
		t.Fatal("open did not pass a store to the TUI")
	}
	events := opened.Timeline("s1")
	if len(events) != 1 {
		t.Fatalf("timeline has %d events, want 1", len(events))
	}
	assertCapturedSessionRedacted(t, events[0].Raw)
	assertFileUnchanged(t, input, original)
}

func TestCapturedSessionReadDefaultsRemainUnredacted(t *testing.T) {
	input, original := writeUnredactedSession(t)
	output := filepath.Join(t.TempDir(), "session.json")
	if code := execute([]string{"export", input, "--format", "json", "--output", output}); code != 0 {
		t.Fatalf("export exit = %d, want 0", code)
	}
	exported, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"common-secret", "key-secret", "value-only-789", "path-secret"} {
		if !bytes.Contains(exported, []byte(secret)) {
			t.Fatalf("default export omitted %q:\n%s", secret, exported)
		}
	}

	var opened *store.Store
	previous := runOpenTUIFn
	runOpenTUIFn = func(_ context.Context, st *store.Store) error {
		opened = st
		return nil
	}
	defer func() { runOpenTUIFn = previous }()
	if code := execute([]string{"open", input}); code != 0 {
		t.Fatalf("open exit = %d, want 0", code)
	}
	if opened == nil {
		t.Fatal("open did not pass a store to the TUI")
	}
	events := opened.Timeline("s1")
	if len(events) != 1 {
		t.Fatalf("timeline has %d events, want 1", len(events))
	}
	for _, secret := range []string{"common-secret", "key-secret", "value-only-789", "path-secret"} {
		if !bytes.Contains(events[0].Raw, []byte(secret)) {
			t.Fatalf("default open omitted %q:\n%s", secret, events[0].Raw)
		}
	}
	assertFileUnchanged(t, input, original)
}

func TestCapturedSessionCommandsExposeRedactionFlags(t *testing.T) {
	exportCmd, openCmd := newExportCmd(), newOpenCmd()
	for _, flag := range []string{"redact-secrets", "redact-key", "redact-value", "redact-path"} {
		if exportCmd.Flags().Lookup(flag) == nil {
			t.Errorf("export command is missing --%s", flag)
		}
		if openCmd.Flags().Lookup(flag) == nil {
			t.Errorf("open command is missing --%s", flag)
		}
	}
}

func writeUnredactedSession(t *testing.T) (string, []byte) {
	t.Helper()
	input := filepath.Join(t.TempDir(), "session.jsonl")
	env := proxy.Envelope{
		SessionID:   "s1",
		ServerLabel: "server",
		Seq:         1,
		TS:          time.Unix(1, 0),
		Direction:   proxy.ClientToServer,
		Transport:   proxy.TransportStdio,
		Raw:         json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"authorization":"common-secret","project_token":"key-secret","note":"value-only-789","nested":{"private":"path-secret"},"keep":"visible"}}}`),
	}
	line, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	original := append(line, '\n')
	if err := os.WriteFile(input, original, 0o600); err != nil {
		t.Fatal(err)
	}
	return input, original
}

func capturedSessionRedactionFlags() []string {
	return []string{
		"--redact-secrets",
		"--redact-key", "project_token",
		"--redact-value", `value-only-[0-9]+`,
		"--redact-path", "$.params.arguments.nested.private",
	}
}

func assertCapturedSessionRedacted(t *testing.T, got []byte) {
	t.Helper()
	for _, secret := range []string{"common-secret", "key-secret", "value-only-789", "path-secret"} {
		if bytes.Contains(got, []byte(secret)) {
			t.Errorf("redacted view still contains %q:\n%s", secret, got)
		}
	}
	if !bytes.Contains(got, []byte("[REDACTED]")) {
		t.Errorf("redacted view does not contain the redaction marker:\n%s", got)
	}
	if !bytes.Contains(got, []byte("visible")) {
		t.Errorf("redacted view omitted the safe value:\n%s", got)
	}
}

func assertFileUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, want) {
		t.Fatal("read command modified the captured session")
	}
}

func TestResolveOpenSessionPathSupportsSessionIDNewestAndStdin(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", stateDir)

	older := paths.SessionLogPath("older")
	newer := paths.SessionLogPath("newer")
	if err := os.WriteFile(older, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	olderTime := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	newerTime := olderTime.Add(time.Hour)
	if err := os.Chtimes(older, olderTime, olderTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, newerTime, newerTime); err != nil {
		t.Fatal(err)
	}

	path, usedStdin, err := resolveOpenSessionPath("newer")
	if err != nil {
		t.Fatal(err)
	}
	if usedStdin || path != newer {
		t.Fatalf("resolveOpenSessionPath(\"newer\") = %q, %v; want %q, false", path, usedStdin, newer)
	}

	path, usedStdin, err = resolveOpenSessionPath("")
	if err != nil {
		t.Fatal(err)
	}
	if usedStdin || path != newer {
		t.Fatalf("resolveOpenSessionPath(\"\") = %q, %v; want newest %q, false", path, usedStdin, newer)
	}

	// An existing path outside the sessions directory passes through unchanged.
	external := filepath.Join(t.TempDir(), "capture.jsonl")
	if err := os.WriteFile(external, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, usedStdin, err = resolveOpenSessionPath(external)
	if err != nil {
		t.Fatal(err)
	}
	if usedStdin || path != external {
		t.Fatalf("resolveOpenSessionPath(%q) = %q, %v; want it unchanged, false", external, path, usedStdin)
	}

	path, usedStdin, err = resolveOpenSessionPath("-")
	if err != nil {
		t.Fatal(err)
	}
	if !usedStdin || path != "" {
		t.Fatalf("resolveOpenSessionPath(\"-\") = %q, %v; want empty path, true", path, usedStdin)
	}
}

func TestRedactValuesFlagParsesRepeatedRegexes(t *testing.T) {
	var flag redactValuesFlag
	if err := flag.Set(`sk-[A-Za-z0-9]+`); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set(`Bearer\s+\S+`); err != nil {
		t.Fatal(err)
	}

	cfg := redactConfig(false, nil, flag, nil)
	if got, want := cfg.ValuePatterns, []string{`sk-[A-Za-z0-9]+`, `Bearer\s+\S+`}; !slices.Equal(got, want) {
		t.Fatalf("ValuePatterns = %v, want %v", got, want)
	}
	if got := flag.String(); got != `sk-[A-Za-z0-9]+,Bearer\s+\S+` {
		t.Fatalf("String() = %q, want repeated regexes", got)
	}
}

func TestRedactValuesFlagRejectsInvalidRegex(t *testing.T) {
	var flag redactValuesFlag
	if err := flag.Set(`[`); err == nil {
		t.Fatal("Set returned nil, want invalid regex error")
	}
}

func TestRedactPathsFlagParsesRepeatedJSONPaths(t *testing.T) {
	var flag redactPathsFlag
	if err := flag.Set("$.params.arguments.password"); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set("$.result.token"); err != nil {
		t.Fatal(err)
	}

	if got, want := flag.String(), "$.params.arguments.password,$.result.token"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	cfg := redactConfig(false, nil, nil, flag)
	if len(cfg.Paths) != 2 {
		t.Fatalf("len(Paths) = %d, want 2", len(cfg.Paths))
	}
}

func TestRedactPathsFlagRejectsInvalidJSONPath(t *testing.T) {
	var flag redactPathsFlag
	if err := flag.Set("$.["); err == nil {
		t.Fatal("Set returned nil, want invalid JSONPath error")
	}
}

func TestOTLPHeadersFlagParsesRepeatedValues(t *testing.T) {
	var flag otlpHeadersFlag
	for _, value := range []string{
		"Authorization=Bearer test-token",
		"X-Tenant=team-a",
		"X-Tenant=team-b",
	} {
		if err := flag.Set(value); err != nil {
			t.Fatal(err)
		}
	}
	header := http.Header(flag)
	if got := header.Get("Authorization"); got != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := header.Values("X-Tenant"); !slices.Equal(got, []string{"team-a", "team-b"}) {
		t.Fatalf("X-Tenant = %v", got)
	}
}

func TestOTLPHeadersFlagRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"Authorization", "=token", "Bad Header=value", "X-Test=one\ntwo"} {
		var flag otlpHeadersFlag
		if err := flag.Set(value); err == nil {
			t.Fatalf("Set(%q) returned nil", value)
		}
	}
}

func TestParseTraceOptionsValidatesEndpointAndHeaderDependency(t *testing.T) {
	for _, endpoint := range []string{"collector:4318/v1/traces", "ftp://collector/v1/traces", "http:///v1/traces"} {
		if _, err := parseTraceOptions(endpoint, nil); err == nil {
			t.Fatalf("parseTraceOptions(%q) returned nil error", endpoint)
		}
	}
	if _, err := parseTraceOptions("", otlpHeadersFlag{"Authorization": {"Bearer token"}}); err == nil {
		t.Fatal("header without endpoint returned nil error")
	}
	got, err := parseTraceOptions("https://collector.example/v1/traces", otlpHeadersFlag{"Authorization": {"Bearer token"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.OTLPEndpoint != "https://collector.example/v1/traces" || got.OTLPHeaders.Get("Authorization") != "Bearer token" {
		t.Fatalf("trace options = %+v", got)
	}
}

func TestProxyCommandsExposeLiveOTLPFlags(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"otlp-endpoint", "otlp-header"} {
		if root.Flags().Lookup(name) == nil {
			t.Fatalf("root command is missing --%s", name)
		}
	}
	httpCmd, _, err := root.Find([]string{"http"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"otlp-endpoint", "otlp-header"} {
		if httpCmd.Flags().Lookup(name) == nil {
			t.Fatalf("http command is missing --%s", name)
		}
	}
}

func TestTraceSinkStreamsCompletedCallToOTLP(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []json.RawMessage `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		} else if len(payload.ResourceSpans) != 1 || len(payload.ResourceSpans[0].ScopeSpans) != 1 || len(payload.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
			t.Errorf("unexpected OTLP payload: %+v", payload)
		}
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	traceFile := filepath.Join(t.TempDir(), "session.jsonl")
	sink := traceSink("s1", traceFile, false, proxy.RedactConfig{}, traceOptions{
		OTLPEndpoint: server.URL,
		OTLPHeaders:  http.Header{"Authorization": {"Bearer test-token"}},
	})
	defer sink.Close()
	started := time.Unix(1_700_000_000, 0)
	sink.Emit(proxy.Envelope{
		SessionID: "s1", ServerLabel: "inventory", Seq: 1, TS: started,
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup"}}`),
	})
	sink.Emit(proxy.Envelope{
		SessionID: "s1", ServerLabel: "inventory", Seq: 2, TS: started.Add(20 * time.Millisecond),
		Direction: proxy.ServerToClient,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
	})

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live OTLP delivery")
	}
	if data, err := os.ReadFile(traceFile); err != nil {
		t.Fatal(err)
	} else if len(data) == 0 {
		t.Fatal("durable trace file is empty")
	}
}

func TestTraceSinkRedactsFileAndLiveSocket(t *testing.T) {
	stateDir := filepath.Join(os.TempDir(), fmt.Sprintf("mcpsnoop-test-%d", os.Getpid()))
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("MCPSNOOP_HOME", stateDir)

	ln, err := net.Listen("unix", paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	live := make(chan proxy.Envelope, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		defer conn.Close()

		var env proxy.Envelope
		if err := json.NewDecoder(conn).Decode(&env); err != nil {
			acceptErr <- err
			return
		}
		live <- env
	}()

	traceFile := filepath.Join(t.TempDir(), "session.jsonl")
	path, err := proxy.ParseRedactPath("$.params.token")
	if err != nil {
		t.Fatal(err)
	}
	sink := traceSink("s1", traceFile, false, proxy.RedactConfig{Paths: []proxy.RedactPath{path}}, traceOptions{})
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = sink.Close()
		}
	})

	sink.Emit(proxy.Envelope{
		SessionID: "s1",
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"params":{"token":"secret","keep":"visible"}}`),
	})

	select {
	case got := <-live:
		assertRawTokenRedacted(t, got.Raw)
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for redacted live socket envelope")
	}

	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	data, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("trace contains unredacted secret: %s", data)
	}
	var got proxy.Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("trace is invalid JSONL envelope: %v", err)
	}
	assertRawTokenRedacted(t, got.Raw)
}

func assertRawTokenRedacted(t *testing.T, raw json.RawMessage) {
	t.Helper()
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("raw payload contains unredacted secret: %s", raw)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("raw payload is invalid JSON: %v", err)
	}
	params := payload["params"].(map[string]any)
	if params["token"] != "[REDACTED]" {
		t.Fatalf("token = %v, want redacted", params["token"])
	}
	if params["keep"] != "visible" {
		t.Fatalf("keep = %v, want visible", params["keep"])
	}
}
