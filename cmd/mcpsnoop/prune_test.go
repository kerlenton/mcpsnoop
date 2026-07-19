package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
)

func executePrune(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	cmd := newPruneCmd()
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

func writePruneLog(t *testing.T, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(paths.SessionsDir(), name)
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneDryRunRemovesNothing(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	old := writePruneLog(t, "old.jsonl", 40*24*time.Hour)
	writePruneLog(t, "new.jsonl", time.Hour)

	code, stdout, stderr := executePrune(t, []string{"--older-than", "30d", "--dry-run"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "would remove 1") || !strings.Contains(stdout, old) {
		t.Fatalf("dry run should list the old log, got %q", stdout)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("a dry run must remove nothing, but %q is gone: %v", old, err)
	}
}

func TestPruneRemovesOnlyOldLogs(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	old := writePruneLog(t, "old.jsonl", 40*24*time.Hour)
	recent := writePruneLog(t, "new.jsonl", time.Hour)

	// A baseline is keyed by server label, not session, so it must survive a prune.
	basePath := filepath.Join(paths.ToolBaselinesDir(), "abcd.json")
	if err := os.WriteFile(basePath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldMt := time.Now().Add(-100 * 24 * time.Hour)
	_ = os.Chtimes(basePath, oldMt, oldMt)

	code, stdout, stderr := executePrune(t, []string{"--older-than", "30d", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "removed 1") {
		t.Fatalf("stdout %q", stdout)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the log older than the cutoff should have been removed")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("the recent log must be kept")
	}
	if _, err := os.Stat(basePath); err != nil {
		t.Fatal("tool baselines must be left alone by prune")
	}
}

func TestPruneRequiresOlderThan(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	code, _, stderr := executePrune(t, []string{"--yes"}, "")
	if code != 2 || !strings.Contains(stderr, "--older-than is required") {
		t.Fatalf("code %d stderr %q, want exit 2 naming the missing flag", code, stderr)
	}
}

func TestPruneRejectsUnparseableOlderThan(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	code, _, stderr := executePrune(t, []string{"--older-than", "soon"}, "")
	if code != 2 {
		t.Fatalf("code %d, want exit 2 for an unparseable value", code)
	}
	for _, want := range []string{"30d", "72h"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the error should name the accepted forms, got %q", stderr)
		}
	}
}

func TestPruneNothingMatchedExitsZero(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writePruneLog(t, "new.jsonl", time.Hour) // not old enough

	code, stdout, stderr := executePrune(t, []string{"--older-than", "30d", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q, want exit 0", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to prune") {
		t.Fatalf("a run that matched nothing should say so, got %q", stdout)
	}
}
