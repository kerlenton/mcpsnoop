package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

const headerH = 6 // fixed header height (k9s-style banner)

func (m Model) View() string {
	if !m.ready {
		return "starting mcpsnoop…"
	}
	if m.overlay != overlayNone {
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).
			Width(m.width - 2).Height(m.height - 4)
		var footer string
		switch {
		case m.flashActive():
			footer = " " + m.styles.logo.Render(m.flash)
		case m.inputMode == inputSearch:
			footer = " " + m.styles.prompt.Render(m.input.View())
		case m.overlaySearch != "":
			n := len(m.overlayMatches)
			pos := m.styles.dim.Render(fmt.Sprintf("  ·  /%s  %d match(es)", m.overlaySearch, n))
			if n > 0 {
				pos = m.styles.dim.Render(fmt.Sprintf("  ·  /%s  %d/%d", m.overlaySearch, m.overlayMatchIx+1, n))
			}
			footer = m.styles.dim.Render(" n/N next/prev · esc/enter close") + pos
		default:
			footer = m.styles.dim.Render(" ↑↓/pgup/pgdn scroll · / search · y copy · esc/enter close")
		}
		return box.Render(m.vp.View()) + "\n" + footer
	}

	header := m.renderHeader()
	topbar := m.renderTopBar()
	bodyH := m.bodyHeight()

	var body string
	switch {
	case m.showHelp:
		body = m.renderHelp(bodyH)
	case m.view == viewStream:
		body = m.renderStreamTable(m.width, bodyH)
	default:
		body = m.renderSessionsTable(m.width, bodyH)
	}
	rule := m.styles.rule.Render(strings.Repeat("─", max(m.width, 1)))
	body = padLines(body, bodyH) // fill the region so the rule+status sit at the bottom
	// header · rule · breadcrumb · body · rule · status — horizontal rules frame
	// the content region and give the layout structure (k9s/lazygit style).
	return strings.Join([]string{header, rule, topbar, "", body, rule, m.renderStatus()}, "\n")
}

// padLines pads s with blank lines up to n lines total.
func padLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m Model) bodyHeight() int {
	return max(m.height-headerH-5, 1) // header + 2 rules + breadcrumb + blank + status
}

// renderStatus is the k9s-style bottom bar: counts on the left, mode flags on
// the right, across a full-width subtle band.
func (m Model) renderStatus() string {
	sep := m.styles.dim.Render("  ·  ")
	live := m.styles.resp.Render("● live")
	if m.paused {
		live = m.styles.slow.Render("⏸ paused")
	}
	count := fmt.Sprintf("%d/%d sessions", len(m.sessions), len(m.allSessions))
	if m.view == viewStream {
		count = fmt.Sprintf("%d/%d frames", len(m.timeline), m.total)
	}
	left := live + sep + m.styles.dim.Render(count)
	if e := m.totalErrors(); e > 0 {
		left += sep + m.styles.respErr.Render(fmt.Sprintf("%d err", e))
	}

	var right string
	if m.flashActive() {
		right = m.styles.logo.Render(m.flash) // transient "✓ copied" etc, in accent
	} else {
		var flags []string
		if m.view == viewStream && m.follow {
			flags = append(flags, "⟳ follow")
		}
		if st := m.sortFor(); st.col != "" {
			dir := "▲"
			if st.desc {
				dir = "▼"
			}
			flags = append(flags, "sort "+st.col+dir)
		}
		right = m.styles.dim.Render(strings.Join(flags, "  ·  "))
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return " " + left + strings.Repeat(" ", gap) + right + " "
}

func (m Model) sortFor() sortState {
	if m.view == viewStream {
		return m.streamSort
	}
	return m.sessionSort
}

// ---- header ---------------------------------------------------------------

func (m Model) renderHeader() string {
	hints := padBlock(m.headerHints(), headerH)
	logo := padBlock(m.headerLogo(), headerH)
	gap := m.width - lipgloss.Width(hints) - lipgloss.Width(logo) - 1
	if gap < 2 {
		gap = 2
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, hints, strings.Repeat(" ", gap), logo)
	return lipgloss.NewStyle().MarginLeft(1).Render(row)
}

func (m Model) totalErrors() int {
	n := 0
	for _, s := range m.allSessions {
		n += s.Errors
	}
	return n
}

type hint struct{ key, desc string }

func (m Model) headerHints() string {
	var hs []hint
	if m.view == viewStream {
		hs = []hint{
			{"enter", "Inspect"}, {"r", "Replay"},
			{"c", "Caps"}, {"y", "Copy JSON"},
			{"/", "Filter"}, {"p", "Pause"},
			{"f", "Follow"}, {"esc", "Back"},
			{":", "Command"}, {"?", "Help"},
			{"ctrl-d", "Delete"}, {":q", "Quit"},
		}
	} else {
		hs = []hint{
			{"enter", "Open"}, {"/", "Filter"},
			{"y", "Copy path"}, {":", "Command"},
			{"g/G", "Top/Bot"}, {"ctrl-f", "Page"},
			{"ctrl-d", "Delete"}, {"p", "Pause"},
			{"?", "Help"}, {":q", "Quit"},
		}
	}
	// Fixed-width key column so the descriptions line up regardless of key length.
	keyW := 0
	for _, h := range hs {
		if w := len(h.key) + 2; w > keyW { // +2 for the angle brackets
			keyW = w
		}
	}
	half := (len(hs) + 1) / 2
	col := func(items []hint) string {
		var rows []string
		for _, h := range items {
			key := m.styles.hintKey.Width(keyW).Render("<" + h.key + ">")
			rows = append(rows, key+" "+m.styles.hintDesc.Render(h.desc))
		}
		return lipgloss.JoinVertical(lipgloss.Left, rows...)
	}
	left := col(hs[:half])
	right := col(hs[half:])
	left = lipgloss.NewStyle().Width(maxLineWidth(left) + 3).Render(left)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) headerLogo() string {
	return m.styles.logo.Render("mcpsnoop")
}

// ---- breadcrumb -----------------------------------------------------------

func (m Model) renderTopBar() string {
	if m.inputMode != inputNone {
		return " " + m.styles.prompt.Render(m.input.View())
	}
	var seg string
	if m.view == viewStream {
		seg = m.styles.crumbPrev.Render("sessions") +
			m.styles.dim.Render("›") +
			m.styles.crumbCur.Render(fmt.Sprintf("%s[%d]", m.streamLabel, m.total))
	} else {
		seg = m.styles.crumbCur.Render(fmt.Sprintf("sessions[%d]", len(m.sessions)))
	}
	if q := m.activeFilter(); q != "" {
		seg += m.styles.dim.Render("  /" + q)
	}
	return " " + seg
}

func (m Model) activeFilter() string {
	if m.view == viewStream {
		return m.query
	}
	return m.sessionQuery
}

// ---- sessions table -------------------------------------------------------

func (m Model) renderSessionsTable(w, h int) string {
	inner := w - 1
	const nameW, reqW, respW, errW, lastW = 30, 4, 4, 4, 6

	st := m.sessionSort
	var b strings.Builder
	head := cellL("NAME"+sortMark(st, "name"), nameW) + "  " + cellR("REQ"+sortMark(st, "req"), reqW) +
		"  " + cellR("RESP"+sortMark(st, "resp"), respW) + "  " + cellR("ERR"+sortMark(st, "err"), errW) +
		"  " + cellR("LAST"+sortMark(st, "last"), lastW)
	b.WriteString(" " + m.styles.tableHead.Render(head) + "\n")

	if len(m.sessions) == 0 {
		if m.sessionQuery != "" {
			b.WriteString(m.styles.dim.Render(" no sessions match /" + m.sessionQuery))
		} else {
			b.WriteString(m.onboarding())
		}
		return b.String()
	}

	rows := h - 1
	start, end := window(m.selSession, len(m.sessions), rows)
	for i := start; i < end; i++ {
		s := m.sessions[i]
		req := fmt.Sprintf("%d", s.Requests)
		resp := fmt.Sprintf("%d", s.Responses)
		errs := fmt.Sprintf("%d", s.Errors)
		last := humanAge(s.Last)

		plain := cellL(s.Label, nameW) + "  " + cellR(req, reqW) + "  " + cellR(resp, respW) +
			"  " + cellR(errs, errW) + "  " + cellR(last, lastW)
		if i == m.selSession {
			b.WriteString(m.styles.rowSel.Width(w).Render(" "+truncate(plain, inner)) + "\n")
			continue
		}
		errCell := m.styles.dim.Render(cellR(errs, errW))
		if s.Errors > 0 {
			errCell = m.styles.respErr.Render(cellR(errs, errW))
		}
		row := m.styles.infoVal.Render(cellL(s.Label, nameW)) + "  " + m.styles.dim.Render(cellR(req, reqW)) + "  " +
			m.styles.dim.Render(cellR(resp, respW)) + "  " + errCell + "  " +
			m.styles.dim.Render(cellR(last, lastW))
		b.WriteString(" " + row + "\n")
	}
	return b.String()
}

// ---- stream table ---------------------------------------------------------

func (m Model) renderStreamTable(w, h int) string {
	inner := w - 1
	const timeW, dirW, methodW, idW, durW, statW = 12, 1, 34, 4, 9, 7
	detailW := max(inner-(timeW+dirW+methodW+idW+durW+statW)-11, 8)

	st := m.streamSort
	var b strings.Builder
	head := cellL("TIME"+sortMark(st, "time"), timeW) + "  " + cellL("", dirW) + " " +
		cellL("METHOD / TOOL"+sortMark(st, "method"), methodW) + "  " + cellR("ID"+sortMark(st, "id"), idW) +
		"  " + cellR("DUR"+sortMark(st, "dur"), durW) + "  " + cellL("STATUS"+sortMark(st, "status"), statW) +
		"  " + cellL("DETAIL", detailW)
	b.WriteString(" " + m.styles.tableHead.Render(head) + "\n")

	if len(m.timeline) == 0 {
		if m.query != "" {
			b.WriteString(m.styles.dim.Render(" no frames match /" + m.query))
		} else {
			b.WriteString(m.styles.dim.Render(" no frames yet"))
		}
		return b.String()
	}

	rows := h - 1
	start, end := window(m.selEvent, len(m.timeline), rows)
	for i := start; i < end; i++ {
		e := m.timeline[i]
		c := m.streamCells(e)
		plain := cellL(c.time, timeW) + "  " + cellL(c.dir, dirW) + " " + cellL(c.method, methodW) +
			"  " + cellR(c.id, idW) + "  " + cellR(c.dur, durW) + "  " + cellL(c.status, statW) +
			"  " + cellL(c.detail, detailW)
		if i == m.selEvent {
			b.WriteString(m.styles.rowSel.Width(w).Render(" "+truncate(plain, inner)) + "\n")
			continue
		}
		kc := lipgloss.NewStyle().Foreground(m.kindColor(e))
		row := m.styles.dim.Render(cellL(c.time, timeW)) + "  " + kc.Render(cellL(c.dir, dirW)) + " " +
			kc.Render(cellL(truncate(c.method, methodW), methodW)) + "  " +
			m.styles.dim.Render(cellR(c.id, idW)) + "  " +
			m.styles.dim.Render(cellR(c.dur, durW)) + "  " +
			m.statusStyle(e).Render(cellL(c.status, statW)) + "  " +
			m.styles.dim.Render(cellL(truncate(c.detail, detailW), detailW))
		b.WriteString(" " + row + "\n")
	}
	return b.String()
}

type streamCell struct{ time, dir, method, id, dur, status, detail string }

func (m Model) streamCells(e store.EventView) streamCell {
	c := streamCell{time: e.TS.Format("15:04:05.000"), id: e.ID}
	switch e.Kind {
	case store.EventStderr:
		c.dir, c.method = "⚠", "stderr"
		c.detail = e.Text
	case store.EventRequest:
		c.dir = arrow(e.Dir)
		c.method = e.Method
		if e.Call != nil && e.Call.IsTool && e.Call.ToolName != "" {
			c.method = "tools/call " + e.Call.ToolName
		}
		if e.Call != nil {
			c.detail = compactJSON(e.Call.Params)
			// Surface in-flight (possibly hung) calls: PENDING + live elapsed.
			if e.Call.State == store.Pending {
				c.status = "PENDING"
				c.dur = e.Call.Duration().Round(time.Millisecond).String()
			}
		}
	case store.EventResponse:
		c.dir = arrow(e.Dir)
		c.method = "response"
		if e.Call != nil {
			c.dur = e.Call.Duration().Round(time.Millisecond).String()
			switch {
			case e.Call.Err != nil:
				c.status = "ERR"
				c.detail = e.Call.Err.Message
			case e.Call.ToolErr:
				c.status = "ERR"
				c.detail = toolErrorText(e.Call.Result)
			case e.Call.Slow(m.store.SlowThreshold()):
				c.status = "SLOW"
				c.detail = compactJSON(e.Call.Result)
			default:
				c.status = "OK"
				c.detail = compactJSON(e.Call.Result)
			}
		}
	case store.EventNotification:
		c.dir, c.method = "·", "notify "+e.Method
		c.detail = compactJSON(e.Raw)
	default:
		c.dir, c.method = "?", "frame"
		c.detail = string(e.Raw)
	}
	return c
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

func (m Model) kindColor(e store.EventView) lipgloss.Color {
	switch e.Kind {
	case store.EventStderr:
		return colStderr
	case store.EventRequest:
		return colReq
	case store.EventResponse:
		if e.Call != nil && e.Call.Failed() {
			return colErr
		}
		if e.Call != nil && e.Call.Slow(m.store.SlowThreshold()) {
			return colSlow
		}
		return colResp
	case store.EventNotification:
		return colNotif
	default:
		return colFaint
	}
}

// ---- help -----------------------------------------------------------------

func (m Model) renderHelp(h int) string {
	groups := []struct {
		title string
		keys  [][2]string
	}{
		{"NAVIGATION", [][2]string{
			{"j / k, ↑ / ↓", "move up / down"},
			{"g / G", "go to top / bottom"},
			{"ctrl-f / ctrl-b", "page down / up"},
			{"enter", "drill in (open session / inspect frame)"},
			{"esc", "back up a level / clear filter"},
		}},
		{"VIEWS & SEARCH", [][2]string{
			{"/", "filter the current table"},
			{":", "command: sessions · stream · <name> · q"},
			{"shift+N/R/S/E/L", "sort sessions (name/req/resp/err/last)"},
			{"shift+T/M/I/D/S", "sort stream (time/method/id/dur/status)"},
			{"?", "toggle this help"},
		}},
		{"STREAM FILTER QUERY (/)", [][2]string{
			{"<text>", "substring over method / tool / id / payload"},
			{"tool:echo", "by tool name"},
			{"status:err|slow|ok|pending", "by outcome"},
			{"kind:req|resp|notify|stderr", "by message type"},
			{"dir:c2s|s2c", "by direction (who sent it — orthogonal to kind)"},
			{"method:tools/call  id:7", "by method / id (tokens are ANDed)"},
		}},
		{"FRAME ACTIONS (stream)", [][2]string{
			{"r", "replay the selected tool call"},
			{"c", "show negotiated capabilities"},
			{"p", "pause / resume the live stream"},
			{"f", "toggle follow (auto-scroll)"},
		}},
		{"IN A FRAME (inspector)", [][2]string{
			{"/", "search within the open frame"},
			{"n / N", "jump to next / previous match"},
			{"y", "copy the frame JSON to the clipboard"},
		}},
		{"MANAGE", [][2]string{
			{"y", "copy frame JSON / session log path"},
			{"ctrl-d", "delete the selected session (and its log)"},
		}},
		{"GENERAL", [][2]string{
			{":q · ctrl-c", "quit"},
		}},
	}
	var b strings.Builder
	b.WriteString(m.styles.panelTitle.Render(" mcpsnoop — keybindings") + "\n\n")
	for _, g := range groups {
		// Align descriptions within each group, sized to its widest key.
		keyW := 0
		for _, k := range g.keys {
			if w := lipgloss.Width(k[0]); w > keyW {
				keyW = w
			}
		}
		b.WriteString("  " + m.styles.tableHead.Render(g.title) + "\n")
		for _, k := range g.keys {
			gap := keyW - lipgloss.Width(k[0]) + 3
			b.WriteString("    " + m.styles.hintKey.Render(k[0]) +
				strings.Repeat(" ", gap) + m.styles.hintDesc.Render(k[1]) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(m.styles.dim.Render("  press ? or esc to close"))
	return b.String()
}

// ---- inspector / capabilities / replay content ----------------------------

func (m Model) inspectorContent() string {
	if m.selEvent >= len(m.timeline) {
		return ""
	}
	e := m.timeline[m.selEvent]
	var b strings.Builder
	b.WriteString(m.styles.panelTitle.Render(" FRAME") + "\n")
	b.WriteString(fmt.Sprintf(" seq=%d  dir=%s  time=%s\n", e.Seq, e.Dir, e.TS.Format("15:04:05.000")))
	if e.Call != nil {
		c := e.Call
		b.WriteString(fmt.Sprintf(" method=%s  id=%s  state=%s  dur=%s\n",
			c.Method, c.ID, c.State, c.Duration().Round(time.Millisecond)))
		if c.IsTool {
			b.WriteString(" tool=" + c.ToolName + "\n")
		}
	}
	b.WriteString("\n")
	if e.Text != "" {
		b.WriteString(e.Text + "\n")
	}
	if len(e.Raw) > 0 {
		b.WriteString(prettyJSON(e.Raw))
	}
	return b.String()
}

func (m Model) capsContent() string {
	sid := m.currentSessionID()
	label := m.streamLabel
	if m.view == viewSessions && len(m.sessions) > 0 {
		label = m.sessions[m.selSession].Label
	}
	var b strings.Builder
	b.WriteString(m.styles.panelTitle.Render(" CAPABILITIES — "+label) + "\n")
	caps, ok := m.store.Capabilities(sid)
	if !ok {
		b.WriteString(m.styles.dim.Render(" no handshake observed yet for this session"))
		return b.String()
	}
	b.WriteString(" protocolVersion: " + valueOr(caps.ProtocolVersion, "(unknown)") + "\n\n")
	b.WriteString(m.styles.req.Render(" CLIENT  "+infoLine(caps.ClientInfo)) + "\n")
	b.WriteString(capsBlock(caps.Client) + "\n\n")
	b.WriteString(m.styles.resp.Render(" SERVER  "+infoLine(caps.ServerInfo)) + "\n")
	b.WriteString(capsBlock(caps.Server))
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

func capsBlock(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "{}" {
		return "  (none)"
	}
	return indentLines(prettyJSON(raw), "  ")
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
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

// sortMark returns the ▲/▼ arrow appended to an active column header.
func sortMark(st sortState, col string) string {
	if st.col != col {
		return ""
	}
	if st.desc {
		return " ▼"
	}
	return " ▲"
}

// onboarding is the first-run empty state: it tells the user exactly how to
// attach mcpsnoop, instead of a bare "waiting…".
func (m Model) onboarding() string {
	key := m.styles.hintKey.Render
	val := m.styles.infoVal.Render
	dim := m.styles.dim.Render
	lines := []string{
		"",
		" " + m.styles.panelTitle.Render("Waiting for MCP traffic…") + dim("  (the spinner stays until a client connects)"),
		"",
		" " + val("1.") + dim(" Wrap your server in your MCP client config (Claude Desktop, Cursor, …):"),
		"      " + key(`"command": "mcpsnoop", "args": ["--", "node", "build/index.js"]`),
		"",
		" " + val("2.") + dim(" Use your client normally — every tool call shows up here, live."),
		"",
		" " + dim(" HTTP server?  ") + key("mcpsnoop http --target http://localhost:3000/mcp"),
	}
	return strings.Join(lines, "\n")
}

// frameText is the copy-to-clipboard payload for a frame: pretty JSON, or the
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

func padBlock(s string, n int) string {
	lines := strings.Split(s, "\n")
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines[:n], "\n")
}

func maxLineWidth(s string) int {
	w := 0
	for _, l := range strings.Split(s, "\n") {
		if lw := lipgloss.Width(l); lw > w {
			w = lw
		}
	}
	return w
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
