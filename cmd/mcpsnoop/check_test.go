package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestCheckFailsOnSelectedSessionSignals(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writeCheckLog(t, paths.SessionLogPath("s1"), checkSignalEnvelopes()...)

	code, stdout, stderr := executeCheck(t, []string{"s1"}, "")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "session s1: errors=2 invalid=1 warnings=1 slow=0 pending=1\ncheck failed: error,invalid,warn\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckFailsOnlyOnSelectedSignals(t *testing.T) {
	log := encodeCheckLog(t, checkSignalEnvelopes()...)

	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "invalid", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 because the fixture contains an invalid frame", code)
	}
	if stdout != "session s1: errors=2 invalid=1 warnings=1 slow=0 pending=1\ncheck failed: invalid\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckIgnoresUnselectedSignals(t *testing.T) {
	errorOnly := encodeCheckLog(t, checkErrorEnvelopes()...)
	code, stdout, stderr := executeCheck(t, []string{"--fail-on", "invalid,warn", "-"}, errorOnly)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "session s1: errors=2 invalid=0 warnings=0 slow=0 pending=0\ncheck passed\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckPassesCleanSessionFromStdin(t *testing.T) {
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	)

	code, stdout, stderr := executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "session s1: errors=0 invalid=0 warnings=0 slow=0 pending=0\ncheck passed\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestCheckRejectsUnknownOrEmptyFailOnValues(t *testing.T) {
	for _, value := range []string{"error,bogus", ""} {
		t.Run(value, func(t *testing.T) {
			code, stdout, stderr := executeCheck(t, []string{"--fail-on", value, "-"}, "{}\n")
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "mcpsnoop check: --fail-on") {
				t.Fatalf("stderr = %q, want --fail-on error", stderr)
			}
		})
	}
}

func TestCheckReportsMalformedInput(t *testing.T) {
	code, stdout, stderr := executeCheck(t, []string{"-"}, "not-json\n")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "mcpsnoop check: stdin: invalid JSONL envelope") {
		t.Fatalf("stderr = %q, want malformed-input error", stderr)
	}
}

func TestCheckFailsOnSlowCallOverThreshold(t *testing.T) {
	t0 := time.Unix(100, 0)
	log := encodeCheckLog(t,
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0, Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`)},
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(2 * time.Second), Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)},
	)

	code, stdout, _ := executeCheck(t, []string{"--fail-on", "slow", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a 2s call over the 1s default", code)
	}
	if !strings.Contains(stdout, "slow=1") || !strings.Contains(stdout, "check failed: slow") {
		t.Fatalf("stdout = %q", stdout)
	}

	// Raising the threshold above the call clears it.
	code, stdout, _ = executeCheck(t, []string{"--fail-on", "slow", "--slow-threshold", "5s", "-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 once the threshold is above the call", code)
	}
	if !strings.Contains(stdout, "slow=0") || !strings.Contains(stdout, "check passed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCheckGatesEverySessionNotJustTheFirst(t *testing.T) {
	env := func(session string, seq uint64, dir proxy.Direction, raw string) proxy.Envelope {
		return proxy.Envelope{SessionID: session, ServerLabel: "srv", Seq: seq, TS: time.Unix(int64(seq), 0), Direction: dir, Raw: json.RawMessage(raw)}
	}
	// The first session is clean, the second carries an error.
	log := encodeCheckLog(t,
		env("s1", 1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		env("s1", 2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
		env("s2", 3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`),
		env("s2", 4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"boom"}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--fail-on", "error", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 because the second session has an error", code)
	}
	for _, want := range []string{"session s1:", "session s2:", "check failed: error"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q\n%s", want, stdout)
		}
	}
}

func TestCheckFailsOnHungCall(t *testing.T) {
	// A request with no response is a hung call that only the pending signal sees.
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hang"}}`),
	)

	code, stdout, _ := executeCheck(t, []string{"--fail-on", "pending", "-"}, log)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a request that never got a response", code)
	}
	if !strings.Contains(stdout, "pending=1") || !strings.Contains(stdout, "check failed: pending") {
		t.Fatalf("stdout = %q", stdout)
	}

	// The default signals do not gate on pending, so the same log passes.
	code, stdout, _ = executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 since pending is opt-in", code)
	}
	if !strings.Contains(stdout, "check passed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func executeCheck(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	cmd := newCheckCmd()
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

func writeCheckLog(t *testing.T, path string, envelopes ...proxy.Envelope) {
	t.Helper()
	data := encodeCheckLog(t, envelopes...)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func encodeCheckLog(t *testing.T, envelopes ...proxy.Envelope) string {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, env := range envelopes {
		if err := enc.Encode(env); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func checkSignalEnvelopes() []proxy.Envelope {
	envelopes := checkErrorEnvelopes()
	return append(envelopes,
		proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 5, TS: time.Unix(5, 0), Direction: proxy.ServerToClient, Text: "not json-rpc"},
		checkEnvelope(6, proxy.ClientToServer, `{"id":3,"method":"tools/list"}`),
	)
}

func checkErrorEnvelopes() []proxy.Envelope {
	return []proxy.Envelope{
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"missing"}}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"missing"}}`),
		checkEnvelope(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"broken"}}`),
		checkEnvelope(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[]}}`),
	}
}

func checkEnvelope(seq uint64, direction proxy.Direction, raw string) proxy.Envelope {
	return proxy.Envelope{
		SessionID:   "s1",
		ServerLabel: "srv",
		Seq:         seq,
		TS:          time.Unix(int64(seq), 0),
		Direction:   direction,
		Raw:         json.RawMessage(raw),
	}
}
