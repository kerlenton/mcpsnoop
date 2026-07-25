package store

import (
	"encoding/base64"
	"encoding/json"
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
			raw := `{"jsonrpc":"2.0","id":1,"method":"` + tc.method + `","params":` +
				strings.Replace(tc.params, "%q", `"`+tc.target+`"`, 1) + `}`
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
