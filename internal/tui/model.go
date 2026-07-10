package tui

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// sortState tracks the active column sort for a table (shift+<key>).
type sortState struct {
	col  string
	desc bool
}

func (s sortState) toggled(col string) sortState {
	if s.col == col {
		return sortState{col: col, desc: !s.desc}
	}
	return sortState{col: col, desc: false}
}

// viewMode is the current table, you drill from the sessions list
// into a session's frame stream and back out with esc.
type viewMode int

const (
	viewSessions viewMode = iota
	viewStream
)

func (v viewMode) String() string {
	if v == viewStream {
		return "stream"
	}
	return "sessions"
}

// overlayMode is the full-screen panel layered over the table, if any.
type overlayMode int

const (
	overlayNone overlayMode = iota
	overlayInspector
	overlayCaps
	overlayReplay
)

// inputMode is the active bottom prompt ("/" filter and ":" command).
type inputMode int

const (
	inputNone inputMode = iota
	inputFilter
	inputCommand
	inputSearch // search within the open overlay (frame inspector etc.)
)

// frameMsg signals that the store ingested a new envelope.
type frameMsg struct{}

// tickMsg drives periodic refresh.
type tickMsg time.Time

const tickEvery = 400 * time.Millisecond

// Model is the Bubble Tea model for the hub view.
type Model struct {
	store  *store.Store
	keys   keyMap
	styles styles

	view viewMode

	allSessions  []store.SessionHeader
	sessions     []store.SessionHeader // after sessionQuery + sort
	selSession   int
	sessionQuery string
	sessionSort  sortState

	streamSessionID string // session whose stream we drilled into
	streamLabel     string
	timeline        []store.EventView
	streamSignals   streamSignalCounts
	selEvent        int
	query           string // stream filter
	total           int
	follow          bool
	streamSort      sortState

	paused bool

	overlay        overlayMode
	vp             viewport.Model
	overlayRaw     string // overlay body before soft-wrapping (re-wrapped on resize)
	overlayContent string // soft-wrapped, un-highlighted body, for re-search
	overlaySearch  string
	overlayMatches []int // line numbers containing a match
	overlayMatchIx int
	showHelp       bool

	inputMode inputMode
	input     textinput.Model

	flash      string // transient status message ("copied", "deleted", …)
	flashUntil time.Time

	width, height int
	ready         bool
}

// setFlash shows a transient message in the status bar for ~2s.
func (m *Model) setFlash(s string) {
	m.flash = s
	m.flashUntil = time.Now().Add(2 * time.Second)
}

func (m Model) flashActive() bool {
	return m.flash != "" && time.Now().Before(m.flashUntil)
}

// New builds the model around a store the hub feeds.
func New(st *store.Store) Model {
	ti := textinput.New()
	ti.Prompt = ""

	m := Model{
		store:  st,
		keys:   defaultKeys(),
		styles: newStyles(),
		view:   viewSessions,
		follow: true,
		input:  ti,
	}

	m.refresh()

	return m
}

func (m Model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layoutOverlay()
		if m.overlay != overlayNone {
			m.setOverlayBody(m.overlayRaw) // re-wrap to the new width
			if m.overlaySearch != "" {
				m.applyOverlaySearch(m.overlaySearch)
			}
		}
		m.ready = true
		m.refresh()
		return m, nil

	case tickMsg:
		if !m.paused {
			m.refresh()
		}
		return m, tick()

	case frameMsg:
		if !m.paused {
			m.refresh()
		}
		return m, nil

	case replayDoneMsg:
		if m.overlay == overlayReplay {
			m.setOverlayBody(m.replayContent(msg))
			m.vp.GotoTop()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	// Bottom prompt (":" command / "/" filter) captures input while open.
	if m.inputMode != inputNone {
		return m.handleInput(msg)
	}

	// Help screen, any of esc/?/q closes it.
	if m.showHelp {
		if key.Matches(msg, m.keys.Back, m.keys.Help, m.keys.Quit) {
			m.showHelp = false
		}
		return m, nil
	}

	// Overlays scroll. "/" searches within them, n/N jump matches, esc/enter/q close.
	if m.overlay != overlayNone {
		switch {
		case key.Matches(msg, m.keys.Filter):
			m.inputMode = inputSearch
			m.input.Prompt = "/"
			m.input.Placeholder = "search in frame…"
			m.input.SetValue(m.overlaySearch)
			m.input.CursorEnd()
			return m, m.input.Focus()
		case msg.String() == "n":
			m.overlayJump(1)
		case msg.String() == "N":
			m.overlayJump(-1)
		case key.Matches(msg, m.keys.Copy):
			m.copyCurrent()
		case key.Matches(msg, m.keys.Enter, m.keys.Back, m.keys.Quit),
			key.Matches(msg, m.keys.Caps):
			m.closeOverlay()
		default:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	// Shift+<letter> sorts the current table by a column.
	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && m.applySortKey(msg.Runes[0]) {
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.showHelp = true
	case key.Matches(msg, m.keys.Command):
		m.openInput(inputCommand)
		return m, m.input.Focus()
	case key.Matches(msg, m.keys.Filter):
		m.openInput(inputFilter)
		return m, m.input.Focus()

	case key.Matches(msg, m.keys.Enter):
		m.drillIn()
	case key.Matches(msg, m.keys.Back):
		return m, m.back()

	case key.Matches(msg, m.keys.Pause):
		m.paused = !m.paused
		if !m.paused {
			m.refresh()
		}
	case key.Matches(msg, m.keys.Follow):
		if m.view == viewStream {
			m.follow = !m.follow
			if m.follow {
				m.refresh()
			}
		}

	case key.Matches(msg, m.keys.Caps):
		if m.currentSessionID() != "" {
			m.openOverlay(overlayCaps, m.capsContent())
		}
	case key.Matches(msg, m.keys.Replay):
		if cmd := m.startReplay(); cmd != nil {
			return m, cmd
		}

	case key.Matches(msg, m.keys.Copy):
		m.copyCurrent()
	case key.Matches(msg, m.keys.Export):
		m.exportCurrent("", "")
	case key.Matches(msg, m.keys.Delete):
		m.deleteCurrentSession()

	case key.Matches(msg, m.keys.Up):
		m.step(-1)
	case key.Matches(msg, m.keys.Down):
		m.step(1)
	case key.Matches(msg, m.keys.PageUp):
		m.move(-m.pageSize())
	case key.Matches(msg, m.keys.PageDown):
		m.move(m.pageSize())
	case key.Matches(msg, m.keys.Top):
		m.moveTo(0)
	case key.Matches(msg, m.keys.Bottom):
		m.moveTo(1 << 30)
	}
	return m, nil
}

// handleInput drives the bottom prompt for ":" command and "/" filter modes.
func (m Model) handleInput(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEnter:
		val := strings.TrimSpace(m.input.Value())
		mode := m.inputMode
		m.closeInput()
		switch mode {
		case inputCommand:
			return m.runCommand(val)
		case inputSearch:
			m.applyOverlaySearch(val)
		default:
			m.applyFilter(val)
		}
		return m, nil
	case tea.KeyEsc:
		switch m.inputMode {
		case inputFilter:
			m.applyFilter("")
		case inputSearch:
			m.applyOverlaySearch("")
		}
		m.closeInput()
		return m, nil
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		// Live filter/search as you type.
		switch m.inputMode {
		case inputFilter:
			m.applyFilter(strings.TrimSpace(m.input.Value()))
		case inputSearch:
			m.applyOverlaySearch(strings.TrimSpace(m.input.Value()))
		}
		return m, cmd
	}
}

// runCommand handles ":" commands, q/quit, sessions, stream, export, or a session name.
func (m Model) runCommand(cmd string) (Model, tea.Cmd) {
	fields := strings.Fields(cmd)
	base := strings.ToLower(cmd)
	if len(fields) > 0 {
		base = strings.ToLower(fields[0])
	}
	switch base {
	case "":
		return m, nil
	case "q", "quit", "exit":
		return m, tea.Quit
	case "help", "?":
		m.showHelp = true
		return m, nil
	case "sessions", "session", "s", "..":
		m.view = viewSessions
		m.refresh()
		return m, nil
	case "stream", "frames":
		if m.currentSessionID() != "" {
			m.enterStream(m.selSession)
		}
		return m, nil
	case "export":
		format, out := "", ""
		if len(fields) > 1 {
			format = fields[1]
		}
		if len(fields) > 2 {
			out = fields[2]
		}
		m.exportCurrent(format, out)
		return m, nil
	}
	// Otherwise treat it as a session-name jump.
	for i, s := range m.sessions {
		if strings.Contains(strings.ToLower(s.Label), strings.ToLower(cmd)) {
			m.enterStream(i)
			return m, nil
		}
	}
	return m, nil
}

func (m *Model) openInput(mode inputMode) {
	m.inputMode = mode
	if mode == inputCommand {
		m.input.Prompt = ":"
		m.input.Placeholder = "sessions · stream · export [format] [path] · <name> · q"
		m.input.SetValue("")
	} else {
		m.input.Prompt = "/"
		m.input.Placeholder = m.filterPlaceholder()
		if m.view == viewSessions {
			m.input.SetValue(m.sessionQuery)
		} else {
			m.input.SetValue(m.query)
		}
	}
	m.input.CursorEnd()
}

func (m *Model) closeInput() {
	m.inputMode = inputNone
	m.input.Blur()
}

func (m *Model) filterPlaceholder() string {
	if m.view == viewSessions {
		return "filter sessions by name…"
	}
	return "text · or tool:echo status:err dir:s2c kind:resp id:7"
}

// drillIn. In the sessions table, enter a session's stream. In the stream, open
// the frame inspector.
func (m *Model) drillIn() {
	if m.view == viewSessions {
		if len(m.sessions) > 0 {
			m.enterStream(m.selSession)
		}
		return
	}
	if m.selEvent < len(m.timeline) {
		m.openOverlay(overlayInspector, m.inspectorContent())
	}
}

// back pops one level, clear an active filter, then stream→sessions. At
// the root it does NOTHING, quitting is deliberately only `:q`/Ctrl-C so you
// can't fall out of the UI by mashing esc/q.
func (m *Model) back() tea.Cmd {
	if m.view == viewStream {
		if m.query != "" {
			m.applyFilter("")
			return nil
		}
		m.view = viewSessions
		m.refresh()
		return nil
	}
	if m.sessionQuery != "" {
		m.applyFilter("")
	}
	return nil
}

func (m *Model) enterStream(idx int) {
	if idx < 0 || idx >= len(m.sessions) {
		return
	}
	m.selSession = idx
	m.streamSessionID = m.sessions[idx].ID
	m.streamLabel = m.sessions[idx].Label
	m.view = viewStream
	m.follow = true
	m.refresh()
}

// applyFilter sets the filter for the current view.
func (m *Model) applyFilter(q string) {
	if m.view == viewSessions {
		m.sessionQuery = q
	} else {
		m.query = q
	}
	m.refresh()
}

// step moves the cursor by delta with wrap-around (for j/k, ↑/↓).
func (m *Model) step(delta int) {
	if m.view == viewSessions {
		if n := len(m.sessions); n > 0 {
			m.selSession = ((m.selSession+delta)%n + n) % n
		}
		return
	}
	if n := len(m.timeline); n > 0 {
		m.selEvent = ((m.selEvent+delta)%n + n) % n
		m.follow = m.selEvent == n-1
	}
}

// move shifts the cursor by delta, clamped to the ends (for paging).
func (m *Model) move(delta int) {
	if m.view == viewSessions {
		m.selSession = clamp(m.selSession+delta, 0, len(m.sessions)-1)
		return
	}
	m.selEvent = clamp(m.selEvent+delta, 0, len(m.timeline)-1)
	m.follow = m.selEvent == len(m.timeline)-1
}

func (m *Model) moveTo(idx int) {
	if m.view == viewSessions {
		m.selSession = clamp(idx, 0, len(m.sessions)-1)
		return
	}
	m.selEvent = clamp(idx, 0, len(m.timeline)-1)
	m.follow = m.selEvent == len(m.timeline)-1
}

// pageSize is the number of body rows, used for ctrl-f/ctrl-b paging.
func (m Model) pageSize() int {
	n := m.bodyHeight() - 1 // minus the column header row
	if n < 1 {
		return 1
	}
	return n
}

// currentSessionID returns the session the current view refers to.
func (m Model) currentSessionID() string {
	if m.view == viewStream {
		return m.streamSessionID
	}
	if len(m.sessions) > 0 {
		return m.sessions[m.selSession].ID
	}
	return ""
}

// startReplay launches an async replay of the selected request frame.
func (m *Model) startReplay() tea.Cmd {
	if m.view != viewStream || m.selEvent >= len(m.timeline) {
		return nil
	}
	ev := m.timeline[m.selEvent]
	if ev.Call == nil || ev.Kind != store.EventRequest {
		m.openOverlay(overlayReplay, "Select a request frame to replay (a → line).")
		return nil
	}
	command, cwd, ok := m.store.Command(m.streamSessionID)
	if !ok {
		m.openOverlay(overlayReplay, "Cannot replay: this session has no recorded server command\n(its meta frame was not captured).")
		return nil
	}
	m.openOverlay(overlayReplay, "Replaying "+ev.Call.Method+" against an isolated server copy…")
	return replayCmd(command, cwd, ev.Call.Method, ev.Call.Params)
}

// applySortKey maps a shift+<letter> to a column sort for the current view.
// Returns false for keys that aren't sort triggers (e.g. G = bottom).
func (m *Model) applySortKey(r rune) bool {
	var col string
	if m.view == viewSessions {
		switch r {
		case 'N':
			col = "name"
		case 'R':
			col = "req"
		case 'S':
			col = "resp"
		case 'E':
			col = "err"
		case 'L':
			col = "last"
		default:
			return false
		}
		m.sessionSort = m.sessionSort.toggled(col)
	} else {
		switch r {
		case 'T':
			col = "time"
		case 'M':
			col = "method"
		case 'I':
			col = "id"
		case 'D':
			col = "dur"
		case 'S':
			col = "status"
		default:
			return false
		}
		m.streamSort = m.streamSort.toggled(col)
	}
	m.refresh()
	return true
}

// refresh pulls fresh snapshots from the store into the model.
func (m *Model) refresh() {
	m.allSessions = m.store.Sessions()
	m.sessions = filterSessions(m.allSessions, m.sessionQuery)
	sortSessions(m.sessions, m.sessionSort)
	m.selSession = clamp(m.selSession, 0, max(len(m.sessions)-1, 0))

	if m.view != viewStream {
		return
	}
	full := m.store.Timeline(m.streamSessionID)
	m.total = len(full)
	m.timeline = m.filterEvents(full)
	m.streamSignals = countStreamSignals(m.timeline, m.store.SlowThreshold())
	m.sortStream()
	// A non-chronological sort means we're inspecting, not tailing.
	if m.streamSort.col != "" && m.streamSort.col != "time" {
		m.follow = false
	}
	if m.follow {
		m.selEvent = len(m.timeline) - 1
	}
	m.selEvent = clamp(m.selEvent, 0, max(len(m.timeline)-1, 0))
}

type streamSignalCounts struct {
	errors int
	bad    int
	warn   int
	slow   int
}

func countStreamSignals(events []store.EventView, slowThreshold time.Duration) streamSignalCounts {
	var c streamSignalCounts
	for _, e := range events {
		switch {
		case e.Kind != store.EventInvalid && e.Warning != "":
			c.warn++
		case e.Kind == store.EventInvalid:
			c.bad++
		case e.Kind == store.EventResponse && e.Call != nil && e.Call.Failed():
			c.errors++
		case e.Kind == store.EventResponse && e.Call != nil && e.Call.Slow(slowThreshold):
			c.slow++
		}
	}
	return c
}

func sortSessions(s []store.SessionHeader, st sortState) {
	if st.col == "" {
		return
	}
	slices.SortStableFunc(s, func(a, b store.SessionHeader) int {
		var c int
		switch st.col {
		case "name":
			c = cmp.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
		case "req":
			c = cmp.Compare(a.Requests, b.Requests)
		case "resp":
			c = cmp.Compare(a.Responses, b.Responses)
		case "err":
			c = cmp.Compare(a.Errors, b.Errors)
		case "last":
			c = a.Last.Compare(b.Last)
		}
		if st.desc {
			return -c
		}
		return c
	})
}

func (m *Model) sortStream() {
	st := m.streamSort
	if st.col == "" || st.col == "time" {
		if st.desc {
			slices.Reverse(m.timeline)
		}
		return
	}
	slices.SortStableFunc(m.timeline, func(a, b store.EventView) int {
		var c int
		switch st.col {
		case "method":
			c = cmp.Compare(strings.ToLower(a.Method), strings.ToLower(b.Method))
		case "id":
			c = cmp.Compare(a.ID, b.ID)
		case "dur":
			c = cmp.Compare(callDur(a), callDur(b))
		case "status":
			c = cmp.Compare(statusRank(a), statusRank(b))
		}
		if c == 0 {
			c = cmp.Compare(a.Seq, b.Seq) // stable tiebreak by arrival
		}
		if st.desc {
			return -c
		}
		return c
	})
}

func callDur(e store.EventView) int64 {
	if e.Call != nil && e.Call.Done() {
		return int64(e.Call.Duration())
	}
	return -1
}

func statusRank(e store.EventView) int {
	if e.Kind == store.EventInvalid {
		return 5 // stream corruption sorts above call errors when sorting by status
	}
	if e.Call != nil && e.Call.Failed() {
		return 4
	}
	if e.Warning != "" {
		return 3
	}
	if e.Call == nil {
		return 0
	}
	switch {
	case e.Call.State == store.Pending:
		return 1
	default:
		return 2
	}
}

func filterSessions(sessions []store.SessionHeader, query string) []store.SessionHeader {
	if query == "" {
		return sessions
	}
	q := strings.ToLower(query)
	out := sessions[:0:0]
	for _, s := range sessions {
		if strings.Contains(strings.ToLower(s.Label), q) {
			out = append(out, s)
		}
	}
	return out
}

// filterEvents applies the stream query, space-separated tokens, ANDed. A token
// `key:value` matches a field (tool/method/id/dir/kind/status), a bare token is
// a case-insensitive substring over method/tool/id/stderr/raw JSON.
func (m *Model) filterEvents(events []store.EventView) []store.EventView {
	toks := strings.Fields(m.query)
	if len(toks) == 0 {
		return events
	}
	out := events[:0:0]
	for _, e := range events {
		if m.eventMatchesAll(e, toks) {
			out = append(out, e)
		}
	}
	return out
}

func (m *Model) eventMatchesAll(e store.EventView, toks []string) bool {
	for _, t := range toks {
		if !m.matchToken(e, t) {
			return false
		}
	}
	return true
}

func (m *Model) matchToken(e store.EventView, tok string) bool {
	if k, v, ok := strings.Cut(tok, ":"); ok && v != "" {
		switch strings.ToLower(k) {
		case "tool", "t":
			return e.Call != nil && containsFold(e.Call.ToolName, v)
		case "method", "m":
			return containsFold(e.Method, v) || (e.Call != nil && containsFold(e.Call.Method, v))
		case "id":
			return strings.EqualFold(e.ID, v)
		case "dir", "d":
			return matchDir(e.Dir, v)
		case "kind", "k":
			return matchKind(e.Kind, v)
		case "status", "s":
			return m.matchStatus(e, v)
		}
	}
	return eventSubstr(e, strings.ToLower(tok))
}

func eventSubstr(e store.EventView, q string) bool {
	if strings.Contains(strings.ToLower(e.Method), q) ||
		strings.Contains(strings.ToLower(e.ID), q) ||
		strings.Contains(strings.ToLower(e.Text), q) ||
		strings.Contains(strings.ToLower(e.Warning), q) ||
		strings.Contains(strings.ToLower(string(e.Raw)), q) {
		return true
	}
	return e.Call != nil && strings.Contains(strings.ToLower(e.Call.ToolName), q)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func matchDir(d proxy.Direction, v string) bool {
	switch strings.ToLower(v) {
	case "c2s", "client", "in", "req", "request", "->", "→":
		return d == proxy.ClientToServer
	case "s2c", "server", "out", "resp", "response", "<-", "←":
		return d == proxy.ServerToClient
	case "stderr", "err":
		return d == proxy.ServerStderr
	}
	return strings.EqualFold(string(d), v)
}

func matchKind(k store.EventKind, v string) bool {
	switch strings.ToLower(v) {
	case "req", "request":
		return k == store.EventRequest
	case "resp", "response":
		return k == store.EventResponse
	case "notify", "notification", "ntf":
		return k == store.EventNotification
	case "stderr":
		return k == store.EventStderr
	case "invalid", "corrupt", "bad":
		return k == store.EventInvalid
	}
	return false
}

func (m *Model) matchStatus(e store.EventView, v string) bool {
	v = strings.ToLower(v)
	if v == "bad" || v == "invalid" {
		// Invalid frames are not calls, so match them before the Call check.
		return e.Kind == store.EventInvalid
	}
	if v == "warn" || v == "warning" {
		return e.Warning != ""
	}
	if e.Call == nil {
		return false
	}
	switch v {
	case "err", "error", "fail", "failed":
		return e.Call.Failed()
	case "slow":
		return e.Call.Slow(m.store.SlowThreshold())
	case "pending", "pend", "inflight":
		return e.Call.State == store.Pending
	case "ok", "success":
		return e.Call.State == store.Completed && !e.Call.Failed()
	}
	return false
}

// openOverlay shows a full-screen scrollable panel with the given content.
func (m *Model) openOverlay(mode overlayMode, content string) {
	m.overlay = mode
	m.overlaySearch = ""
	m.overlayMatches = nil
	m.overlayMatchIx = 0
	m.layoutOverlay()
	m.setOverlayBody(content)
	m.vp.GotoTop()
}

// setOverlayBody stores the raw panel text and renders it soft-wrapped to the
// viewport width, so long single-line JSON values stay readable instead of
// running off the edge.
func (m *Model) setOverlayBody(content string) {
	m.overlayRaw = content
	m.overlayContent = softWrap(content, m.vp.Width)
	m.vp.SetContent(m.overlayContent)
}

// copyCurrent copies the most relevant thing for the current context to the
// system clipboard, the frame JSON (inspector / stream), the open panel, or the
// session log path (sessions list).
func (m *Model) copyCurrent() {
	var text, label string
	switch {
	case m.overlay == overlayInspector && m.selEvent < len(m.timeline):
		text, label = frameText(m.timeline[m.selEvent]), "frame JSON"
	case m.overlay != overlayNone:
		text, label = m.overlayRaw, "panel"
	case m.view == viewStream && m.selEvent < len(m.timeline):
		text, label = frameText(m.timeline[m.selEvent]), "frame JSON"
	case m.view == viewSessions && len(m.sessions) > 0:
		text, label = paths.SessionLogPath(m.sessions[m.selSession].ID), "log path"
	default:
		return
	}
	if err := clipboard.WriteAll(text); err != nil {
		m.setFlash("copy failed (no clipboard)")
		return
	}
	m.setFlash("✓ copied " + label)
}

// exportCurrent writes the selected/open session to a portable file.
func (m *Model) exportCurrent(formatArg, outPath string) {
	id := m.currentSessionID()
	if id == "" {
		return
	}
	format := exporter.FormatHTML
	if formatArg != "" {
		var err error
		format, err = exporter.ParseFormat(formatArg)
		if err != nil {
			m.setFlash("export failed: " + err.Error())
			return
		}
	}
	data, err := exporter.Build(m.store, id)
	if err != nil {
		m.setFlash("export failed: " + err.Error())
		return
	}
	if outPath == "" {
		outPath = exporter.DefaultOutputPath(id, format)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		m.setFlash("export failed: " + err.Error())
		return
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		m.setFlash("export failed: " + err.Error())
		return
	}
	err = exporter.Write(f, data, exporter.Options{Format: format})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		m.setFlash("export failed: " + err.Error())
		return
	}
	m.setFlash("✓ exported " + outPath)
}

// deleteCurrentSession removes the selected/open session and its on-disk log.
func (m *Model) deleteCurrentSession() {
	id := m.currentSessionID()
	if id == "" {
		return
	}
	m.store.Delete(id)
	_ = os.Remove(paths.SessionLogPath(id))
	if m.view == viewStream {
		m.view = viewSessions
	}
	m.refresh()
	m.setFlash("✓ deleted session")
}

// closeOverlay dismisses the overlay and clears any in-overlay search.
func (m *Model) closeOverlay() {
	m.overlay = overlayNone
	m.overlayRaw = ""
	m.overlayContent = ""
	m.overlaySearch = ""
	m.overlayMatches = nil
}

// applyOverlaySearch finds matches for q and renders the overlay with them
// highlighted (a less-style "/" search inside the frame inspector).
func (m *Model) applyOverlaySearch(q string) {
	m.overlaySearch = q
	m.overlayMatches = nil
	m.overlayMatchIx = 0
	if q == "" {
		m.vp.SetContent(m.overlayContent)
		return
	}
	lq := strings.ToLower(q)
	for i, line := range strings.Split(m.overlayContent, "\n") {
		if strings.Contains(strings.ToLower(line), lq) {
			m.overlayMatches = append(m.overlayMatches, i)
		}
	}
	m.renderOverlaySearch()
}

// renderOverlaySearch highlights every match, with the CURRENT one in a distinct
// style, and scrolls it into view.
func (m *Model) renderOverlaySearch() {
	cur := -1
	if len(m.overlayMatches) > 0 {
		cur = m.overlayMatches[m.overlayMatchIx]
	}
	lines := strings.Split(m.overlayContent, "\n")
	for _, ln := range m.overlayMatches {
		style := m.styles.match
		if ln == cur {
			style = m.styles.matchCur
		}
		lines[ln] = highlightMatches(lines[ln], m.overlaySearch, style)
	}
	m.vp.SetContent(strings.Join(lines, "\n"))
	if cur >= 0 {
		m.vp.SetYOffset(cur)
	}
}

// overlayJump scrolls to the next (dir=1) or previous (dir=-1) match.
func (m *Model) overlayJump(dir int) {
	n := len(m.overlayMatches)
	if n == 0 {
		return
	}
	m.overlayMatchIx = (m.overlayMatchIx + dir + n) % n
	m.renderOverlaySearch()
}

func (m *Model) layoutOverlay() {
	w := max(m.width-4, 1)  // inside the border
	h := max(m.height-6, 1) // inside the border + footer line
	if m.vp.Width == 0 {
		m.vp = viewport.New(w, h)
	} else {
		m.vp.Width, m.vp.Height = w, h
	}
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}
