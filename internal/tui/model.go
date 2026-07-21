package tui

import (
	"cmp"
	"fmt"
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
	"github.com/charmbracelet/x/ansi"

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
	overlaySummary
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

// historyTruncatedMsg tells the TUI that startup intentionally loaded only the
// newest saved sessions.
type historyTruncatedMsg struct {
	loaded int
	total  int
}

// tickMsg drives the shared animation and refresh clock.
type tickMsg time.Time

// tickEvery is the single shared cadence. It is fast enough for a smooth spinner
// (about 12fps) while the heavier store refresh runs every refreshEvery ticks so
// reads stay at roughly 400ms.
const (
	tickEvery    = 80 * time.Millisecond
	refreshEvery = 5
)

// The activity sparkline is 8 buckets of 15s covering the last two minutes.
const (
	sparkBuckets = 8
	sparkSpan    = 2 * time.Minute
)

// Model is the Bubble Tea model for the hub view.
type Model struct {
	store  *store.Store
	keys   keyMap
	theme  theme
	styles styles

	view viewMode

	allSessions  []store.SessionHeader
	sessions     []store.SessionHeader // after sessionQuery + sort
	selSession   int
	sessionQuery string
	sessionSort  sortState
	activity     map[string][]int  // per-session frame-count sparkline buckets
	clients      map[string]string // per-session client name and version from initialize

	streamSessionID string // session whose stream we drilled into
	streamLabel     string
	full            []store.EventView // whole session, unfiltered, the inspector navigates this
	timeline        []store.EventView // full after the stream filter, what the table shows
	streamSignals   streamSignalCounts
	streamCalls     int           // completed calls in the session
	streamP50       time.Duration // median call latency
	streamP95       time.Duration // 95th percentile call latency
	selEvent        int           // index into timeline, the table selection
	inspect         int           // index into full, the frame the inspector shows
	query           string        // stream filter
	total           int
	follow          bool
	streamSort      sortState

	paused bool

	overlay        overlayMode
	replayReq      store.CallView // last request sent to replay, so r can re-run it from the result
	replaying      bool           // an async replay is in flight; a footer spinner shows until the result lands
	vp             viewport.Model
	overlayRaw     string // overlay body before styling (re-rendered on resize)
	overlayDisplay string // styled body shown in the viewport (highlight, numbers)
	overlayContent string // plain body with identical wrapping, for search matching
	overlayHeaderH int    // fixed chrome lines above the viewport (inspector meta)
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
	spin          int  // shared spinner frame, advanced by tickMsg
	dirty         bool // a frame arrived since the last refresh, set by frameMsg
}

// setFlash shows a transient message in the status bar for ~2s.
func (m *Model) setFlash(s string) {
	m.flash = s
	m.flashUntil = time.Now().Add(2500 * time.Millisecond)
}

// dismissTransient clears a pending flash and abandons an in-flight replay, so a
// stale toast or spinner never carries over to the next screen. Navigation calls
// this; actions that set their own flash (copy, export, delete) do not go through
// here, so their message survives.
func (m *Model) dismissTransient() {
	m.flash = ""
	m.flashUntil = time.Time{}
	m.replaying = false
}

func (m Model) flashActive() bool {
	return m.flash != "" && time.Now().Before(m.flashUntil)
}

// New builds the model around a store the hub feeds.
// Version is the build version shown in the help overlay. main sets it once from
// its own version resolution, so New stays test friendly.
var Version = "dev"

func New(st *store.Store) Model {
	ti := textinput.New()
	ti.Prompt = ""

	th := newTheme()
	m := Model{
		store:  st,
		keys:   defaultKeys(),
		theme:  th,
		styles: newStyles(th),
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
			m.spin++
			// The spinner advances every tick, the store refresh only every fifth and
			// only when a frame arrived since, so a burst of traffic cannot force a
			// refresh per envelope (which is quadratic over a session).
			if m.spin%refreshEvery == 0 && m.dirty {
				m.refresh()
				m.dirty = false
			}
			// An open live overlay rebuilds every tick so a pending spinner animates
			// at the tick cadence. The content-diff guard makes this a no-op when
			// nothing changed.
			m.refreshLiveOverlay()
		}
		return m, tick()

	case frameMsg:
		// Only mark the store dirty. The throttled tick above does the actual
		// refresh, so cost stays bounded no matter how fast frames arrive.
		if !m.paused {
			m.dirty = true
		}
		return m, nil

	case historyTruncatedMsg:
		m.setFlash(fmt.Sprintf("loaded newest %d of %d sessions; older traces stay on disk", msg.loaded, msg.total))
		return m, nil

	case replayDoneMsg:
		if !m.replaying {
			return m, nil // the replay was abandoned by navigating away
		}
		m.replaying = false
		m.openOverlay(overlayReplay, m.replayContent(msg))
		m.vp.GotoTop()
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
		case m.overlay == overlayInspector && msg.String() == "x":
			// Jump to the paired frame over the whole session, so the pair is
			// reachable even when the stream filter hides it from the table.
			if pi, ok := m.pairIndex(m.inspect); ok {
				m.inspect = pi
				m.openOverlay(overlayInspector, m.inspectorBody())
			}
		case m.overlay == overlayInspector && key.Matches(msg, m.keys.Replay):
			if cmd := m.startReplay(); cmd != nil {
				return m, cmd
			}
		case m.overlay == overlayReplay && key.Matches(msg, m.keys.Replay):
			if cmd := m.replayAgain(); cmd != nil {
				return m, cmd
			}
		case key.Matches(msg, m.keys.Copy):
			m.copyCurrent()
		case key.Matches(msg, m.keys.Enter, m.keys.Back, m.keys.Quit),
			key.Matches(msg, m.keys.Caps, m.keys.Summary):
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
	case msg.String() == "[":
		m.switchSession(-1)
	case msg.String() == "]":
		m.switchSession(1)

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
	case key.Matches(msg, m.keys.Summary):
		if m.currentSessionID() != "" {
			m.openOverlay(overlaySummary, m.summaryContent())
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
	// sessions and stream stay as quiet aliases for esc and enter, they are not
	// advertised in the palette since a single key already does the same.
	case "sessions", "session", "s", "..":
		m.view = viewSessions
		m.refresh()
		return m, nil
	case "stream", "frames":
		if m.currentSessionID() != "" {
			m.enterStream(m.selSession)
		}
		return m, nil
	case "summary":
		if m.currentSessionID() != "" {
			m.openOverlay(overlaySummary, m.summaryContent())
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
		// enter and esc already handle drill in and back, so the palette lists only
		// what a single key cannot do, export with args, a jump by session name,
		// and quit.
		m.input.Placeholder = "export [format] [path] · <name> · q"
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
	return "text · or tool:echo status:err dir:s2c kind:resp id:7 task:01J…"
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
		m.inspect = m.fullIndexOf(m.timeline[m.selEvent].Seq)
		m.openOverlay(overlayInspector, m.inspectorBody())
	}
}

// fullIndexOf finds a frame in the unfiltered timeline by its unique seq, so the
// inspector can navigate past the stream filter.
func (m Model) fullIndexOf(seq uint64) int {
	for i, e := range m.full {
		if e.Seq == seq {
			return i
		}
	}
	return 0
}

// back pops one level, clear an active filter, then stream→sessions. At
// the root it does NOTHING, quitting is deliberately only `:q`/Ctrl-C so you
// can't fall out of the UI by mashing esc/q.
func (m *Model) back() tea.Cmd {
	m.dismissTransient()
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
	m.dismissTransient()
	m.selSession = idx
	m.streamSessionID = m.sessions[idx].ID
	m.streamLabel = m.sessions[idx].Label
	m.view = viewStream
	m.follow = true
	m.refresh()
}

// switchSession steps to the neighbouring session with [ and ]. In the stream it
// jumps straight into that session's stream, in the list it moves the selection.
func (m *Model) switchSession(delta int) {
	n := len(m.sessions)
	if n == 0 {
		return
	}
	if m.view != viewStream {
		m.step(delta)
		return
	}
	cur := m.selSession
	for i, s := range m.sessions {
		if s.ID == m.streamSessionID {
			cur = i
			break
		}
	}
	m.query = ""
	m.enterStream((cur + delta + n) % n)
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

// currentLabel is the human name for the session the overlays act on: the
// selected row in the sessions list, otherwise the streamed session's label.
func (m Model) currentLabel() string {
	if m.view == viewSessions && len(m.sessions) > 0 {
		return m.sessions[m.selSession].Label
	}
	return m.streamLabel
}

// startReplay launches an async replay of the selected request frame.
// focusedFrame is the frame the current context acts on, the inspected frame
// when the inspector is open, otherwise the stream table selection.
func (m Model) focusedFrame() (store.EventView, bool) {
	if m.overlay == overlayInspector && m.inspect >= 0 && m.inspect < len(m.full) {
		return m.full[m.inspect], true
	}
	if m.view == viewStream && m.selEvent >= 0 && m.selEvent < len(m.timeline) {
		return m.timeline[m.selEvent], true
	}
	return store.EventView{}, false
}

// canReplay reports whether the focused frame is a request this session can
// actually replay (its server command was captured), so r is only offered, and
// acted on, when it will work.
func (m Model) canReplay() bool {
	ev, ok := m.focusedFrame()
	if !ok || ev.Call == nil || ev.Kind != store.EventRequest {
		return false
	}
	return m.sessionReplayable()
}

// sessionReplayable reports whether the streamed session recorded the server
// command a replay needs. It gates r at the session level, stable with no
// per-frame flicker, so replay is never offered for a session that can never run
// it (for example a log opened without its meta frame).
func (m Model) sessionReplayable() bool {
	_, _, ok := m.store.Command(m.streamSessionID)
	return ok
}

func (m *Model) startReplay() tea.Cmd {
	ev, ok := m.focusedFrame()
	if !ok {
		return nil
	}
	if ev.Call == nil || ev.Kind != store.EventRequest {
		m.setFlash("replay needs a request frame")
		return nil
	}
	return m.runReplay(*ev.Call)
}

// replayAgain re-runs the last replayed request, so r works straight from the
// replay result overlay without going back to the stream.
func (m *Model) replayAgain() tea.Cmd {
	if m.replayReq.Method == "" {
		return nil
	}
	return m.runReplay(m.replayReq)
}

func (m *Model) runReplay(call store.CallView) tea.Cmd {
	if m.replaying {
		return nil // a replay is already in flight; ignore until it lands or is abandoned
	}
	command, cwd, ok := m.store.Command(m.streamSessionID)
	if !ok {
		m.setFlash("no recorded server command to replay")
		return nil
	}
	m.replayReq = call
	m.replaying = true
	return replayCmd(command, cwd, call.Method, call.Params)
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

	// Cache the sparkline and client label per session at the data cadence, so the
	// render (which runs on every spinner tick) never touches the store.
	m.activity = make(map[string][]int, len(m.allSessions))
	m.clients = make(map[string]string, len(m.allSessions))
	for _, s := range m.allSessions {
		m.activity[s.ID] = m.store.Activity(s.ID, sparkBuckets, sparkSpan)
		if caps, ok := m.store.Capabilities(s.ID); ok {
			m.clients[s.ID] = infoLine(caps.ClientInfo)
		}
	}

	if m.view != viewStream {
		return
	}
	full := m.store.Timeline(m.streamSessionID)
	m.full = full
	m.total = len(full)
	// m.inspect indexes m.full, and several inspector readers index it directly, so
	// keep it in range when the timeline shrinks (e.g. a session delete), the same
	// way m.selEvent is clamped below.
	m.inspect = clamp(m.inspect, 0, max(len(m.full)-1, 0))
	m.timeline = m.filterEvents(full)
	// Count signals over the whole session, not the filtered view, so a stream
	// filter never hides the session's health in the footer.
	m.streamSignals = countStreamSignals(full)
	m.streamCalls, m.streamP50, m.streamP95 = callStats(full)
	m.sortStream()
	// A non-chronological sort means we're inspecting, not tailing.
	if m.streamSort.col != "" && m.streamSort.col != "time" {
		m.follow = false
	}
	// Follow snaps to the newest frame, but not while an overlay is open, or a
	// tick would drag the selection off the frame the inspector is showing and
	// fight an x jump to the paired frame.
	if m.follow && m.overlay == overlayNone {
		m.selEvent = len(m.timeline) - 1
	}
	m.selEvent = clamp(m.selEvent, 0, max(len(m.timeline)-1, 0))
}

// refreshLiveOverlay rebuilds an open live overlay from the current store state,
// so a capabilities or tool-summary view left open keeps up with the session
// instead of going stale until it is closed and reopened. It reads the store
// directly, so it works in any view, and it re-renders only when the content
// actually changed so the scroll position never jumps on an idle tick.
func (m *Model) refreshLiveOverlay() {
	var content string
	switch m.overlay {
	case overlayCaps:
		content = m.capsContent()
	case overlaySummary:
		content = m.summaryContent()
	default:
		return
	}
	if content == m.overlayRaw {
		return
	}
	off := m.vp.YOffset
	m.setOverlayBody(content)
	m.vp.SetYOffset(off)
}

// callStats computes the count, median, and 95th percentile latency over the
// completed calls in events.
func callStats(events []store.EventView) (n int, p50, p95 time.Duration) {
	var ds []time.Duration
	for _, e := range events {
		if e.Kind == store.EventResponse && e.Call != nil && e.Call.Done() {
			ds = append(ds, e.Call.End.Sub(e.Call.Start))
		}
	}
	if len(ds) == 0 {
		return 0, 0, 0
	}
	slices.Sort(ds)
	return len(ds), percentile(ds, 50), percentile(ds, 95)
}

// percentile returns the p-th percentile of sorted by nearest rank, so p95
// surfaces the tail even with few samples.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	return sorted[clamp(rank-1, 0, len(sorted)-1)]
}

type streamSignalCounts struct {
	errors  int
	bad     int
	warn    int
	pending int
}

func countStreamSignals(events []store.EventView) streamSignalCounts {
	var c streamSignalCounts
	for _, e := range events {
		switch {
		case e.Kind != store.EventInvalid && (e.Warning != "" || e.Truncated || e.Deprecated != ""):
			c.warn++
		case e.Kind == store.EventInvalid:
			c.bad++
		case e.Kind == store.EventResponse && e.Call != nil && e.Call.Failed():
			c.errors++
		case e.Kind == store.EventRequest && e.Call != nil && e.Call.State == store.Pending:
			c.pending++
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
	if e.Warning != "" || e.Truncated || e.Deprecated != "" {
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
			// A retry belongs to the operation it continues, so filtering by the
			// original id gathers the whole multi round-trip chain rather than
			// just its first leg. Its own id still finds it on its own.
			return strings.EqualFold(e.ID, v) || strings.EqualFold(e.MRTRRoot, v)
		case "task":
			return strings.EqualFold(e.TaskID, v)
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
	return strings.Contains(strings.ToLower(e.TaskID), q) ||
		e.Call != nil && strings.Contains(strings.ToLower(e.Call.ToolName), q)
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
		// A capped observation reads as a warning in the row, so status:warn finds it
		// too, even though it rides a structured flag rather than the warning text.
		return e.Warning != "" || e.Truncated || e.Deprecated != ""
	}
	if v == "mismatch" {
		// A routing header disagreeing with the body (Mcp-Method/Mcp-Name, SEP-2243).
		// It is a structured subset of warnings, so it stays discoverable on its own.
		return e.RoutingMismatch
	}
	if e.Call == nil {
		return false
	}
	switch v {
	case "err", "error", "fail", "failed":
		// The "something went wrong" axis, not the Failed state: a cancelled call is
		// Failed() but not an error, so it belongs under status:cancelled, not here.
		return e.Call.Errored
	case "cancelled", "canceled":
		// The row already labels a cancelled task "cancelled"; find it the same way.
		return e.Call.TaskStatus == "cancelled"
	case "pending", "pend", "inflight":
		return e.Call.State == store.Pending
	case "ok", "success":
		return e.Call.State == store.Completed && !e.Call.Failed()
	}
	return false
}

// openOverlay shows a centered scrollable panel with the given body. The
// inspector reserves one chrome line for its meta header.
func (m *Model) openOverlay(mode overlayMode, content string) {
	m.dismissTransient()
	m.overlay = mode
	m.overlaySearch = ""
	m.overlayMatches = nil
	m.overlayMatchIx = 0
	m.overlayHeaderH = 0
	if mode == overlayInspector {
		m.overlayHeaderH = m.inspectorHeaderH() // meta line, plus routing headers when present
	}
	m.layoutOverlay()
	m.setOverlayBody(content)
	m.vp.GotoTop()
}

// setOverlayBody stores the raw body and renders both a styled display form for
// the viewport and a plain form with identical wrapping for search matching. The
// inspector numbers and syntax-highlights its JSON, other panels arrive already
// styled and are only soft-wrapped.
func (m *Model) setOverlayBody(content string) {
	m.overlayRaw = content
	if m.overlay == overlayInspector {
		m.overlayDisplay = m.numberBody(content, m.vp.Width, true)
		m.overlayContent = m.numberBody(content, m.vp.Width, false)
	} else {
		m.overlayDisplay = softWrap(content, m.vp.Width)
		m.overlayContent = ansi.Strip(m.overlayDisplay)
	}
	m.vp.SetContent(m.overlayDisplay)
	// Shrink the viewport to its content so a short payload sizes the modal to
	// itself, capped at maxVpH so long payloads still scroll inside the box.
	_, maxVpH := m.overlayDims()
	m.vp.Height = max(min(m.vp.TotalLineCount(), maxVpH), 1)
}

// copyCurrent copies the most relevant thing for the current context to the
// system clipboard, the frame JSON (inspector / stream), the open panel, or the
// session log path (sessions list).
func (m *Model) copyCurrent() {
	var text, label string
	switch {
	case m.overlay == overlayInspector && m.inspect < len(m.full):
		text, label = frameText(m.full[m.inspect]), "frame JSON"
	case m.overlay != overlayNone:
		// The panel body (caps, replay) is already styled, strip the ANSI so the
		// clipboard gets clean text.
		text, label = ansi.Strip(m.overlayRaw), "panel"
	case m.view == viewStream && m.selEvent < len(m.timeline):
		text, label = frameText(m.timeline[m.selEvent]), "frame JSON"
	case m.view == viewSessions && len(m.sessions) > 0:
		text, label = paths.SessionLogPath(m.sessions[m.selSession].ID), "log path"
	case m.view == viewSessions && len(m.allSessions) == 0:
		text, label = onboardingSnippet, "snippet" // first-run empty state
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
	label := valueOr(m.deleteTargetLabel(), "session")
	m.store.Delete(id)
	_ = os.Remove(paths.SessionLogPath(id))
	if m.view == viewStream {
		m.view = viewSessions
	}
	m.refresh()
	m.setFlash("✓ deleted " + label)
}

// closeOverlay dismisses the overlay and clears any in-overlay search.
func (m *Model) closeOverlay() {
	m.dismissTransient()
	m.overlay = overlayNone
	m.overlayRaw = ""
	m.overlayDisplay = ""
	m.overlayContent = ""
	m.overlayHeaderH = 0
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
		m.vp.SetContent(m.overlayDisplay)
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
	w, maxVpH := m.overlayDims()
	if m.vp.Width == 0 {
		m.vp = viewport.New(w, maxVpH)
	} else {
		m.vp.Width, m.vp.Height = w, maxVpH
	}
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}
