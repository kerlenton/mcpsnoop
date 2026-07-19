package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
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
