package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func TestBaselineCommandAcceptsShowsAndResetsDefinitionDrift(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	initial := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"old","inputSchema":{}}]}}`),
	)
	code, stdout, stderr := executeBaseline(t, []string{"--accept", "-"}, initial)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "accepted baseline for srv") {
		t.Fatalf("accept = code %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	changed := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"new","inputSchema":{}}]}}`),
	)
	code, stdout, stderr = executeBaseline(t, []string{"-"}, changed)
	if code != 1 || stderr != "" || !strings.Contains(stdout, "description changed: search") {
		t.Fatalf("show = code %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = executeBaseline(t, []string{"--reset", "-"}, changed)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "reset baseline for srv") {
		t.Fatalf("reset = code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func executeBaseline(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	cmd := newBaselineCmd()
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

// TestWriteToolDriftPrintsEveryKind is the CLI half of the renderer guard. The
// drift gate counts every kind, so a kind missing from this table would fail a
// run and then print nothing the operator could act on.
func TestWriteToolDriftPrintsEveryKind(t *testing.T) {
	var drift store.ToolDrift
	for _, kind := range store.ToolDriftKinds {
		drift.Add(kind, "tool-"+string(kind))
	}
	var buf bytes.Buffer
	writeToolDrift(&buf, drift)
	for _, kind := range store.ToolDriftKinds {
		if !strings.Contains(buf.String(), "tool-"+string(kind)) {
			t.Fatalf("kind %q is counted but never printed:\n%s", kind, buf.String())
		}
	}
}

// TestUnverifiedCoverageIsReported. A baseline that predates a field compares
// fewer things than the operator may assume, so a clean run has to say which
// fields it could not check rather than reading as a full all-clear.
func TestUnverifiedCoverageIsReported(t *testing.T) {
	var buf bytes.Buffer
	writeUnverifiedCoverage(&buf, store.ToolDrift{
		Unverified: []store.ToolDriftKind{store.DriftAnnotations, store.DriftOutputSchema},
	})
	for _, want := range []string{"annotations", "output_schema", "--accept"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("the notice should mention %q, got %q", want, buf.String())
		}
	}
	// A fully covered baseline says nothing.
	buf.Reset()
	writeUnverifiedCoverage(&buf, store.ToolDrift{})
	if buf.Len() != 0 {
		t.Fatalf("a complete baseline should print no notice, got %q", buf.String())
	}
}
