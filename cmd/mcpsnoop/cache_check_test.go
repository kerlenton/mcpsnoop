package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestCheckPassesForCacheStaleRefetch(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	now := time.Now()
	writeCheckLog(t, paths.SessionLogPath("s1"),
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
			Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`),
		},
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
			Raw: []byte(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","tools":[],"ttlMs":60000}}`),
		},
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: now.Add(time.Second), Direction: proxy.ClientToServer,
			Raw: []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`),
		},
	)

	code, stdout, stderr := executeCheck(t, []string{"s1"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "check passed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCheckFailsForPublicScopeViolation(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	now := time.Now()
	writeCheckLog(t, paths.SessionLogPath("s1"),
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
			Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///user/profile","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`),
		},
		proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
			Raw: []byte(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","contents":[{"uri":"file:///user/profile","text":"{\"userId\":\"123\"}"}],"cacheScope":"public"}}`),
		},
	)

	code, stdout, stderr := executeCheck(t, []string{"s1"}, "")
	if code == 0 {
		t.Fatalf("exit = 0, want failure; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stdout, "warnings=1") {
		t.Fatalf("stdout = %q", stdout)
	}
}
