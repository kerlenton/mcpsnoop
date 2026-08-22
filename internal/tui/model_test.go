package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// metaEnv is the control frame the shim writes first, carrying the command the
// replay key would run.
func metaEnv(id string, command []string) proxy.Envelope {
	raw, _ := json.Marshal(proxy.SessionMeta{Command: command, CWD: "/tmp"})
	return proxy.Envelope{SessionID: id, ServerLabel: "demo", Seq: 0, TS: time.Now(),
		Direction: proxy.DirectionMeta, Raw: raw}
}

func seed(st *store.Store) {
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"sampling":{}},"clientInfo":{"name":"cli"}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"demo"}}}`))
	st.Ingest(env(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`))
	st.Ingest(env(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`))
}

func TestTaskLifecycleFramesCanBeFilteredAndShowTheirParent(t *testing.T) {
	m := Model{}
	e := store.EventView{
		Kind:   store.EventNotification,
		Method: "notifications/tasks",
		TaskID: "task-7",
		TaskCall: &store.CallView{
			ID: "1", Method: "tools/call", TaskID: "task-7", TaskStatus: "input_required", State: store.Pending,
		},
	}
	if !m.matchToken(e, "task:task-7") {
		t.Fatal("task filter did not match the linked task id")
	}
	cells := m.streamCells(e)
	if !strings.Contains(cells.detail, "tools/call id 1") || cells.status != "input_required" {
		t.Fatalf("task link/status not surfaced: %+v", cells)
	}

	handle := store.EventView{
		Kind: store.EventResponse, TaskID: "task-7",
		Call: &store.CallView{ID: "1", Method: "tools/call", TaskID: "task-7", TaskStatus: "working", State: store.Pending},
	}
	if got := m.streamCells(handle).status; got != "working" {
		t.Fatalf("task handle status = %q, want working", got)
	}
}

func TestMRTRRequestStateFindingHasDedicatedMarker(t *testing.T) {
	m := Model{}
	e := store.EventView{
		Kind:           store.EventRequest,
		Warning:        "MRTR retry changed requestState",
		MRTRRoot:       "1",
		MRTRStateIssue: store.MRTRStateChanged,
	}
	cells := m.streamCells(e)
	if cells.status != "state!" {
		t.Fatalf("requestState finding status = %q, want state!", cells.status)
	}
	if !strings.Contains(cells.detail, "changed") || !strings.Contains(cells.detail, "continues id 1") {
		t.Fatalf("requestState finding detail did not include its category and MRTR link: %q", cells.detail)
	}
}

func TestStatusFilterSeparatesCancelledFromError(t *testing.T) {
	m := Model{}

	// A cancelled task is Failed() (it delivered no result) but not on the error
	// axis, so status:err must miss it and status:cancelled, the label the row uses,
	// must find it.
	cancelled := store.EventView{
		Kind: store.EventResponse,
		Call: &store.CallView{ID: "1", Method: "tools/call", State: store.Failed, TaskID: "t1", TaskStatus: "cancelled"},
	}
	if m.matchToken(cancelled, "status:err") {
		t.Fatal("status:err must not match a cancelled call")
	}
	if !m.matchToken(cancelled, "status:cancelled") {
		t.Fatal("status:cancelled must match a cancelled call")
	}

	// A genuine error still matches status:err and not status:cancelled.
	errored := store.EventView{
		Kind: store.EventResponse,
		Call: &store.CallView{ID: "2", Method: "tools/call", State: store.Failed, Errored: true, ToolErr: true},
	}
	if !m.matchToken(errored, "status:err") {
		t.Fatal("status:err must match a genuinely errored call")
	}
	if m.matchToken(errored, "status:cancelled") {
		t.Fatal("status:cancelled must not match a non-cancelled error")
	}
}

func TestCallCancellationRowsAndFilters(t *testing.T) {
	m := Model{}
	t0 := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	cancelledCall := &store.CallView{
		ID: "7", Method: "tools/call", State: store.Cancelled,
		Start: t0, CancelledAt: t0.Add(250 * time.Millisecond), CancelReason: "moved on",
	}
	request := store.EventView{Kind: store.EventRequest, Method: "tools/call", Call: cancelledCall}
	if cells := m.streamCells(request); cells.status != "cancel" || cells.dur != "" {
		t.Fatalf("cancelled request cells = %+v", cells)
	}
	cancel := store.EventView{Kind: store.EventNotification, Method: "notifications/cancelled", Call: cancelledCall}
	if cells := m.streamCells(cancel); cells.status != "cancel" {
		t.Fatalf("cancellation notification cells = %+v", cells)
	}
	if !m.matchToken(cancel, "status:cancel") || m.matchToken(cancel, "status:cancelled") || m.matchToken(cancel, "status:late") {
		t.Fatal("call cancellation filter overlapped task cancellation or late result")
	}

	lateCall := &store.CallView{
		ID: "7", Method: "tools/call", State: store.Cancelled, LateResult: true,
		Start: t0, End: t0.Add(time.Second), Result: json.RawMessage(`{"content":[]}`),
	}
	late := store.EventView{
		Kind: store.EventResponse, Call: lateCall,
		Observation: "result arrived 750ms after cancellation",
	}
	cells := m.streamCells(late)
	if cells.status != "late" || cells.dur != "1s" || !strings.Contains(cells.detail, late.Observation) {
		t.Fatalf("late result cells = %+v", cells)
	}
	if !m.matchToken(late, "status:late-result") || m.matchToken(late, "status:cancel") || m.matchToken(late, "status:cancelled") {
		t.Fatal("late result filter overlapped cancellation filters")
	}
	m.full = []store.EventView{request}
	m.inspect = 0
	if got := m.pairWidget(); !strings.Contains(got, "cancel") {
		t.Fatalf("cancelled request pair widget = %q", got)
	}
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
	drift := store.ToolDrift{}
	drift.Add(store.DriftDescription, "search")
	st.SetToolDrift("sess", drift)

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

func TestInspectorShowsMCPParamHeaders(t *testing.T) {
	m := New(store.New())
	m.full = []store.EventView{{
		MCPParamHeaders: []proxy.MCPParamHeader{
			{Name: "Mcp-Param-Region", Value: "us-west1"},
			{Name: "Mcp-Param-Count", Value: "42"},
		},
	}}
	m.inspect = 0
	if got := m.inspectorHeaderH(); got != 2 {
		t.Fatalf("inspectorHeaderH() = %d, want 2 for parameter headers", got)
	}
	header := m.inspectorHeader(200)
	for _, want := range []string{"Mcp-Param-Count", "42", "Mcp-Param-Region", "us-west1"} {
		if !strings.Contains(header, want) {
			t.Fatalf("inspector header missing %q: %q", want, header)
		}
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

// TestCapsContentShowsAgreedExtension checks the block renders, and renders the
// id verbatim in reverse-DNS form rather than prettified, since that is the
// string someone will grep for in the traffic.
func TestCapsContentShowsAgreedExtension(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","clientInfo":{"name":"cli"},"capabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-07-28","capabilities":{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}},"serverInfo":{"name":"demo"}}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "c")
	out := m.overlayRaw

	for _, want := range []string{"extensions", "● io.modelcontextprotocol/tasks"} {
		if !strings.Contains(out, want) {
			t.Fatalf("caps body missing %q\n%s", want, out)
		}
	}
	// An agreed extension is in play, so it must not carry a one-sided note.
	for _, absent := range []string{"client only", "server only"} {
		if strings.Contains(out, absent) {
			t.Fatalf("an agreed extension should not read as one-sided (%q)\n%s", absent, out)
		}
	}
	// The extensions map is a container, not a capability, so it must not also
	// appear as a capability row in either section.
	if strings.Contains(out, "● extensions") || strings.Contains(out, "○ extensions") {
		t.Fatalf("extensions must not render as a capability row\n%s", out)
	}
}

// TestCapsContentMarksOneSidedExtension is the case the section exists for: an
// extension one side advertised and the other did not is not in play, and has
// to look that way rather than look supported.
func TestCapsContentMarksOneSidedExtension(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","clientInfo":{"name":"cli"},"capabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-07-28","capabilities":{"tools":{}},"serverInfo":{"name":"demo"}}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "c")
	out := m.overlayRaw

	if !strings.Contains(out, "○ io.modelcontextprotocol/tasks (client only)") {
		t.Fatalf("a client-only extension should read as not agreed\n%s", out)
	}
	if strings.Contains(out, "● io.modelcontextprotocol/tasks") {
		t.Fatalf("a one-sided extension must not carry the agreed marker\n%s", out)
	}
}

// TestCapsContentOmitsEmptyExtensionsSection locks the no-regression half: a
// session without extensions renders exactly as it did before this section
// existed, with no empty heading.
func TestCapsContentOmitsEmptyExtensionsSection(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli"},"capabilities":{"roots":{}}}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"demo"}}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "c")
	if strings.Contains(m.overlayRaw, "extensions") {
		t.Fatalf("no side advertised extensions, so the section must be absent\n%s", m.overlayRaw)
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

func TestEditReplayFromInspector(t *testing.T) {
	st := store.New()
	st.Ingest(metaEnv("s1", []string{"true"}))
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // inspector on the response
	m = typeRunes(t, m, "x")                        // paired request

	// Keep any editor fixture in the test's private directory even though this
	// fail-before test only needs to prove that R launches an editor command.
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	got := next.(Model)
	if cmd == nil {
		t.Fatal("R on a replayable request should launch the edit-and-replay editor")
	}
	if got.replaying {
		t.Fatal("editing must finish and validate before a replay can start")
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
	m = typeRunes(t, m, "r")                        // ask, then confirm
	m = typeRunes(t, m, "y")

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
	m = drive(t, m, replayDoneMsg{token: m.replayToken, res: replay.Result{Method: "get_sum", Response: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)}})
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

func TestEditedReplayKeepsConfirmationAndThreeParamLayers(t *testing.T) {
	st := store.New()
	st.Ingest(metaEnv("s1", []string{"safe-server", "--stdio"}))
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // stream

	var captured store.CallView
	for _, ev := range m.full {
		if ev.Kind == store.EventRequest && ev.Call != nil && ev.Call.Method == "tools/call" {
			captured = *ev.Call
			break
		}
	}
	if captured.Method == "" {
		t.Fatal("test request not found")
	}
	edited := captured
	edited.Params = json.RawMessage(`{"x":"edited"}`)
	beforeTimeline := len(st.Timeline("s1"))

	next, cmd := m.Update(replayEditDoneMsg{call: edited, captured: captured.Params})
	m = next.(Model)
	if cmd != nil || m.confirm == "" {
		t.Fatal("a valid edit must enter the existing command confirmation before replay")
	}
	if m.replaying || m.replayReq.Method != "" {
		t.Fatal("editor completion must not mutate replay state before confirmation")
	}

	m = typeRunes(t, m, "y")
	if !m.replaying {
		t.Fatal("confirmed edited replay did not start")
	}
	if !m.replayEdited || string(m.replayCaptured) != string(captured.Params) || string(m.replayReq.Params) != string(edited.Params) {
		t.Fatalf("replay param layers were not retained: captured=%s candidate=%s edited=%v", m.replayCaptured, m.replayReq.Params, m.replayEdited)
	}
	if got := len(st.Timeline("s1")); got != beforeTimeline {
		t.Fatalf("starting an edited replay changed the observed store: got %d want %d", got, beforeTimeline)
	}

	actual := json.RawMessage(`{"x":"edited","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`)
	m = drive(t, m, replayDoneMsg{token: m.replayToken, res: replay.Result{
		Method:   "echo",
		Params:   actual,
		Response: json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"isError":true}}`),
		ToolErr:  true,
	}})
	out := ansi.Strip(m.overlayRaw)
	for _, want := range []string{"REPLAY · EDITED · echo", "ERROR tool reported an error", "captured params", "params sent", "2026-07-28"} {
		if !strings.Contains(out, want) {
			t.Fatalf("edited replay overlay missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ok  (") {
		t.Fatalf("a tool-level error must not render as ok:\n%s", out)
	}
	if got := len(st.Timeline("s1")); got != beforeTimeline {
		t.Fatalf("an edited replay result entered the observed store: got %d want %d", got, beforeTimeline)
	}

	// Lowercase r repeats the candidate, not the captured bytes or the system
	// metadata added to the prior wire request.
	m = typeRunes(t, m, "r")
	if !m.replaying || string(m.replayReq.Params) != string(edited.Params) {
		t.Fatalf("r did not repeat the edited candidate: %s", m.replayReq.Params)
	}
}

func TestReplayEditFailureLeavesCurrentLoopUntouched(t *testing.T) {
	m := New(store.New())
	m.replayReq = store.CallView{Method: "tools/call", Params: json.RawMessage(`{"old":true}`)}
	m.replayCaptured = json.RawMessage(`{"captured":true}`)
	m.replayEdited = true

	next, cmd := m.Update(replayEditDoneMsg{err: os.ErrInvalid})
	got := next.(Model)
	if cmd != nil || got.replaying {
		t.Fatal("an invalid edit must not start a replay")
	}
	if got.replayReq.Method != m.replayReq.Method || string(got.replayReq.Params) != string(m.replayReq.Params) || string(got.replayCaptured) != string(m.replayCaptured) || got.replayEdited != m.replayEdited {
		t.Fatal("an invalid edit partially changed the current replay loop")
	}
	if !strings.Contains(got.flash, "edit replay aborted") {
		t.Fatalf("invalid edit flash = %q", got.flash)
	}
}

func TestReplayContentSeparatesProtocolMetadataFromUserEdits(t *testing.T) {
	m := New(store.New())
	m.replayCaptured = json.RawMessage(`{"name":"echo"}`)
	m.replayEdited = false
	out := ansi.Strip(m.replayContent(replayDoneMsg{res: replay.Result{
		Method:   "tools/call",
		Params:   json.RawMessage(`{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`),
		Response: json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`),
	}}))
	if strings.Contains(out, "EDITED") || strings.Contains(out, "captured params") {
		t.Fatalf("protocol-required metadata must not masquerade as a user edit:\n%s", out)
	}
	for _, want := range []string{"REPLAY · tools/call", "request params", "2026-07-28"} {
		if !strings.Contains(out, want) {
			t.Fatalf("actual stateless wire params missing %q:\n%s", want, out)
		}
	}
}

func TestEditReplayKeyFromStreamAndReplayOverlay(t *testing.T) {
	st := store.New()
	st.Ingest(metaEnv("s1", []string{"true"}))
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("R in the stream should launch the editor for the selected request")
	}

	m.overlay = overlayReplay
	m.replayReq = store.CallView{Method: "tools/call", Params: json.RawMessage(`{"name":"echo","arguments":{"text":"edited"}}`)}
	m.replayCaptured = json.RawMessage(`{"name":"echo","arguments":{"text":"captured"}}`)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("R in the replay overlay should reopen the editor on the current candidate")
	}
	if string(m.replayReq.Params) != `{"name":"echo","arguments":{"text":"edited"}}` {
		t.Fatal("opening the editor must not mutate the current candidate")
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
	m = typeRunes(t, m, "r")                        // ask, then confirm
	m = typeRunes(t, m, "y")
	if !m.replaying {
		t.Fatal("r should start replaying")
	}

	// Leaving the inspector abandons the in-flight replay, so its late result
	// does not pop an overlay over whatever the user moved on to.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.replaying {
		t.Fatal("closing the overlay should abandon the in-flight replay")
	}
	m = drive(t, m, replayDoneMsg{token: m.replayToken, res: replay.Result{Method: "x", Response: json.RawMessage("{}")}})
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
	drift := store.ToolDrift{}
	drift.Add(store.DriftDescription, "search")
	drift.Add(store.DriftToolAdded, "write")
	st.SetToolDrift("s1", drift)

	m := ready(t, st)
	// The sessions row carries the compact "!" marker (drift is warn-colored); the
	// full "tool definition drift" wording lives in the summary overlay below.
	if out := ansi.Strip(m.View()); !strings.Contains(out, "! demo") {
		t.Fatalf("sessions view did not surface drift\n%s", out)
	}
	m = typeRunes(t, m, "s")
	out := ansi.Strip(m.overlayRaw)
	for _, want := range []string{"tool definition drift", "description", "search", "added", "write"} {
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

// summaryCell reads one column of a tool's row by the header above it, so an
// assertion names the column it means. Searching the whole row for a marker
// worked only while exactly one column could produce it, and a column added
// later silently broke that.
func summaryCell(t *testing.T, styled, name, column string) string {
	t.Helper()
	lines := strings.Split(ansi.Strip(styled), "\n")
	header := -1
	for i, ln := range lines {
		if strings.Contains(ln, "TOOL") && strings.Contains(ln, "CALLS") {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatalf("no summary header in:\n%s", ansi.Strip(styled))
	}
	// Indexed in runes, not bytes. The header is ASCII so the two agree there, but
	// a row can hold a multi-byte rune before the column, and slicing by byte cut
	// one in half.
	start := strings.Index(lines[header], column)
	if start < 0 {
		t.Fatalf("no %s column in %q", column, lines[header])
	}
	end := start + len([]rune(column))
	for _, ln := range lines[header+1:] {
		row := []rune(ln)
		if !strings.HasPrefix(strings.TrimSpace(ln), name+" ") || len(row) < start {
			continue
		}
		return strings.TrimSpace(string(row[start:min(end, len(row))]))
	}
	t.Fatalf("no row for %q in:\n%s", name, ansi.Strip(styled))
	return ""
}

func TestSummaryHeaderShowsOnlyCallsAndSort(t *testing.T) {
	st := store.New()
	base := time.Now()
	// Advertise both tools so the DEF column shows a byte count rather than the ·
	// it uses for an unadvertised tool. That · would otherwise collide with the
	// one this test reads in the ERR column, which is the only column it is about.
	st.Ingest(envAt(1, proxy.ClientToServer, base, `{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}`))
	st.Ingest(envAt(2, proxy.ServerToClient, base, `{"jsonrpc":"2.0","id":9,"result":{"tools":[{"name":"good"},{"name":"bad"}]}}`))
	st.Ingest(envAt(3, proxy.ClientToServer, base.Add(time.Millisecond), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"good"}}`))
	st.Ingest(envAt(4, proxy.ServerToClient, base.Add(2*time.Millisecond), `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	st.Ingest(envAt(5, proxy.ClientToServer, base.Add(3*time.Millisecond), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bad"}}`))
	st.Ingest(envAt(6, proxy.ServerToClient, base.Add(4*time.Millisecond), `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[]}}`))

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
	// The clean tool shows · for zero errors and the erroring tool shows a count,
	// read from the ERR column by name rather than by hunting the row for a marker
	// that another column could also produce.
	if got := summaryCell(t, out, "good", "ERR"); got != "·" {
		t.Fatalf("clean tool ERR = %q, want ·\n%s", got, out)
	}
	if got := summaryCell(t, out, "bad", "ERR"); got != "1" {
		t.Fatalf("erroring tool ERR = %q, want 1\n%s", got, out)
	}
	// The erroring tool sorts above the clean one, not alphabetically.
	if strings.Index(out, "bad") > strings.Index(out, "good") {
		t.Fatalf("erroring tool should sort first\n%s", out)
	}
}

// TestSummaryReportsContextCostInBytes checks the numbers reach the screen and
// that the wording never implies a token count, which is the scope line the
// issue drew: bytes are exact, tokens would mean shipping someone's tokeniser.
func TestSummaryReportsContextCostInBytes(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"`+strings.Repeat("long ", 200)+`","inputSchema":{"type":"object"}},{"name":"ping","description":"Ping.","inputSchema":{"type":"object"}}]}}`))
	st.Ingest(env(3, proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`))
	st.Ingest(env(4, proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"`+strings.Repeat("y", 3000)+`"}]}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "s")
	out := m.overlayRaw

	for _, want := range []string{"definitions", "2 tools", "DEF", "RESULT", "KiB", "paid on every conversation", "heaviest"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tool summary missing %q\n%s", want, out)
		}
	}
	// Bytes, never tokens, in any casing.
	if strings.Contains(strings.ToLower(out), "token") {
		t.Fatalf("the summary must not mention tokens\n%s", out)
	}
}

// TestSummaryMarksAnUnfinishedToolList locks the misleading-total guard: a list
// still paginating must not present its partial sum as the cost of the server.
func TestSummaryMarksAnUnfinishedToolList(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"d","inputSchema":{"type":"object"}}],"nextCursor":"page2"}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "s")
	out := m.overlayRaw

	for _, want := range []string{"so far", "never finished paginating"} {
		if !strings.Contains(out, want) {
			t.Fatalf("an unfinished list must say so, missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "paid on every conversation") {
		t.Fatalf("a partial sum must not be worded as the settled cost\n%s", out)
	}
}

// TestSummaryWithoutAToolsListOmitsTheCostLine keeps the screen unchanged for a
// session that never listed tools, where there is nothing measured to report.
func TestSummaryWithoutAToolsListOmitsTheCostLine(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "s")
	if strings.Contains(m.overlayRaw, "definitions") {
		t.Fatalf("no tools/list was seen, so there is no fixed cost to claim\n%s", m.overlayRaw)
	}
}

// TestSummaryZeroToolsReadsAsZeroBytes covers a server that advertises nothing.
// The table's · means "nothing to show" in a cell; in the fixed-cost sentence a
// zero has to read as a zero.
func TestSummaryZeroToolsReadsAsZeroBytes(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))

	m := ready(t, st)
	m = typeRunes(t, m, "s")
	out := ansi.Strip(m.overlayRaw)

	if !strings.Contains(out, "0 tools · 0 B") {
		t.Fatalf("an empty tool list should read as zero bytes\n%s", out)
	}
	// The gutter label must not run into its value.
	if strings.Contains(out, "definitions0") {
		t.Fatalf("the definitions label needs a gap before its value\n%s", out)
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

func TestHistoryTruncatedMessageIsVisible(t *testing.T) {
	m := ready(t, store.New())
	m = drive(t, m, historyTruncatedMsg{loaded: 100, total: 243})

	if !m.flashActive() {
		t.Fatal("truncated history should show a visible notice")
	}
	for _, want := range []string{"100", "243", "older traces stay on disk"} {
		if !strings.Contains(m.flash, want) {
			t.Fatalf("history notice %q does not contain %q", m.flash, want)
		}
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
	// R remains the request-count sort in the sessions view; edit-and-replay is
	// only reachable after drilling into a request frame.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(Model)
	if cmd != nil || m.sessionSort.col != "req" {
		t.Fatalf("R in sessions should sort requests, cmd=%v sort=%q", cmd != nil, m.sessionSort.col)
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
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
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
	// own flash, since delete does not route through the navigation helpers. The
	// delete is irreversible and removes a file, so it asks first.
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.confirm == "" {
		t.Fatal("deleting a session must ask before removing its log")
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
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

// TestStreamRowReadsAnHTTPStatusOutLoud covers the row this event kind exists
// for. A 401 carries its challenge in a header and no body, so the status and
// the challenge are the entire content of the frame.
func TestStreamRowReadsAnHTTPStatusOutLoud(t *testing.T) {
	m := Model{}
	c := m.streamCells(store.EventView{
		Kind: store.EventTransport, Dir: proxy.ServerToClient,
		HTTPStatus: 401, AuthChallenge: `Bearer resource_metadata="https://auth.example/x"`,
	})
	if c.method != "http 401" {
		t.Fatalf("the row should name the status, got %q", c.method)
	}
	if c.status != "err" {
		t.Fatalf("a 401 is a failure in the row, got %q", c.status)
	}
	if !strings.Contains(c.detail, "Unauthorized") || !strings.Contains(c.detail, "resource_metadata") {
		t.Fatalf("the detail should carry the status text and the challenge, got %q", c.detail)
	}
}

// TestStreamRowKeepsATransportBodyOnOneLine guards the layout. The body of a
// transport frame is written by whatever is on the other end, and a gateway's
// HTML page spans many lines, while a row is one.
func TestStreamRowKeepsATransportBodyOnOneLine(t *testing.T) {
	m := Model{}
	c := m.streamCells(store.EventView{
		Kind: store.EventTransport, Dir: proxy.ServerToClient, HTTPStatus: 502,
		Text: "<html>\n<body>\x1b[31m502 Bad Gateway\x1b[0m</body>\n</html>",
	})
	if strings.ContainsAny(c.detail, "\n\r\x1b") {
		t.Fatalf("a row must stay on one line and drive no terminal, got %q", c.detail)
	}
	if !strings.Contains(c.detail, "502 Bad Gateway") {
		t.Fatalf("the readable part of the page should survive, got %q", c.detail)
	}
}

// TestStreamFilterFindsAnHTTPStatus covers the bare-number filter and the fact
// that a transport failure has no call to carry the error flag, so status:err
// has to match it on the status.
func TestStreamFilterFindsAnHTTPStatus(t *testing.T) {
	st := store.New()
	seed(st) // clean calls, no failures
	fail := env(5, proxy.ServerToClient, "")
	fail.Raw = nil
	fail.Transport = "http"
	fail.Status = 401
	st.Ingest(fail)

	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the stream
	total := len(m.timeline)

	// kind is a documented axis too, so a reader who sees "http 401" in the row
	// and tries the obvious query must not get an empty list.
	for _, q := range []string{"status:401", "status:err", "kind:transport", "kind:http"} {
		mm := drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
		mm = typeRunes(t, mm, q)
		f := drive(t, mm, tea.KeyMsg{Type: tea.KeyEnter})
		if len(f.timeline) != 1 || total <= 1 {
			t.Fatalf("%s should find exactly the 401, got %d of %d", q, len(f.timeline), total)
		}
		if f.timeline[0].HTTPStatus != 401 {
			t.Fatalf("%s matched the wrong frame: %+v", q, f.timeline[0])
		}
	}
}

// TestInspectorHeaderHeightFollowsTheStatusLine is the off-by-one guard. The
// renderer and the height calculation both decide whether a second chrome line
// exists, and a frame carrying only a status is the case that used to make them
// disagree.
func TestInspectorHeaderHeightFollowsTheStatusLine(t *testing.T) {
	statusOnly := store.EventView{Kind: store.EventTransport, HTTPStatus: 502}
	if !hasTransportMeta(statusOnly) {
		t.Fatal("a frame carrying only a status still needs its chrome line")
	}
	if hasTransportMeta(store.EventView{Kind: store.EventResponse}) {
		t.Fatal("a stdio frame has no transport chrome line")
	}
}

// TestStreamRowNamesAnErrorCode. The row carried only the message, so a
// spec-defined -32601 was indistinguishable from whatever number a server chose
// for itself, and the code a reader needs to look up was nowhere on screen.
func TestStreamRowNamesAnErrorCode(t *testing.T) {
	st := store.New()
	st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`))
	st.Ingest(env(2, proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no such tool"}}`))

	m := ready(t, st)
	var detail string
	for _, e := range st.Timeline("s1") {
		if e.Kind == store.EventResponse {
			detail = m.streamCells(e).detail
		}
	}
	if !strings.Contains(detail, "-32601") || !strings.Contains(detail, "method not found") {
		t.Fatalf("the row should carry the code and its name, got %q", detail)
	}
	if !strings.Contains(detail, "no such tool") {
		t.Fatalf("the server's own message must survive, got %q", detail)
	}
}

// TestStreamRowLeavesAnImplementationCodeUnnamed. -32000 to -32019 means
// whatever the sender decided, so the row shows the number and the sender's
// message and invents nothing.
func TestStreamRowLeavesAnImplementationCodeUnnamed(t *testing.T) {
	got := rpcErrorText(proxy.RPCError{Code: -32001, Message: "rate limited"})
	if got != "-32001: rate limited" {
		t.Fatalf("an implementation-defined code should render bare, got %q", got)
	}
}

// TestDeleteRefusesAnEscapingSessionID. The id is read out of the log's own
// session_id field, so it is data rather than a name mcpsnoop chose, and a log
// is a file people hand around. One spelling "../../x" made filepath.Join
// resolve outside the sessions directory, and the delete key passed that
// straight to os.Remove.
func TestDeleteRefusesAnEscapingSessionID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", home)
	victim := filepath.Join(home, "victim.jsonl")
	if err := os.WriteFile(victim, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st := store.New()
	st.Ingest(sessionEnv("../victim", "hostile"))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a session id that leaves the sessions directory must not delete a file: %v", err)
	}
	if len(m.sessions) != 0 {
		t.Fatalf("the session should still leave the view, got %d", len(m.sessions))
	}
}

// TestReplayAsksBeforeRunningTheRecordedCommand. The command comes out of the
// log's meta frame, so replaying executes whatever that file says, and the hub
// backfills any .jsonl left in the sessions directory. The command is shown and
// answered for before any process starts.
func TestReplayAsksBeforeRunningTheRecordedCommand(t *testing.T) {
	st := store.New()
	seed(st)
	st.Ingest(metaEnv("s1", []string{"/bin/sh", "-c", "touch /tmp/PWNED"}))
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeRunes(t, m, "x")
	m = typeRunes(t, m, "r")

	if m.confirm == "" {
		t.Fatal("replay must show the command and ask before running it")
	}
	if !strings.Contains(m.confirm, "/bin/sh") {
		t.Fatalf("the prompt must name the command that would run, got %q", m.confirm)
	}
	if m.replaying {
		t.Fatal("nothing may start before the question is answered")
	}
	// Declining runs nothing.
	m = typeRunes(t, m, "n")
	if m.replaying {
		t.Fatal("declining must not start a replay")
	}
}

// TestCancelAndLateRowsAreSelectableByWhatTheySay. The row for a
// notifications/cancelled frame labels itself, and status: has to find it by the
// same word. Labelling every such frame "cancel" while the filter required a call
// that was not yet a late result meant the token shown and the token that selects
// disagreed as soon as the result turned up.
func TestCancelAndLateRowsAreSelectableByWhatTheySay(t *testing.T) {
	build := func(late bool) Model {
		st := store.New()
		t0 := time.Now()
		st.Ingest(env(1, proxy.ClientToServer, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow"}}`))
		st.Ingest(env(2, proxy.ClientToServer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`))
		if late {
			st.Ingest(env(3, proxy.ServerToClient, `{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`))
		}
		_ = t0
		m := ready(t, st)
		return drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	}

	for _, tc := range []struct {
		name, token string
		late        bool
	}{
		{"cancelled", "cancel", false},
		{"late result", "late", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := build(tc.late)
			var labelled bool
			for _, e := range m.full {
				if e.Method != "notifications/cancelled" {
					continue
				}
				labelled = true
				// Both sides, the word the row prints and the word status: selects by.
				if got := m.streamCells(e).status; got != tc.token {
					t.Fatalf("the cancellation row says %q, want %q", got, tc.token)
				}
				if !m.matchStatus(e, tc.token) {
					t.Fatalf("the row says %q but status:%s does not select it", tc.token, tc.token)
				}
			}
			if !labelled {
				t.Fatal("the cancellation frame is missing from the timeline")
			}
		})
	}
}

// TestAnAbandonedReplayResultIsNotAdoptedByTheNextOne. replaying is a bare bool,
// so it only said that some replay was in flight. Walking away from one replay
// clears it, the spawned server keeps running, and when that first result finally
// landed it was rendered against whatever run had started since: one request's
// captured params over another request's answer, with the second run's own result
// then dropped because the same line clears the flag.
func TestAnAbandonedReplayResultIsNotAdoptedByTheNextOne(t *testing.T) {
	st := store.New()
	meta, _ := json.Marshal(proxy.SessionMeta{Command: []string{"true"}, CWD: "/tmp"})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 0, TS: time.Now(), Direction: proxy.DirectionMeta, Raw: meta})
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeRunes(t, m, "x")

	// First replay, then walk away from it the way opening any overlay does.
	m = typeRunes(t, m, "r")
	m = typeRunes(t, m, "y")
	first := m.replayToken
	if !m.replaying {
		t.Fatal("the first replay did not start")
	}
	m.dismissTransient()

	// Second replay, of the same session, which is what the user is now watching.
	m = typeRunes(t, m, "r")
	second := m.replayToken
	if first == second {
		t.Fatal("the two runs share a token, so nothing can tell them apart")
	}

	// The abandoned run answers late.
	m = drive(t, m, replayDoneMsg{token: first, res: replay.Result{Method: "stale", Response: json.RawMessage(`{"stale":true}`)}})
	if m.overlay == overlayReplay {
		t.Fatal("a result from an abandoned replay was rendered over the current one")
	}
	if !m.replaying {
		t.Fatal("the abandoned result cleared the flag the live run needs")
	}

	// The run the user is actually waiting for still lands.
	m = drive(t, m, replayDoneMsg{token: second, res: replay.Result{Method: "live", Response: json.RawMessage(`{"live":true}`)}})
	if m.overlay != overlayReplay {
		t.Fatal("the live result never reached the screen")
	}
	if !strings.Contains(ansi.Strip(m.overlayRaw), "live") {
		t.Fatalf("the overlay shows the wrong run:\n%s", ansi.Strip(m.overlayRaw))
	}
}

// TestReplayRefusesAFrameWhoseBodyWasReleased. The live store drops the bodies
// of old frames to stay inside its memory budget, and a request with no params
// left would replay as {}, quietly sending a different request from the one the
// row on screen describes.
func TestReplayRefusesAFrameWhoseBodyWasReleased(t *testing.T) {
	st := store.New()
	meta, _ := json.Marshal(proxy.SessionMeta{Command: []string{"true"}, CWD: "/tmp"})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 0, TS: time.Now(), Direction: proxy.DirectionMeta, Raw: meta})
	seed(st)
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeRunes(t, m, "x") // onto the request

	if !m.canReplay() {
		t.Fatal("the fixture must be replayable before the body goes, or this proves nothing")
	}
	for i := range m.full {
		m.full[i].BodyReleased = true
	}
	if m.canReplay() {
		t.Fatal("replay is still offered for a frame with no params left to send")
	}

	m = typeRunes(t, m, "r")
	if m.replaying {
		t.Fatal("r replayed a frame whose params are gone")
	}
	if !strings.Contains(m.flash, "released") {
		t.Fatalf("r must say why it refused, flash = %q", m.flash)
	}
	m = typeRunes(t, m, "R")
	if !strings.Contains(m.flash, "released") {
		t.Fatalf("R must say why it refused, flash = %q", m.flash)
	}
}

// TestExportFromABoundedStoreReadsTheLog. The live store releases the bodies of
// old frames, so building an export from it writes an artifact whose oldest
// frames have no payload and says nothing about it. The log on disk is complete,
// so that is what a bounded store exports.
func TestExportFromABoundedStoreReadsTheLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPSNOOP_HOME", home)
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}

	// One frame carrying a payload, written to the log the way the hub writes it,
	// and fed to a store so tight that its body is released immediately.
	const secret = "PAYLOAD-ONLY-IN-THE-LOG"
	env := proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: time.Now(), Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"t":"` + secret + `"}}}`),
	}
	line, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "s1.jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	st := store.NewBounded(1, 0)
	st.Ingest(env)
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)})
	m := ready(t, st)
	if !m.anyBodyReleased("s1") {
		t.Fatal("the fixture must release a body, or this proves nothing")
	}

	data, err := m.exportData("s1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), secret) {
		t.Fatal("the export was built from the bounded store, so the released payload is missing from it")
	}
}

// TestExportRefusesRatherThanWriteATruncatedArtifact. When the bodies are gone
// and the log cannot be read there is nothing complete to export, and writing
// the remains under the usual success message would be the quiet lie.
func TestExportRefusesRatherThanWriteATruncatedArtifact(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir()) // no log for this session
	st := store.NewBounded(1, 0)
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"t":"aaaaaaaaaa"}}}`)})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)})

	m := ready(t, st)
	if _, err := m.exportData("s1"); err == nil {
		t.Fatal("an export missing its oldest frames was written without a word")
	}
}

// TestStreamSaysWhenOlderFramesAreOnlyOnDisk. The memory budget removes the
// oldest frames from the timeline, so the top of the stream stops being the
// start of the session. Saying nothing would let a reader believe they were
// looking at the whole thing.
func TestStreamSaysWhenOlderFramesAreOnlyOnDisk(t *testing.T) {
	st := store.NewBounded(0, 4)
	now := time.Now()
	for i := range 20 {
		st.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "demo", Seq: uint64(i + 1),
			TS: now.Add(time.Duration(i) * time.Millisecond), Direction: proxy.ClientToServer,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":` + strconv.Itoa(i) + `}}`),
		})
	}
	m := ready(t, st)
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // into the stream

	if got := m.currentDroppedFrames(); got != 16 {
		t.Fatalf("dropped = %d, want the 16 frames past the cap", got)
	}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "16 older on disk") {
		t.Fatalf("the stream does not say frames were dropped:\n%s", view)
	}
	// It is not the same thing as a gap in the capture, which means bytes nobody
	// ever saw, so it must not borrow that word.
	if strings.Contains(view, "16 missing") {
		t.Fatalf("dropped frames are being reported as a hole in the capture:\n%s", view)
	}
}

// httpSessionStore builds a capture of an HTTP session with its meta frame and
// one tools/call carrying the routing headers the transport requires.
func httpSessionStore(t *testing.T) *store.Store {
	t.Helper()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Target: "https://api.example.com/mcp?key=[stripped]"})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New()
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "httpdemo", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: meta})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "httpdemo", Seq: 2, TS: t0.Add(time.Millisecond),
		Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
		MCPMethod: "tools/call", MCPName: "echo",
		MCPParamHeaders: []proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "us-west1"}},
		Raw:             json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "httpdemo", Seq: 3, TS: t0.Add(10 * time.Millisecond),
		Direction: proxy.ServerToClient, Transport: proxy.TransportHTTP,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)})
	return st
}

// TestHTTPSessionIsReplayableOnlyWithATarget is the gate. An HTTP capture
// records no command to run, and the endpoint it does record is stripped of
// anything credential-shaped, so it is not an address to dial. Replay is offered
// once somebody has said where it goes and not before.
func TestHTTPSessionIsReplayableOnlyWithATarget(t *testing.T) {
	st := httpSessionStore(t)

	without := New(st)
	without.streamSessionID = "s1"
	without.allSessions = st.Sessions()
	if without.sessionReplayable() {
		t.Fatal("an HTTP session with no target should not offer replay")
	}

	with := New(st, WithHTTPReplay(replay.HTTPTarget{URL: "https://api.example.com/mcp?key=real"}))
	with.streamSessionID = "s1"
	with.allSessions = st.Sessions()
	if !with.sessionReplayable() {
		t.Fatal("an HTTP session with a target should offer replay")
	}
}

// TestHTTPReplayWithoutATargetSaysWhy keeps the key from failing silently, and
// keeps the stdio message off a session that never had a command to begin with.
func TestHTTPReplayWithoutATargetSaysWhy(t *testing.T) {
	st := httpSessionStore(t)
	m := New(st)
	m.streamSessionID = "s1"
	m.allSessions = st.Sessions()
	m.width, m.height = 120, 40
	m.refresh()

	call := store.CallView{RequestSeq: 2, Method: "tools/call", ToolName: "echo", IsTool: true}
	if cmd := m.runReplay(call, nil, false, replay.Routing{}); cmd != nil {
		t.Fatal("a replay was started with nowhere to send it")
	}
	if !strings.Contains(m.flash, "--replay-target") {
		t.Fatalf("flash = %q, want it to name the flag", m.flash)
	}
	if strings.Contains(m.flash, "no recorded server command") {
		t.Fatalf("an HTTP session was told it is missing a command it never had: %q", m.flash)
	}
}

// TestHTTPReplayAsksBeforeItSends keeps the once-per-session confirmation that
// a recorded command already gets, because a replay reaches a live server that
// may be the production one.
func TestHTTPReplayAsksBeforeItSends(t *testing.T) {
	st := httpSessionStore(t)
	const target = "https://api.example.com/mcp?key=real"
	m := New(st, WithHTTPReplay(replay.HTTPTarget{URL: target}))
	m.streamSessionID = "s1"
	m.allSessions = st.Sessions()
	m.width, m.height = 120, 40
	m.refresh()

	call := store.CallView{RequestSeq: 2, Method: "tools/call", ToolName: "echo", IsTool: true}
	if cmd := m.runReplay(call, nil, false, replay.Routing{}); cmd != nil {
		t.Fatal("a replay was sent before it was answered for")
	}
	if !strings.Contains(m.confirm, target) {
		t.Fatalf("the prompt does not name where it posts: %q", m.confirm)
	}
	if m.replaying {
		t.Fatal("the replay is already in flight")
	}
}

// TestHTTPReplayReusesTheCapturedRoutingHeaders keeps a replay from deriving the
// request metadata a second time. The capture holds what the client sent,
// including any base64 sentinel form, so re-sending it cannot disagree with the
// body the way a re-derivation could.
func TestHTTPReplayReusesTheCapturedRoutingHeaders(t *testing.T) {
	st := httpSessionStore(t)
	m := New(st, WithHTTPReplay(replay.HTTPTarget{URL: "https://h/mcp"}))
	m.streamSessionID = "s1"
	m.allSessions = st.Sessions()
	m.width, m.height = 120, 40
	m.refresh()

	call := store.CallView{RequestSeq: 2, Method: "tools/call", ToolName: "echo", IsTool: true}
	_ = call
	// Read off the frame where a replay starts, which is where the headers are.
	m.view = viewStream
	m.refresh()
	ev, ok := m.focusedFrameByMethod("tools/call")
	if !ok {
		t.Fatalf("no request frame in the timeline: %d frames", len(m.timeline))
	}
	routing := routingOf(ev)
	if routing.Name != "echo" {
		t.Fatalf("Mcp-Name = %q, want the captured one", routing.Name)
	}
	if len(routing.ParamHeaders) != 1 || routing.ParamHeaders[0].Name != "Mcp-Param-Region" ||
		routing.ParamHeaders[0].Value != "us-west1" {
		t.Fatalf("param headers = %+v, want the captured one", routing.ParamHeaders)
	}
}

// TestAStdioSessionStillReplaysItsCommand keeps the transport that already
// worked working, and keeps its prompt naming the command rather than an address.
func TestAStdioSessionStillReplaysItsCommand(t *testing.T) {
	st := store.New()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "server.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Millisecond),
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`)})

	m := New(st, WithHTTPReplay(replay.HTTPTarget{URL: "https://h/mcp"}))
	m.streamSessionID = "s1"
	m.allSessions = st.Sessions()
	m.width, m.height = 120, 40
	m.refresh()
	if !m.sessionReplayable() {
		t.Fatal("a stdio session lost its replay")
	}
	call := store.CallView{RequestSeq: 2, Method: "tools/call", ToolName: "echo", IsTool: true}
	m.runReplay(call, nil, false, replay.Routing{})
	if !strings.Contains(m.confirm, "node server.js") {
		t.Fatalf("the prompt should still name the command it runs: %q", m.confirm)
	}
	if strings.Contains(m.confirm, "https://h/mcp") {
		t.Fatalf("a stdio session was offered an HTTP target: %q", m.confirm)
	}
}

// focusedFrameByMethod finds a request frame in the current timeline, so a test
// can read the routing headers a replay would send without driving the cursor.
func (m Model) focusedFrameByMethod(method string) (store.EventView, bool) {
	for _, ev := range m.timeline {
		if ev.Kind == store.EventRequest && ev.Method == method {
			return ev, true
		}
	}
	return store.EventView{}, false
}

// TestEditReplayOnAnHTTPSessionSaysWhy keeps R from telling an HTTP reader that
// a command is missing. It never had one, and the message describes a transport
// they are not using.
func TestEditReplayOnAnHTTPSessionSaysWhy(t *testing.T) {
	st := httpSessionStore(t)
	m := New(st)
	m.streamSessionID = "s1"
	m.allSessions = st.Sessions()
	m.width, m.height = 120, 40
	m.view = viewStream
	m.refresh()
	for i, ev := range m.timeline {
		if ev.Kind == store.EventRequest {
			m.selEvent = i
		}
	}

	if cmd := m.startEditReplay(); cmd != nil {
		t.Fatal("an editor was opened for a session with nowhere to send the result")
	}
	if !strings.Contains(m.flash, "--replay-target") {
		t.Fatalf("flash = %q, want it to name the flag", m.flash)
	}
	if strings.Contains(m.flash, "no recorded server command") {
		t.Fatalf("an HTTP session was told it is missing a command it never had: %q", m.flash)
	}
}

// TestAnEmptySessionIdStillAsksBeforeReplaying keeps the once-per-session
// confirmation from being satisfied by a zero value. A log whose frames carry no
// session_id decodes to a session whose id is empty, which the unset
// replayConfirmed matched.
func TestAnEmptySessionIdStillAsksBeforeReplaying(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "server.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New()
	st.Ingest(proxy.Envelope{ServerLabel: "srv", Seq: 1, TS: t0,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta})
	st.Ingest(proxy.Envelope{ServerLabel: "srv", Seq: 2, TS: t0.Add(time.Millisecond),
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`)})

	m := New(st)
	m.streamSessionID = ""
	m.allSessions = st.Sessions()
	m.width, m.height = 120, 40
	m.refresh()

	call := store.CallView{RequestSeq: 2, Method: "tools/call", ToolName: "echo", IsTool: true}
	if cmd := m.runReplay(call, nil, false, replay.Routing{}); cmd != nil {
		t.Fatal("a replay was sent without being answered for")
	}
	if m.confirm == "" {
		t.Fatal("no confirmation was asked for")
	}
}
