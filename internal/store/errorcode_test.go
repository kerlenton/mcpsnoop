package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestErrorCodeNameRefusesTheImplementationDefinedRange is the half that is easy
// to get wrong by being helpful. -32000 to -32019 belongs to whoever sent it,
// and the spec forbids receivers assigning cross-implementation meaning there,
// so naming one would be inventing a claim the server never made.
func TestErrorCodeNameRefusesTheImplementationDefinedRange(t *testing.T) {
	for code := -32019; code <= -32000; code++ {
		if name := ErrorCodeName(code); name != "" {
			t.Fatalf("code %d is implementation-defined and must stay unnamed, got %q", code, name)
		}
	}
	// The retired codes live in that range and are named only as deprecations,
	// never as a current meaning.
	if ErrorCodeName(-32002) != "" || ErrorCodeName(-32042) != "" {
		t.Fatal("a retired code is not a current one")
	}
}

// TestErrorCodeNameCoversTheAllocatedCodes pins the table against the schema.
func TestErrorCodeNameCoversTheAllocatedCodes(t *testing.T) {
	for code, want := range map[int]string{
		-32700: "parse error",
		-32600: "invalid request",
		-32601: "method not found",
		-32602: "invalid params",
		-32603: "internal error",
		-32020: "header mismatch",
		-32021: "missing required client capability",
		-32022: "unsupported protocol version",
	} {
		if got := ErrorCodeName(code); got != want {
			t.Fatalf("ErrorCodeName(%d) = %q, want %q", code, got, want)
		}
	}
}

// TestErrorCodeTextLeadsWithTheNumber. The number is what a reader matches
// against the spec and against a server's own documentation, and it is all
// there is when the code is implementation-defined.
func TestErrorCodeTextLeadsWithTheNumber(t *testing.T) {
	if got := ErrorCodeText(-32020); got != "-32020 header mismatch" {
		t.Fatalf("ErrorCodeText(-32020) = %q", got)
	}
	if got := ErrorCodeText(-32001); got != "-32001" {
		t.Fatalf("an unnamed code should render bare, got %q", got)
	}
}

// errorResponse is a server frame answering id with a JSON-RPC error.
func errorResponse(seq uint64, id, code int, message string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: proxy.TransportHTTP, Status: 200,
		Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":%d,"message":%q}}`,
			id, code, message)),
	}
}

// TestIngestFlagsAHeaderMismatchRejection. The server validates Mcp-Param values
// and encodings that mcpsnoop never sees, so -32020 is sometimes the only
// evidence a routing mismatch happened, and it has to reach the same filter.
func TestIngestFlagsAHeaderMismatchRejection(t *testing.T) {
	s := New()
	s.Ingest(httpRequest(1, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`,
		"tools/call", "search", "2026-07-28"))
	ev := s.Ingest(errorResponse(2, 1, -32020, "Header mismatch"))
	if !ev.RoutingMismatch {
		t.Fatal("a -32020 rejection is a routing mismatch as the server saw it")
	}

	// An ordinary failure is not one, or the filter stops meaning anything.
	s.Ingest(httpRequest(3, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`,
		"tools/call", "search", "2026-07-28"))
	if other := s.Ingest(errorResponse(4, 2, -32603, "boom")); other.RoutingMismatch {
		t.Fatal("only -32020 is a routing verdict, got a mismatch on -32603")
	}
}

// TestIngestMarksARetiredErrorCode. Both codes are reserved and never reused, so
// one is unambiguous evidence of an older revision. It rides the deprecated
// marker, which never fails check.
func TestIngestMarksARetiredErrorCode(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{-32002, "resource not found"},
		{-32042, "URL elicitation required"},
	} {
		t.Run(ErrorCodeText(tc.code), func(t *testing.T) {
			s := New()
			s.Ingest(httpRequest(1, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///a"}}`,
				"resources/read", "file:///a", "2026-07-28"))
			ev := s.Ingest(errorResponse(2, 1, tc.code, "gone"))
			if !strings.Contains(ev.Deprecated, tc.want) {
				t.Fatalf("a retired code should be marked deprecated, got %q", ev.Deprecated)
			}
			if ev.Warning != "" {
				t.Fatalf("a retired code is a heads-up, not a protocol warning, got %q", ev.Warning)
			}
		})
	}

	// A current code is not a deprecation, or the marker stops meaning anything.
	s := New()
	s.Ingest(httpRequest(1, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///a"}}`,
		"resources/read", "file:///a", "2026-07-28"))
	if ev := s.Ingest(errorResponse(2, 1, -32602, "bad params")); ev.Deprecated != "" {
		t.Fatalf("-32602 is current, got %q", ev.Deprecated)
	}
}
