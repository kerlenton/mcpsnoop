package paths

import (
	"path/filepath"
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
