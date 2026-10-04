package clientprofile

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

type frame struct {
	dir proxy.Direction
	raw string
}

// capture ingests one session and returns its store and header.
func capture(t *testing.T, sessionID string, frames ...frame) (*store.Store, store.SessionHeader) {
	t.Helper()
	st := store.New()
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i, f := range frames {
		if !json.Valid([]byte(f.raw)) {
			t.Fatalf("frame %d is not JSON: %s", i, f.raw)
		}
		st.Ingest(proxy.Envelope{
			SessionID: sessionID, ServerLabel: "ref", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(i) * time.Second), Direction: f.dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f.raw),
		})
	}
	for _, h := range st.Sessions() {
		if h.ID == sessionID {
			return st, h
		}
	}
	t.Fatalf("session %s not found", sessionID)
	return nil, store.SessionHeader{}
}

// meta2026 is the _meta every 2026-07-28 request carries, plus extra keys.
func meta2026(extra string) string {
	m := `"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"probe","version":"1.4.0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{},"url":{}},"extensions":{"io.modelcontextprotocol/tasks":{}}}`
	if extra != "" {
		m += "," + extra
	}
	return `"_meta":{` + m + `}`
}

const traceparent = `"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"`

func modernSession(t *testing.T, id string) (*store.Store, store.SessionHeader) {
	return capture(t, id,
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + meta2026("") + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"serverInfo":{"name":"ref","version":"1"}}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + meta2026(traceparent) + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"complete","tools":[{"name":"confirm","inputSchema":{"type":"object"}}],"ttlMs":600000,"cacheScope":"private"}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"confirm","arguments":{},` + meta2026(traceparent+`,"progressToken":"p1"`) + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"result":{"resultType":"input_required","inputRequests":{"confirm":{"method":"elicitation/create","params":{"mode":"form","message":"Go ahead?","requestedSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}},"requestState":"st-1"}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"confirm","arguments":{},"requestState":"st-1","inputResponses":{"confirm":{"action":"accept","content":{"ok":true}}},` + meta2026(`"traceparent":"not-a-traceparent"`) + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":4,"result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{` + meta2026("") + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":5,"result":{"resultType":"complete","tools":[{"name":"confirm","inputSchema":{"type":"object"}}],"ttlMs":600000,"cacheScope":"private"}}`},
	)
}

func legacySession(t *testing.T) (*store.Store, store.SessionHeader) {
	return capture(t, "legacy",
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"oldie","version":"0.9"},"capabilities":{"roots":{"listChanged":true},"sampling":{},"elicitation":{}}}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{},"logging":{}},"serverInfo":{"name":"ref","version":"1"}}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"info"}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"confirm","arguments":{}}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":"s1","method":"elicitation/create","params":{"message":"Go ahead?","requestedSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":"s1","result":{"action":"decline"}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":"s2","method":"roots/list"}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":"s2","error":{"code":-32601,"message":"Method not found"}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"declined"}]}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3,"reason":"user stopped"}}`},
	)
}

func TestModernClientProfile(t *testing.T) {
	st, h := modernSession(t, "modern")
	p, ok := Fold(st, h)
	if !ok {
		t.Fatal("a session with client requests must fold")
	}
	if p.Client != "probe" || p.Version != "1.4.0" {
		t.Fatalf("identity = %q %q", p.Client, p.Version)
	}
	if !slices.Equal(p.Revisions, []string{"2026-07-28"}) {
		t.Fatalf("revisions = %v", p.Revisions)
	}
	if !slices.Equal(p.Handshakes, []string{"server/discover, then stateless"}) {
		t.Fatalf("handshakes = %v", p.Handshakes)
	}
	if p.Requests != 5 || p.Identified != 5 || p.InitializeIdentified != 0 {
		t.Fatalf("requests %d, identified %d, initialize identified %d", p.Requests, p.Identified, p.InitializeIdentified)
	}
	wantCaps := []string{"elicitation (form, url)", "extension io.modelcontextprotocol/tasks"}
	if !slices.Equal(p.Capabilities, wantCaps) {
		t.Fatalf("capabilities = %v, want %v", p.Capabilities, wantCaps)
	}
	// Two requests carried a good traceparent and one a broken one.
	if p.TraceContext != 2 || p.TraceMalformed != 1 {
		t.Fatalf("trace context %d, malformed %d", p.TraceContext, p.TraceMalformed)
	}
	if p.ToolCalls != 2 || p.ProgressTokens != 1 {
		t.Fatalf("tool calls %d, progress tokens %d", p.ToolCalls, p.ProgressTokens)
	}
	if p.InputRequired != 1 || p.Retries != 1 || p.StateIssues != 0 {
		t.Fatalf("input_required %d, retries %d, state issues %d", p.InputRequired, p.Retries, p.StateIssues)
	}
	if p.Answers["accept"] != 1 || len(p.Answers) != 1 {
		t.Fatalf("answers = %v", p.Answers)
	}
	// The second tools/list came inside the ten-minute window the first declared.
	if p.CacheRefetches != 1 {
		t.Fatalf("cache refetches = %d", p.CacheRefetches)
	}
	if len(p.ServerRequests) != 0 {
		t.Fatalf("a 2026-07-28 session has no server-initiated requests, got %v", p.ServerRequests)
	}
}

func TestLegacyClientProfile(t *testing.T) {
	st, h := legacySession(t)
	p, ok := Fold(st, h)
	if !ok {
		t.Fatal("a session with client requests must fold")
	}
	if p.Client != "oldie" || p.Version != "0.9" {
		t.Fatalf("identity = %q %q", p.Client, p.Version)
	}
	if !slices.Equal(p.Revisions, []string{"2025-11-25"}) || !slices.Equal(p.Handshakes, []string{"initialize"}) {
		t.Fatalf("revisions %v, handshakes %v", p.Revisions, p.Handshakes)
	}
	if p.InitializeIdentified != 1 || p.Identified != 0 {
		t.Fatalf("initialize identified %d, identified %d", p.InitializeIdentified, p.Identified)
	}
	if !slices.Equal(p.Capabilities, []string{"elicitation", "roots", "sampling"}) {
		t.Fatalf("capabilities = %v", p.Capabilities)
	}
	if p.Deprecated["logging"] == 0 {
		t.Fatalf("logging/setLevel is deprecated in 2026-07-28 and must be counted, got %v", p.Deprecated)
	}
	want := map[string]int{"elicitation/create answered": 1, "roots/list error -32601": 1}
	if len(p.ServerRequests) != len(want) {
		t.Fatalf("server requests = %v, want %v", p.ServerRequests, want)
	}
	for k, n := range want {
		if p.ServerRequests[k] != n {
			t.Fatalf("server requests = %v, want %v", p.ServerRequests, want)
		}
	}
	if p.Answers["decline"] != 1 {
		t.Fatalf("a legacy elicitation answer counts too, got %v", p.Answers)
	}
	if p.Cancellations != 1 {
		t.Fatalf("cancellations = %d", p.Cancellations)
	}
	if p.InputRequired != 0 || p.Retries != 0 {
		t.Fatalf("no multi round-trip in a 2025-11-25 session, got %d and %d", p.InputRequired, p.Retries)
	}
}

func TestRetryWithAChangedRequestStateIsAStateIssue(t *testing.T) {
	st, h := capture(t, "tampered",
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"confirm","arguments":{},` + meta2026("") + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","inputRequests":{"confirm":{"method":"elicitation/create","params":{"message":"Go ahead?","requestedSchema":{"type":"object","properties":{}}}}},"requestState":"st-1"}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"confirm","arguments":{},"requestState":"st-1-edited","inputResponses":{"confirm":{"action":"accept"}},` + meta2026("") + `}}`},
	)
	p, _ := Fold(st, h)
	if p.Retries != 1 || p.StateIssues != 1 {
		t.Fatalf("retries %d, state issues %d, want 1 and 1", p.Retries, p.StateIssues)
	}
	if got := Table([]Profile{p})[9].Cells[0]; got != "1 input_required, 1 retried, 1 with a bad requestState" {
		t.Fatalf("multi round-trip cell = %q", got)
	}
}

func TestQuestionNobodyAnsweredIsCountedAsUnanswered(t *testing.T) {
	st, h := capture(t, "abandoned",
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"confirm","arguments":{},` + meta2026("") + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","inputRequests":{"confirm":{"method":"elicitation/create","params":{"message":"Go ahead?","requestedSchema":{"type":"object","properties":{}}}}},"requestState":"st-1"}}`},
	)
	p, _ := Fold(st, h)
	if p.InputRequired != 1 || p.Retries != 0 {
		t.Fatalf("input_required %d, retries %d", p.InputRequired, p.Retries)
	}
	if p.Answers["unanswered"] != 1 || len(p.Answers) != 1 {
		t.Fatalf("a question no retry ever answered must read as unanswered, got %v", p.Answers)
	}
}

func TestDiscoverThenInitializeIsTheBackwardCompatibilityFallback(t *testing.T) {
	st, h := capture(t, "fallback",
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + meta2026("") + `}}`},
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`},
		frame{proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"probe","version":"1.4.0"},"capabilities":{}}}`},
	)
	p, _ := Fold(st, h)
	if !slices.Equal(p.Handshakes, []string{"server/discover, then initialize"}) {
		t.Fatalf("handshakes = %v", p.Handshakes)
	}
	if !slices.Equal(p.Revisions, []string{"2025-11-25", "2026-07-28"}) {
		t.Fatalf("both revisions it put on the wire must show, got %v", p.Revisions)
	}
}

func TestHandshakeNamesEveryWayOfOpening(t *testing.T) {
	for _, c := range []struct {
		discover, initialize, stateless int
		want                            string
	}{
		{0, 3, 1, "server/discover, then initialize"},
		{4, 1, 0, "initialize, then server/discover"},
		{-1, 0, 2, "initialize and stateless requests"},
		{-1, 0, 0, "initialize"},
		{0, -1, 3, "server/discover, then stateless"},
		{-1, -1, 3, "stateless, without server/discover"},
		{-1, -1, 0, "no handshake seen"},
	} {
		if got := handshake(c.discover, c.initialize, c.stateless); got != c.want {
			t.Errorf("handshake(%d, %d, %d) = %q, want %q", c.discover, c.initialize, c.stateless, got, c.want)
		}
	}
}

func TestSessionWithoutClientRequestsSaysNothing(t *testing.T) {
	st, h := capture(t, "quiet",
		frame{proxy.ServerToClient, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"hi"}}`},
	)
	if _, ok := Fold(st, h); ok {
		t.Fatal("a session where the client sent nothing must not produce a profile")
	}
}

func TestMergePoolsSessionsOfOneClientVersionAndKeepsVersionsApart(t *testing.T) {
	st1, h1 := modernSession(t, "a")
	st2, h2 := modernSession(t, "b")
	stL, hL := legacySession(t)
	a, _ := Fold(st1, h1)
	b, _ := Fold(st2, h2)
	l, _ := Fold(stL, hL)
	b2 := b
	b2.Version = "1.5.0"

	merged := Merge([]Profile{l, a, b, b2})
	if len(merged) != 3 {
		t.Fatalf("want three columns, probe 1.4.0, probe 1.5.0 and oldie 0.9, got %d", len(merged))
	}
	names := []string{merged[0].Name(), merged[1].Name(), merged[2].Name()}
	if !slices.Equal(names, []string{"oldie 0.9", "probe 1.4.0", "probe 1.5.0"}) {
		t.Fatalf("columns = %v", names)
	}
	pooled := merged[1]
	if pooled.Sessions != 2 || pooled.Requests != 10 || pooled.Retries != 2 || pooled.Answers["accept"] != 2 {
		t.Fatalf("pooled = sessions %d, requests %d, retries %d, answers %v", pooled.Sessions, pooled.Requests, pooled.Retries, pooled.Answers)
	}
	if !slices.Equal(pooled.Revisions, []string{"2026-07-28"}) {
		t.Fatalf("pooled revisions = %v", pooled.Revisions)
	}
}

func TestMergeSortsAnUnnamedClientLast(t *testing.T) {
	merged := Merge([]Profile{{Client: ""}, {Client: "zed"}, {Client: "Alpha"}})
	if got := []string{merged[0].Name(), merged[1].Name(), merged[2].Name()}; !slices.Equal(got, []string{"Alpha", "zed", "unidentified client"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestTableReadsAsTheFactsItCarries(t *testing.T) {
	stM, hM := modernSession(t, "modern")
	stL, hL := legacySession(t)
	m, _ := Fold(stM, hM)
	l, _ := Fold(stL, hL)
	rows := Table(Merge([]Profile{m, l}))
	cell := func(label string, col int) string {
		for _, r := range rows {
			if r.Label == label {
				return r.Cells[col]
			}
		}
		t.Fatalf("no row %q", label)
		return ""
	}
	// Column 0 is oldie, column 1 is probe.
	checks := []struct {
		label string
		col   int
		want  string
	}{
		{"opens with", 0, "initialize"},
		{"opens with", 1, "server/discover, then stateless"},
		{"names itself", 0, "in initialize"},
		{"names itself", 1, "on 5 of 5 requests"},
		{"trace context", 1, "2 of 5 requests, 1 malformed"},
		{"progress tokens", 1, "1 of 2 tool calls"},
		{"multi round-trip", 0, "not exercised"},
		{"multi round-trip", 1, "1 input_required, 1 retried"},
		{"re-fetches inside ttlMs", 1, "1"},
		{"lists", 1, "tools/list ×2"},
		{"lists", 0, "none"},
		{"elicitation answers", 0, "decline 1"},
		{"cancellations", 0, "1"},
	}
	for _, c := range checks {
		if got := cell(c.label, c.col); got != c.want {
			t.Errorf("%s, column %d = %q, want %q", c.label, c.col, got, c.want)
		}
	}
	if got := cell("server requests", 0); !strings.Contains(got, "roots/list error -32601 ×1") || !strings.Contains(got, "elicitation/create answered ×1") {
		t.Errorf("server requests = %q", got)
	}
}

func TestValidTraceparent(t *testing.T) {
	for tp, want := range map[string]bool{
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01":       true,
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01":       false,
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01":       false,
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01":       false,
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01":       false,
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra": false,
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra": true,
		"not-a-traceparent": false,
	} {
		if got := validTraceparent(tp); got != want {
			t.Errorf("validTraceparent(%q) = %v, want %v", tp, got, want)
		}
	}
}
