package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func env(seq uint64, dir proxy.Direction, raw string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: time.Now(),
		Direction: dir, Transport: "stdio", Raw: json.RawMessage(raw),
	}
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

func TestSessionsTableAndDrillIn(t *testing.T) {
	st := store.New(0)
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

func TestInspectorOverlay(t *testing.T) {
	st := store.New(0)
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
	st := store.New(0)
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
	st := store.New(0)
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
	st := store.New(0)
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

func TestStreamFooterShowsSignalCounts(t *testing.T) {
	st := store.New(100 * time.Millisecond)
	seed(st)
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fail"}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"unknown tool"}}`))
	st.Ingest(env(7, proxy.ServerToClient, `{"note":"stray line"}`))
	st.Ingest(env(8, proxy.ClientToServer, `{"id":4,"method":"tools/list"}`))

	t0 := time.Now()
	slowReq := env(9, proxy.ClientToServer, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"slow"}}`)
	slowReq.TS = t0
	st.Ingest(slowReq)
	slowResp := env(10, proxy.ServerToClient, `{"jsonrpc":"2.0","id":5,"result":{"content":[]}}`)
	slowResp.TS = t0.Add(200 * time.Millisecond)
	st.Ingest(slowResp)

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream
	out := m.View()
	for _, want := range []string{"10 frames", "1 err", "1 bad", "1 warn", "1 slow"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream footer missing %q\n%s", want, out)
		}
	}
}

func TestStreamFooterCountsSpanWholeSessionUnderFilter(t *testing.T) {
	st := store.New(0)
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
	st := store.New(0)
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

func TestCapsContentShowsToolUsage(t *testing.T) {
	st := store.New(0)
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli"}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"demo"}}}`))
	// The server advertises three tools.
	st.Ingest(env(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	st.Ingest(env(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo"},{"name":"sum"},{"name":"search"}]}}`))
	// echo and search are called (used), sum never is (unused), ghost was never
	// advertised (called but not advertised).
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`))
	st.Ingest(env(7, proxy.ClientToServer, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search"}}`))
	st.Ingest(env(8, proxy.ServerToClient, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`))
	st.Ingest(env(9, proxy.ClientToServer, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"ghost"}}`))
	st.Ingest(env(10, proxy.ServerToClient, `{"jsonrpc":"2.0","id":5,"result":{"content":[]}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatal("c should open capabilities")
	}
	// overlayRaw is the full unwrapped caps body, so a bottom section is never
	// lost below the viewport fold the way it could be in View().
	out := m.overlayRaw
	for _, want := range []string{"unused", "called but not advertised", "echo", "search", "sum", "ghost"} {
		if !strings.Contains(out, want) {
			t.Fatalf("caps tool usage missing %q\n%s", want, out)
		}
	}
}

func TestCapabilitiesAndHelp(t *testing.T) {
	st := store.New(0)
	seed(st)
	m := ready(t, st)

	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatal("c should open capabilities")
	}
	out := m.View()
	for _, want := range []string{"capabilities", "protocolVersion", "client", "server"} {
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
	st := store.New(0)
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
	st := store.New(0)
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
	st := store.New(0)
	seed(st) // echo request (id2) + response
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream (follow -> last = a response)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector
	m = typeRunes(t, m, "x")                        // jump to the paired request
	if m.full[m.inspect].Kind != store.EventRequest {
		t.Fatalf("x should land on the request, got kind %v", m.full[m.inspect].Kind)
	}
	// r replays the inspected request. The seeded session has no recorded command,
	// so it opens the replay overlay with a cannot-replay note rather than doing
	// nothing, which proves r is wired and used the inspected frame.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.overlay != overlayReplay {
		t.Fatalf("r in the inspector should open the replay overlay, got overlay %d", m.overlay)
	}
	if strings.Contains(m.overlayRaw, "Select a request") {
		t.Fatal("r treated the inspected request as non-replayable")
	}
}

func TestReplayAgainFromResult(t *testing.T) {
	st := store.New(0)
	meta, _ := json.Marshal(proxy.SessionMeta{Command: []string{"true"}, CWD: "/tmp"})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 0, TS: time.Now(), Direction: proxy.DirectionMeta, Raw: meta})
	seed(st) // request/response frames on the same session, now with a command
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on the last frame (a response)
	m = typeRunes(t, m, "x")                        // jump to the request
	m = typeRunes(t, m, "r")                        // replay it
	if m.overlay != overlayReplay {
		t.Fatalf("r should open the replay overlay, got %d", m.overlay)
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
	if m.overlay != overlayReplay || m.replayReq.Method != before {
		t.Fatalf("r in the replay overlay should re-run the same replay, got overlay %d method %q", m.overlay, m.replayReq.Method)
	}
}

func TestPairJumpReachesFilteredOutPair(t *testing.T) {
	st := store.New(0)
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
	st := store.New(0)
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
	st := store.New(0)
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
	st := store.New(0)
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

func TestSortSessions(t *testing.T) {
	st := store.New(0)
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
	st := store.New(0)
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
	m := ready(t, store.New(0))
	out := m.View()
	for _, want := range []string{"waiting for MCP traffic", "mcpsnoop", "--"} {
		if !strings.Contains(out, want) {
			t.Fatalf("onboarding missing %q\n%s", want, out)
		}
	}
}

func TestPendingCallShown(t *testing.T) {
	st := store.New(0)
	// A request with no response yet → in-flight.
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"slow"}}`))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // drill into the stream
	if !strings.Contains(m.View(), "pending") {
		t.Fatalf("a pending request should show pending:\n%s", m.View())
	}
}

func TestDeleteSession(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	st := store.New(0)
	seed(st)
	st.Ingest(sessionEnv("s2", "search-api"))
	m := ready(t, st)
	if len(m.sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(m.sessions))
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if !m.confirmDelete {
		t.Fatal("ctrl-d should open a delete confirmation")
	}
	if len(m.sessions) != 2 {
		t.Fatalf("nothing should be deleted before confirming, got %d", len(m.sessions))
	}
	m = typeRunes(t, m, "y")
	if m.confirmDelete {
		t.Fatal("y should close the confirmation")
	}
	if len(m.sessions) != 1 {
		t.Fatalf("confirmed delete should remove the selected session, got %d", len(m.sessions))
	}
	if !m.flashActive() {
		t.Fatal("delete should set a flash message")
	}
}

func TestDeleteSessionCancel(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	st := store.New(0)
	seed(st)
	st.Ingest(sessionEnv("s2", "search-api"))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	m = typeRunes(t, m, "n")
	if m.confirmDelete {
		t.Fatal("n should close the confirmation")
	}
	if len(m.sessions) != 2 {
		t.Fatalf("cancel should keep both sessions, got %d", len(m.sessions))
	}
}

func TestOverlaySearch(t *testing.T) {
	st := store.New(0)
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
	st := store.New(0)
	seed(st) // no meta frame → command unknown
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into stream, follow → last frame (a response)

	m = typeRunes(t, m, "r")
	if m.overlay != overlayReplay || !strings.Contains(m.View(), "Select a request") {
		t.Fatalf("replay on a response should prompt for a request:\n%s", m.View())
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	m = drive(t, m, tea.KeyMsg{Type: tea.KeyUp}) // to the request frame
	m = typeRunes(t, m, "r")
	if m.overlay != overlayReplay || !strings.Contains(m.View(), "Cannot replay") {
		t.Fatalf("replay without a recorded command should say so:\n%s", m.View())
	}
}
