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

func mockEnvelope(session string, seq uint64, dir proxy.Direction, raw string, redacted bool) proxy.Envelope {
	env := proxy.Envelope{
		SessionID:   session,
		ServerLabel: "test",
		Seq:         seq,
		TS:          time.Unix(1, 0),
		Direction:   dir,
		Transport:   proxy.TransportStdio,
		Redacted:    redacted,
	}
	if raw != "" {
		env.Raw = json.RawMessage(raw)
	}
	return env
}

func writeMockLog(t *testing.T, path string, envs ...proxy.Envelope) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func executeMock(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	cmd := newMockCmd()
	cmd.SetArgs(args)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if stdin != "" {
		cmd.SetIn(strings.NewReader(stdin))
	} else {
		cmd.SetIn(strings.NewReader(""))
	}
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

func mockCapture() []proxy.Envelope {
	return []proxy.Envelope{
		mockEnvelope("s-1", 1, proxy.DirectionMeta, `{"command":["srv"]}`, false),
		mockEnvelope("s-1", 2, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, false),
		mockEnvelope("s-1", 3, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`, false),
	}
}

// TestMockTakesPrecedenceOverWrappedCommand guards the meaning change: `mock`
// is a subcommand now, not a server binary to wrap.
func TestMockTakesPrecedenceOverWrappedCommand(t *testing.T) {
	var got []string
	defer stubShim(&got)()

	if code := execute([]string{"mock", filepath.Join(t.TempDir(), "missing.jsonl")}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got != nil {
		t.Fatalf("mock must not reach the shim, wrapped %v", got)
	}
}

func TestMockServesFileCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.jsonl")
	writeMockLog(t, path, mockCapture()...)

	code, stdout, stderr := executeMock(t, []string{path},
		`{"jsonrpc":"2.0","id":42,"method":"tools/list","params":{}}`+"\n")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var msg proxy.RPCMessage
	if _, ok := proxy.ParseRPC([]byte(strings.TrimSpace(stdout))); !ok {
		t.Fatalf("stdout is not a protocol frame: %q", stdout)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &msg); err != nil {
		t.Fatal(err)
	}
	if string(msg.ID) != "42" {
		t.Fatalf("id = %s, want 42", msg.ID)
	}
	if !strings.Contains(string(msg.Result), `"tools"`) {
		t.Fatalf("result = %s", msg.Result)
	}
}

func TestMockStdinCaptureRejected(t *testing.T) {
	// One stdin cannot carry both the cassette and the MCP session: the mock
	// serves requests on stdin, so a piped capture would consume the very
	// stream the server role needs. Rejected loudly rather than silently
	// serving nothing.
	code, stdout, stderr := executeMock(t, []string{"-"}, "{}\n")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "stdin") {
		t.Fatalf("stderr = %q, want the stdin conflict explained", stderr)
	}
}

func TestMockRejectsHTTPCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "http.jsonl")
	envs := []proxy.Envelope{
		mockEnvelope("s-http", 1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, false),
		mockEnvelope("s-http", 2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`, false),
	}
	for i := range envs {
		envs[i].Transport = proxy.TransportHTTP
	}
	writeMockLog(t, path, envs...)
	code, _, stderr := executeMock(t, []string{path},
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`+"\n")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "stdio") {
		t.Fatalf("stderr = %q, want the stdio-only refusal", stderr)
	}
}

func TestMockRejectsBadCapture(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	if code, _, stderr := executeMock(t, []string{missing}, ""); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	} else if !strings.Contains(stderr, "mcpsnoop mock:") {
		t.Fatalf("stderr = %q, want the mock to identify itself", stderr)
	}

	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := executeMock(t, []string{empty}, ""); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	} else if !strings.Contains(stderr, "no envelopes found") {
		t.Fatalf("stderr = %q, want the cause", stderr)
	}
}

func TestMockRedactionNeedsExplicitFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redacted.jsonl")
	writeMockLog(t, path,
		mockEnvelope("s-1", 1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, false),
		mockEnvelope("s-1", 2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`, true),
	)
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"
	if code, stdout, stderr := executeMock(t, []string{path}, input); code != 1 {
		t.Fatalf("exit = %d, want 1 (refusal)", code)
	} else {
		if stdout != "" {
			t.Fatalf("refusal must write nothing to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "redacted") {
			t.Fatalf("stderr = %q, want redacted", stderr)
		}
	}
	if code, stdout, _ := executeMock(t, []string{path, "--allow-redacted"}, input); code != 0 {
		t.Fatalf("exit = %d, want 0 with --allow-redacted", code)
	} else if !strings.Contains(stdout, `"tools"`) {
		t.Fatalf("stdout = %q, want the recorded answer", stdout)
	}
}

func TestMockUnmatchedRequestErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.jsonl")
	writeMockLog(t, path, mockCapture()...)

	code, stdout, stderr := executeMock(t, []string{path},
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"ghost"}}`+"\n")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (the miss rides the response, not the exit)", code)
	}
	if !strings.Contains(stdout, "no recorded response for tools/call") {
		t.Fatalf("stdout = %q, want the naming error", stdout)
	}
	if !strings.Contains(stderr, "tools/call") {
		t.Fatalf("stderr = %q, want the miss reported", stderr)
	}
}
