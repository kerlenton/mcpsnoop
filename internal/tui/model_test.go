package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/replay"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func env(seq uint64, dir proxy.Direction, raw string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: time.Now(),
		Direction: dir, Transport: "stdio", Raw: json.RawMessage(raw),
	}
}

// envAt is env with an explicit timestamp, so a test can give calls real
// durations (a request and its response at known times).
func envAt(seq uint64, dir proxy.Direction, ts time.Time, raw string) proxy.Envelope {
	e := env(seq, dir, raw)
	e.TS = ts
	return e
}

func sessionEnv(id, label string) proxy.Envelope {
	return proxy.Envelope{SessionID: id, ServerLabel: label, Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)}
}

func seed(st *store.Store) {
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"sampling":{}},"clientInfo":{"name":"cli"}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"demo"}}}`))
	st.Ingest(env(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`))
	st.Ingest(env(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`))
}

func drive(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	tm, _ := m.Update(msg)
	got, ok := tm.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", tm)
	}
	return got
}

func typeRunes(t *testing.T, m Model, s string) Model {
	t.Helper()
	return drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func ready(t *testing.T, st *store.Store) Model {
	t.Helper()
	m := New(st)
	m = drive(t, m, tea.WindowSizeMsg{Width: 160, Height: 44})
	return drive(t, m, frameMsg{})
}

func TestSessionsTableDriftMarkerKeepsLabel(t *testing.T) {
	st := store.New()
	// A label long enough that the old wide "! drift " marker truncated its tail
	// inside the fixed name column; the one-char marker must keep the whole label.
	label := "filesystem-server1"
	st.Ingest(proxy.Envelope{
		SessionID: "sess", ServerLabel: label, Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: "stdio",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	st.SetToolDrift("sess", store.ToolDrift{ChangedDescriptions: []string{"search"}})

	m := ready(t, st)
	out := m.View()
	if !strings.Contains(out, "! "+label) {
		t.Fatalf("drift row should keep the full label with a one-char marker\n%s", out)
	}
	if strings.Contains(out, "! drift ") {
		t.Fatalf("marker should no longer be the wide '! drift '\n%s", out)
	}
}

func TestStreamRowShowsSupersededStatusInWarnStyle(t *testing.T) {
	st := store.New()
	// Two requests reuse id 1 while the first is still in flight, so the first is
	// superseded.
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	st.Ingest(env(2, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the stream

	if len(m.full) == 0 || m.full[0].Call == nil || m.full[0].Call.State != store.Superseded {
		t.Fatalf("first frame should be a superseded call, got %+v", m.full[0].Call)
	}
	// The request row now carries an in-row superseded status rather than an empty
	// cell (the STATUS column truncates it, so assert the cell before truncation).
	if got := m.streamCells(m.full[0]).status; got != "superseded" {
		t.Fatalf("superseded request status = %q, want superseded", got)
	}
	// And it is styled as a warning (yellow), not a success (green). Compare the
	// style foreground, which survives the color-stripping the test env applies to
	// rendered output.
	fg := m.statusStyle(m.full[0]).GetForeground()
	if fg != m.styles.warn.GetForeground() {
		t.Fatal("superseded status should use the warn style")
	}
	if fg == m.styles.resp.GetForeground() {
		t.Fatal("superseded status must not use the success style")
	}
}

func TestRefreshClampsInspectWhenTimelineShrinks(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the stream
	m.inspect = len(m.full) - 1
	if m.inspect <= 0 {
		t.Fatal("expected a multi-frame timeline to inspect")
	}

	// The inspected session's timeline vanishes out from under the inspector.
	st.Delete(m.streamSessionID)
	m.refresh()

	if m.inspect < 0 || (len(m.full) > 0 && m.inspect >= len(m.full)) {
		t.Fatalf("inspect %d not clamped into range for full len %d", m.inspect, len(m.full))
	}
	// The direct m.full[m.inspect] readers must not panic on the shrunk timeline.
	_ = m.inspectorHeader(80)
	_ = m.inspectorHeaderH()
	_ = m.pairWidget()
}

func TestFrameMsgDefersRefreshToThrottledTick(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the stream
	// Settle by running one refresh cycle so dirty clears and m.full is current.
	for range refreshEvery {
		m = drive(t, m, tickMsg(time.Now()))
	}
	before := len(m.full)
	if m.dirty {
		t.Fatal("dirty should be clear after a settling tick")
	}

	// Deliver a frameMsg per envelope, exactly as the hub callback does.
	for i := range 20 {
		st.Ingest(env(uint64(5+i), proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/progress"}`))
		m = drive(t, m, frameMsg{})
	}
	// Not one of them triggered a refresh. The timeline is unchanged and the model
	// is only marked dirty, so the cost of a burst is bounded rather than per frame.
	if len(m.full) != before {
		t.Fatalf("frameMsg refreshed per frame: full %d -> %d", before, len(m.full))
	}
	if !m.dirty {
		t.Fatal("frameMsg should mark the model dirty")
	}

	// One throttled tick cycle performs a single refresh and clears the flag.
	for range refreshEvery {
		m = drive(t, m, tickMsg(time.Now()))
	}
	if len(m.full) <= before {
		t.Fatalf("a throttled tick should refresh once, full still %d", len(m.full))
	}
	if m.dirty {
		t.Fatal("refresh should clear the dirty flag")
	}
}

func TestSessionsTableAndDrillIn(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)

	if m.view != viewSessions {
		t.Fatalf("default view = %v, want sessions", m.view)
	}
	out := m.View()
	for _, want := range []string{"mcpsnoop", "NAME", "REQ", "demo", "sessions"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sessions view missing %q\n%s", want, out)
		}
	}

	// enter drills into the session's stream.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.view != viewStream {
		t.Fatal("enter should drill into the stream")
	}
	out = m.View()
	for _, want := range []string{"TIME", "METHOD", "initialize", "tools/call echo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream view missing %q\n%s", want, out)
		}
	}

	// esc backs out to the sessions table.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != viewSessions {
		t.Fatal("esc should return to the sessions table")
	}
}

func TestInspectorHeaderHSyncsWithProtocolVersionOnly(t *testing.T) {
	// A frame carrying ONLY MCP-Protocol-Version (no Mcp-Method/Mcp-Name) must still
	// reserve the second chrome line, so the height gate stays in lockstep with the
	// render gate in inspectorHeader. If they diverge the body is clipped or misplaced.
	m := Model{full: []store.EventView{{MCPProtocolVersion: "2026-07-28"}}, inspect: 0}
	if got := m.inspectorHeaderH(); got != 2 {
		t.Fatalf("inspectorHeaderH() = %d, want 2 for a protocol-version-only frame", got)
	}
	// A frame with no request headers reserves no extra line.
	m.full = []store.EventView{{}}
	if got := m.inspectorHeaderH(); got != 1 {
		t.Fatalf("inspectorHeaderH() = %d, want 1 for a header-less frame", got)
	}
}

func TestInspectorOverlay(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream (follow → last frame)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspect
	if m.overlay != overlayInspector {
		t.Fatal("enter on a frame should open the inspector")
	}
	out := m.View()
	for _, want := range []string{"FRAME", "jsonrpc"} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspector missing %q\n%s", want, out)
		}
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.overlay != overlayNone {
		t.Fatal("esc should close the inspector")
	}
}

func TestPause(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)
	m = typeRunes(t, m, "p")
	if !m.paused {
		t.Fatal("p should pause")
	}
	if !strings.Contains(m.View(), "paused") {
		t.Fatalf("header should show paused:\n%s", m.View())
	}
}

func TestStreamFilter(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	total := len(m.timeline)

	m = typeRunes(t, m, "/")
	if m.inputMode != inputFilter {
		t.Fatal("/ should open the filter input")
	}
	m = typeRunes(t, m, "echo")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.query != "echo" || len(m.timeline) == 0 || len(m.timeline) >= total {
		t.Fatalf("filter should narrow: query=%q %d of %d", m.query, len(m.timeline), total)
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc}) // clears filter
	if m.query != "" || len(m.timeline) != total {
		t.Fatalf("esc should clear filter: query=%q len=%d", m.query, len(m.timeline))
	}
}

func TestStreamQueryFilter(t *testing.T) {
	st := store.New()
	seed(st) // id1 initialize, id2 tools/call echo (ok)
	// a tool-level error call
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add"}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"not found"}],"isError":true}}`))
	// a stray non-JSON-RPC frame on the protocol channel (stdout corruption)
	st.Ingest(env(7, proxy.ServerToClient, `{"note":"stray line"}`))
	// a best-effort JSON-RPC validation warning, method but no jsonrpc marker.
	st.Ingest(env(8, proxy.ClientToServer, `{"id":4,"method":"tools/list"}`))

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	total := len(m.timeline)

	apply := func(q string) Model {
		mm := drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
		mm = drive(t, mm, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(q)})
		return drive(t, mm, tea.KeyMsg{Type: tea.KeyEnter})
	}

	// status:err → only the failed call's frames.
	fe := apply("status:err")
	if len(fe.timeline) == 0 || len(fe.timeline) >= total {
		t.Fatalf("status:err should narrow: %d of %d", len(fe.timeline), total)
	}
	for _, e := range fe.timeline {
		if e.Call == nil || !e.Call.Failed() {
			t.Fatalf("status:err returned a non-error frame: %+v", e)
		}
	}

	// tool:add → only the add tool frames.
	ft := apply("tool:add")
	if len(ft.timeline) == 0 {
		t.Fatal("tool:add should match the add call")
	}
	for _, e := range ft.timeline {
		if e.Call == nil || e.Call.ToolName != "add" {
			t.Fatalf("tool:add returned wrong frame: %+v", e)
		}
	}

	// kind:invalid and status:bad → only the stray non-JSON-RPC frame.
	for _, q := range []string{"kind:invalid", "status:bad"} {
		fb := apply(q)
		if len(fb.timeline) != 1 {
			t.Fatalf("%s should match exactly the invalid frame, got %d of %d", q, len(fb.timeline), total)
		}
		if fb.timeline[0].Kind != store.EventInvalid {
			t.Fatalf("%s returned a non-invalid frame: %+v", q, fb.timeline[0])
		}
	}

	fw := apply("status:warn")
	if len(fw.timeline) != 1 || fw.timeline[0].Warning == "" {
		t.Fatalf("status:warn should match exactly the warning frame, got %+v", fw.timeline)
	}
}

func TestStreamFilterFindsTruncatedUnderWarn(t *testing.T) {
	st := store.New()
	seed(st) // clean calls, no warnings
	trunc := env(5, proxy.ServerToClient, `{"jsonrpc":"2.0","result":{}}`)
	trunc.Truncated = true
	st.Ingest(trunc)

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the stream
	total := len(m.timeline)

	mm := drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	mm = drive(t, mm, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("status:warn")})
	fw := drive(t, mm, tea.KeyMsg{Type: tea.KeyEnter})

	if len(fw.timeline) != 1 || total <= 1 {
		t.Fatalf("status:warn should find exactly the truncated frame, got %d of %d", len(fw.timeline), total)
	}
	if !fw.timeline[0].Truncated {
		t.Fatalf("status:warn matched a non-truncated frame: %+v", fw.timeline[0])
	}
}

func TestCountStreamSignalsCountsTruncatedAsWarn(t *testing.T) {
	events := []store.EventView{
		{Kind: store.EventOther, Truncated: true},
		{Kind: store.EventOther}, // neither a warning nor truncated
	}
	if c := countStreamSignals(events); c.warn != 1 {
		t.Fatalf("a truncated frame should count as one warn, got %d", c.warn)
	}
}

func TestStatusRankTruncatedRanksAsWarn(t *testing.T) {
	if r := statusRank(store.EventView{Kind: store.EventOther, Truncated: true}); r != 3 {
		t.Fatalf("a truncated frame should rank 3 like a warning, got %d", r)
	}
}

func TestStreamFilterMismatch(t *testing.T) {
	st := store.New()
	seed(st) // two clean calls
	// A frame whose routing header disagrees with the body (Mcp-Name shadowing).
	shadow := env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dangerous"}}`)
	shadow.MCPMethod, shadow.MCPName, shadow.Transport = "tools/call", "safe", "http"
	st.Ingest(shadow)

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	total := len(m.timeline)

	mm := drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	mm = drive(t, mm, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("status:mismatch")})
	fm := drive(t, mm, tea.KeyMsg{Type: tea.KeyEnter})

	if len(fm.timeline) != 1 || total <= 1 {
		t.Fatalf("status:mismatch should match exactly the shadowing frame, got %d of %d", len(fm.timeline), total)
	}
	if !fm.timeline[0].RoutingMismatch {
		t.Fatalf("status:mismatch matched a frame without the flag: %+v", fm.timeline[0])
	}
}

func TestStreamFooterShowsSignalCounts(t *testing.T) {
	st := store.New()
	seed(st)
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fail"}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"unknown tool"}}`))
	st.Ingest(env(7, proxy.ServerToClient, `{"note":"stray line"}`))
	st.Ingest(env(8, proxy.ClientToServer, `{"id":4,"method":"tools/list"}`))

	// A 200ms call is just a normal completed call now, never a "slow" signal.
	t0 := time.Now()
	longReq := env(9, proxy.ClientToServer, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"search"}}`)
	longReq.TS = t0
	st.Ingest(longReq)
	longResp := env(10, proxy.ServerToClient, `{"jsonrpc":"2.0","id":5,"result":{"content":[]}}`)
	longResp.TS = t0.Add(200 * time.Millisecond)
	st.Ingest(longResp)

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	out := m.View()
	for _, want := range []string{"10 frames", "1 err", "1 bad", "1 warn"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream footer missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "slow") {
		t.Fatalf("the slow verdict should be gone\n%s", out)
	}
}

func TestStreamFooterCountsSpanWholeSessionUnderFilter(t *testing.T) {
	st := store.New()
	seed(st) // id2 is a tools/call to echo that succeeds
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fail"}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"unknown tool"}}`))

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	total := len(m.timeline)

	// Filter to the echo tool, which hides the failed call's frames from the view.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tool:echo")})
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.timeline) >= total {
		t.Fatalf("filter should hide the error frames, got %d of %d", len(m.timeline), total)
	}

	out := m.View()
	// Under a filter the footer shows the matched-over-total fraction.
	if !strings.Contains(out, "2/6 frames") {
		t.Fatalf("footer should show the filtered fraction\n%s", out)
	}
	// The error is filtered out of the view, but the footer still counts it,
	// because session health should not depend on the active filter.
	if !strings.Contains(out, "1 err") {
		t.Fatalf("footer should count the error across the whole session under a filter\n%s", out)
	}
}

func TestCountLabel(t *testing.T) {
	cases := []struct {
		shown, total int
		noun, want   string
	}{
		{5, 5, "frame", "5 frames"},
		{1, 1, "frame", "1 frame"},
		{0, 0, "frame", "0 frames"},
		{2, 6, "frame", "2/6 frames"},
		{0, 1, "frame", "0/1 frame"},
		{1, 1, "session", "1 session"},
		{3, 10, "session", "3/10 sessions"},
	}
	for _, c := range cases {
		if got := countLabel(c.shown, c.total, c.noun); got != c.want {
			t.Errorf("countLabel(%d, %d, %q) = %q, want %q", c.shown, c.total, c.noun, got, c.want)
		}
	}
}

// TestStatusRankInvalid checks that sorting by status surfaces invalid frames,
// stream corruption ranks above call errors, then protocol warnings.
func TestStatusRankInvalid(t *testing.T) {
	invalid := statusRank(store.EventView{Kind: store.EventInvalid})
	errored := statusRank(store.EventView{Kind: store.EventResponse, Call: &store.CallView{Err: &proxy.RPCError{}}})
	warned := statusRank(store.EventView{Kind: store.EventRequest, Warning: "missing jsonrpc=2.0"})
	none := statusRank(store.EventView{Kind: store.EventStderr})
	if !(invalid > errored && errored > warned && warned > none) {
		t.Fatalf("statusRank order wrong: invalid=%d error=%d warning=%d none=%d", invalid, errored, warned, none)
	}
}

func TestSessionFilterAndCommandJump(t *testing.T) {
	st := store.New()
	seed(st) // demo
	st.Ingest(sessionEnv("s2", "search-api"))
	st.Ingest(sessionEnv("s3", "github"))
	m := ready(t, st)

	// Session filter narrows the list.
	m = typeRunes(t, m, "/")
	m = typeRunes(t, m, "hub")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.sessions) != 1 || m.sessions[0].Label != "github" {
		t.Fatalf("session filter 'hub' should leave only github, got %d", len(m.sessions))
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc}) // clear

	// Command-mode jump by name.
	m = typeRunes(t, m, ":")
	if m.inputMode != inputCommand {
		t.Fatal(": should open command input")
	}
	m = typeRunes(t, m, "search")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.view != viewStream || m.streamLabel != "search-api" {
		t.Fatalf(": jump should open search-api stream, got view=%v label=%q", m.view, m.streamLabel)
	}
	// ":sessions" returns to the list.
	m = typeRunes(t, m, ":")
	m = typeRunes(t, m, "sessions")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.view != viewSessions {
		t.Fatal(":sessions should return to the sessions table")
	}
}

func TestCapsContentShowsDeclaredCapabilities(t *testing.T) {
	st := store.New()
	// The client declares roots; the server declares tools (with a listChanged
	// sub-flag) plus an experimental capability outside the known set.
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli"},"capabilities":{"roots":{}}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true},"experimental":{}},"serverInfo":{"name":"demo","version":"1.0.0"}}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatal("c should open capabilities")
	}
	// overlayRaw is the full unwrapped caps body, so a bottom section is never
	// lost below the viewport fold the way it could be in View().
	out := m.overlayRaw
	// Title, both implementation rows, declared caps (●), known absent caps (○),
	// and a declared cap outside the standard set are all present.
	for _, want := range []string{"capabilities", "protocol", "2025-06-18", "client", "cli", "server", "demo", "1.0.0", "●", "○", "roots", "sampling", "tools", "resources", "experimental"} {
		if !strings.Contains(out, want) {
			t.Fatalf("caps body missing %q\n%s", want, out)
		}
	}
	// The rebuilt screen shows only the marker: no per-row status text, no
	// sub-flag detail, and no tool-usage block.
	for _, absent := range []string{"not offered", "not negotiated", "listChanged", "unused", "unadvertised", "✓"} {
		if strings.Contains(out, absent) {
			t.Fatalf("caps body should not contain %q\n%s", absent, out)
		}
	}
}

func TestCapsContentStatelessModel(t *testing.T) {
	st := store.New()
	// No initialize handshake (removed in 2026-07-28). The client declares itself in
	// a request's _meta and the server answers server/discover, yet the inspector
	// must populate exactly as it did for the legacy handshake.
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"ExampleClient","version":"1.0"},"io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"instructions":"Prefer the search tool.","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"ExampleServer","version":"2.0"}}}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatal("c should open capabilities")
	}
	out := m.overlayRaw
	for _, want := range []string{"capabilities", "protocol", "2026-07-28", "ExampleClient", "ExampleServer", "● elicitation", "● tools", "instructions", "Prefer the search tool."} {
		if !strings.Contains(out, want) {
			t.Fatalf("stateless caps body missing %q\n%s", want, out)
		}
	}
}

func TestCapsOverlayUpdatesLive(t *testing.T) {
	st := store.New()
	// Only the client half of the handshake so far, so the server is still unknown.
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli"},"capabilities":{"roots":{}}}}`))
	m := ready(t, st)
	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatal("c should open capabilities")
	}
	// The session label is "demo", so assert on the distinct server impl name and
	// the tools marker, both absent until the server's response is seen.
	if strings.Contains(m.overlayRaw, "srv-impl") || strings.Contains(m.overlayRaw, "● tools") {
		t.Fatalf("server should read unknown before its initialize response\n%s", m.overlayRaw)
	}

	// The server's initialize response arrives while the overlay stays open.
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"srv-impl"}}}`))
	m = drive(t, m, frameMsg{})
	if m.overlay != overlayCaps {
		t.Fatal("a live frame must not close the overlay")
	}
	// The live overlay refreshes on the tick, not per frame, so advance one tick.
	m = drive(t, m, tickMsg(time.Now()))
	if !strings.Contains(m.overlayRaw, "srv-impl") || !strings.Contains(m.overlayRaw, "● tools") {
		t.Fatalf("caps overlay did not pick up the server handshake live\n%s", m.overlayRaw)
	}
}

func TestCapabilitiesAndHelp(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)

	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatal("c should open capabilities")
	}
	out := m.View()
	for _, want := range []string{"capabilities", "protocol", "client", "server"} {
		if !strings.Contains(out, want) {
			t.Fatalf("caps missing %q\n%s", want, out)
		}
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	m = typeRunes(t, m, "?")
	if !m.showHelp || !strings.Contains(m.View(), "KEYBINDINGS") {
		t.Fatalf("? should show help:\n%s", m.View())
	}
	m = typeRunes(t, m, "?")
	if m.showHelp {
		t.Fatal("? should toggle help off")
	}
}

func TestInspectorModalSizesToContent(t *testing.T) {
	st := store.New()
	seed(st)                                        // short frames
	m := ready(t, st)                               // 160x44
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on a short frame

	// The viewport shrinks to the payload instead of filling the tall screen.
	if m.vp.Height >= m.height-8 {
		t.Fatalf("short frame should shrink the viewport to its content, got %d in %d rows", m.vp.Height, m.height)
	}
	// The modal is centered, so the box does not start at the very top.
	lines := strings.Split(m.View(), "\n")
	top := -1
	for i, ln := range lines {
		if strings.Contains(ln, "╭") {
			top = i
			break
		}
	}
	if top < 2 {
		t.Fatalf("centered modal should have blank margin above, box starts at line %d", top)
	}
}

func TestPairJump(t *testing.T) {
	st := store.New()
	seed(st) // includes a tools/call echo request and its response
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on the selected frame
	if m.overlay != overlayInspector {
		t.Fatalf("enter should open the inspector, got overlay %d", m.overlay)
	}
	before := m.inspect
	m = typeRunes(t, m, "x") // jump to the paired frame, still a full-width inspector
	if m.overlay != overlayInspector {
		t.Fatalf("x should stay in the inspector, got overlay %d", m.overlay)
	}
	if m.inspect == before {
		t.Fatal("x should move the inspector to the paired frame")
	}
	// A refresh under follow must not disturb the inspected frame.
	jumped := m.inspect
	if !m.follow {
		t.Fatal("this test needs follow on to cover the regression")
	}
	for range refreshEvery {
		m = drive(t, m, tickMsg(time.Now()))
	}
	if m.inspect != jumped {
		t.Fatalf("follow refresh moved the inspected frame, inspect %d want %d", m.inspect, jumped)
	}
	m = typeRunes(t, m, "x") // and back again
	if m.inspect != before {
		t.Fatalf("x again should jump back to the original frame, got %d want %d", m.inspect, before)
	}
}

func TestReplayFromInspector(t *testing.T) {
	st := store.New()
	seed(st) // echo request (id2) + response
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream (follow -> last = a response)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector
	m = typeRunes(t, m, "x")                        // jump to the paired request
	if m.full[m.inspect].Kind != store.EventRequest {
		t.Fatalf("x should land on the request, got kind %v", m.full[m.inspect].Kind)
	}
	// r replays the inspected request. The seeded session has no recorded command,
	// so it flashes the no-command note rather than the needs-a-request one, which
	// proves r is wired and acted on the inspected request frame.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.overlay != overlayInspector {
		t.Fatalf("r without a command should stay in the inspector, got overlay %d", m.overlay)
	}
	if !m.flashActive() || !strings.Contains(m.flash, "no recorded server command") {
		t.Fatalf("r on the inspected request should flash the no-command note, got flash=%q", m.flash)
	}
}

func TestInspectorFooterConditionalKeys(t *testing.T) {
	st := store.New()
	meta, _ := json.Marshal(proxy.SessionMeta{Command: []string{"true"}, CWD: "/tmp"})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 0, TS: time.Now(), Direction: proxy.DirectionMeta, Raw: meta})
	seed(st) // now the session has a recorded command
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on the last frame (a response)

	// A response frame has a pair (offer x) but is not a request (hide r).
	out := m.View()
	if !strings.Contains(out, "pair") {
		t.Fatalf("a paired frame should offer x pair:\n%s", out)
	}
	if strings.Contains(out, "replay") {
		t.Fatalf("a response frame should not offer r replay:\n%s", out)
	}

	// Jump to the request: replay is now offered (a request with a command).
	m = typeRunes(t, m, "x")
	out = m.View()
	if !strings.Contains(out, "replay") {
		t.Fatalf("a replayable request should offer r replay:\n%s", out)
	}
	if !strings.Contains(out, "pair") {
		t.Fatalf("the request should still offer x pair:\n%s", out)
	}
}

func TestReplayAgainFromResult(t *testing.T) {
	st := store.New()
	meta, _ := json.Marshal(proxy.SessionMeta{Command: []string{"true"}, CWD: "/tmp"})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 0, TS: time.Now(), Direction: proxy.DirectionMeta, Raw: meta})
	seed(st) // request/response frames on the same session, now with a command
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on the last frame (a response)
	m = typeRunes(t, m, "x")                        // jump to the request
	m = typeRunes(t, m, "r")                        // start the replay

	// r starts an async replay shown as a footer spinner, not a placeholder window.
	if !m.replaying {
		t.Fatal("r should start a replay, not open a placeholder window")
	}
	if m.overlay == overlayReplay {
		t.Fatal("the replay overlay should wait for the result, not open on a spinner")
	}
	if !strings.Contains(m.View(), "replaying") {
		t.Fatalf("a footer spinner should show while replaying:\n%s", m.View())
	}

	// The result lands and opens the replay overlay.
	m = drive(t, m, replayDoneMsg{res: replay.Result{Method: "get_sum", Response: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)}})
	if m.overlay != overlayReplay || m.replaying {
		t.Fatalf("the result should open the replay overlay, overlay=%d replaying=%v", m.overlay, m.replaying)
	}
	if m.replayReq.Method == "" {
		t.Fatal("replay should remember the request so it can be re-run")
	}
	if !strings.Contains(m.View(), "replay again") {
		t.Fatalf("the replay overlay footer should offer replay again:\n%s", m.View())
	}

	// r straight from the result re-runs the same replay, no esc needed.
	before := m.replayReq.Method
	m = typeRunes(t, m, "r")
	if !m.replaying || m.replayReq.Method != before {
		t.Fatalf("r in the replay overlay should re-run the same replay, replaying=%v method=%q", m.replaying, m.replayReq.Method)
	}
}

func TestReplayAbandonedOnNavigation(t *testing.T) {
	st := store.New()
	meta, _ := json.Marshal(proxy.SessionMeta{Command: []string{"true"}, CWD: "/tmp"})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 0, TS: time.Now(), Direction: proxy.DirectionMeta, Raw: meta})
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on the response
	m = typeRunes(t, m, "x")                        // to the request
	m = typeRunes(t, m, "r")                        // start replaying
	if !m.replaying {
		t.Fatal("r should start replaying")
	}

	// Leaving the inspector abandons the in-flight replay, so its late result
	// does not pop an overlay over whatever the user moved on to.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.replaying {
		t.Fatal("closing the overlay should abandon the in-flight replay")
	}
	m = drive(t, m, replayDoneMsg{res: replay.Result{Method: "x", Response: json.RawMessage("{}")}})
	if m.overlay == overlayReplay {
		t.Fatal("an abandoned replay result should not open an overlay")
	}
}

func TestStreamFooterReplayGatedOnCommand(t *testing.T) {
	// Without a recorded server command a session can never replay, so the stream
	// footer hides r, matching how the inspector already gates it.
	noCmd := store.New()
	seed(noCmd) // no meta frame
	m := ready(t, noCmd)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream
	if strings.Contains(m.View(), "replay") {
		t.Fatalf("a session with no recorded command should not offer r replay:\n%s", m.View())
	}

	// With a command, the footer offers r replay.
	withCmd := store.New()
	meta, _ := json.Marshal(proxy.SessionMeta{Command: []string{"true"}, CWD: "/tmp"})
	withCmd.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 0, TS: time.Now(), Direction: proxy.DirectionMeta, Raw: meta})
	seed(withCmd)
	m2 := ready(t, withCmd)
	m2 = drive(t, m2, tea.KeyMsg{Type: tea.KeyEnter}) // stream
	if !strings.Contains(m2.View(), "replay") {
		t.Fatalf("a session with a recorded command should offer r replay:\n%s", m2.View())
	}
}

func TestPairJumpReachesFilteredOutPair(t *testing.T) {
	st := store.New()
	seed(st) // echo request (id2) + its response
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream

	// Filter to requests only, so responses are hidden from the table.
	m = typeRunes(t, m, "/")
	m = typeRunes(t, m, "kind:req")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, e := range m.timeline {
		if e.Kind == store.EventResponse {
			t.Fatal("kind:req filter should hide responses from the table")
		}
	}

	// Inspect the echo request, then x must still reach its response even though
	// the response is filtered out of the table.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on a request
	if m.full[m.inspect].Kind != store.EventRequest {
		t.Fatalf("expected to inspect a request, got kind %v", m.full[m.inspect].Kind)
	}
	m = typeRunes(t, m, "x")
	if m.full[m.inspect].Kind != store.EventResponse {
		t.Fatalf("x should jump to the filtered-out response, landed on kind %v", m.full[m.inspect].Kind)
	}
}

func TestStreamStatsAndActivity(t *testing.T) {
	st := store.New()
	seed(st) // initialize + tools/call, both completed, timestamped now
	m := ready(t, st)

	sv := m.View()
	if !strings.Contains(sv, "ACTIVITY") {
		t.Fatalf("sessions header missing the ACTIVITY column\n%s", sv)
	}
	if !strings.ContainsAny(sv, string(sparkRamp)) {
		t.Fatalf("sessions view missing a sparkline glyph\n%s", sv)
	}

	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	if m.streamCalls == 0 {
		t.Fatal("seed has completed calls, streamCalls should be > 0")
	}
	if got := m.View(); !strings.Contains(got, "p50 ") || !strings.Contains(got, "p95 ") {
		t.Fatalf("stream header missing p50/p95\n%s", got)
	}
}

func TestResizeFuzzNoPanic(t *testing.T) {
	st := store.New()
	seed(st)
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x"}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"result":{}}`))
	st.Ingest(sessionEnv("s2", "search-api"))
	base := New(st)
	base = drive(t, base, frameMsg{})

	sizes := [][2]int{{120, 36}, {80, 24}, {1, 1}, {0, 0}, {200, 60}, {40, 10}, {99, 24}, {89, 24}, {70, 24}, {2, 60}}
	// Exercise every screen, then hammer each with a spread of window sizes.
	openers := map[string][]tea.Msg{
		"sessions":  {tea.WindowSizeMsg{Width: 100, Height: 24}},
		"stream":    {tea.WindowSizeMsg{Width: 100, Height: 24}, tea.KeyMsg{Type: tea.KeyEnter}},
		"inspector": {tea.WindowSizeMsg{Width: 100, Height: 24}, tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyEnter}},
		"pairjump":  {tea.WindowSizeMsg{Width: 100, Height: 24}, tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}},
		"caps":      {tea.WindowSizeMsg{Width: 100, Height: 24}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")}},
		"help":      {tea.WindowSizeMsg{Width: 100, Height: 24}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")}},
		"confirm":   {tea.WindowSizeMsg{Width: 100, Height: 24}, tea.KeyMsg{Type: tea.KeyCtrlD}},
	}
	for name, msgs := range openers {
		m := base
		for _, msg := range msgs {
			m = drive(t, m, msg)
		}
		for _, sz := range sizes {
			m = drive(t, m, tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			if got := m.View(); got == "" && sz[0] > 0 {
				t.Fatalf("%s at %dx%d rendered empty", name, sz[0], sz[1])
			}
		}
	}
}

func TestSwitchSessionWithBrackets(t *testing.T) {
	st := store.New()
	seed(st) // demo
	st.Ingest(sessionEnv("s2", "search-api"))
	st.Ingest(sessionEnv("s3", "github"))
	m := ready(t, st)

	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the first session's stream
	if m.view != viewStream {
		t.Fatal("enter should open a stream")
	}
	first := m.streamSessionID

	m = typeRunes(t, m, "]") // next session, still in the stream
	if m.view != viewStream {
		t.Fatal("] should keep us in the stream view")
	}
	if m.streamSessionID == first {
		t.Fatal("] should switch to a different session")
	}

	m = typeRunes(t, m, "[") // back to the first
	if m.streamSessionID != first {
		t.Fatalf("[ should return to the first session, got %s", m.streamLabel)
	}
}

func TestFormatLatency(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "-"},
		{250 * time.Microsecond, "250µs"},
		{1500 * time.Microsecond, "1.5ms"},
		{25300 * time.Microsecond, "25.3ms"},
		{2500 * time.Millisecond, "2.5s"},
		{1234567 * time.Microsecond, "1.23s"}, // stays compact, not 1.234567s
	} {
		if got := formatLatency(c.d); got != c.want {
			t.Errorf("formatLatency(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestToolSummaryOverlay(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)

	m = typeRunes(t, m, "s")
	if m.overlay != overlaySummary {
		t.Fatal("s should open the tool summary")
	}
	out := m.View()
	for _, want := range []string{"tool summary", "echo", "CALLS", "ERR", "LATENCY"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q\n%s", want, out)
		}
	}
	for _, absent := range []string{"P50", "P95", "P99", "SLOWEST CALLS"} {
		if strings.Contains(out, absent) {
			t.Fatalf("summary should no longer contain %q\n%s", absent, out)
		}
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.overlay != overlayNone {
		t.Fatal("esc should close the summary")
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeRunes(t, m, "s")
	if m.view != viewStream || m.overlay != overlaySummary {
		t.Fatal("s should also open the summary from the stream")
	}
}

func TestToolSummaryListsEveryAdvertisedTool(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli"}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"demo"}}}`))
	// The server advertises echo, ping, and search.
	st.Ingest(env(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	st.Ingest(env(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo"},{"name":"ping"},{"name":"search"}]}}`))
	// echo is called; ping and search never are; ghost is called though it was
	// never advertised (drift).
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`))
	st.Ingest(env(7, proxy.ClientToServer, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ghost"}}`))
	st.Ingest(env(8, proxy.ServerToClient, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "s")
	if m.overlay != overlaySummary {
		t.Fatal("s should open the tool summary")
	}
	// overlayRaw is the full unwrapped body, so no row is lost below the fold.
	out := m.overlayRaw
	// Every advertised tool is a table row, called (echo) or not (ping, search),
	// and ghost shows too, flagged as drift.
	for _, want := range []string{"echo", "ping", "search", "ghost", "undeclared"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q\n%s", want, out)
		}
	}
	// The coverage and unused jargon lines are gone.
	for _, absent := range []string{"coverage", "unused", "advertised tools called"} {
		if strings.Contains(out, absent) {
			t.Fatalf("summary should no longer contain %q\n%s", absent, out)
		}
	}
	// An advertised-but-uncalled tool renders a 0-call row.
	if !summaryHasRow(out, "ping", "0") {
		t.Fatalf("ping should be a 0-call row\n%s", out)
	}
}

func TestDefinitionDriftIsVisibleInSessionsAndToolSummary(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search"}]}}`))
	st.SetToolDrift("s1", store.ToolDrift{
		ChangedDescriptions: []string{"search"},
		AddedTools:          []string{"write"},
	})

	m := ready(t, st)
	// The sessions row carries the compact "!" marker (drift is warn-colored); the
	// full "tool definition drift" wording lives in the summary overlay below.
	if out := ansi.Strip(m.View()); !strings.Contains(out, "! demo") {
		t.Fatalf("sessions view did not surface drift\n%s", out)
	}
	m = typeRunes(t, m, "s")
	out := ansi.Strip(m.overlayRaw)
	for _, want := range []string{"tool definition drift", "description changed", "search", "added", "write"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q\n%s", want, out)
		}
	}
}

func TestToolBaselineErrorsAreVisible(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	st.SetToolDrift("s1", store.ToolDrift{BaselineError: "baseline file is invalid"})

	m := ready(t, st)
	// The compact "!" marker is respErr-colored for a baseline error; the full
	// "tool baseline error" wording lives in the summary overlay below.
	if out := ansi.Strip(m.View()); !strings.Contains(out, "! demo") {
		t.Fatalf("sessions view did not surface the baseline error\n%s", out)
	}
	m = typeRunes(t, m, "s")
	out := ansi.Strip(m.overlayRaw)
	for _, want := range []string{"tool baseline error", "baseline file is invalid"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q\n%s", want, out)
		}
	}
}

// summaryHasRow reports whether a stripped summary line starts with name and
// contains cell somewhere after it.
func summaryHasRow(styled, name, cell string) bool {
	for _, ln := range strings.Split(ansi.Strip(styled), "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, name+" ") && strings.Contains(trimmed, cell) {
			return true
		}
	}
	return false
}

func TestSummaryHeaderShowsOnlyCallsAndSort(t *testing.T) {
	st := store.New()
	base := time.Now()
	st.Ingest(envAt(1, proxy.ClientToServer, base, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"good"}}`))
	st.Ingest(envAt(2, proxy.ServerToClient, base.Add(time.Millisecond), `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	st.Ingest(envAt(3, proxy.ClientToServer, base.Add(2*time.Millisecond), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bad"}}`))
	st.Ingest(envAt(4, proxy.ServerToClient, base.Add(3*time.Millisecond), `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[]}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "s")
	out := ansi.Strip(m.overlayRaw)

	// The header stat is only the call total, no err/slow/pending breakdown.
	header := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(header, "2 calls") {
		t.Fatalf("header should total the calls: %q", header)
	}
	for _, banned := range []string{"err", "slow", "pending"} {
		if strings.Contains(header, banned) {
			t.Fatalf("header should show only calls, found %q: %q", banned, header)
		}
	}
	// The clean tool shows · for zero errors; the erroring tool shows a count.
	if !summaryHasRow(out, "good", "·") || summaryHasRow(out, "bad", "·") {
		t.Fatalf("ERR column should show · only for the clean tool\n%s", out)
	}
	// The erroring tool sorts above the clean one, not alphabetically.
	if strings.Index(out, "bad") > strings.Index(out, "good") {
		t.Fatalf("erroring tool should sort first\n%s", out)
	}
}

func TestSummaryUpdatesLive(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	m := ready(t, st)
	m = typeRunes(t, m, "s")
	if m.overlay != overlaySummary {
		t.Fatal("s should open the summary")
	}
	if strings.Contains(m.overlayRaw, "search") {
		t.Fatalf("search should not appear before it is called\n%s", m.overlayRaw)
	}

	// A new tool call arrives while the summary stays open.
	st.Ingest(env(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`))
	st.Ingest(env(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`))
	m = drive(t, m, frameMsg{})
	if m.overlay != overlaySummary {
		t.Fatal("a live frame must not close the summary")
	}
	// The live overlay refreshes on the tick, not per frame, so advance one tick.
	m = drive(t, m, tickMsg(time.Now()))
	if !strings.Contains(m.overlayRaw, "search") {
		t.Fatalf("summary did not pick up the new call live\n%s", m.overlayRaw)
	}
}

func TestSummaryPendingToolShowsSpinner(t *testing.T) {
	st := store.New()
	// A tool call requested but never answered: pending, with no latency yet.
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hang"}}`))
	m := ready(t, st)
	m = typeRunes(t, m, "s")
	if m.overlay != overlaySummary {
		t.Fatal("s should open the summary")
	}

	var row string
	for _, ln := range strings.Split(ansi.Strip(m.overlayRaw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "hang ") {
			row = strings.TrimSpace(ln)
			break
		}
	}
	if row == "" {
		t.Fatalf("hang row missing\n%s", ansi.Strip(m.overlayRaw))
	}
	// LATENCY is a spinner frame, not a dash, so the pending tool is obvious.
	if strings.Contains(row, "-") || !strings.ContainsAny(row, string(spinnerFrames)) {
		t.Fatalf("pending tool LATENCY should be a spinner, not a dash: %q", row)
	}

	// The spinner animates on the shared tick clock.
	before := m.overlayRaw
	m = drive(t, m, tickMsg(time.Now()))
	if m.overlayRaw == before {
		t.Fatal("a tick should advance the pending spinner")
	}
}

func TestSortSessions(t *testing.T) {
	st := store.New()
	st.Ingest(sessionEnv("s1", "gamma"))
	st.Ingest(sessionEnv("s2", "alpha"))
	st.Ingest(sessionEnv("s3", "beta"))
	m := ready(t, st)

	// shift+N sorts by name ascending.
	m = typeRunes(t, m, "N")
	if got := []string{m.sessions[0].Label, m.sessions[1].Label, m.sessions[2].Label}; got[0] != "alpha" || got[2] != "gamma" {
		t.Fatalf("shift+N asc = %v, want alpha..gamma", got)
	}
	// shift+N again flips to descending.
	m = typeRunes(t, m, "N")
	if m.sessions[0].Label != "gamma" {
		t.Fatalf("shift+N twice should be desc, got %s first", m.sessions[0].Label)
	}
}

func TestWrapAroundNavigation(t *testing.T) {
	st := store.New()
	seed(st) // demo
	st.Ingest(sessionEnv("s2", "search-api"))
	st.Ingest(sessionEnv("s3", "github"))
	m := ready(t, st) // 3 sessions, selSession=0

	// k (up) at the top wraps to the bottom.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.selSession != 2 {
		t.Fatalf("up at top should wrap to last, got %d", m.selSession)
	}
	// j (down) at the bottom wraps to the top.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.selSession != 0 {
		t.Fatalf("down at bottom should wrap to first, got %d", m.selSession)
	}
}

func TestOnboardingEmptyState(t *testing.T) {
	m := ready(t, store.New())
	out := m.View()
	for _, want := range []string{"waiting for MCP traffic", "mcpsnoop", "--"} {
		if !strings.Contains(out, want) {
			t.Fatalf("onboarding missing %q\n%s", want, out)
		}
	}
}

func TestPendingCallShown(t *testing.T) {
	st := store.New()
	// A request with no response yet → in-flight.
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search"}}`))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // drill into the stream
	if !strings.Contains(m.View(), "pending") {
		t.Fatalf("a pending request should show pending:\n%s", m.View())
	}
}

func TestDeleteSession(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	st := store.New()
	seed(st)
	st.Ingest(sessionEnv("s2", "search-api"))
	m := ready(t, st)
	if len(m.sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(m.sessions))
	}
	// ctrl-d deletes the selected session immediately, no confirmation.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if len(m.sessions) != 1 {
		t.Fatalf("ctrl-d should remove the selected session, got %d", len(m.sessions))
	}
	if !m.flashActive() || !strings.Contains(m.flash, "deleted") {
		t.Fatalf("delete should flash which session went, got flash=%q", m.flash)
	}
}

func TestFlashClearsOnNavigation(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the stream

	// A flash from an action in the stream is dismissed by opening the inspector.
	m.setFlash("✓ exported foo.html")
	if !m.flashActive() {
		t.Fatal("flash should be active before navigating")
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector
	if m.overlay != overlayInspector {
		t.Fatal("enter should open the inspector")
	}
	if m.flashActive() {
		t.Fatalf("opening the inspector should clear the stale flash, got %q", m.flash)
	}

	// esc back out clears any flash too, so nothing bleeds into the stream footer.
	m.setFlash("✓ copied frame JSON")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc}) // close inspector
	if m.overlay != overlayNone || m.flashActive() {
		t.Fatalf("closing the overlay should clear the flash, overlay=%v flash=%q", m.overlay, m.flash)
	}
}

func TestDeleteFlashSurvivesViewChange(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	st := store.New()
	seed(st)
	st.Ingest(sessionEnv("s2", "other"))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the streamed session

	// Deleting the streamed session drops back to the sessions list but keeps its
	// own flash, since delete does not route through the navigation helpers.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.view != viewSessions {
		t.Fatal("deleting the streamed session should return to the sessions list")
	}
	if !m.flashActive() || !strings.Contains(m.flash, "deleted") {
		t.Fatalf("the delete flash should survive the view change, got %q", m.flash)
	}
}

func TestOverlaySearch(t *testing.T) {
	st := store.New()
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspect the selected frame
	if m.overlay != overlayInspector {
		t.Fatal("expected inspector overlay")
	}

	// "/" inside the overlay starts an in-frame search.
	m = typeRunes(t, m, "/")
	if m.inputMode != inputSearch {
		t.Fatal("/ in an overlay should start in-frame search")
	}
	m = typeRunes(t, m, "jsonrpc")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.overlaySearch != "jsonrpc" || len(m.overlayMatches) == 0 {
		t.Fatalf("search should find matches: q=%q matches=%v", m.overlaySearch, m.overlayMatches)
	}

	// esc clears the search but keeps the overlay open.
	m = typeRunes(t, m, "/")
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.overlaySearch != "" || m.overlay != overlayInspector {
		t.Fatalf("esc should clear search, keep overlay: search=%q overlay=%v", m.overlaySearch, m.overlay)
	}
}

func TestReplayGuards(t *testing.T) {
	st := store.New()
	seed(st) // no meta frame → command unknown
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream, follow → last frame (a response)

	// r on a response frame flashes a hint instead of opening an error overlay.
	m = typeRunes(t, m, "r")
	if m.overlay != overlayNone {
		t.Fatalf("replay on a response should not open an overlay, got %v", m.overlay)
	}
	if !m.flashActive() || !strings.Contains(m.flash, "request frame") {
		t.Fatalf("replay on a response should flash a hint, got flash=%q", m.flash)
	}

	// r on a request in a session with no recorded command flashes as well.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyUp}) // to the request frame
	m = typeRunes(t, m, "r")
	if m.overlay != overlayNone {
		t.Fatalf("replay without a command should not open an overlay, got %v", m.overlay)
	}
	if !m.flashActive() || !strings.Contains(m.flash, "no recorded server command") {
		t.Fatalf("replay without a recorded command should flash, got flash=%q", m.flash)
	}
}
