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
// file name, where a separator or ".." escapes SessionsDir entirely.
func TestCheckLabelRejectsPathHostileValues(t *testing.T) {
	bad := []string{
		"../../evil",
		"a/b",
		`a\b`,
		"..",
		"prod..",
		"nul\x00label",
	}
	for _, label := range bad {
		err := CheckLabel(label)
		if err == nil {
			t.Fatalf("CheckLabel(%q) = nil, want an error", label)
		}
		if !strings.Contains(err.Error(), "state dir") {
			t.Fatalf("CheckLabel(%q) = %q, the error should say why the label names files", label, err)
		}
	}
	// The file name still holds real labels: dots, spaces, unicode, and an empty
	// label (which means "derive one from the command") all pass.
	for _, label := range []string{"", "server-everything", "index.js", "my server", "café", "v1.2.3"} {
		if err := CheckLabel(label); err != nil {
			t.Fatalf("CheckLabel(%q) = %v, want nil", label, err)
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
