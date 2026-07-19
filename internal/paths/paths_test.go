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
