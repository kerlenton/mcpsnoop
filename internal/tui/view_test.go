package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// TestStatusStyleWarnsOnTruncated locks the row colour to the "warn" text a
// truncated frame shows. The marker moved off the Warning field, so statusStyle
// must check the flag or the cell falls through to the muted style.
func TestStatusStyleWarnsOnTruncated(t *testing.T) {
	m := New(store.New())
	fg := m.statusStyle(store.EventView{Kind: store.EventOther, Truncated: true}).GetForeground()
	if fg != m.styles.warn.GetForeground() {
		t.Fatal("a truncated event should render its status in the warn style")
	}
	if fg == m.styles.dim.GetForeground() {
		t.Fatal("a truncated event must not fall through to the muted style")
	}
}

// TestTruncateMeasuresInCells locks the fix for the panic where truncate mixed
// cell width with rune count. Every assertion is on lipgloss.Width of the result,
// never its rune or byte length, so the test cannot repeat the bug it guards.
func TestTruncateMeasuresInCells(t *testing.T) {
	cjk := strings.Repeat("あ", 20)  // 20 runes, 40 cells
	emoji := strings.Repeat("😀", 3) // 3 runes, 6 cells
	mixed := "abcあいうdef漢字"          // mix of one- and two-cell runes

	cases := []struct {
		name string
		s    string
		w    int
		want string // exact result to assert, empty means only bound the width
	}{
		{"ascii longer than w", strings.Repeat("a", 30), 10, strings.Repeat("a", 9) + "…"},
		{"exact fit unchanged", "hello", 5, "hello"},
		{"twenty cjk at w=30 (old panic)", cjk, 30, ""},
		{"three emoji at w=5 (small panic)", emoji, 5, ""},
		{"wide runes w=0", cjk, 0, ""},
		{"wide runes w=1", cjk, 1, ""},
		{"wide runes w=2", cjk, 2, ""},
		{"mixed ascii and cjk", mixed, 9, ""},
		// An invalid byte (stderr is raw server bytes) decodes to U+FFFD; the offset
		// must advance by one byte, not the three of the re-encoded form.
		{"invalid utf-8 byte", "ab\xffcd", 3, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.w) // must not panic
			if w := lipgloss.Width(got); w > tc.w {
				t.Fatalf("truncate(%q, %d) width = %d cells, want at most %d (%q)", tc.s, tc.w, w, tc.w, got)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.s, tc.w, got, tc.want)
			}
		})
	}
}

func TestTruncateAppendsEllipsisWhenItCuts(t *testing.T) {
	got := truncate(strings.Repeat("a", 30), 10)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a cut result should end with an ellipsis, got %q", got)
	}
}

// TestSchemaColumnLabelsFitBesideTheMoreMarker locks the two things the SCHEMA
// column promises. The label is shown whole, and the trailing "+" that means
// "more than one kind" is still visible when it applies. Both are decided by
// cellL truncating to sumSchemaW, so a label one cell too long silently eats the
// marker and the row for a tool with five problems becomes the row for a tool
// with one. Nothing else in the TUI lists a tool's findings, so that marker is
// the only sign more exists.
func TestSchemaColumnLabelsFitBesideTheMoreMarker(t *testing.T) {
	blank := cellL("", sumSchemaW)
	byCell := map[string]store.SchemaFindingKind{}
	for _, kind := range store.SchemaFindingKinds {
		label := schemaKindLabel(kind)
		one, many := cellL(label, sumSchemaW), cellL(label+"+", sumSchemaW)
		if strings.Contains(one, "…") {
			t.Errorf("%s: label %q does not fit the %d-cell column, renders %q", kind, label, sumSchemaW, one)
		}
		if !strings.HasSuffix(strings.TrimRight(many, " "), "+") {
			t.Errorf("%s: the + meaning more than one kind is truncated away, renders %q", kind, many)
		}
		for _, cell := range []string{one, many} {
			if cell == blank {
				t.Errorf("%s renders as the blank cell a clean schema uses", kind)
			}
			if prev, dup := byCell[cell]; dup {
				t.Errorf("%s renders %q, which is already what %s renders", kind, cell, prev)
			}
			byCell[cell] = kind
		}
	}
}

// TestSchemaHeadlineOrderRanksEveryKind. headlineFinding falls back to
// findings[0], which is the order the schema walk happened to meet them in, so a
// kind missing from the ranking can outrank a violation. schemaKindLabel falls
// back to the raw enum name, which for a kind like nonObjectRoot is 13 cells in
// an 8-cell column. Driven off store.SchemaFindingKinds for the same reason the
// drift section is driven off store.ToolDriftKinds.
func TestSchemaHeadlineOrderRanksEveryKind(t *testing.T) {
	ranked := make(map[store.SchemaFindingKind]bool, len(schemaHeadlineOrder))
	for _, kind := range schemaHeadlineOrder {
		ranked[kind] = true
	}
	for _, kind := range store.SchemaFindingKinds {
		if !ranked[kind] {
			t.Errorf("%s is not ranked, so it wins the column only by where the walk met it", kind)
		}
	}
	if len(schemaHeadlineOrder) != len(store.SchemaFindingKinds) {
		t.Errorf("ranking has %d kinds, the store emits %d", len(schemaHeadlineOrder), len(store.SchemaFindingKinds))
	}

	// The violation outranks every observation, which is the whole reason the
	// order is written down rather than taken from the walk.
	for _, other := range store.SchemaFindingKinds[1:] {
		findings := []store.SchemaFinding{{Kind: other}, {Kind: store.FindingNonObjectRoot}}
		if got := headlineFinding(findings); got != store.FindingNonObjectRoot {
			t.Errorf("a schema with %s and a non-object root headlines %s, want the violation", other, got)
		}
	}
}

// TestErrCellSeparatesTheTwoKindsOfFailure locks the ERR column's colours, which
// are the answer rather than decoration. Red means the server side broke, warn
// means the tool answered isError, and a tool with both shows the counts joined
// rather than one number that reads as a single finding.
func TestErrCellSeparatesTheTwoKindsOfFailure(t *testing.T) {
	m := New(store.New())
	red := m.styles.respErr.GetForeground()
	yellow := m.styles.warn.GetForeground()
	if red == yellow {
		t.Fatal("the two error styles share a colour, so the column cannot separate them")
	}

	for _, tc := range []struct {
		name   string
		tool   store.ToolStats
		text   string
		colour lipgloss.TerminalColor
	}{
		{"clean", store.ToolStats{}, "·", m.styles.faint.GetForeground()},
		{"server broke", store.ToolStats{Errors: 3, ProtocolErrors: 3}, "3", red},
		{"tool reported", store.ToolStats{Errors: 3, ToolErrors: 3}, "3", yellow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := m.errCellParts(tc.tool)
			if len(parts) != 1 {
				t.Fatalf("parts = %d, want 1", len(parts))
			}
			if parts[0].text != tc.text {
				t.Fatalf("text = %q, want %q", parts[0].text, tc.text)
			}
			if got := parts[0].style.GetForeground(); got != tc.colour {
				t.Fatalf("colour = %v, want %v", got, tc.colour)
			}
		})
	}

	mixed := m.errCellParts(store.ToolStats{Errors: 5, ProtocolErrors: 2, ToolErrors: 3})
	if len(mixed) != 3 {
		t.Fatalf("a tool with both kinds gave %d parts, want 3", len(mixed))
	}
	if mixed[0].text != "2" || mixed[0].style.GetForeground() != red {
		t.Fatalf("the protocol count leads in red, got %q", mixed[0].text)
	}
	if mixed[2].text != "3" || mixed[2].style.GetForeground() != yellow {
		t.Fatalf("the reported count follows in warn, got %q", mixed[2].text)
	}
	if cell := m.errCell(store.ToolStats{Errors: 5, ProtocolErrors: 2, ToolErrors: 3}); cell != "2+3" {
		t.Fatalf("joined cell = %q, want 2+3", cell)
	} else if lipgloss.Width(cell) > sumErrW {
		t.Fatalf("the joined cell is %d cells wide, wider than the %d-cell column", lipgloss.Width(cell), sumErrW)
	}
}

// TestSummarySortsABrokenServerAboveAToolReportingFailure is the behaviour the
// split exists for. A search that legitimately finds nothing shared one error
// count with a server returning -32603, and so sorted above it.
func TestSummarySortsABrokenServerAboveAToolReportingFailure(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	st := store.New()
	seq := uint64(0)
	call := func(tool, response string) {
		seq++
		id := fmt.Sprintf("%d", seq)
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ClientToServer, Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":%q}}`, id, tool))})
		seq++
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ServerToClient, Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,%s}`, id, response))})
	}
	for range 3 {
		call("search", `"result":{"content":[],"isError":true}`)
	}
	call("write", `"error":{"code":-32603,"message":"boom"}`)

	m := New(st)
	m.streamSessionID = "s1"
	m.width, m.height = 120, 40
	out := m.summaryContent()

	searchAt := strings.Index(out, "search")
	writeAt := strings.Index(out, "write")
	if searchAt < 0 || writeAt < 0 {
		t.Fatalf("both tools should appear in the summary:\n%s", out)
	}
	if writeAt > searchAt {
		t.Fatalf("the broken server sorts below the tool that answered isError:\n%s", out)
	}
	if !strings.Contains(out, "reported by the tool itself with isError") {
		t.Fatalf("the summary never explains the second colour:\n%s", out)
	}
}

// TestFooterNamesRetiredExchanges keeps the parking cap visible where the counts
// it affects are shown. Unlike frames the memory budget released, a retired
// operation makes the numbers beside it wrong, so it carries the warn colour.
func TestFooterNamesRetiredExchanges(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	st := store.New()
	var seq uint64
	for i := range 200 {
		seq++
		id := fmt.Sprintf("%d", i)
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ClientToServer, Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"ask"}}`, id))})
		seq++
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(time.Duration(seq) * time.Millisecond),
			Direction: proxy.ServerToClient, Raw: json.RawMessage(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%q,"result":{"resultType":"input_required","requestState":"s-%d","inputRequests":{"k1":{"method":"elicitation/create","params":{}}}}}`, id, i))})
	}

	m := New(st)
	m.streamSessionID = "s1"
	m.allSessions = st.Sessions()
	m.view = viewStream
	if got := m.currentRetiredExchanges(); got == 0 {
		t.Fatal("the model reports no retired exchanges for a session that hit the cap")
	}
	if footer := m.footerCounters(); !strings.Contains(footer, "unlinked") {
		t.Fatalf("the footer does not name the retired exchanges: %q", footer)
	}

	clean := New(store.New())
	clean.view = viewStream
	if footer := clean.footerCounters(); strings.Contains(footer, "unlinked") {
		t.Fatalf("a clean session shows the marker anyway: %q", footer)
	}
}

// TestElicitOverlayShowsTheLedger covers the panel and the boundary it keeps.
// The values a user typed stay in the capture; a panel people screenshot must
// not repeat them.
func TestElicitOverlayShowsTheLedger(t *testing.T) {
	const submitted = "hunter2-do-not-repeat-me"
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	st := store.New()
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	seq := uint64(0)
	ingest := func(dir proxy.Direction, off time.Duration, raw string) {
		seq++
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0.Add(off),
			Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)})
	}
	seq++
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta})
	ingest(proxy.ClientToServer, time.Second, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"login_legacy"}}`)
	ingest(proxy.ServerToClient, 2*time.Second, `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"st","inputRequests":{"creds":{"method":"elicitation/create","params":{"message":"Enter your admin password","requestedSchema":{"type":"object","properties":{"password":{"type":"string"}}}}}}}}`)
	ingest(proxy.ClientToServer, 5*time.Second, fmt.Sprintf(`{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"login_legacy","inputResponses":{"creds":{"action":"decline","content":{"password":%q}}},"requestState":"st"}}`, submitted))
	ingest(proxy.ClientToServer, 10*time.Second, `{"jsonrpc":"2.0","id":"3","method":"tools/call","params":{"name":"sync"}}`)
	ingest(proxy.ServerToClient, 11*time.Second, `{"jsonrpc":"2.0","id":"3","result":{"resultType":"input_required","requestState":"st2","inputRequests":{"auth":{"method":"elicitation/create","params":{"mode":"url","url":"https://mcp.example.com/ui/key","message":"api key"}}}}}`)

	m := New(st)
	m.streamSessionID = "s1"
	m.width, m.height = 120, 40
	out := m.elicitContent()

	for _, want := range []string{"login_legacy", "creds", "password", "decline", "Enter your admin password",
		"https://mcp.example.com/ui/key", "mcp.example.com", "pending"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the panel does not show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, submitted) {
		t.Fatalf("the panel repeats a submitted value:\n%s", out)
	}
	if !strings.Contains(out, "submitted values stay in the capture") {
		t.Fatalf("the panel does not say what it is deliberately not showing:\n%s", out)
	}
}

// TestElicitOverlayOnAQuietSessionSaysSo keeps an empty panel from reading as a
// broken one.
func TestElicitOverlayOnAQuietSessionSaysSo(t *testing.T) {
	m := New(store.New())
	m.streamSessionID = "s1"
	if out := m.elicitContent(); !strings.Contains(out, "no server asked the user for anything") {
		t.Fatalf("out = %q", out)
	}
}

// TestElicitKeyIsBoundAndDocumented keeps the panel reachable and the help
// honest, since a panel nobody can find is a panel that does not exist.
func TestElicitKeyIsBoundAndDocumented(t *testing.T) {
	m := New(store.New())
	if !m.keys.Elicit.Enabled() {
		t.Fatal("the elicitations key is not bound")
	}
	if got := m.keys.Elicit.Keys(); len(got) != 1 || got[0] != "l" {
		t.Fatalf("bound to %v, want l", got)
	}
	m.width, m.height = 120, 40
	if help := m.renderHelp(); !strings.Contains(help, "asked the user for") {
		t.Fatalf("the help never mentions the panel:\n%s", help)
	}
}
