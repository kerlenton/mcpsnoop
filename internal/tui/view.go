package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func (m Model) View() string {
	if !m.ready {
		return "starting mcpsnoop…"
	}
	if m.confirmDelete {
		return m.renderConfirmDelete()
	}
	if m.showHelp {
		return m.renderHelp()
	}
	if m.overlay != overlayNone {
		return m.renderOverlay()
	}

	bodyH := m.bodyHeight()
	body := m.renderTablePanel(bodyH)
	// A 1-line header, a spacer that carries the filter input when active, the
	// framed table, a toast line one row above the footer, then the 1-line footer.
	return strings.Join([]string{m.renderHeader(), m.renderInputLine(), body, m.toastLine(), m.renderStatus()}, "\n")
}

// renderTablePanel frames the active table in a rounded panel, the title and any
// stats embedded in the top border.
func (m Model) renderTablePanel(bodyH int) string {
	// First run, no sessions captured yet, show the standalone onboarding card
	// centered on its own, not wrapped in the sessions panel.
	if m.view == viewSessions && len(m.allSessions) == 0 {
		return lipgloss.Place(m.width, bodyH, lipgloss.Center, lipgloss.Center, m.onboardingCard())
	}
	innerW := max(m.width-2, 1)
	innerH := max(bodyH-2, 1)
	var table, title, rightTitle string
	if m.view == viewStream {
		table = m.renderStreamTable(innerW, innerH)
		title = "stream · " + m.streamLabel
		if m.streamCalls > 0 {
			noun := "calls"
			if m.streamCalls == 1 {
				noun = "call"
			}
			rightTitle = fmt.Sprintf("%d %s · p50 %s · p95 %s", m.streamCalls, noun, shortDur(m.streamP50), shortDur(m.streamP95))
		}
	} else {
		table = m.renderSessionsTable(innerW, innerH)
		title = fmt.Sprintf("sessions · %d", len(m.sessions))
		rightTitle = homeAbbrev(paths.SessionsDir())
	}
	return m.panelBox(title, rightTitle, m.styles.dim, m.theme.border, padLines(table, innerH), m.width, bodyH)
}

// homeAbbrev shortens a path under the home directory to a leading ~.
func homeAbbrev(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h) {
		return "~" + p[len(h):]
	}
	return p
}

// renderConfirmDelete is the centered red-bordered confirmation shown before a
// session and its log are removed.
func (m Model) renderConfirmDelete() string {
	prompt := m.styles.bright.Render("delete session ") +
		m.styles.infoVal.Render(`"`+m.deleteTargetLabel()+`"`) +
		m.styles.bright.Render(" and its log?") + "    " +
		m.styles.hintKey.Render("y") + m.styles.hintDesc.Render(" / ") + m.styles.hintKey.Render("n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.red).Padding(0, 2)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box.Render(prompt))
}

// deleteTargetLabel names the session the confirmation is about.
func (m Model) deleteTargetLabel() string {
	if m.view == viewStream {
		return m.streamLabel
	}
	if len(m.sessions) > 0 {
		return m.sessions[m.selSession].Label
	}
	return ""
}

// toastLine is a transient bottom-right notification one row above the footer,
// green on surface for success and red for failure. It is blank when idle.
func (m Model) toastLine() string {
	if !m.flashActive() {
		return ""
	}
	fg := m.theme.green
	if strings.Contains(m.flash, "failed") {
		fg = m.theme.red
	}
	toast := lipgloss.NewStyle().Foreground(fg).Background(m.theme.surface).Padding(0, 1).Render(m.flash)
	gap := max(m.width-lipgloss.Width(toast)-1, 0)
	return strings.Repeat(" ", gap) + toast
}

// padLines pads s with blank lines up to n lines total.
func padLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// bar lays left and right onto a single width-wide line, one padding space each
// end and a flexible gap between. The right side (status, counts) is primary and
// kept whole. The left (hints, breadcrumb) is truncated first when the two would
// not fit, so the header and footer stay exactly one line at any width.
func bar(width int, left, right string) string {
	if width < 1 {
		return ""
	}
	if lipgloss.Width(right) > width-2 {
		right = ansi.Truncate(right, max(width-2, 0), "…")
	}
	if avail := width - lipgloss.Width(right) - 3; lipgloss.Width(left) > avail {
		left = ansi.Truncate(left, max(avail, 0), "…")
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return " " + left + strings.Repeat(" ", gap) + right + " "
}

func (m Model) bodyHeight() int {
	return max(m.height-4, 1) // header + input spacer + body + spacer + footer
}

// renderStatus is the bottom footer, contextual key hints on the left and the
// signal counters on the right. A transient flash takes over the right briefly.
func (m Model) renderStatus() string {
	return bar(m.width, m.footerHints(), m.footerCounters())
}

// footerHints lists the handful of keys that matter in the current view, key in
// blue and label dim. ? opens the full reference.
func (m Model) footerHints() string {
	// Nothing to act on until a session connects, the snippet copy hint lives on
	// the card, so the footer stays minimal.
	if m.view == viewSessions && len(m.allSessions) == 0 {
		return m.hintsRow([]hint{{":", "cmd"}, {"?", "help"}, {":q", "quit"}})
	}
	hs := []hint{
		{"enter", "open"}, {"y", "copy path"}, {"e", "export"},
		{"ctrl-d", "delete"}, {"/", "filter"}, {":", "cmd"}, {"?", "help"},
	}
	if m.view == viewStream {
		hs = []hint{
			{"enter", "inspect"}, {"r", "replay"}, {"c", "caps"},
			{"/", "filter"}, {"p", "pause"}, {"?", "help"},
		}
	}
	return m.hintsRow(hs)
}

// hintsRow renders key hints, key in blue and label dim, two spaces between.
func (m Model) hintsRow(hs []hint) string {
	var parts []string
	for _, h := range hs {
		parts = append(parts, m.styles.hintKey.Render(h.key)+" "+m.styles.hintDesc.Render(h.desc))
	}
	return strings.Join(parts, "  ")
}

// footerCounters is the signal tally on the right, the frame or session count
// then any nonzero err/bad/warn/slow/pending counts, each in its verdict color.
func (m Model) footerCounters() string {
	sep := m.styles.sep.Render(" · ")
	if m.view != viewStream {
		parts := []string{m.styles.dim.Render(countLabel(len(m.sessions), len(m.allSessions), "session"))}
		if e := m.totalErrors(); e > 0 {
			parts = append(parts, m.styles.respErr.Render(fmt.Sprintf("%d err", e)))
		}
		return strings.Join(parts, sep)
	}
	parts := []string{m.styles.dim.Render(countLabel(len(m.timeline), m.total, "frame"))}
	c := m.streamSignals
	for _, sig := range []struct {
		n     int
		label string
		style lipgloss.Style
	}{
		{c.errors, "err", m.styles.respErr},
		{c.bad, "bad", m.styles.invalid},
		{c.warn, "warn", m.styles.warn},
		{c.slow, "slow", m.styles.slow},
		{c.pending, "pending", m.styles.pending},
	} {
		if sig.n > 0 {
			parts = append(parts, sig.style.Render(fmt.Sprintf("%d %s", sig.n, sig.label)))
		}
	}
	return strings.Join(parts, sep)
}

// countLabel renders a plain total, or shown/total when a filter is hiding some
// of the rows. The breadcrumb already carries the session total, so the footer
// stays quiet until a filter actually narrows the view. noun is singular and is
// pluralized to match the total.
func countLabel(shown, total int, noun string) string {
	if total != 1 {
		noun += "s"
	}
	if shown != total {
		return fmt.Sprintf("%d/%d %s", shown, total, noun)
	}
	return fmt.Sprintf("%d %s", total, noun)
}

// ---- header ---------------------------------------------------------------

// renderHeader is the single top line, brand and breadcrumb on the left, follow
// and the live indicator on the right.
func (m Model) renderHeader() string {
	left := m.styles.brand.Render("▍mcpsnoop") + "  " + m.breadcrumb()
	return bar(m.width, left, m.headerStatus())
}

// breadcrumb is sessions at the root and sessions › demo inside a session, with
// the active filter appended when one is set.
func (m Model) breadcrumb() string {
	var seg string
	switch {
	case m.view != viewStream:
		seg = m.styles.crumbCur.Render("sessions")
	case m.width < 100:
		// Narrow, collapse to the label plus its position, cueing [ and ].
		pos, total := m.sessionPos()
		seg = m.styles.crumbCur.Render(m.streamLabel) + m.styles.faint.Render(fmt.Sprintf(" (%d/%d)", pos, total))
	default:
		seg = m.styles.crumbPrev.Render("sessions") + m.styles.faint.Render(" › ") + m.styles.crumbCur.Render(m.streamLabel)
	}
	if q := m.activeFilter(); q != "" {
		seg += m.styles.faint.Render("  /") + m.styles.dim.Render(q)
	}
	return seg
}

// sessionPos is the 1-based index of the streamed session among the visible
// sessions, and the total, for the compact breadcrumb.
func (m Model) sessionPos() (int, int) {
	for i, s := range m.sessions {
		if s.ID == m.streamSessionID {
			return i + 1, len(m.sessions)
		}
	}
	return 0, len(m.sessions)
}

// headerStatus is the right side of the header, follow when on, then the live,
// paused, or listening indicator.
func (m Model) headerStatus() string {
	var parts []string
	if m.view == viewStream && m.follow {
		parts = append(parts, m.styles.follow.Render("⇣ follow"))
	}
	switch {
	case m.paused:
		parts = append(parts, m.styles.paused.Render("● paused"))
	case len(m.allSessions) == 0:
		parts = append(parts, m.styles.follow.Render(m.spinnerFrame()+" listening"))
	default:
		parts = append(parts, m.styles.live.Render("● live"))
	}
	return strings.Join(parts, m.styles.sep.Render(" · "))
}

// spinnerFrames is the shared braille spinner, advanced by the tick clock and
// reused for the listening indicator and in-flight PENDING calls.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

func (m Model) spinnerFrame() string {
	return string(spinnerFrames[m.spin%len(spinnerFrames)])
}

func (m Model) totalErrors() int {
	n := 0
	for _, s := range m.allSessions {
		n += s.Errors
	}
	return n
}

type hint struct{ key, desc string }

// ---- input line -----------------------------------------------------------

// renderInputLine is the spacer under the header. It carries the filter or
// command input when active, with a live match count for filters, and is
// otherwise blank.
func (m Model) renderInputLine() string {
	if m.inputMode == inputNone {
		return ""
	}
	left := m.styles.prompt.Render(m.input.View())
	if m.inputMode != inputFilter {
		return bar(m.width, left, "")
	}
	shown, total := len(m.sessions), len(m.allSessions)
	if m.view == viewStream {
		shown, total = len(m.timeline), m.total
	}
	right := m.styles.dim.Render(fmt.Sprintf("%d/%d match", shown, total)) + m.styles.faint.Render("   esc clear")
	return bar(m.width, left, right)
}

func (m Model) activeFilter() string {
	if m.view == viewStream {
		return m.query
	}
	return m.sessionQuery
}

// ---- sessions table -------------------------------------------------------

func (m Model) renderSessionsTable(w, h int) string {
	// Empty state, no table header (it'd be an orphan), just a centered card.
	if len(m.sessions) == 0 {
		card := m.onboardingCard()
		if m.sessionQuery != "" {
			card = m.styles.dim.Render("no sessions match ") + m.styles.infoVal.Render("/"+m.sessionQuery)
		}
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, card)
	}

	const nameW, clientW, reqW, respW, errW, actW, lastW = 22, 20, 4, 4, 4, 8, 6
	// ACTIVITY drops first as the panel narrows, then CLIENT.
	showAct := w >= 88
	showClient := w >= 72

	st := m.sessionSort
	gap := seg("  ", m.styles.faint)
	var b strings.Builder
	head := cellL("NAME"+sortMark(st, "name"), nameW)
	if showClient {
		head += "  " + cellL("CLIENT", clientW)
	}
	head += "  " + cellR("REQ"+sortMark(st, "req"), reqW) + "  " + cellR("RESP"+sortMark(st, "resp"), respW) +
		"  " + cellR("ERR"+sortMark(st, "err"), errW)
	if showAct {
		head += "  " + cellL("ACTIVITY", actW)
	}
	head += "  " + cellR("LAST"+sortMark(st, "last"), lastW)
	b.WriteString(" " + m.styles.tableHead.Render(head) + "\n")

	rows := h - 1
	start, end := window(m.selSession, len(m.sessions), rows)
	for i := start; i < end; i++ {
		s := m.sessions[i]
		errStyle := m.styles.faint
		if s.Errors > 0 {
			errStyle = m.styles.respErr // red when the session carries errors
		}
		lastStyle := m.styles.dim
		if time.Since(s.Last) > 30*time.Minute {
			lastStyle = m.styles.faint // idle sessions recede
		}
		buckets := m.activity[s.ID]
		actStyle := m.styles.faint // idle
		for _, v := range buckets {
			if v > 0 {
				actStyle = m.styles.resp // recent traffic, green
				break
			}
		}
		segs := []cell{seg(cellL(s.Label, nameW), m.styles.neutral)}
		if showClient {
			client := valueOr(m.clients[s.ID], "-")
			segs = append(segs, gap, seg(cellL(client, clientW), m.styles.dim))
		}
		segs = append(segs,
			gap, seg(cellR(fmt.Sprintf("%d", s.Requests), reqW), m.styles.dim),
			gap, seg(cellR(fmt.Sprintf("%d", s.Responses), respW), m.styles.dim),
			gap, seg(cellR(fmt.Sprintf("%d", s.Errors), errW), errStyle),
		)
		if showAct {
			segs = append(segs, gap, seg(cellL(spark(buckets), actW), actStyle))
		}
		segs = append(segs, gap, seg(cellR(humanAge(s.Last), lastW), lastStyle))
		b.WriteString(m.rowLine(segs, w, i == m.selSession) + "\n")
	}
	return b.String()
}

// ---- stream table ---------------------------------------------------------

// Stream column widths. DETAIL flexes to fill the rest of the row.
const (
	streamTimeW   = 12
	streamDirW    = 1
	streamMethodW = 34
	streamIDW     = 4
	streamDurW    = 9
	streamStatW   = 7
)

// streamLayout carries the width-dependent choices for the stream table, a
// shorter TIME on narrow terminals and DETAIL hidden on narrower ones.
type streamLayout struct {
	timeW      int
	timeFmt    string
	detailW    int
	showDetail bool
}

func (m Model) streamLayoutFor(w int) streamLayout {
	lay := streamLayout{timeW: streamTimeW, timeFmt: "15:04:05.000", showDetail: m.width >= 100}
	if m.width < 90 {
		lay.timeW, lay.timeFmt = 7, "05.0000" // ss.SSSS
	}
	fixed := lay.timeW + streamDirW + streamMethodW + streamIDW + streamDurW + streamStatW
	if lay.showDetail {
		lay.detailW = max((w-1)-fixed-11, 8) // 11 is the six column gaps
	}
	return lay
}

func (m Model) renderStreamTable(w, h int) string {
	lay := m.streamLayoutFor(w)
	st := m.streamSort
	var b strings.Builder
	head := cellL("TIME"+sortMark(st, "time"), lay.timeW) + "  " + cellL("", streamDirW) + " " +
		cellL("METHOD / TOOL"+sortMark(st, "method"), streamMethodW) + "  " + cellR("ID"+sortMark(st, "id"), streamIDW) +
		"  " + cellR("DUR"+sortMark(st, "dur"), streamDurW) + "  " + cellL("STATUS"+sortMark(st, "status"), streamStatW)
	if lay.showDetail {
		head += "  " + cellL("DETAIL", lay.detailW)
	}
	b.WriteString(" " + m.styles.tableHead.Render(head) + "\n")

	if len(m.timeline) == 0 {
		if m.query != "" {
			b.WriteString(m.styles.faint.Render(" no frames match /" + m.query))
		} else {
			b.WriteString(m.styles.faint.Render(" no frames yet"))
		}
		return b.String()
	}

	rows := h - 1
	start, end := window(m.selEvent, len(m.timeline), rows)
	for i := start; i < end; i++ {
		b.WriteString(m.rowLine(m.streamRow(m.timeline[i], lay), w, i == m.selEvent) + "\n")
	}
	return b.String()
}

// cell is one styled segment of a table row, its text already padded to width.
type cell struct {
	text  string
	style lipgloss.Style
}

func seg(text string, style lipgloss.Style) cell { return cell{text: text, style: style} }

// rowLine renders one row from its segments. A selected row gets a blue ▌ marker
// in the gutter and a subtle surface band across the full width, and every cell
// keeps its own hue so verdict and kind colors stay readable and stable when the
// selection moves. Unselected rows are the plain segments behind a one-space
// gutter. Either way the line is capped to the width so it never wraps.
func (m Model) rowLine(segs []cell, w int, selected bool) string {
	var b strings.Builder
	if selected {
		b.WriteString(m.styles.req.Background(m.theme.surface).Render("▌"))
	} else {
		b.WriteString(" ")
	}
	for _, s := range segs {
		st := s.style
		if selected {
			st = st.Background(m.theme.surface)
		}
		b.WriteString(st.Render(s.text))
	}
	line := b.String()
	if selected {
		if pad := w - lipgloss.Width(line); pad > 0 {
			line += lipgloss.NewStyle().Background(m.theme.surface).Render(strings.Repeat(" ", pad))
		}
	}
	if lipgloss.Width(line) > w {
		line = ansi.Truncate(line, w, "")
	}
	return line
}

// streamRow turns a frame into its row segments. The glyph and METHOD carry the
// kind color, the tool name is bright, DUR and STATUS carry the verdict, and
// DETAIL is uniformly faint (progress notifications aside).
func (m Model) streamRow(e store.EventView, lay streamLayout) []cell {
	c := m.streamCells(e)
	kind := m.kindStyle(e)

	segs := []cell{
		seg(cellL(e.TS.Format(lay.timeFmt), lay.timeW), m.styles.dim),
		seg("  ", m.styles.faint),
		seg(cellL(c.dir, streamDirW), kind),
		seg(" ", m.styles.faint),
	}
	if c.tool != "" {
		prefix := c.method + " "
		segs = append(segs,
			seg(prefix, kind),
			seg(cellL(c.tool, streamMethodW-lipgloss.Width(prefix)), m.styles.bright),
		)
	} else {
		segs = append(segs, seg(cellL(c.method, streamMethodW), kind))
	}
	segs = append(segs,
		seg("  ", m.styles.faint),
		seg(cellR(c.id, streamIDW), m.styles.dim),
		seg("  ", m.styles.faint),
		seg(cellR(c.dur, streamDurW), m.durStyle(e)),
		seg("  ", m.styles.faint),
		seg(cellL(c.status, streamStatW), m.statusStyle(e)),
	)
	if lay.showDetail {
		segs = append(segs, seg("  ", m.styles.faint))
		segs = append(segs, m.detailSegs(c, lay.detailW)...)
	}
	return segs
}

// durStyle colors the DUR value. The slow verdict already lives in STATUS, so
// DUR only turns cyan for a live pending timer and is dim otherwise.
func (m Model) durStyle(e store.EventView) lipgloss.Style {
	if e.Call != nil && e.Call.State == store.Pending {
		return m.styles.pending
	}
	return m.styles.dim
}

// detailSegs is the DETAIL column, a single faint line uniform across every row,
// or a small bar for a progress notification.
func (m Model) detailSegs(c streamCell, detailW int) []cell {
	if c.progress != nil {
		return m.progressSegs(*c.progress, detailW)
	}
	return []cell{seg(cellL(c.detail, detailW), m.styles.faint)}
}

// progressSegs draws a thin ━━──── bar and the done/total fraction, the filled
// part cyan and the rest faint, matching the panel line-art. The heavy and light
// rules also read without color.
func (m Model) progressSegs(p progressBar, detailW int) []cell {
	const barW = 8
	filled := 0
	if p.total > 0 {
		filled = clamp(p.done*barW/p.total, 0, barW)
	}
	done := strings.Repeat("━", filled)
	rest := strings.Repeat("─", barW-filled) + fmt.Sprintf("  %d/%d", p.done, p.total)
	if p.token != "" {
		rest += " · " + p.token
	}
	rest = cellL(rest, max(detailW-lipgloss.Width(done), 0))
	return []cell{
		seg(done, m.styles.pending),
		seg(rest, m.styles.faint),
	}
}

type streamCell struct {
	time, dir, method, id, dur, status, detail string
	tool                                       string       // tool name, rendered bright after the method
	progress                                   *progressBar // set for a progress notification carrying a total
}

type progressBar struct {
	done, total int
	token       string
}

func (m Model) streamCells(e store.EventView) streamCell {
	c := streamCell{time: e.TS.Format("15:04:05.000"), id: e.ID}
	switch e.Kind {
	case store.EventStderr:
		c.dir, c.method = "┆", "stderr"
		c.detail = e.Text
	case store.EventRequest:
		c.dir = arrow(e.Dir)
		c.method = e.Method
		if e.Call != nil && e.Call.IsTool && e.Call.ToolName != "" {
			c.method, c.tool = "tools/call", e.Call.ToolName
		}
		if e.Call != nil {
			c.detail = compactJSON(e.Call.Params)
			// Surface in-flight (possibly hung) calls, a spinner plus live elapsed.
			if e.Call.State == store.Pending {
				c.status = "pending"
				c.dur = m.spinnerFrame() + " " + e.Call.Duration().Round(100*time.Millisecond).String()
			}
		}
	case store.EventResponse:
		c.dir = arrow(e.Dir)
		c.method = "response"
		if e.Call != nil {
			c.dur = e.Call.Duration().Round(time.Millisecond).String()
			switch {
			case e.Call.Err != nil:
				c.status = "err"
				c.detail = e.Call.Err.Message
			case e.Call.ToolErr:
				c.status = "err"
				c.detail = toolErrorText(e.Call.Result)
			case e.Call.Slow(m.store.SlowThreshold()):
				c.status = "slow"
				c.detail = compactJSON(e.Call.Result)
			default:
				c.status = "ok"
				c.detail = compactJSON(e.Call.Result)
			}
		}
	case store.EventNotification:
		c.dir, c.method = "·", "notify "+e.Method
		c.detail = compactJSON(e.Raw)
		if e.Method == "notifications/progress" {
			if p, ok := parseProgress(e.Raw); ok {
				c.progress = &p
			}
		}
	case store.EventInvalid:
		c.dir, c.method, c.status = "!", "invalid rpc", "bad"
		if len(e.Raw) > 0 {
			c.detail = string(e.Raw)
		} else {
			c.detail = e.Text
		}
	default:
		c.dir, c.method = "?", "frame"
		c.detail = string(e.Raw)
	}
	if e.Kind != store.EventInvalid && e.Warning != "" {
		c.status = "warn"
		if c.detail == "" {
			c.detail = e.Warning
		} else {
			c.detail = e.Warning + " · " + c.detail
		}
	}
	return c
}

// parseProgress reads a notifications/progress payload. It reports ok only when a
// total is present, so the caller can fall back to raw JSON otherwise.
func parseProgress(raw json.RawMessage) (progressBar, bool) {
	var msg struct {
		Params struct {
			Progress      float64         `json:"progress"`
			Total         float64         `json:"total"`
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Params.Total == 0 {
		return progressBar{}, false
	}
	token := strings.Trim(string(msg.Params.ProgressToken), `"`)
	return progressBar{done: int(msg.Params.Progress), total: int(msg.Params.Total), token: token}, true
}

// toolErrorText pulls the human message out of a tool-error result
// ({"content":[{"type":"text","text":"…"}],"isError":true}).
func toolErrorText(result json.RawMessage) string {
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(result, &r) == nil && len(r.Content) > 0 && r.Content[0].Text != "" {
		return r.Content[0].Text
	}
	return compactJSON(result)
}

// compactJSON renders raw JSON on a single line for the DETAIL preview column.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return strings.TrimSpace(string(raw))
	}
	return b.String()
}

func (m Model) statusStyle(e store.EventView) lipgloss.Style {
	if e.Kind == store.EventInvalid {
		return m.styles.invalid
	}
	if e.Warning != "" {
		return m.styles.warn
	}
	if e.Call != nil {
		switch {
		case e.Call.State == store.Pending:
			return m.styles.pending
		case e.Call.Failed():
			return m.styles.respErr
		case e.Call.Slow(m.store.SlowThreshold()):
			return m.styles.slow
		default:
			return m.styles.resp
		}
	}
	return m.styles.dim
}

// kindStyle colors the direction glyph and METHOD by frame kind, requests blue,
// responses neutral fg, notifications and invalid frames comment, stderr comment
// italic. Verdict color never lands here, only in STATUS, DUR, and the counters.
func (m Model) kindStyle(e store.EventView) lipgloss.Style {
	switch e.Kind {
	case store.EventRequest:
		return m.styles.req
	case store.EventResponse:
		return m.styles.neutral
	default: // notification, stderr, invalid, all neutral comment, told apart by glyph
		return m.styles.notif
	}
}

// ---- help -----------------------------------------------------------------

type helpGroup struct {
	title string
	keys  [][2]string
}

// renderHelp is the keybindings reference, a centered bordered overlay (the
// inspector chrome) with a two-column grid of sections and a title line. The keys
// are the ones the model actually binds, descriptions avoid slashes except in key
// forms like x / y.
func (m Model) renderHelp() string {
	nav := helpGroup{"NAVIGATION", [][2]string{
		{"j / k · ↑ / ↓", "move up or down"},
		{"g / G", "go to top or bottom"},
		{"ctrl-f / ctrl-b", "page down or up"},
		{"[ / ]", "previous or next session"},
		{"enter", "open session or frame"},
		{"esc", "back up or clear filter"},
	}}
	frameActions := helpGroup{"FRAME ACTIONS", [][2]string{
		{"r", "replay the selected tool call"},
		{"c", "show negotiated capabilities"},
		{"p", "pause or resume the stream"},
		{"f", "toggle follow"},
	}}
	manage := helpGroup{"MANAGE", [][2]string{
		{"y", "copy frame JSON or log path"},
		{"e", "export session as HTML"},
		{"ctrl-d", "delete the selected session"},
	}}
	views := helpGroup{"VIEWS & SEARCH", [][2]string{
		{"/", "filter the current table"},
		{":", "command mode"},
		{"shift+N/R/S/E/L", "sort sessions by column"},
		{"shift+T/M/I/D/S", "sort stream by column"},
		{"?", "toggle this help"},
	}}
	inFrame := helpGroup{"IN A FRAME", [][2]string{
		{"/", "search within the frame"},
		{"n / N", "next or previous match"},
		{"x", "jump to the paired frame"},
		{"y", "copy the frame JSON"},
	}}
	general := helpGroup{"GENERAL", [][2]string{
		{":q · ctrl-c", "quit"},
	}}
	filter := helpGroup{"STREAM FILTER QUERY", [][2]string{
		{"<text>", "substring over method, tool, id, payload"},
		{"tool:echo", "by tool name"},
		{"status:err|warn|slow|ok|pending|bad", "by outcome"},
		{"kind:req|resp|notify|stderr|invalid", "by message type"},
		{"dir:c2s|s2c", "by direction"},
		{"method:tools/call", "by method"},
		{"id:7", "by id"},
	}}

	// One shared key-column width across both halves, so every description starts
	// at the same column and the grid reads evenly.
	keyW := max(helpKeyWidth(nav, frameActions, manage), helpKeyWidth(views, inFrame, general))
	left := m.helpColumn(keyW, nav, frameActions, manage)
	right := m.helpColumn(keyW, views, inFrame, general)
	grid := lipgloss.JoinHorizontal(lipgloss.Top, left, "    ", right)
	filterBlock := m.helpSection(helpKeyWidth(filter), filter)

	contentW := max(lipgloss.Width(grid), lipgloss.Width(filterBlock))
	ver := Version
	if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	title := m.styles.infoVal.Render("KEYBINDINGS")
	verText := m.styles.faint.Render("mcpsnoop " + ver)
	titleGap := max(contentW-lipgloss.Width(title)-lipgloss.Width(verText), 1)
	titleLine := title + strings.Repeat(" ", titleGap) + verText

	body := lipgloss.JoinVertical(lipgloss.Left, titleLine, "", grid, "", filterBlock)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.blue).Padding(0, 2).
		Render(body)
	footer := " " + m.styles.faint.Render("? or esc to close")
	panel := lipgloss.JoinVertical(lipgloss.Left, box, footer)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panel)
}

// helpKeyWidth is the widest key cell across the given groups.
func helpKeyWidth(gs ...helpGroup) int {
	w := 0
	for _, g := range gs {
		for _, k := range g.keys {
			w = max(w, lipgloss.Width(k[0]))
		}
	}
	return w
}

// helpSection renders one group, a blue title over key/description rows whose
// descriptions all start at keyW so the column stays even.
func (m Model) helpSection(keyW int, g helpGroup) string {
	rows := []string{m.styles.sectionHead.Render(g.title)}
	for _, k := range g.keys {
		gap := keyW - lipgloss.Width(k[0]) + 2
		rows = append(rows, "  "+m.styles.hintKey.Render(k[0])+strings.Repeat(" ", gap)+m.styles.hintDesc.Render(k[1]))
	}
	return strings.Join(rows, "\n")
}

// helpColumn stacks sections into one column with a blank line between them, all
// sharing keyW so descriptions line up across groups.
func (m Model) helpColumn(keyW int, gs ...helpGroup) string {
	parts := make([]string, len(gs))
	for i, g := range gs {
		parts[i] = m.helpSection(keyW, g)
	}
	return strings.Join(parts, "\n\n")
}

// ---- inspector / capabilities / replay content ----------------------------

// inspectorBody is the scrollable content of the frame inspector, the pretty
// JSON (or the raw stderr line) in plain form. It is numbered and highlighted for
// display and searched in plain form.
func (m Model) inspectorBody() string {
	if m.inspect < 0 || m.inspect >= len(m.full) {
		return ""
	}
	e := m.full[m.inspect]
	var b strings.Builder
	if e.Text != "" {
		b.WriteString(e.Text)
		if len(e.Raw) > 0 {
			b.WriteString("\n")
		}
	}
	if len(e.Raw) > 0 {
		b.WriteString(prettyJSON(e.Raw))
	}
	return b.String()
}

// inspectorHeader is the single fixed line above the inspector body, the frame
// meta joined by faint dots on the left and the pair widget plus timestamp on the
// right, sized to width w.
func (m Model) inspectorHeader(w int) string {
	e := m.full[m.inspect]
	c := m.streamCells(e)
	sep := m.styles.faint.Render(" · ")
	parts := []string{m.styles.dim.Render(dirLabel(e.Dir))}
	if e.Call != nil {
		if e.Call.Method != "" {
			parts = append(parts, m.styles.req.Render(e.Call.Method))
		}
		parts = append(parts, m.styles.dim.Render("id "+e.Call.ID),
			m.styles.dim.Render(e.Call.Duration().Round(time.Millisecond).String()))
	}
	if c.status != "" {
		parts = append(parts, m.statusStyle(e).Render(c.status))
	}
	left := m.styles.infoVal.Render(fmt.Sprintf("FRAME %d/%d", m.inspect+1, len(m.full))) + "  " + strings.Join(parts, sep)
	right := m.pairWidget() + sep + m.styles.faint.Render(e.TS.Format("15:04:05.000"))
	return bar(w, left, right)
}

// pairWidget is the right side of the inspector header, req N ⇄ resp N with the
// current frame bright bold and the partner (what x jumps to) blue, or req N ⇄
// pending in cyan while a request awaits its response, or a plain seq N for a
// frame with no pair.
func (m Model) pairWidget() string {
	e := m.full[m.inspect]
	plain := m.styles.faint.Render(fmt.Sprintf("seq %d", e.Seq))
	if e.Call == nil {
		return plain
	}
	arrow := m.styles.faint.Render(" ⇄  ")
	switch e.Kind {
	case store.EventRequest:
		cur := m.styles.infoVal.Render(fmt.Sprintf("req %d", e.Seq))
		if e.Call.State == store.Pending {
			return cur + arrow + m.styles.pending.Render("pending")
		}
		if pi, ok := m.pairIndex(m.inspect); ok {
			return cur + arrow + m.styles.req.Render(fmt.Sprintf("resp %d", m.full[pi].Seq))
		}
	case store.EventResponse:
		cur := m.styles.infoVal.Render(fmt.Sprintf("resp %d", e.Seq))
		if pi, ok := m.pairIndex(m.inspect); ok {
			return m.styles.req.Render(fmt.Sprintf("req %d", m.full[pi].Seq)) + arrow + cur
		}
	}
	return plain
}

// pairIndex finds the request for a response (or vice versa) so x can jump
// between the two halves of one exchange. Each event carries its own CallView
// copy, so the match is on the call identity, its id and start time.
func (m Model) pairIndex(sel int) (int, bool) {
	if sel < 0 || sel >= len(m.full) || m.full[sel].Call == nil {
		return 0, false
	}
	c := m.full[sel].Call
	want := store.EventRequest
	if m.full[sel].Kind == store.EventRequest {
		want = store.EventResponse
	}
	for i, e := range m.full {
		if i != sel && e.Kind == want && e.Call != nil && e.Call.ID == c.ID && e.Call.Start.Equal(c.Start) {
			return i, true
		}
	}
	return 0, false
}

func dirLabel(d proxy.Direction) string {
	if d == proxy.ServerToClient {
		return "s2c"
	}
	return "c2s"
}

// renderOverlay draws the active overlay, a centered rounded panel with fixed
// chrome above a scrollable body and a hint footer below.
func (m Model) renderOverlay() string {
	cw, _ := m.overlayDims()
	ch := m.overlayHeaderH + m.vp.Height + 1 // header lines, the content-sized viewport, the indicator
	inner := m.vp.View()
	if m.overlay == overlayInspector {
		inner = m.inspectorHeader(cw) + "\n" + inner
	}
	inner += "\n" + m.scrollLine(cw)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.blue).
		Width(cw).Height(ch)
	panel := lipgloss.JoinVertical(lipgloss.Left, box.Render(inner), m.overlayFooter(cw))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panel)
}

// panelBox renders body inside a rounded border with title embedded in the top
// edge and an optional faint rightTitle before the closing corner, sized to width
// w and height h. Body lines are clipped to the height with a faint count of what
// is hidden.
func (m Model) panelBox(title, rightTitle string, titleStyle lipgloss.Style, border lipgloss.TerminalColor, body string, w, h int) string {
	bs := lipgloss.NewStyle().Foreground(border)
	inner := max(w-2, 1)
	head := bs.Render("╭─") + " " + titleStyle.Render(ansi.Truncate(title, max(inner-3, 1), "…")) + " "
	tail := bs.Render("╮")
	if rightTitle != "" {
		rightTitle = ansi.Truncate(rightTitle, max(w-lipgloss.Width(head)-6, 0), "…")
		tail = " " + m.styles.faint.Render(rightTitle) + " " + bs.Render("─╮")
	}
	top := head + bs.Render(strings.Repeat("─", max(w-lipgloss.Width(head)-lipgloss.Width(tail), 0))) + tail

	lines := strings.Split(body, "\n")
	bodyH := max(h-2, 1)
	var rows []string
	for i := range bodyH {
		var content string
		switch {
		case i == bodyH-1 && len(lines) > bodyH:
			content = m.styles.faint.Render(fmt.Sprintf("… %d more lines", len(lines)-bodyH+1))
		case i < len(lines):
			content = lines[i]
		}
		rows = append(rows, bs.Render("│")+fitCell(content, inner)+bs.Render("│"))
	}
	bottom := bs.Render("╰" + strings.Repeat("─", inner) + "╯")
	return top + "\n" + strings.Join(rows, "\n") + "\n" + bottom
}

// fitCell pads or ansi-truncates s to exactly width w.
func fitCell(s string, w int) string {
	if lipgloss.Width(s) > w {
		return ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}

// overlayDims is the centered panel content width, capped near 110 columns, and
// the maximum viewport height. The whole modal (two borders, the header, the
// viewport, the indicator line, and the footer) is capped at rows-4, so a short
// payload sizes to its content instead of stretching to full screen.
func (m Model) overlayDims() (w, maxVpH int) {
	w = max(min(110, m.width-4)-2, 1)
	maxVpH = max(m.height-8-m.overlayHeaderH, 1) // borders(2) + header + indicator(1) + footer(1)
	return w, maxVpH
}

// overlayFooter is the hint line under the panel, or the search prompt and live
// match count while searching.
func (m Model) overlayFooter(w int) string {
	if m.inputMode == inputSearch {
		right := ""
		if n := len(m.overlayMatches); n > 0 {
			right = m.styles.dim.Render(fmt.Sprintf("%d/%d match", m.overlayMatchIx+1, n))
		} else if m.overlaySearch != "" {
			right = m.styles.dim.Render("no match")
		}
		return bar(w, m.styles.prompt.Render(m.input.View()), right)
	}
	hs := []hint{{"↑↓", "scroll"}, {"y", "copy"}, {"esc", "close"}}
	switch {
	case m.overlay == overlayInspector:
		hs = []hint{{"↑↓", "scroll"}, {"/", "search"}, {"n/N", "match"}, {"y", "copy"}, {"r", "replay"}, {"x", "pair"}, {"esc", "close"}}
	case m.overlay == overlayReplay && m.replayReq.Method != "":
		hs = []hint{{"↑↓", "scroll"}, {"r", "replay again"}, {"y", "copy"}, {"esc", "close"}}
	}
	right := ""
	if m.flashActive() {
		right = m.styles.live.Render(m.flash)
	}
	return bar(w, m.hintsRow(hs), right)
}

// scrollLine is the faint indicator of how much body is hidden below the fold,
// blank when everything fits.
func (m Model) scrollLine(w int) string {
	total, visible := m.vp.TotalLineCount(), m.vp.Height
	if total <= visible {
		return ""
	}
	pct := int(m.vp.ScrollPercent() * 100)
	hidden := max(total-visible-m.vp.YOffset, 0)
	txt := fmt.Sprintf("▲▼ %d%%", pct)
	if hidden > 0 {
		txt = fmt.Sprintf("▼ %d more lines · %d%%", hidden, pct)
	}
	return m.styles.faint.Render(" " + txt)
}

func (m Model) capsContent() string {
	sid := m.currentSessionID()
	label := m.streamLabel
	if m.view == viewSessions && len(m.sessions) > 0 {
		label = m.sessions[m.selSession].Label
	}
	caps, ok := m.store.Capabilities(sid)
	if !ok {
		return m.styles.faint.Render(" no handshake observed yet for this session")
	}
	var b strings.Builder
	// client and server share one style so the two roles read consistently, the
	// dim label plus a neutral name matches the protocolVersion line above.
	role := func(name, info string) string {
		return " " + m.styles.dim.Render(name+" · ") + m.styles.neutral.Render(valueOr(info, "unknown")) + "\n"
	}
	b.WriteString(m.styles.panelTitle.Render(" capabilities · "+label) + "\n\n")
	b.WriteString(" " + m.styles.dim.Render("protocolVersion ") + m.styles.neutral.Render(valueOr(caps.ProtocolVersion, "unknown")) + "\n\n")
	b.WriteString(role("client", infoLine(caps.ClientInfo)))
	b.WriteString(m.capsBlock(caps.Client) + "\n\n")
	b.WriteString(role("server", infoLine(caps.ServerInfo)))
	b.WriteString(m.capsBlock(caps.Server))
	return b.String()
}

func infoLine(raw json.RawMessage) string {
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &info) != nil || info.Name == "" {
		return ""
	}
	if info.Version != "" {
		return info.Name + " v" + info.Version
	}
	return info.Name
}

// capsBlock is one capability object, its pretty JSON syntax-highlighted and
// indented, or a faint (none) when empty.
func (m Model) capsBlock(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "{}" {
		return "   " + m.styles.faint.Render("(none)")
	}
	var out []string
	for _, ln := range strings.Split(prettyJSON(raw), "\n") {
		out = append(out, "   "+m.highlightJSON(ln))
	}
	return strings.Join(out, "\n")
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ---- small helpers --------------------------------------------------------

func arrow(d proxy.Direction) string {
	if d == proxy.ServerToClient {
		return "←"
	}
	return "→"
}

// highlightMatches wraps every case-insensitive occurrence of q in line with
// style. Content is mostly plain JSON, so byte indexing is safe enough here.
func highlightMatches(line, q string, style lipgloss.Style) string {
	if q == "" {
		return line
	}
	low, lq := strings.ToLower(line), strings.ToLower(q)
	var b strings.Builder
	for i := 0; ; {
		j := strings.Index(low[i:], lq)
		if j < 0 {
			b.WriteString(line[i:])
			return b.String()
		}
		j += i
		b.WriteString(line[i:j])
		b.WriteString(style.Render(line[j : j+len(q)]))
		i = j + len(q)
	}
}

// numberBody wraps each logical line of body to width, prefixes a 3-wide faint
// line number (blank on wrapped continuations), and optionally syntax-highlights
// the JSON. The highlighted and plain forms wrap identically so search line
// indices line up.
func (m Model) numberBody(body string, width int, highlight bool) string {
	const gutterW = 5 // a 3-wide number plus two spaces
	textW := max(width-gutterW, 1)
	num := lipgloss.NewStyle().Foreground(m.theme.dim).Faint(true)
	var out []string
	for i, line := range strings.Split(body, "\n") {
		content := line
		if highlight {
			content = m.highlightJSON(line)
		}
		for j, disp := range strings.Split(ansi.Hardwrap(content, textW, false), "\n") {
			switch {
			case j > 0:
				out = append(out, strings.Repeat(" ", gutterW)+disp)
			case highlight:
				out = append(out, num.Render(fmt.Sprintf("%3d  ", i+1))+disp)
			default:
				out = append(out, fmt.Sprintf("%3d  ", i+1)+disp)
			}
		}
	}
	return strings.Join(out, "\n")
}

// highlightJSON colors one line of pretty-printed JSON, keys blue, string values
// green, numbers yellow, true/false/null red, and structural punctuation faint.
// It scans runes so a color never lands inside a multibyte sequence.
func (m Model) highlightJSON(line string) string {
	// Only keys carry color. Values (strings, numbers, true/false/null) stay
	// neutral so the verdict hues (yellow slow, red err) never leak into the body
	// and read as false signals, and punctuation recedes.
	key := lipgloss.NewStyle().Foreground(m.theme.blue)
	str := lipgloss.NewStyle().Foreground(m.theme.fg)
	num := str
	lit := str
	punc := lipgloss.NewStyle().Foreground(m.theme.dim).Faint(true)

	r := []rune(line)
	var b strings.Builder
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(r) {
				if r[j] == '\\' {
					j += 2
					continue
				}
				if r[j] == '"' {
					break
				}
				j++
			}
			end := min(j+1, len(r))
			text := string(r[i:end])
			k := end
			for k < len(r) && r[k] == ' ' {
				k++
			}
			if k < len(r) && r[k] == ':' {
				b.WriteString(key.Render(text)) // a key is a string followed by a colon
			} else {
				b.WriteString(str.Render(text))
			}
			i = end
		case c >= '0' && c <= '9' || (c == '-' && i+1 < len(r) && r[i+1] >= '0' && r[i+1] <= '9'):
			j := i + 1
			for j < len(r) && isNumberRune(r[j]) {
				j++
			}
			b.WriteString(num.Render(string(r[i:j])))
			i = j
		case c == 't' || c == 'f' || c == 'n':
			if w := literalAt(r, i); w != "" {
				b.WriteString(lit.Render(w))
				i += len([]rune(w))
				continue
			}
			b.WriteRune(c)
			i++
		case strings.ContainsRune("{}[],:", c):
			b.WriteString(punc.Render(string(c)))
			i++
		default:
			b.WriteRune(c)
			i++
		}
	}
	return b.String()
}

func isNumberRune(c rune) bool {
	return c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E'
}

// literalAt reports the JSON literal (true, false, null) starting at i, if any.
func literalAt(r []rune, i int) string {
	for _, lit := range []string{"true", "false", "null"} {
		n := len([]rune(lit))
		if i+n <= len(r) && string(r[i:i+n]) == lit && (i+n == len(r) || !isLetterRune(r[i+n])) {
			return lit
		}
	}
	return ""
}

func isLetterRune(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

var sparkRamp = []rune("⣀⣄⣤⣶⣿")

// spark renders bucket counts as a sparkline, scaled to the busiest bucket.
func spark(buckets []int) string {
	hi := 0
	for _, v := range buckets {
		hi = max(hi, v)
	}
	var b strings.Builder
	for _, v := range buckets {
		i := 0
		if hi > 0 {
			i = v * (len(sparkRamp) - 1) / hi
		}
		b.WriteRune(sparkRamp[i])
	}
	return b.String()
}

// shortDur formats a latency compactly, ms below a second and one decimal second
// above.
func shortDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Round(time.Millisecond)/time.Millisecond)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// sortMark returns the ▾/▴ arrow appended to the active column header.
func sortMark(st sortState, col string) string {
	if st.col != col {
		return ""
	}
	if st.desc {
		return " ▾"
	}
	return " ▴"
}

// onboardingSnippet is the config line shown in the empty state and copied by y.
const onboardingSnippet = `"command": "mcpsnoop", "args": ["--", "node", "build/index.js"]`

// onboardingCard is the first-run empty state, a self-bordered centered card
// telling the user how to attach mcpsnoop. Rendered via lipgloss.Place by the
// caller.
func (m Model) onboardingCard() string {
	num := m.styles.hintKey.Render
	dim := m.styles.dim.Render
	const cardW = 68

	brand := m.styles.brand.Render("▍mcpsnoop")
	waiting := dim("waiting for MCP traffic ") + m.styles.follow.Render(m.spinnerFrame())
	gap := max(cardW-lipgloss.Width(brand)-lipgloss.Width(waiting), 1)
	titleRow := brand + strings.Repeat(" ", gap) + waiting
	rule := m.styles.faint.Render(strings.Repeat("─", cardW))

	snippet := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.border).Padding(0, 1).
		Render(m.highlightJSON(onboardingSnippet))
	copyHint := "  " + num("y") + " " + dim("copy snippet")

	step1 := num("1.") + "  " + dim("wrap your server in your client's MCP config")
	step2 := num("2.") + "  " + dim("use your client, every tool call lands here live")

	label := func(s string) string { return dim(s + strings.Repeat(" ", max(18-len(s), 1))) }
	http := label("Streamable HTTP") + m.styles.hintKey.Render("mcpsnoop http --target <url>")
	demo := label("Just want a look") + m.styles.hintKey.Render("mcpsnoop demo")

	footer := m.styles.faint.Render("shim socket ready · " + homeAbbrev(paths.Base()))

	inner := lipgloss.JoinVertical(lipgloss.Left,
		titleRow, rule, "",
		step1, "", snippet, copyHint, "", step2, "",
		http, demo, "",
		footer,
	)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.border).
		Padding(1, 3).Render(inner)
}

// frameText is the copy-to-clipboard payload for a frame, pretty JSON, or the
// raw stderr line.
func frameText(e store.EventView) string {
	if len(e.Raw) > 0 {
		return prettyJSON(e.Raw)
	}
	return e.Text
}

func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// cellL / cellR pad (or truncate) s to width w, left/right aligned.
func cellL(s string, w int) string {
	s = truncate(s, w)
	if pad := w - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

func cellR(s string, w int) string {
	s = truncate(s, w)
	if pad := w - lipgloss.Width(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

func window(sel, n, rows int) (int, int) {
	if rows <= 0 || n == 0 {
		return 0, 0
	}
	if n <= rows {
		return 0, n
	}
	start := sel - rows/2
	if start < 0 {
		start = 0
	}
	if start+rows > n {
		start = n - rows
	}
	return start, start + rows
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	if w <= 1 || len(r) <= 1 {
		return string(r[:max(0, min(len(r), w))])
	}
	return string(r[:w-1]) + "…"
}

// softWrap hard-wraps any line wider than width so long values (e.g. a big JSON
// string) stay visible in the inspector instead of running off the edge. Lines
// already within width are left untouched. ANSI-aware.
func softWrap(s string, width int) string {
	if width <= 1 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if lipgloss.Width(line) > width {
			lines[i] = ansi.Hardwrap(line, width, false)
		}
	}
	return strings.Join(lines, "\n")
}
