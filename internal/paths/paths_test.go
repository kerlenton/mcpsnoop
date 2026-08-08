package paths

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckSocketPathExplainsOverLongPath(t *testing.T) {
	long := "/" + strings.Repeat("a", maxSocketPathLen) + "/hub.sock"
	err := CheckSocketPath(long)
	if err == nil {
		t.Fatal("an over-long socket path should be rejected")
	}
	for _, want := range []string{"unix socket limit", "MCPSNOOP_HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	if err := CheckSocketPath("/tmp/mcpsnoop/hub.sock"); err != nil {
		t.Fatalf("a short path should pass, got %v", err)
	}
}

// CheckLabel guards the one hole labelFor already closes for derived labels: an
// explicit --label flows verbatim into the session id and so into the trace
// file name, where a separator escapes SessionsDir entirely and the other cases
// leave a session that open and export cannot address again.
func TestCheckLabelRejectsPathHostileValues(t *testing.T) {
	for _, tc := range []struct{ label, why string }{
		{"../../evil", "traversal"},
		{"a/b", "unix separator"},
		{`a\b`, "windows separator"},
		{"nul\x00label", "NUL reaches the file name"},
		{"a\nb", "newline reaches the file name and the banner"},
		{"a\rb", "a carriage return rewrites the banner line"},
		{"red\x1b[31mX", "an escape sequence drives the terminal"},
		{"-rf", "a leading dash parses as a flag in open and export"},
		{strings.Repeat("a", maxLabelLen+1), "over the file name budget"},
	} {
		err := CheckLabel(tc.label)
		if err == nil {
			t.Fatalf("CheckLabel(%q) = nil, want an error (%s)", tc.label, tc.why)
		}
	}

	// The file name still holds real labels. Dots, spaces, unicode and an empty
	// label (which means "derive one from the command") all pass, and so does a
	// doubled dot: once separators are banned it cannot traverse, and refusing it
	// would turn away ordinary version strings. This is the case the two
	// competing fixes for this issue disagreed on, so it is pinned here.
	for _, label := range []string{
		"", "server-everything", "index.js", "my server", "café", "v1.2.3",
		"...", "foo..bar", "v1..v2", "prod..", "..",
		strings.Repeat("a", maxLabelLen),
	} {
		if err := CheckLabel(label); err != nil {
			t.Fatalf("CheckLabel(%q) = %v, want nil", label, err)
		}
	}
}

// TestCheckLabelKeepsADoubledDotContained is the reason "\u002e\u002e" is accepted rather
// than refused. The predicate alone does not prove containment, so this walks the
// value through the real path builder the way a session does.
func TestCheckLabelKeepsADoubledDotContained(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", root)
	t.Setenv("XDG_STATE_HOME", "")

	for _, label := range []string{"..", "...", "foo..bar"} {
		// newSessionID appends "-<pid>-<nonce>", which is what makes the id safe.
		sessionID := label + "-4242-deadbeefcafe"
		got := SessionLogPath(sessionID)
		if want := filepath.Join(SessionsDir(), sessionID+".jsonl"); got != want {
			t.Fatalf("SessionLogPath(%q) = %q, want %q", sessionID, got, want)
		}
		if !strings.HasPrefix(filepath.Clean(got), filepath.Clean(SessionsDir())+string(filepath.Separator)) {
			t.Fatalf("label %q escaped the sessions dir: %s", label, got)
		}
	}
}

// TestClaudeDesktopConfigTracksTheOSConfigDir keeps the one assumption the
// helper rests on honest across every platform CI builds for: os.UserConfigDir
// already resolves to the directory Claude Desktop keeps its config under, so
// there is no per-OS branch here to drift.
func TestClaudeDesktopConfigTracksTheOSConfigDir(t *testing.T) {
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir on this machine: %v", err)
	}
	got, err := ClaudeDesktopConfig()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "Claude", "claude_desktop_config.json"); got != want {
		t.Fatalf("ClaudeDesktopConfig() = %q, want %q", got, want)
	}
}

// TestClaudeDesktopConfigCreatesNothing. The path belongs to another
// application, so resolving it must not bring any part of it into existence the
// way Base and its callers deliberately do.
//
// The user config dir is pointed at an empty root first. Sampling existence on
// the real one proves nothing, because every machine with Claude Desktop
// installed already has the directory and the check passes whatever the helper
// does, which is how it passed with an os.MkdirAll injected into it.
func TestClaudeDesktopConfigCreatesNothing(t *testing.T) {
	root := setUserConfigDir(t)

	got, err := ClaudeDesktopConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, root) {
		t.Fatalf("ClaudeDesktopConfig() = %q, which is not under the redirected root %q", got, root)
	}
	for dir := filepath.Dir(got); len(dir) > len(root); dir = filepath.Dir(dir) {
		if exists(dir) {
			t.Fatalf("resolving the path created %q", dir)
		}
	}
	if exists(got) {
		t.Fatalf("resolving the path created %q", got)
	}
}

// setUserConfigDir points os.UserConfigDir at an empty directory and returns it,
// through whichever variable the running platform actually consults.
func setUserConfigDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", root)
		return root
	case "darwin":
		t.Setenv("HOME", root)
		return filepath.Join(root, "Library", "Application Support")
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
		return root
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// TestClaudeDesktopConfigFollowsXDGConfigHome is linux-only because that is the
// only platform where os.UserConfigDir consults XDG.
func TestClaudeDesktopConfigFollowsXDGConfigHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG_CONFIG_HOME is only consulted on linux")
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	got, err := ClaudeDesktopConfig()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "Claude", "claude_desktop_config.json"); got != want {
		t.Fatalf("ClaudeDesktopConfig() = %q, want %q", got, want)
	}
	if exists(filepath.Dir(got)) {
		t.Fatalf("ClaudeDesktopConfig created %q under a fresh config home", filepath.Dir(got))
	}
}

func TestToolBaselinesDirUsesConfiguredStateRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", root)
	t.Setenv("XDG_STATE_HOME", "")

	if got, want := ToolBaselinesDir(), filepath.Join(root, "tool-baselines"); got != want {
		t.Fatalf("ToolBaselinesDir() = %q, want %q", got, want)
	}
}

func TestToolBaselinesDirUsesXDGStateHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", "")
	t.Setenv("XDG_STATE_HOME", root)

	if got, want := ToolBaselinesDir(), filepath.Join(root, "mcpsnoop", "tool-baselines"); got != want {
		t.Fatalf("ToolBaselinesDir() = %q, want %q", got, want)
	}
}
