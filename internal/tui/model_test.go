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
	// a best-effort JSON-RPC validation warning: method but no jsonrpc marker.
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

// TestStatusRankInvalid checks that sorting by status surfaces invalid frames:
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

func TestCapabilitiesAndHelp(t *testing.T) {
	st := store.New(0)
	seed(st)
	m := ready(t, st)

	m = typeRunes(t, m, "c")
	if m.overlay != overlayCaps {
		t.Fatal("c should open capabilities")
	}
	out := m.View()
	for _, want := range []string{"CAPABILITIES", "protocolVersion", "CLIENT", "SERVER"} {
		if !strings.Contains(out, want) {
			t.Fatalf("caps missing %q\n%s", want, out)
		}
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	m = typeRunes(t, m, "?")
	if !m.showHelp || !strings.Contains(m.View(), "keybindings") {
		t.Fatalf("? should show help:\n%s", m.View())
	}
	m = typeRunes(t, m, "?")
	if m.showHelp {
		t.Fatal("? should toggle help off")
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
	for _, want := range []string{"Waiting for MCP traffic", "mcpsnoop", "--"} {
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
	if !strings.Contains(m.View(), "PENDING") {
		t.Fatalf("a pending request should show PENDING:\n%s", m.View())
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
	if len(m.sessions) != 1 {
		t.Fatalf("ctrl-d should delete the selected session, got %d", len(m.sessions))
	}
	if !m.flashActive() {
		t.Fatal("delete should set a flash message")
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
