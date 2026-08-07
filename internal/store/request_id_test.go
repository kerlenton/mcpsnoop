package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestNullRequestIDWarnsAndDoesNotFalseReuse(t *testing.T) {
	s := New()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	meta := `,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

	frames := []struct {
		seq uint64
		dir proxy.Direction
		raw string
	}{
		{1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28"` + meta + `,"capabilities":{},"clientInfo":{"name":"demo","version":"1.0"}}}`},
		{2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","protocolVersion":"2026-07-28","capabilities":{"tools":{}},"serverInfo":{"name":"files","version":"0.1"}}}`},
		{3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"/etc/hosts"}` + meta + `}}`},
		{4, proxy.ClientToServer, `{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"/tmp/out"}` + meta + `}}`},
	}

	for _, f := range frames {
		s.Ingest(proxy.Envelope{
			SessionID: "n1", ServerLabel: "files", Seq: f.seq, TS: now.Add(time.Duration(f.seq) * time.Second),
			Direction: f.dir, Raw: json.RawMessage(f.raw),
		})
	}

	calls := s.Calls("n1")
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(calls))
	}
	if calls[1].ToolName != "read_file" || calls[2].ToolName != "write_file" {
		t.Fatalf("expected distinct tool calls, got %+v, %+v", calls[1], calls[2])
	}
	if calls[1].State != Pending || calls[2].State != Pending {
		t.Fatalf("both null-id calls should stay pending, got %v and %v", calls[1].State, calls[2].State)
	}

	events := s.Timeline("n1")
	var nullWarnings int
	for _, ev := range events {
		if ev.Kind == EventRequest && ev.ID == "null" {
			if !strings.Contains(ev.Warning, "request id is null") {
				t.Fatalf("expected null-id warning on seq %d, got %q", ev.Seq, ev.Warning)
			}
			if strings.Contains(ev.Warning, "reuses an id already in flight") {
				t.Fatalf("seq %d should not report false id reuse, got %q", ev.Seq, ev.Warning)
			}
			nullWarnings++
		}
	}
	if nullWarnings != 2 {
		t.Fatalf("expected 2 null-id warnings, got %d", nullWarnings)
	}

	header := s.Sessions()[0]
	if header.Pending != 2 {
		t.Fatalf("expected 2 pending null-id calls, got %d", header.Pending)
	}

	if calls[1].CorrID == calls[2].CorrID {
		t.Fatalf("null-id calls should have distinct correlation keys, both %q", calls[1].CorrID)
	}
}

func TestInvalidRequestIDKindsWarn(t *testing.T) {
	cases := []struct {
		id       string
		contains string
	}{
		{`1.5`, "floating-point"},
		{`true`, "true"},
		{`{"a":1}`, "object"},
		{`[]`, "array"},
	}
	for _, tc := range cases {
		msg := proxy.RPCMessage{
			JSONRPC: "2.0",
			ID:      json.RawMessage(tc.id),
			Method:  "ping",
		}
		w := requestIDWarning(msg)
		if !strings.Contains(w, tc.contains) {
			t.Fatalf("id %s: expected warning containing %q, got %q", tc.id, tc.contains, w)
		}
	}
}

func TestTitleCaseResourceNotFoundStillValidID(t *testing.T) {
	if !isValidRequestID(json.RawMessage(`1`)) {
		t.Fatal("integer id should be valid")
	}
	if !isValidRequestID(json.RawMessage(`"abc"`)) {
		t.Fatal("string id should be valid")
	}
	if isValidRequestID(json.RawMessage(`null`)) {
		t.Fatal("null id should be invalid")
	}
}
