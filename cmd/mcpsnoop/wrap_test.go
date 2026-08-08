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
			want: []string{"as JSON", "invalid character"},
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

// TestWrapFollowsASymlinkedConfig. A config kept under stow or chezmoi is a
// symlink into a dotfile repository. os.Rename does not follow the final
// component, so renaming onto the link replaced it with a regular file and the
// repository copy stopped being the file Claude Desktop reads, with nothing said
// about it either way.
func TestWrapFollowsASymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "claude.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(wrapFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	link := newWrapTest(t, wrapFixture)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this filesystem will not take a symlink: %v", err)
	}

	wrapOK(t, newWrapCmd, "everything", "--config", link)

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file, so the dotfile repository no longer owns the config")
	}
	if !strings.Contains(readConfig(t, real), stubWrapperPath) {
		t.Fatal("the edit did not reach the file the link points at")
	}
}

// TestUnwrapKeepsAnEditJSONNormalisationWouldHide. The restore overwrites the
// whole file, so the check that the backup still describes it has to keep every
// distinction the file makes. Deciding it by unmarshalling into any does not:
// every number lands in a float64, so correcting an id from ...92 to ...93 while
// wrapped compared equal, and unwrap put the old value back and deleted the
// backup, leaving no way to it.
func TestUnwrapKeepsAnEditJSONNormalisationWouldHide(t *testing.T) {
	const withID = `{
    "accountId": 9007199254740992,
    "mcpServers": {
        "everything": {
            "command": "npx",
            "args": ["server.js"]
        }
    }
}
`
	path := newWrapTest(t, withID)
	wrapOK(t, newWrapCmd, "everything", "--config", path)

	edited := strings.Replace(readConfig(t, path), "9007199254740992", "9007199254740993", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	wrapOK(t, newUnwrapCmd, "everything", "--config", path)

	if got := readConfig(t, path); !strings.Contains(got, "9007199254740993") {
		t.Fatalf("the correction was reverted by the restore:\n%s", got)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatal("the backup was removed even though it no longer describes the config")
	}
}

// TestWrapRefusesADuplicateServerName. Every JSON parser keeps the last of a
// repeated name and this walk finds the first, so editing here rewrote an entry
// the client never reads. wrap reported success, the traffic did not change, and
// there was nothing to go on.
func TestWrapRefusesADuplicateServerName(t *testing.T) {
	const duplicated = `{
    "mcpServers": {
        "everything": { "command": "old", "args": ["v1"] },
        "everything": { "command": "new", "args": ["v2"] }
    }
}
`
	path := newWrapTest(t, duplicated)
	before := readConfig(t, path)

	code, _, stderr := executeWrapCmd(t, newWrapCmd, "everything", "--config", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "appears twice") {
		t.Fatalf("the message must name the duplicate: %q", stderr)
	}
	if readConfig(t, path) != before {
		t.Fatal("the config was rewritten anyway")
	}
}

// TestWrapKeepsTheUntouchedBackupAcrossTwoServers. There is one backup per
// config, and its whole value is being the file as it was before mcpsnoop
// touched anything. Overwriting it on the second wrap put an already-wrapped
// config there, and unwrapping that second server then matched it, restored it
// and deleted it while the first server was still wrapped, leaving a modified
// config and no backup at all under a message that reads as an all-clear.
func TestWrapKeepsTheUntouchedBackupAcrossTwoServers(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	backup := path + backupSuffix

	wrapOK(t, newWrapCmd, "everything", "--config", path)
	wrapOK(t, newWrapCmd, "other", "--config", path)
	if got := readConfig(t, backup); got != wrapFixture {
		t.Fatalf("the second wrap overwrote the backup:\n%s", got)
	}

	wrapOK(t, newUnwrapCmd, "other", "--config", path)
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("the backup went while \"everything\" was still wrapped")
	}

	out := wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if got := readConfig(t, path); got != wrapFixture {
		t.Fatalf("the config did not come back byte for byte:\n%s", got)
	}
	if _, err := os.Stat(backup); err == nil {
		t.Fatal("the backup stayed even though nothing is wrapped any more")
	}
	if !strings.Contains(out, "nothing is wrapped any more") {
		t.Fatalf("unwrap must say why it removed the backup: %q", out)
	}
}

// TestUnwrapSaysTheBackupIsStillThere. On the splice path the backup stays, and
// it holds a second copy of every env block in the config. A user who is not
// told it is there has no reason to go looking for it.
func TestUnwrapSaysTheBackupIsStillThere(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	wrapOK(t, newWrapCmd, "everything", "--config", path)

	edited := strings.Replace(readConfig(t, path), "Ctrl+Space", "Alt+Space", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out := wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if !strings.Contains(out, "secrets in env blocks included") {
		t.Fatalf("unwrap must say the backup is a copy of the config: %q", out)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatal("the backup should be kept when the config moved on")
	}
}

// TestWrapKeepsTheConfigsLineEndings. encoding/json ends every line with a bare
// \n, so a Windows config came back with CRLF outside the rewritten entry and LF
// inside it. Mixed terminators in one file are a whole-file diff the next time
// anything normalises it, on the platform this command goes out of its way to
// support.
func TestWrapKeepsTheConfigsLineEndings(t *testing.T) {
	path := newWrapTest(t, strings.ReplaceAll(wrapFixture, "\n", "\r\n"))

	wrapOK(t, newWrapCmd, "everything", "--config", path)

	got := readConfig(t, path)
	if cr, lf := strings.Count(got, "\r\n"), strings.Count(got, "\n"); cr != lf {
		t.Fatalf("%d CRLF against %d LF, so the file now mixes terminators:\n%q", cr, lf, got)
	}
	wrapOK(t, newUnwrapCmd, "everything", "--config", path)
	if got := readConfig(t, path); got != strings.ReplaceAll(wrapFixture, "\n", "\r\n") {
		t.Fatalf("a CRLF config did not survive the round trip:\n%q", got)
	}
}

// TestWrapRefusesAnEmptyConfigFlag. Cobra cannot tell a flag that was never
// passed from one passed as empty, and treating both as absent meant a script
// written as `mcpsnoop wrap "$SRV" --config "$CFG"` with CFG unset edited the
// user's live Claude Desktop config. It is the one path where this writes to a
// file the caller never named.
func TestWrapRefusesAnEmptyConfigFlag(t *testing.T) {
	for _, newCmd := range []func() *cobra.Command{newWrapCmd, newUnwrapCmd} {
		code, _, stderr := executeWrapCmd(t, newCmd, "everything", "--config", "")
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if !strings.Contains(stderr, "empty path") {
			t.Fatalf("the message must say what was wrong: %q", stderr)
		}
	}
}

// TestWrapRefusesToWriteOverAConfigThatMoved. wrap is a read, a rewrite and a
// write, and Claude Desktop rewrites this same file whenever a connector is
// toggled. Without the recheck the whole of somebody else's edit disappears and
// both sides report success.
func TestWrapRefusesToWriteOverAConfigThatMoved(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	t.Cleanup(func() { writeConfigHook = nil })

	moved := strings.Replace(wrapFixture, `"Ctrl+Space"`, `"Alt+Space"`, 1)
	writeConfigHook = func() {
		writeConfigHook = nil
		if err := os.WriteFile(path, []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	code, _, stderr := executeWrapCmd(t, newWrapCmd, "everything", "--config", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "changed while mcpsnoop was working on it") {
		t.Fatalf("the message must say why nothing was written: %q", stderr)
	}
	if got := readConfig(t, path); got != moved {
		t.Fatalf("the other writer's config was overwritten:\n%s", got)
	}
}

// TestWrapSaysWhenTheWrappedCommandIsNotThisBinary. isWrapped matches the name
// and not the path, so an entry can be wrapped around an mcpsnoop that has since
// moved. Saying only "nothing to do" left the user running the one command that
// would fix it and being told there was nothing to fix.
func TestWrapSaysWhenTheWrappedCommandIsNotThisBinary(t *testing.T) {
	path := newWrapTest(t, wrapFixture)
	wrapOK(t, newWrapCmd, "everything", "--config", path)

	orig := wrapperPath
	wrapperPath = func() string { return "/usr/local/bin/mcpsnoop" }
	t.Cleanup(func() { wrapperPath = orig })

	out := wrapOK(t, newWrapCmd, "everything", "--config", path)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("a second wrap is still a no-op: %q", out)
	}
	if !strings.Contains(out, stubWrapperPath) || !strings.Contains(out, "/usr/local/bin/mcpsnoop") {
		t.Fatalf("the note must name both paths: %q", out)
	}
	if !strings.Contains(out, "unwrap") {
		t.Fatalf("the note must say how to re-point it: %q", out)
	}
}

// TestSameJSONKeepsEveryDistinctionTheFileMakes. sameJSON decides whether unwrap
// overwrites the whole config with the backup, so a false yes throws away
// whatever the user changed in between. json.Unmarshal into any answers this
// question wrongly twice over, collapsing large integers into a float64 and
// keeping only the last of a repeated name.
func TestSameJSONKeepsEveryDistinctionTheFileMakes(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		{"reordered keys", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"reindented", "{\n  \"a\": [1, 2]\n}", `{"a":[1,2]}`, true},
		{"integers past float64", `{"id":9007199254740992}`, `{"id":9007199254740993}`, false},
		{"integer against float", `{"id":1}`, `{"id":1.0}`, false},
		{"exponent against digits", `{"id":100}`, `{"id":1e2}`, false},
		{"a discarded duplicate", `{"a":9,"a":2}`, `{"a":7,"a":2}`, false},
		{"a genuine difference", `{"a":1}`, `{"a":2}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameJSON([]byte(tc.a), []byte(tc.b)); got != tc.same {
				t.Fatalf("sameJSON(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.same)
			}
		})
	}
}

// TestUnwrapKeepsTheBackupWhileAHandWrappedServerRemains. A user can wrap an
// entry by hand before mcpsnoop ever runs, and then the backup mcpsnoop takes
// holds a config that is already partly wrapped. Restoring it is right, removing
// it is not: the other server still runs through mcpsnoop and the copy of the
// config is still the only way back.
func TestUnwrapKeepsTheBackupWhileAHandWrappedServerRemains(t *testing.T) {
	handWrapped := strings.Replace(wrapFixture,
		`"other": { "command": "python", "args": ["server.py"] }`,
		`"other": { "command": "`+stubWrapperPath+`", "args": ["--", "python", "server.py"] }`, 1)
	path := newWrapTest(t, handWrapped)

	wrapOK(t, newWrapCmd, "everything", "--config", path)
	out := wrapOK(t, newUnwrapCmd, "everything", "--config", path)

	if got := readConfig(t, path); got != handWrapped {
		t.Fatalf("the config did not come back byte for byte:\n%s", got)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatal(`the backup went while "other" still runs through mcpsnoop`)
	}
	if strings.Contains(out, "nothing is wrapped any more") {
		t.Fatalf("unwrap claimed the config is clear while a server is still wrapped: %q", out)
	}
}
