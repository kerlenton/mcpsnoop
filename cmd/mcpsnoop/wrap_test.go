package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// wrapFixture is deliberately not a testdata file. It is written into t.TempDir
// by each test, so the bytes under test are the ones in this source and nothing
// on the way to a Windows checkout can rewrite them.
//
// It is also deliberately awkward: a four-space indent rather than the README's
// two, a top-level key that is not mcpServers, an entry carrying env as well as
// command and args, a second entry written on one line, and a trailing newline.
// Every one of those is something a whole-file re-encode would quietly destroy.
const wrapFixture = `{
    "globalShortcut": "Ctrl+Space",
    "mcpServers": {
        "everything": {
            "command": "npx",
            "args": [
                "-y",
                "@modelcontextprotocol/server-everything"
            ],
            "env": {
                "TOKEN": "secret"
            }
        },
        "other": { "command": "python", "args": ["server.py"] }
    }
}
`

// stubWrapperPath pins the command wrap writes, so assertions do not depend on
// where the test binary happens to live.
const stubWrapperPath = "/opt/homebrew/bin/mcpsnoop"

func newWrapTest(t *testing.T, config string) string {
	t.Helper()
	orig := wrapperPath
	wrapperPath = func() string { return stubWrapperPath }
	t.Cleanup(func() { wrapperPath = orig })

	path := filepath.Join(t.TempDir(), "claude_desktop_config.json")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func executeWrapCmd(t *testing.T, newCmd func() *cobra.Command, args ...string) (int, string, string) {
	t.Helper()
	cmd := newCmd()
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

func wrapOK(t *testing.T, newCmd func() *cobra.Command, args ...string) string {
	t.Helper()
	code, stdout, stderr := executeWrapCmd(t, newCmd, args...)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	return stdout
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// readEntry returns one server's command and args as the client would read them.
func readEntry(t *testing.T, path, server string) (string, []string) {
	t.Helper()
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			URL     string   `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readConfig(t, path)), &doc); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	entry, ok := doc.MCPServers[server]
	if !ok {
		t.Fatalf("config has no server %q", server)
	}
	return entry.Command, entry.Args
}

// TestWrapThenUnwrapRestoresTheConfigByteForByte is the acceptance criterion the
// whole design exists for. A decode-and-re-encode implementation cannot pass it:
// it reorders keys and reflows the file.
func TestWrapThenUnwrapRestoresTheConfigByteForByte(t *testing.T) {
	path := newWrapTest(t, wrapFixture)

	wrapOK(t, newWrapCmd, "everything", "--config", path)
	if got := readConfig(t, path); got == wrapFixture {
		t.Fatal("wrap did not change the config")
	}
	wrapOK(t, newUnwrapCmd, "everything", "--config", path)

	if got := readConfig(t, path); got != wrapFixture {
		t.Fatalf("unwrap did not restore the config byte for byte:\n got %q\nwant %q", got, wrapFixture)
	}
	if _, err := os.Stat(path + backupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a byte-for-byte restore should remove the backup, stat gave %v", err)
	}
}

// TestWrapRewritesOnlyTheNamedEntry pins the byte-splice: the bytes outside the
// target entry, including the sibling server's one-line formatting, the
// unrelated top-level key and the trailing newline, come through untouched.
func TestWrapRewritesOnlyTheNamedEntry(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	wrapOK(t, newWrapCmd, "everything", "--config", path)
	got := readConfig(t, path)

	for _, untouched := range []string{
		`    "globalShortcut": "Ctrl+Space",`,
		`        "other": { "command": "python", "args": ["server.py"] }`,
		`    "mcpServers": {`,
	} {
		if !strings.Contains(got, untouched) {
			t.Fatalf("wrap disturbed bytes outside the target entry, %q is gone:\n%s", untouched, got)
		}
	}
	if !strings.HasSuffix(got, "}\n") {
		t.Fatalf("wrap dropped the trailing newline:\n%q", got)
	}
	if !strings.Contains(got, `"TOKEN": "secret"`) {
		t.Fatalf("wrap dropped the entry's env block:\n%s", got)
	}

	command, args := readEntry(t, path, "everything")
	if command != stubWrapperPath {
		t.Fatalf("command = %q, want %q", command, stubWrapperPath)
	}
	want := []string{"--", "npx", "-y", "@modelcontextprotocol/server-everything"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}

	backup, err := os.ReadFile(path + backupSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != wrapFixture {
		t.Fatalf("the backup should hold the original bytes:\n got %q\nwant %q", backup, wrapFixture)
	}
}

func TestWrapIsIdempotent(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	wrapOK(t, newWrapCmd, "everything", "--config", path)
	afterFirst := readConfig(t, path)

	stdout := wrapOK(t, newWrapCmd, "everything", "--config", path)
	if !strings.Contains(stdout, "already wrapped") {
		t.Fatalf("a second wrap should say so, got %q", stdout)
	}
	if got := readConfig(t, path); got != afterFirst {
		t.Fatalf("a second wrap changed the config:\n got %q\nwant %q", got, afterFirst)
	}
	// The backup must still hold the pre-wrap config, not a wrapped one, or the
	// byte-for-byte restore would put a wrapped entry back.
	backup, err := os.ReadFile(path + backupSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != wrapFixture {
		t.Fatalf("a second wrap overwrote the backup:\n%s", backup)
	}

	// And unwrap still gets all the way home from there.
	wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if got := readConfig(t, path); got != wrapFixture {
		t.Fatalf("unwrap after a doubled wrap:\n got %q\nwant %q", got, wrapFixture)
	}
}

func TestUnwrapIsIdempotent(t *testing.T) {
	path := newWrapTest(t, wrapFixture)

	stdout := wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if !strings.Contains(stdout, "not wrapped") {
		t.Fatalf("unwrap on a plain entry should say so, got %q", stdout)
	}
	if got := readConfig(t, path); got != wrapFixture {
		t.Fatalf("unwrap on a plain entry changed the config:\n%s", got)
	}

	wrapOK(t, newWrapCmd, "everything", "--config", path)
	wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	stdout = wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if !strings.Contains(stdout, "not wrapped") {
		t.Fatalf("a second unwrap should say so, got %q", stdout)
	}
	if got := readConfig(t, path); got != wrapFixture {
		t.Fatalf("a second unwrap changed the config:\n%s", got)
	}
}

func TestWrapAndUnwrapDryRunWriteNothing(t *testing.T) {
	path := newWrapTest(t, wrapFixture)

	stdout := wrapOK(t, newWrapCmd, "everything", "--config", path, "--dry-run")
	for _, want := range []string{"dry run", "npx -y @modelcontextprotocol/server-everything", stubWrapperPath} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry run should report %q, got %q", want, stdout)
		}
	}
	if got := readConfig(t, path); got != wrapFixture {
		t.Fatalf("--dry-run wrote to the config:\n%s", got)
	}
	if _, err := os.Stat(path + backupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--dry-run created a backup, stat gave %v", err)
	}

	wrapOK(t, newWrapCmd, "everything", "--config", path)
	wrapped := readConfig(t, path)
	stdout = wrapOK(t, newUnwrapCmd, "everything", "--config", path, "--dry-run")
	if !strings.Contains(stdout, "dry run") {
		t.Fatalf("unwrap --dry-run should say so, got %q", stdout)
	}
	if got := readConfig(t, path); got != wrapped {
		t.Fatalf("unwrap --dry-run wrote to the config:\n%s", got)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatalf("unwrap --dry-run removed the backup: %v", err)
	}
}

// TestUnwrapKeepsAnEditMadeWhileWrapped is the reason the restore is conditional.
// Restoring the backup unconditionally would throw away a server the user added
// after wrapping.
func TestUnwrapKeepsAnEditMadeWhileWrapped(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	wrapOK(t, newWrapCmd, "everything", "--config", path)

	const added = `        "third": { "command": "sh" },` + "\n"
	edited := strings.Replace(readConfig(t, path), `    "mcpServers": {`+"\n", `    "mcpServers": {`+"\n"+added, 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout := wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if !strings.Contains(stdout, "changed since it was wrapped") {
		t.Fatalf("unwrap should say the config moved on, got %q", stdout)
	}
	got := readConfig(t, path)
	if !strings.Contains(got, `"third"`) {
		t.Fatalf("unwrap clobbered an edit made while wrapped:\n%s", got)
	}
	if command, args := readEntry(t, path, "everything"); command != "npx" || len(args) != 2 {
		t.Fatalf("the target entry was not unwrapped, command %q args %q", command, args)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatalf("the backup should be kept when the restore could not be confirmed: %v", err)
	}
}

// TestUnwrapWithoutABackupStillUnwraps: the backup is an optimisation for the
// byte-for-byte restore, never a dependency.
func TestUnwrapWithoutABackupStillUnwraps(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	wrapOK(t, newWrapCmd, "everything", "--config", path)
	if err := os.Remove(path + backupSuffix); err != nil {
		t.Fatal(err)
	}

	wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	command, args := readEntry(t, path, "everything")
	if command != "npx" {
		t.Fatalf("command = %q, want npx", command)
	}
	want := []string{"-y", "@modelcontextprotocol/server-everything"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
	if !sameJSON([]byte(readConfig(t, path)), []byte(wrapFixture)) {
		t.Fatalf("unwrap without a backup should still restore the document:\n%s", readConfig(t, path))
	}
}

func TestWrapReportsAProblemTheUserCanFix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		config string
		code   int
		want   []string
	}{
		{
			name: "unknown server",
			args: []string{"nope"}, config: wrapFixture, code: 2,
			want: []string{`no server named "nope"`, "everything", "other"},
		},
		{
			name: "unknown client",
			args: []string{"everything", "--client", "emacs"}, config: wrapFixture, code: 2,
			want: []string{`unknown client "emacs"`, claudeDesktopClient},
		},
		{
			name: "not a stdio server",
			args: []string{"remote"}, code: 2,
			config: `{"mcpServers":{"remote":{"url":"https://example.test/mcp","type":"http"}}}`,
			want:   []string{"not a stdio server", "mcpsnoop http --target"},
		},
		{
			name: "malformed config",
			args: []string{"everything"}, config: "{ not json", code: 1,
			want: []string{"as a JSON object"},
		},
		{
			name: "no mcpServers section",
			args: []string{"everything"}, config: `{"globalShortcut":"Ctrl+Space"}`, code: 1,
			want: []string{`no "mcpServers" section`},
		},
		{
			name: "empty mcpServers section",
			args: []string{"everything"}, config: `{"mcpServers":{}}`, code: 2,
			want: []string{`no server named "everything"`, "is empty"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := newWrapTest(t, tc.config)
			code, _, stderr := executeWrapCmd(t, newWrapCmd, append(tc.args, "--config", path)...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d (stderr %q)", code, tc.code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr %q should mention %q", stderr, want)
				}
			}
			if got := readConfig(t, path); got != tc.config {
				t.Fatalf("a failed wrap wrote to the config:\n%s", got)
			}
		})
	}
}

func TestWrapNamesTheMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "claude_desktop_config.json")
	code, _, stderr := executeWrapCmd(t, newWrapCmd, "everything", "--config", missing)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr)
	}
	for _, want := range []string{missing, "Claude Desktop", "--config"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q should mention %q", stderr, want)
		}
	}
}

// TestUnwrapRecoversTheCommandAfterTheFirstSeparator covers an entry a user wrote
// by hand with mcpsnoop's own flags in front of the wrapped command.
func TestUnwrapRecoversTheCommandAfterTheFirstSeparator(t *testing.T) {
	const config = `{
  "mcpServers": {
    "everything": {
      "command": "mcpsnoop",
      "args": ["--redact-secrets", "--", "npx", "-y", "server", "--", "extra"]
    }
  }
}
`
	path := newWrapTest(t, config)
	wrapOK(t, newUnwrapCmd, "everything", "--config", path)

	command, args := readEntry(t, path, "everything")
	if command != "npx" {
		t.Fatalf("command = %q, want npx", command)
	}
	// Everything after the first "--" belongs to the server, including a second
	// "--" that is the server's own argument.
	want := []string{"-y", "server", "--", "extra"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

// TestUnwrapRefusesAnEntryItCannotRecover: better a clear error than a guess at
// what the user meant to run.
func TestUnwrapRefusesAnEntryItCannotRecover(t *testing.T) {
	for _, config := range []string{
		`{"mcpServers":{"everything":{"command":"mcpsnoop","args":["--redact-secrets"]}}}`,
		`{"mcpServers":{"everything":{"command":"mcpsnoop","args":["--"]}}}`,
	} {
		path := newWrapTest(t, config)
		code, _, stderr := executeWrapCmd(t, newUnwrapCmd, "everything", "--config", path)
		if code != 1 {
			t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stderr, "edit the entry by hand") {
			t.Fatalf("stderr %q should say what to do", stderr)
		}
		if got := readConfig(t, path); got != config {
			t.Fatalf("a failed unwrap wrote to the config:\n%s", got)
		}
	}
}

// TestWrapRecognisesAWindowsWrappedEntry. Windows paths are inspected on every
// OS, by this test among others, and filepath.Base does not split a backslash
// off Windows, so an already-wrapped entry would be wrapped again.
func TestWrapRecognisesAWindowsWrappedEntry(t *testing.T) {
	const config = `{"mcpServers":{"everything":{"command":"C:\\Program Files\\mcpsnoop\\MCPSnoop.exe","args":["--","npx","server"]}}}`
	path := newWrapTest(t, config)

	if stdout := wrapOK(t, newWrapCmd, "everything", "--config", path); !strings.Contains(stdout, "already wrapped") {
		t.Fatalf("a windows-style mcpsnoop command should read as wrapped, got %q", stdout)
	}
	if got := readConfig(t, path); got != config {
		t.Fatalf("wrap rewrote an already-wrapped entry:\n%s", got)
	}
}

// TestWrapLeavesMarkupInAnArgumentAlone pins the jsonwire encoder. encoding/json
// would write & as \u0026 into the user's config, changing the argument the
// server is launched with.
func TestWrapLeavesMarkupInAnArgumentAlone(t *testing.T) {
	const config = `{"mcpServers":{"everything":{"command":"node","args":["--url=https://a.test/x?a=1&b=2<3"]}}}`
	path := newWrapTest(t, config)
	wrapOK(t, newWrapCmd, "everything", "--config", path)

	got := readConfig(t, path)
	if !strings.Contains(got, "https://a.test/x?a=1&b=2<3") {
		t.Fatalf("wrap escaped an argument:\n%s", got)
	}
	_, args := readEntry(t, path, "everything")
	want := []string{"--", "node", "--url=https://a.test/x?a=1&b=2<3"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

// TestWrapKeepsAOneLineEntryOnOneLine, and unwrap drops an args key the original
// entry never had rather than leaving an empty list behind.
func TestWrapKeepsAOneLineEntryOnOneLine(t *testing.T) {
	const config = `{
  "mcpServers": {
    "everything": { "command": "server-everything" }
  }
}
`
	path := newWrapTest(t, config)
	wrapOK(t, newWrapCmd, "everything", "--config", path)

	got := readConfig(t, path)
	if strings.Count(got, "\n") != strings.Count(config, "\n") {
		t.Fatalf("wrap reflowed a one-line entry:\n%s", got)
	}
	wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if got := readConfig(t, path); got != config {
		t.Fatalf("unwrap did not restore a one-line entry:\n got %q\nwant %q", got, config)
	}
}

// TestWrapKeepsTheConfigPermissions. A config's env block routinely holds API
// keys, so a rewrite must not widen the file, and the backup is owner-only
// whatever the config is.
func TestWrapKeepsTheConfigPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("unix permission bits")
	}
	path := newWrapTest(t, wrapFixture)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	wrapOK(t, newWrapCmd, "everything", "--config", path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("config mode = %o, want 640", got)
	}
	backup, err := os.Stat(path + backupSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if got := backup.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup mode = %o, want 600", got)
	}
}

// TestWrapClientsAreRegisteredNotHardcoded pins the extension seam: the commands
// read the registry, so a second client is a new file rather than an edit here.
func TestWrapClientsAreRegisteredNotHardcoded(t *testing.T) {
	client, err := lookupWrapClient(claudeDesktopClient)
	if err != nil {
		t.Fatal(err)
	}
	if client.serversKey != "mcpServers" || client.display == "" || client.restartHint == "" {
		t.Fatalf("claude desktop is registered incomplete: %+v", client)
	}
	path, err := client.configPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "claude_desktop_config.json" {
		t.Fatalf("configPath() = %q", path)
	}

	registerWrapClient(wrapClient{name: "test-client", display: "Test Client", serversKey: "servers",
		configPath: func() (string, error) { return "", nil }, restartHint: "restart it"})
	t.Cleanup(func() { delete(wrapClients, "test-client") })

	const config = `{"servers":{"everything":{"command":"node","args":["s.js"]}}}`
	cfgPath := newWrapTest(t, config)
	wrapOK(t, newWrapCmd, "everything", "--client", "test-client", "--config", cfgPath)
	if !strings.Contains(readConfig(t, cfgPath), stubWrapperPath) {
		t.Fatalf("wrap did not edit the registered client's config:\n%s", readConfig(t, cfgPath))
	}
	wrapOK(t, newUnwrapCmd, "everything", "--client", "test-client", "--config", cfgPath)
	if got := readConfig(t, cfgPath); got != config {
		t.Fatalf("unwrap for a registered client:\n got %q\nwant %q", got, config)
	}
}
