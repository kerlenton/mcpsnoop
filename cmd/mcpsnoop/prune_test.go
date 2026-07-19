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

func TestPruneRemovesOthersAndReportsReclaimedBytes(t *testing.T) {
	okDir := t.TempDir()
	okPath := filepath.Join(okDir, "ok.jsonl")
	if err := os.WriteFile(okPath, []byte("12345"), 0o600); err != nil { // 5 bytes
		t.Fatal(err)
	}

	// A file in a read-only directory cannot be removed, the least platform-specific
	// way to force one failure while the other still goes.
	roDir := t.TempDir()
	badPath := filepath.Join(roDir, "bad.jsonl")
	if err := os.WriteFile(badPath, []byte("999"), 0o600); err != nil { // 3 bytes
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	if os.Remove(badPath) == nil {
		t.Skip("directory permissions do not prevent removal here (root?)")
	}

	var errBuf bytes.Buffer
	removed, freed, anyFailed := removeLogs(&errBuf, []prunableLog{
		{path: okPath, size: 5},
		{path: badPath, size: 3},
	})
	if !anyFailed {
		t.Fatal("a failed removal must be flagged so the command can exit non-zero")
	}
	if removed != 1 || freed != 5 {
		t.Fatalf("removed=%d freed=%d, want 1 file and only its 5 bytes, not the candidate total", removed, freed)
	}
	if _, err := os.Stat(okPath); err == nil {
		t.Fatal("the removable log should be gone")
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatal("the unremovable log should remain")
	}
	if errBuf.Len() == 0 {
		t.Fatal("the removal error should be printed to stderr")
	}
}

func TestPruneExitsNonZeroOnRemovalFailure(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writePruneLog(t, "old.jsonl", 40*24*time.Hour)

	sessDir := paths.SessionsDir()
	if err := os.Chmod(sessDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sessDir, 0o700) })
	if os.Remove(filepath.Join(sessDir, "old.jsonl")) == nil {
		t.Skip("directory permissions do not prevent removal here (root?)")
	}

	code, stdout, stderr := executePrune(t, []string{"--older-than", "30d", "--yes"}, "")
	if code != 1 {
		t.Fatalf("a removal failure should exit 1, got %d", code)
	}
	if !strings.Contains(stdout, "removed 0 session log(s)") {
		t.Fatalf("the summary should report what actually went, got %q", stdout)
	}
	if stderr == "" {
		t.Fatal("the removal error should be printed to stderr")
	}
}

func TestPruneRejectsZeroOlderThan(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	for _, v := range []string{"0d", "0s"} {
		code, _, stderr := executePrune(t, []string{"--older-than", v}, "")
		if code != 2 {
			t.Fatalf("--older-than %s should exit 2, got %d", v, code)
		}
		if !strings.Contains(stderr, "greater than zero") {
			t.Fatalf("--older-than %s error should name the constraint, got %q", v, stderr)
		}
	}
}
