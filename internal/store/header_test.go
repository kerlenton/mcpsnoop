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
// routing headers mandatory, which is what opens the check. It also carries
// clientCapabilities, which that revision requires on every request, so a test
// using it is varying only the thing it is about.
const call2026 = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search",` +
	`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientCapabilities":{}}}}`

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

// TestIngestDoesNotDemandHeadersOnANotification pins the reading of the released
// revision, which is narrower than "All requests" looks. Of a notification POST
// the transport says "header requirements for notification POSTs are not defined
// by this revision", and it also says the core protocol defines no
// client-to-server notification on Streamable HTTP at all, naming
// notifications/cancelled as stdio-only. Flagging one would be asserting a rule
// the spec declines to state, and warnings fail a default check run.
func TestIngestDoesNotDemandHeadersOnANotification(t *testing.T) {
	s := New()
	// The session is on the revision that requires the headers, so only the
	// requests-only guard stands between this frame and three accusations.
	s.Ingest(httpRequest(1, call2026, "tools/call", "search", "2026-07-28"))
	ntf := httpRequest(2, `{"jsonrpc":"2.0","method":"notifications/cancelled"}`, "", "", "")
	if ev := s.Ingest(ntf); ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("a notification owes no routing header, got %q", ev.Warning)
	}

	// A request with an id that declares the revision still owes them, or the
	// narrowing has switched the check off rather than scoped it.
	req := httpRequest(3, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`, "", "", "2026-07-28")
	if ev := s.Ingest(req); !strings.Contains(ev.Warning, "Mcp-Method") {
		t.Fatalf("a request still owes its routing headers, got %q", ev.Warning)
	}
}

// TestIngestSaysNothingAboutAnUndatedRequest is the cost of judging each request
// by its own declaration, recorded so it is a decision rather than a surprise. A
// request that declares no revision in _meta and no MCP-Protocol-Version header
// cannot be dated, so the headers it owes are unknown, and this stays quiet even
// when a neighbouring request on the same capture is on 2026-07-28.
//
// The missing-MCP-Protocol-Version arm is not thereby unreachable: it fires on
// the request that declares the version in _meta and omits the header, which is
// the case the header exists to serve.
func TestIngestSaysNothingAboutAnUndatedRequest(t *testing.T) {
	s := New()
	s.Ingest(httpRequest(1, call2026, "tools/call", "search", "2026-07-28"))

	undated := httpRequest(2, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`, "", "", "")
	if ev := s.Ingest(undated); ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("an undated request must not be judged by a neighbour, got %q", ev.Warning)
	}

	// Declared in _meta, header absent. This is the arm that still has to fire.
	viaMeta := httpRequest(3, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search",`+
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, "tools/call", "search", "")
	if ev := s.Ingest(viaMeta); !strings.Contains(ev.Warning, "MCP-Protocol-Version") {
		t.Fatalf("a request that dates itself in _meta still owes the header, got %q", ev.Warning)
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

// TestRoutingHeadersJudgeEachRequestByItsOwnRevision is the same false positive
// #179 removed from the resultType check, kept out of its sibling. The session's
// version is last-write-wins across every client request, and the spec forbids
// inferring a request's revision from earlier ones on the same connection while
// explicitly allowing clients to interleave unrelated requests.
func TestRoutingHeadersJudgeEachRequestByItsOwnRevision(t *testing.T) {
	t.Run("older request is not flagged by a newer neighbour", func(t *testing.T) {
		s := New()
		// A 2026-07-28 request, fully compliant, establishes the newer revision in
		// the session. It must not spill onto the next request.
		s.Ingest(httpRequest(1, call2026, "tools/call", "search", "2026-07-28"))
		older := httpRequest(2, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search",`+
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}}`, "", "", "2025-11-25")
		if ev := s.Ingest(older); ev.Warning != "" || ev.RoutingMismatch {
			t.Fatalf("a 2025-11-25 request owes no routing header, got %q", ev.Warning)
		}
	})

	t.Run("newer request is still flagged behind an older neighbour", func(t *testing.T) {
		s := New()
		s.Ingest(httpRequest(1, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search",`+
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}}`, "", "", "2025-11-25"))
		newer := httpRequest(2, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search",`+
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, "", "", "2026-07-28")
		ev := s.Ingest(newer)
		for _, want := range []string{"Mcp-Method", "Mcp-Name"} {
			if !strings.Contains(ev.Warning, want) {
				t.Fatalf("a 2026-07-28 request owes %s, got %q", want, ev.Warning)
			}
		}
	})
}

// TestIngestDoesNotDemandHeadersOnAHandshake keeps the initialize case working
// without a special case for it. The proposed version rides params.protocolVersion
// rather than _meta or the header, so the request dates itself nowhere this check
// reads and the gate closes on its own.
func TestIngestDoesNotDemandHeadersOnAHandshake(t *testing.T) {
	s := New()
	init := httpRequest(1, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"cli"}}}`, "", "", "")
	if ev := s.Ingest(init); ev.Warning != "" || ev.RoutingMismatch {
		t.Fatalf("a handshake owes no routing header, got %q", ev.Warning)
	}
}

// TestIngestFlagsMissingClientCapabilities covers the other required per-request
// _meta field. From 2026-07-28 it is REQUIRED on every request and a server MUST
// reject a request without it with -32602, so a client omitting it is broken in a
// way this tool exists to show.
func TestIngestFlagsMissingClientCapabilities(t *testing.T) {
	const declares = `"io.modelcontextprotocol/clientCapabilities":{}`
	req := func(seq uint64, meta string) proxy.Envelope {
		return proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(),
			Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` + meta + `}}}`),
		}
	}
	const v2026 = `"io.modelcontextprotocol/protocolVersion":"2026-07-28"`

	for _, tc := range []struct {
		name string
		meta string
		want bool
	}{
		{"absent", v2026, true},
		{"explicit null", v2026 + `,"io.modelcontextprotocol/clientCapabilities":null`, true},
		// An empty object is a statement: the client has nothing to offer.
		{"empty object", v2026 + `,` + declares, false},
		{"populated", v2026 + `,"io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}`, false},
		// Earlier revisions had no per-request _meta at all.
		{"pre-2026 revision", `"io.modelcontextprotocol/protocolVersion":"2025-11-25"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := New().Ingest(req(1, tc.meta))
			got := strings.Contains(ev.Warning, "clientCapabilities")
			if got != tc.want {
				t.Fatalf("flagged = %v, want %v (warning %q)", got, tc.want, ev.Warning)
			}
			// It is a -32602 rejection, not the -32020 header condition, so it must
			// not borrow the routing-mismatch flag.
			if ev.RoutingMismatch {
				t.Fatalf("a missing _meta field is not a routing mismatch: %q", ev.Warning)
			}
		})
	}
}

// TestIngestDoesNotDemandClientCapabilitiesOfANotification. Same narrowing as the
// routing headers: the spec puts the requirement on requests, and a notification
// is not one.
func TestIngestDoesNotDemandClientCapabilitiesOfANotification(t *testing.T) {
	ev := New().Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"_meta":` +
			`{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`),
	})
	if strings.Contains(ev.Warning, "clientCapabilities") {
		t.Fatalf("a notification is not a request, got %q", ev.Warning)
	}
}
