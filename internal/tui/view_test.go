package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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

func TestStreamCellSafeForTableQuotesWireControls(t *testing.T) {
	p := progressBar{done: 1, total: 2, token: "tok\n\x1b[2J"}
	got := (streamCell{
		method: "tools/call\nnext", id: "1\r2", status: "work\ting",
		detail: "line one\nline two\x1b[H", tool: "echo\x1b[31m", progress: &p,
	}).safeForTable()

	for name, value := range map[string]string{
		"method": got.method, "id": got.id, "status": got.status,
		"detail": got.detail, "tool": got.tool, "progress token": got.progress.token,
	} {
		if strings.ContainsAny(value, "\r\n\t\x1b") {
			t.Errorf("%s still contains a terminal control after table sanitizing: %q", name, value)
		}
	}
	if !strings.Contains(got.detail, `\n`) || !strings.Contains(got.detail, `\x1b`) {
		t.Fatalf("detail should preserve controls as visible escapes, got %q", got.detail)
	}
	if p.token != "tok\n\x1b[2J" {
		t.Fatalf("safeForTable mutated the source progress token: %q", p.token)
	}
}

func TestSessionsTableQuotesDynamicControls(t *testing.T) {
	m := New(store.New())
	m.sessions = []store.SessionHeader{{ID: "s1", Label: "bad\nname"}}
	m.clients = map[string]string{"s1": "cli\x1bX"}
	m.activity = map[string][]int{}

	out := ansi.Strip(m.renderSessionsTable(100, 4))
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("sessions table = %d physical lines, want header + one session:\n%s", len(lines), out)
	}
	for i, line := range lines {
		if strings.ContainsAny(line, "\r\t\x1b") {
			t.Fatalf("sessions table line %d still contains a terminal control: %q", i, line)
		}
	}
	for _, want := range []string{`bad\nname`, `cli\x1bX`} {
		if !strings.Contains(out, want) {
			t.Fatalf("sessions table should preserve %q as visible escapes:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "▌"); got != 1 {
		t.Fatalf("selected session row should remain visible exactly once, marker count = %d:\n%s", got, out)
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

// mrtrStore builds the capture from issue #201 in a store.
func mrtrStore(t *testing.T) *store.Store {
	t.Helper()
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := store.New()
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight"}}`},
		{proxy.ServerToClient, 400, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"confirm":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 12400, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","requestState":"st-1","inputResponses":{"confirm":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 12700, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"input_required","requestState":"st-2","inputRequests":{"seat":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 37700, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"book_flight","requestState":"st-2","inputResponses":{"seat":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 38200, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`},
		{proxy.ClientToServer, 39000, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lookup_price"}}`},
		{proxy.ServerToClient, 39200, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`},
	}
	for i, f := range frames {
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "booking", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f.raw)})
	}
	return st
}

// TestInteractPanelSplitsTheTotal covers the panel where a wall clock comes
// apart into the server's share and the client's.
func TestInteractPanelSplitsTheTotal(t *testing.T) {
	m := New(mrtrStore(t))
	m.streamSessionID = "s1"
	m.width, m.height = 120, 40
	out := m.interactContent()

	for _, want := range []string{"book_flight", "3 round trips", "hop 1", "hop 2", "hop 3", "elicitation/create"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the panel does not show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "lookup_price") {
		t.Fatalf("an ordinary one hop call is already a line in the stream:\n%s", out)
	}
	// The three figures, rendered the way the rest of the TUI renders a latency.
	for _, want := range []string{"38.2s", "1.2s", "37s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the panel does not show %q:\n%s", want, out)
		}
	}
}

// TestInteractPanelOnASessionWithNoChainSaysSo keeps an empty panel from reading
// as a broken one.
func TestInteractPanelOnASessionWithNoChainSaysSo(t *testing.T) {
	m := New(store.New())
	m.streamSessionID = "s1"
	if out := m.interactContent(); !strings.Contains(out, "no operation in this session took more than one request") {
		t.Fatalf("out = %q", out)
	}
}

// TestSummaryShowsRoundTrips covers the column that makes a chatty tool visible
// without opening a panel, and keeps it quiet for the ordinary case.
func TestSummaryShowsRoundTrips(t *testing.T) {
	m := New(mrtrStore(t))
	m.streamSessionID = "s1"
	m.width, m.height = 120, 40
	out := m.summaryContent()
	if !strings.Contains(out, "TRIPS") {
		t.Fatalf("the summary has no round trips column:\n%s", out)
	}
	// Read by column name, since CALLS also carries small numbers and hunting the
	// row for one would find the wrong column.
	if got := summaryCell(t, out, "book_flight", "TRIPS"); got != "3" {
		t.Fatalf("book_flight TRIPS = %q, want 3\n%s", got, out)
	}
	// One round trip is the ordinary case and says nothing.
	if got := summaryCell(t, out, "lookup_price", "TRIPS"); got != "" {
		t.Fatalf("lookup_price TRIPS = %q, want blank\n%s", got, out)
	}
}

// TestInteractKeyIsBoundAndDocumented keeps the panel reachable and the help
// honest.
func TestInteractKeyIsBoundAndDocumented(t *testing.T) {
	m := New(store.New())
	if got := m.keys.Interact.Keys(); len(got) != 1 || got[0] != "i" {
		t.Fatalf("bound to %v, want i", got)
	}
	m.width, m.height = 120, 40
	if help := m.renderHelp(); !strings.Contains(help, "per-hop timing") {
		t.Fatalf("the help never mentions the panel:\n%s", help)
	}
}

// TestInteractPanelCannotBeMadeToForgeRows covers the class of defect the
// inventory and stats tables were fixed for. A tool name and a method come from
// whoever wrote the server, so a newline in one would close the row it lands in
// and an escape sequence would drive the terminal.
func TestInteractPanelCannotBeMadeToForgeRows(t *testing.T) {
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := store.New()
	const forged = "book\x1b[31mflight\r\n  total     FORGED"
	// Encoded as JSON, not with %q. Go's quoting spells an escape as \x1b, which
	// JSON has no such escape for, so the frame would be malformed and the store
	// would drop it rather than render it.
	name, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, fmt.Sprintf(`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":%s}}`, name)},
		{proxy.ServerToClient, 400, `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"st","inputRequests":{"k":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 5000, fmt.Sprintf(`{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":%s,"requestState":"st","inputResponses":{"k":{"action":"accept"}}}}`, name)},
		{proxy.ServerToClient, 5100, `{"jsonrpc":"2.0","id":"2","result":{"content":[]}}`},
	}
	for i, f := range frames {
		st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f.raw)})
	}

	m := New(st)
	m.streamSessionID = "s1"
	m.width, m.height = 120, 40
	out := m.interactContent()
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("an escape sequence reached the panel:\n%q", out)
	}
	if strings.Contains(out, "\n  total     FORGED") {
		t.Fatalf("a forged line reached the panel:\n%s", out)
	}
	if !strings.Contains(out, `\x1b`) {
		t.Fatalf("the value was dropped rather than quoted, so it is not recoverable:\n%s", out)
	}
}

// TestSummaryTableFitsANarrowTerminal keeps the new column from wrapping every
// row on an eighty-column screen, which is exactly the width the panel was
// already sized to fill.
func TestSummaryTableFitsANarrowTerminal(t *testing.T) {
	for _, width := range []int{80, 84, 90, 120} {
		m := New(mrtrStore(t))
		m.streamSessionID = "s1"
		m.width, m.height = width, 40
		panelW, _ := m.overlayDims()
		out := m.summaryContent()
		for _, line := range strings.Split(out, "\n") {
			if got := lipgloss.Width(line); got > panelW {
				t.Fatalf("at %d columns a row is %d cells wide against a %d-cell panel:\n%s",
					width, got, panelW, line)
			}
		}
		// The column is there when there is room for it, and gone when there is not.
		if wantTrips := panelW >= sumWidthWithoutTrips+sumTripsW; strings.Contains(out, "TRIPS") != wantTrips {
			t.Fatalf("at %d columns TRIPS present = %v, want %v", width, strings.Contains(out, "TRIPS"), wantTrips)
		}
	}
}

// TestRenderSafeKeepsStylingAndDropsEverythingElse locks the rule the whole
// escape defence rests on. lipgloss emits SGR and nothing else, so SGR is the
// only escape a frame can legitimately contain, and everything else in it came
// off the wire. Getting this wrong in either direction is bad: keep too much and
// a server drives the terminal, keep too little and the UI loses its colour.
func TestRenderSafeKeepsStylingAndDropsEverythingElse(t *testing.T) {
	esc := string(rune(27))
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain text survives", "hello", "hello"},
		{"SGR colour survives", esc + "[31mred" + esc + "[0m", esc + "[31mred" + esc + "[0m"},
		{"SGR with sub-parameters survives", esc + "[38;2;1;2;3mx", esc + "[38;2;1;2;3mx"},
		{"screen erase is dropped", "a" + esc + "[2Jb", "ab"},
		{"cursor home is dropped", "a" + esc + "[Hb", "ab"},
		{"device status query is dropped", "a" + esc + "[6nb", "ab"},
		{"OSC 52 clipboard write, BEL terminated", "a" + esc + "]52;c;aGk=" + string(rune(7)) + "b", "ab"},
		{"OSC 52 clipboard write, ST terminated", "a" + esc + "]52;c;aGk=" + esc + "\\" + "b", "ab"},
		{"OSC 8 hyperlink is dropped", "a" + esc + "]8;;http://x" + string(rune(7)) + "b", "ab"},
		{"DCS is dropped", "a" + esc + "Pq#0" + esc + "\\" + "b", "ab"},
		{"APC is dropped", "a" + esc + "_G f=1" + esc + "\\" + "b", "ab"},
		{"two-byte escape is dropped", "a" + esc + "7b", "ab"},
		{"a lone trailing ESC is dropped", "ab" + esc, "ab"},
		{"newlines are the frame's own rows", "a" + "\n" + "b", "a" + "\n" + "b"},
		{"carriage return cannot overwrite a row", "a" + "\r" + "b", "ab"},
		{"BEL is dropped", "a" + string(rune(7)) + "b", "ab"},
		{"C1 CSI is dropped", "a" + string(rune(0x9b)) + "2Jb", "a2Jb"},
		{"bidi override is dropped", "safe" + string(rune(0x202e)) + "txet", "safetxet"},
		{"wide runes are untouched", "中文", "中文"},
		{"emoji ZWJ sequences keep their width", "👨‍👩‍👧", "👨‍👩‍👧"},
		{"a malformed CSI resyncs instead of eating the frame", "a" + esc + "[" + string(rune(7)) + "tail", "atail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderSafe(tc.in); got != tc.want {
				t.Fatalf("renderSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCapturedEscapesCannotReachTheTerminal is the end-to-end form of the same
// rule. The spec lets a server write any UTF-8 it likes to stderr, so a line of
// it is the easiest thing in a capture for an attacker to control, and OSC 52 in
// particular writes the clipboard of whoever is watching. None of it may survive
// into the string bubbletea hands the terminal, in the table or the inspector.
func TestCapturedEscapesCannotReachTheTerminal(t *testing.T) {
	esc := string(rune(27))
	hostile := esc + "[2J" + esc + "[H warming up " + esc + "]52;c;cHduZWQ=" + string(rune(7))

	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	e := env(2, proxy.ServerToClient, "")
	e.Raw = nil
	e.Text = hostile
	st.Ingest(e)

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // the stream table
	inspector := drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if inspector.overlay != overlayInspector {
		t.Fatalf("this test needs the inspector open, overlay %d", inspector.overlay)
	}

	for name, frame := range map[string]string{
		"stream table": m.View(),
		"inspector":    inspector.View(),
	} {
		for what, seq := range map[string]string{
			"a screen erase":         esc + "[2J",
			"a cursor move":          esc + "[H",
			"an OSC clipboard write": esc + "]52;",
		} {
			if strings.Contains(frame, seq) {
				t.Errorf("%s handed the terminal %s from captured stderr", name, what)
			}
		}
	}

	// Dropping them silently would be its own failure, so the inspector shows
	// what the server sent, as text.
	if body := inspector.inspectorBody(); !strings.Contains(body, `\`+"x1b[2J") {
		t.Fatalf("inspector should show the captured escape as text, got %q", body)
	}
}

// TestCopiedFrameTextCannotRunInAnotherTerminal covers the one sink that never
// passes through View. Pasting a captured escape into a shell, an issue or a
// colleague's terminal would run it there.
func TestCopiedFrameTextCannotRunInAnotherTerminal(t *testing.T) {
	esc := string(rune(27))
	got := frameText(store.EventView{Text: "log " + esc + "]52;c;cHduZWQ=" + string(rune(7))})
	if strings.ContainsRune(got, 27) {
		t.Fatalf("clipboard payload still carries a raw escape: %q", got)
	}
	if !strings.Contains(got, `\`+"x1b]52;") {
		t.Fatalf("clipboard payload should spell the escape out, got %q", got)
	}
}

// TestSafeCellQuotesEverythingQuoteWouldEscape keeps safeCell's test and its
// action in agreement. Triggering on unicode.IsControl alone let every bidi and
// zero-width rune through, which is the Trojan Source shape: a tool named with a
// right-to-left override draws as a different tool in the table.
func TestSafeCellQuotesEverythingQuoteWouldEscape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		quote bool
	}{
		{"plain ascii", "tools/call", false},
		{"wide runes", "工具", false},
		{"punctuation and symbols", `{"a": 1} ±§`, false},
		{"escape", "x" + string(rune(27)) + "[2J", true},
		{"newline", "a" + "\n" + "b", true},
		{"right-to-left override", "safe" + string(rune(0x202e)) + "txet", true},
		{"left-to-right mark", "a" + string(rune(0x200e)) + "b", true},
		{"zero width space", "ec" + string(rune(0x200b)) + "ho", true},
		{"byte order mark", "echo" + string(rune(0xfeff)), true},
		{"non-breaking space", "a" + string(rune(0xa0)) + "b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := safeCell(tc.in)
			if quoted := got != tc.in; quoted != tc.quote {
				t.Fatalf("safeCell(%q) = %q, quoted=%v want %v", tc.in, got, quoted, tc.quote)
			}
		})
	}
}

// TestSafeBodyEscapesInPlaceWithoutFlattening is why the inspector needs its own
// helper. A cell has to stay one row so safeCell quotes the whole value, but
// quoting a payload would collapse it into one unreadable line.
func TestSafeBodyEscapesInPlaceWithoutFlattening(t *testing.T) {
	in := "first" + "\n" + "second " + string(rune(27)) + "[2J" + "\n" + "third"
	got := safeBody(in)
	if n := strings.Count(got, "\n"); n != 2 {
		t.Fatalf("safeBody flattened the payload to %d newlines, want 2: %q", n, got)
	}
	if strings.ContainsRune(got, 27) {
		t.Fatalf("safeBody left a raw escape: %q", got)
	}
	if !strings.Contains(got, `\`+"x1b[2J") {
		t.Fatalf("safeBody should spell the escape out, got %q", got)
	}
	if plain := "nothing to escape" + "\n" + "here"; safeBody(plain) != plain {
		t.Fatal("safeBody must leave ordinary text byte for byte")
	}
}

// TestViewStripsEscapesFromAPanelThatDoesNotQuoteItsOwnValues is why View filters
// at all. The capabilities panel prints the server's instructions text straight
// from the wire, as several panels print several wire values, and expecting every
// one of them to remember to quote is how this class of bug keeps coming back.
// The filter in View is what makes the guarantee hold for panels that forgot.
func TestViewStripsEscapesFromAPanelThatDoesNotQuoteItsOwnValues(t *testing.T) {
	esc := string(rune(27))
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cli"}}}`))
	st.Ingest(env(2, proxy.ServerToClient,
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"demo"},"instructions":"be helpful \u001b]52;c;cHduZWQ=\u0007\u001b[2J"}}`))

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatalf("c should open the capabilities panel, overlay %d", m.overlay)
	}
	// The fixture only reaches the backstop while this panel still prints the
	// value unquoted. Failing rather than skipping keeps that from decaying
	// quietly: whoever teaches this panel to quote should point the fixture at
	// another panel that does not, so View's filter stays covered.
	if body := m.capsContent(); !strings.Contains(body, esc+"]52;") {
		t.Fatal("this panel now quotes its own values, so the fixture no longer exercises View's filter, repoint it at one that does not")
	}
	frame := m.View()
	if strings.Contains(frame, esc+"]52;") {
		t.Error("the frame handed the terminal an OSC clipboard write from the server's instructions")
	}
	if strings.Contains(frame, esc+"[2J") {
		t.Error("the frame handed the terminal a screen erase from the server's instructions")
	}
	if !strings.Contains(ansi.Strip(frame), "be helpful") {
		t.Errorf("the surrounding text should survive the filter:\n%s", frame)
	}
}

// TestStreamRowStaysOneRowForAnyWireValue covers the sanitizing call in
// streamRow rather than the helper it calls. A tool error has its own flattening
// step, so without a case like this one the call that protects every other wire
// value in the row, the method, the id, the status and the progress token, can be
// deleted with the suite still green.
func TestStreamRowStaysOneRowForAnyWireValue(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":"7\n8","method":"tools/\ncall","params":{"name":"echo"}}`))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	rows := m.streamRow(m.full[0], m.streamLayoutFor(120))
	for _, c := range rows {
		if strings.ContainsAny(c.text, "\n\r") {
			t.Fatalf("a wire value put a line break in a table cell: %q", c.text)
		}
	}
	line := m.rowLine(rows, 120, false)
	if n := strings.Count(line, "\n"); n != 0 {
		t.Fatalf("one frame rendered as %d physical rows: %q", n+1, line)
	}
}
