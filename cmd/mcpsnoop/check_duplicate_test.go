package main

import (
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestCheckFailsOnARepeatedCall walks the duplicate signal end to end. The
// client cancels a call to a tool that never said it was safe to repeat, sends
// it again, and the server answers both, which is the case spec issue #3394
// reproduces. An idempotent tool treated the same way is not reported.
func TestCheckFailsOnARepeatedCall(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	log := encodeCheckLog(t,
		checkEnvelope(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":"l","method":"tools/list"}`),
		checkEnvelope(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":"l","result":{"tools":[`+
			`{"name":"append","inputSchema":{"type":"object"}},`+
			`{"name":"set","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}}]}}`),
		checkEnvelope(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"append","arguments":{"line":"x"}}}`),
		checkEnvelope(4, proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"timed out"}}`),
		checkEnvelope(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"append","arguments":{"line":"x"}}}`),
		checkEnvelope(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
		checkEnvelope(7, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`),
		checkEnvelope(8, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"set","arguments":{"k":"v"}}}`),
		checkEnvelope(9, proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`),
		checkEnvelope(10, proxy.ClientToServer, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"set","arguments":{"k":"v"}}}`),
		checkEnvelope(11, proxy.ServerToClient, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`),
	)

	// What the wire shows is that it may have happened, and here that it did, but
	// a retry is not a protocol violation, so a default run counts it and passes.
	code, stdout, _ := executeCheck(t, []string{"-"}, log)
	if code != 0 {
		t.Fatalf("exit = %d on the default gate, want 0\n%s", code, stdout)
	}
	if got := checkTextSignalCount(t, stdout, "duplicates"); got != 1 {
		t.Fatalf("duplicates = %d, want 1, the idempotent tool left out\n%s", got, stdout)
	}
	const line = "  frame 5, append was called again with the same arguments after the client cancelled the call on frame 3, and the server answered both, so it ran twice\n"
	if !strings.Contains(stdout, "duplicate calls:\n"+line) {
		t.Fatalf("stdout does not list the repeated call:\n%s", stdout)
	}

	code, stdout, _ = executeCheck(t, []string{"--fail-on", "duplicate", "-"}, log)
	if code != 1 || !strings.Contains(stdout, "check failed: duplicate") {
		t.Fatalf("exit = %d under --fail-on duplicate, want 1\n%s", code, stdout)
	}

	_, junit, _ := executeCheck(t, []string{"--format", "junit", "--fail-on", "duplicate", "-"}, log)
	if !strings.Contains(junit, `<failure message="session s1 has 1 tool call that may have run twice"`) {
		t.Fatalf("junit does not name the duplicate failure:\n%s", junit)
	}

	_, sarif, _ := executeCheck(t, []string{"--format", "sarif", "--fail-on", "duplicate", "-"}, log)
	results := sarifResultsFor(decodeCheckSARIF(t, sarif), "mcpsnoop/duplicate")
	if len(results) != 1 || results[0].Level != "error" ||
		!strings.Contains(results[0].Message.Text, `session s1 frame 5: tool "append" was called again`) {
		t.Fatalf("duplicate results = %+v, want one error naming the repeat", results)
	}
}
