package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestDecodeHeaderValue works through the sentinel by example, using the
// encodings the specification itself tabulates, so a change to the decoder has
// to keep agreeing with the document rather than with our reading of it.
func TestDecodeHeaderValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain ascii is untouched", "get_weather", "get_weather"},
		{"an ascii uri is untouched", "file:///projects/app/config.json", "file:///projects/app/config.json"},
		{"non-ascii", "=?base64?SGVsbG8sIOS4lueVjA==?=", "Hello, 世界"},
		{"leading and trailing space", "=?base64?IHBhZGRlZCA=?=", " padded "},
		{"embedded newline", "=?base64?bGluZTEKbGluZTI=?=", "line1\nline2"},
		// A plain value matching the sentinel is itself encoded, so decoding has
		// to happen exactly once or this comes back as something else entirely.
		{"a literal that matches the sentinel", "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?=", "=?base64?literal?="},
		// The markers are case-sensitive, so this is an ordinary value.
		{"uppercase marker is not a sentinel", "=?BASE64?SGVsbG8=?=", "=?BASE64?SGVsbG8=?="},
		{"prefix without suffix", "=?base64?SGVsbG8=", "=?base64?SGVsbG8="},
		{"bare prefix", "=?base64?", "=?base64?"},
		{"unpadded is accepted", "=?base64?SGVsbG8?=", "Hello"},
		{"undecodable payload falls back to the literal", "=?base64?@@@?=", "=?base64?@@@?="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeHeaderValue(tc.in); got != tc.want {
				t.Fatalf("decodeHeaderValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func encodeHeader(value string) string {
	return base64SentinelPrefix + base64.StdEncoding.EncodeToString([]byte(value)) + base64SentinelSuffix
}

// TestIngestAcceptsAnEncodedMcpNameThatMatches is the regression. A compliant
// client with a non-ASCII path was told its routing header disagreed with its
// own body, and because that rides the warning signal, a default `check` run
// failed on correct traffic.
func TestIngestAcceptsAnEncodedMcpNameThatMatches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		params string
		target string
	}{
		{"non-ascii resource path", "resources/read", `{"uri":%q}`, "file:///проект/config.json"},
		{"non-ascii tool name", "tools/call", `{"name":%q}`, "天気を取得"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`,
				tc.method, fmt.Sprintf(tc.params, tc.target))
			ev := s.Ingest(proxy.Envelope{
				SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
				Direction: proxy.ClientToServer, Transport: "http",
				MCPMethod: tc.method, MCPName: encodeHeader(tc.target),
				Raw: json.RawMessage(raw),
			})
			if ev.Warning != "" {
				t.Fatalf("an encoded header matching the body is not a mismatch, got %q", ev.Warning)
			}
			if ev.RoutingMismatch {
				t.Fatal("a compliant encoded header must not be flagged as a routing mismatch")
			}
		})
	}
}

// TestIngestReportsTheDecodedNameOnMismatch keeps a real disagreement usable.
// Decoding must not blunt the tool-shadowing signal, and the message has to name
// what the header carried rather than its base64, which reads as noise.
func TestIngestReportsTheDecodedNameOnMismatch(t *testing.T) {
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: "http",
		MCPMethod: "tools/call", MCPName: encodeHeader("削除"),
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`),
	})
	if !ev.RoutingMismatch {
		t.Fatal("an encoded header naming a different tool is still a mismatch")
	}
	if !strings.Contains(ev.Warning, "削除") {
		t.Fatalf("the warning should name the decoded header value, got %q", ev.Warning)
	}
	if strings.Contains(ev.Warning, "base64") {
		t.Fatalf("the warning should not leak the encoded form, got %q", ev.Warning)
	}
}

// A decoded value can carry what an HTTP header cannot. The spec's own example
// is a newline, and a raw one would split the text export's one-line-per-frame
// output and break the TUI row it is rendered in.
func TestIngestQuotesAControlCharacterInADecodedName(t *testing.T) {
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: "http",
		MCPMethod: "tools/call", MCPName: encodeHeader("line1\nline2"),
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`),
	})
	if strings.ContainsAny(ev.Warning, "\n\r\x00") {
		t.Fatalf("a warning must stay on one line, got %q", ev.Warning)
	}
	if !strings.Contains(ev.Warning, `line1\nline2`) {
		t.Fatalf("the newline should be escaped, not dropped, got %q", ev.Warning)
	}
}

// httpRequest builds a client frame as it would arrive over Streamable HTTP,
// with whatever headers the case chose to send.
func httpRequest(seq uint64, raw string, method, name, version string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		MCPMethod: method, MCPName: name, MCPProtocolVersion: version,
		Raw: json.RawMessage(raw),
	}
}

// call2026 is a tools/call whose _meta declares the revision that made the
// routing headers mandatory, which is what opens the check.
const call2026 = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search",` +
	`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`

// TestIngestFlagsMissingRoutingHeaders is the feature: all three are REQUIRED in
// 2026-07-28, and a compliant server rejects the request with 400 and -32020.
func TestIngestFlagsMissingRoutingHeaders(t *testing.T) {
	s := New()
	ev := s.Ingest(httpRequest(1, call2026, "", "", ""))

	for _, want := range []string{"MCP-Protocol-Version", "Mcp-Method", "Mcp-Name"} {
		if !strings.Contains(ev.Warning, want) {
			t.Fatalf("missing %s should be reported, got %q", want, ev.Warning)
		}
	}
	if !ev.RoutingMismatch {
		t.Fatal("an omitted required header is the same signal as a disagreeing one")
	}
}

// TestIngestDemandsMcpNameOnlyWhereTheSpecDoes. The spec names three methods;
// operationName maps more than that, and demanding the header on the extras
// would flag a client that is doing nothing wrong.
func TestIngestDemandsMcpNameOnlyWhereTheSpecDoes(t *testing.T) {
	for _, tc := range []struct {
		method, params string
		wantName       bool
	}{
		{"tools/call", `{"name":"search"}`, true},
		{"resources/read", `{"uri":"file:///a"}`, true},
		{"prompts/get", `{"name":"p"}`, true},
		{"tools/list", `{}`, false},
		{"resources/subscribe", `{"uri":"file:///a"}`, false},
	} {
		t.Run(tc.method, func(t *testing.T) {
			s := New()
			raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, tc.method, tc.params)
			// Every header but Mcp-Name is present, so Mcp-Name is the only variable.
			ev := s.Ingest(httpRequest(1, raw, tc.method, "", "2026-07-28"))
			if got := strings.Contains(ev.Warning, "Mcp-Name"); got != tc.wantName {
				t.Fatalf("Mcp-Name demanded = %v, want %v (warning %q)", got, tc.wantName, ev.Warning)
			}
		})
	}
}

// TestIngestDoesNotDemandRoutingHeadersBeforeTheyExisted is the guard that
// matters most. The headers appear in no revision before 2026-07-28, so a
// 2025-11-25 client omitting them is correct, and flagging it would fail a
// default check run on traffic that is doing everything right.
func TestIngestDoesNotDemandRoutingHeadersBeforeTheyExisted(t *testing.T) {
	s := New()
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search",` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}}`
	if ev := s.Ingest(httpRequest(1, raw, "", "", "")); ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("a pre-2026-07-28 session must stay clean, got %q", ev.Warning)
	}
}

// TestIngestStaysSilentOnAnUnknownVersion. No handshake, no _meta, no header:
// nothing here says which revision this is, and accusing a client on no evidence
// is worse than saying nothing.
func TestIngestStaysSilentOnAnUnknownVersion(t *testing.T) {
	s := New()
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`
	if ev := s.Ingest(httpRequest(1, raw, "", "", "")); ev.Warning != "" {
		t.Fatalf("an unknown protocol version is not evidence, got %q", ev.Warning)
	}
}

// TestIngestDoesNotDemandHeadersOnStdio. There are no HTTP headers on stdio, so
// every stdio frame would be flagged if the transport gate were missing.
func TestIngestDoesNotDemandHeadersOnStdio(t *testing.T) {
	s := New()
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(call2026),
	})
	if ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("stdio has no headers to require, got %q", ev.Warning)
	}
}

// TestIngestDoesNotDemandHeadersPerBatchElement. A routing header addresses one
// operation, so it cannot describe a batch; the store already says so once, and
// demanding one per element would bury that under noise.
func TestIngestDoesNotDemandHeadersPerBatchElement(t *testing.T) {
	s := New()
	// The session is on the revision that requires the headers, so only the batch
	// guard stands between this frame and three accusations.
	s.Ingest(httpRequest(1, call2026, "tools/call", "search", "2026-07-28"))
	el := httpRequest(2, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`, "", "", "")
	el.Batch = true
	if ev := s.Ingest(el); strings.Contains(ev.Warning, "is missing") {
		t.Fatalf("a batch element must not be asked for a header that cannot address it, got %q", ev.Warning)
	}
}

// TestIngestDemandsHeadersOnANotification is the interpretation this reads into
// "All requests": a notification carries a method, which is the Source Field the
// gateway routes on, and a compliant server rejects it the same way. The narrow
// reading would restrict the check to frames with an id.
func TestIngestDemandsHeadersOnANotification(t *testing.T) {
	s := New()
	s.Ingest(httpRequest(1, call2026, "tools/call", "search", "2026-07-28"))
	ntf := httpRequest(2, `{"jsonrpc":"2.0","method":"notifications/cancelled"}`, "", "", "")
	ev := s.Ingest(ntf)
	if !strings.Contains(ev.Warning, "Mcp-Method") {
		t.Fatalf("a notification routes on its method too, got %q", ev.Warning)
	}
	if strings.Contains(ev.Warning, "Mcp-Name") {
		t.Fatalf("a notification names no operation, so Mcp-Name is not required, got %q", ev.Warning)
	}
}

// TestIngestAcceptsACompliantRequest closes the loop: everything present, on the
// revision that requires it, is silent.
func TestIngestAcceptsACompliantRequest(t *testing.T) {
	s := New()
	ev := s.Ingest(httpRequest(1, call2026, "tools/call", "search", "2026-07-28"))
	if ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("a compliant request must stay clean, got %q", ev.Warning)
	}
}

// TestIngestDoesNotJudgeAHandshakeByItsOwnProposal is the false positive this
// check is one condition away from producing. A client proposes the newest
// revision it supports and the server negotiates down; the store folds the
// proposal into the session as it reads the request, so the handshake frame
// would be accused on the strength of a version that was then rejected, and one
// warning fails a default check run on a wholly correct session.
func TestIngestDoesNotJudgeAHandshakeByItsOwnProposal(t *testing.T) {
	s := New()
	init := httpRequest(1, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"cli"}}}`, "", "", "")
	if ev := s.Ingest(init); ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("a proposal is not an agreement, got %q", ev.Warning)
	}

	// The server settles on a revision that has no routing headers at all, so
	// everything after the handshake stays clean too.
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: proxy.TransportHTTP, Status: 200,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25",` +
			`"capabilities":{},"serverInfo":{"name":"old"}}}`),
	})
	call := httpRequest(3, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`, "", "", "")
	if ev := s.Ingest(call); ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("a session negotiated down to 2025-11-25 must stay clean, got %q", ev.Warning)
	}
}
