package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestDiffCommandComparesTwoLogPaths(t *testing.T) {
	dir := t.TempDir()
	before := filepath.Join(dir, "before.jsonl")
	after := filepath.Join(dir, "after.jsonl")
	writeDiffLog(t, before,
		diffEnvelope("before", 1, time.Unix(1, 0), proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		diffEnvelope("before", 2, time.Unix(2, 0), proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}}`),
		diffEnvelope("before", 3, time.Unix(3, 0), proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"ruff"}}}`),
		diffEnvelope("before", 4, time.Unix(3, 0).Add(100*time.Millisecond), proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`),
	)
	writeDiffLog(t, after,
		diffEnvelope("after", 1, time.Unix(1, 0), proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		diffEnvelope("after", 2, time.Unix(2, 0), proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object","required":["query"]}}]}}`),
		diffEnvelope("after", 3, time.Unix(3, 0), proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"ruff"}}}`),
		diffEnvelope("after", 4, time.Unix(3, 0).Add(350*time.Millisecond), proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[]}}`),
	)

	code, stdout, stderr := executeDiff(t, []string{"--duration-threshold", "100ms", "--duration-ratio", "2", before, after})
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"mcpsnoop diff before -> after",
		"schema changed: search",
		`status changed: search {"query":"ruff"} ok -> error`,
		`slower: search {"query":"ruff"} 100ms -> 350ms`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestDiffCommandRejectsInvalidDurationRatio(t *testing.T) {
	for _, ratio := range []string{"0.5", "NaN", "+Inf"} {
		t.Run(ratio, func(t *testing.T) {
			code, _, stderr := executeDiff(t, []string{"--duration-ratio", ratio, "a", "b"})
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr, "--duration-ratio must be finite and at least 1") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}

func executeDiff(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	cmd := newDiffCmd()
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

func writeDiffLog(t *testing.T, path string, envelopes ...proxy.Envelope) {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, envelope := range envelopes {
		if err := enc.Encode(envelope); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func diffEnvelope(sessionID string, seq uint64, ts time.Time, direction proxy.Direction, raw string) proxy.Envelope {
	return proxy.Envelope{
		SessionID:   sessionID,
		ServerLabel: sessionID,
		Seq:         seq,
		TS:          ts,
		Direction:   direction,
		Raw:         json.RawMessage(raw),
	}
}
