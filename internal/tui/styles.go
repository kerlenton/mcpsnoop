package tui

import "github.com/charmbracelet/lipgloss"

// styles holds the concrete lipgloss styles the views render with, all built
// from the ANSI-bound theme. The color law has three layers. Chrome is blue and
// cyan, data identity is neutral (responses fg, notify and stderr comment), and
// verdict hues (green ok, yellow slow and warn, red err and bad, cyan pending)
// appear only in the STATUS column, the footer counters, and the dots. err and
// bad share the same red and are told apart by the ! glyph and the label, no
// bold, since terminals render bold red as a brighter orange that reads as a
// second alarm hue.
type styles struct {
	logo        lipgloss.Style
	brand       lipgloss.Style
	infoVal     lipgloss.Style
	hintKey     lipgloss.Style
	hintDesc    lipgloss.Style
	tableHead   lipgloss.Style
	sectionHead lipgloss.Style
	crumbCur    lipgloss.Style
	crumbPrev   lipgloss.Style
	prompt      lipgloss.Style
	panelTitle  lipgloss.Style

	bright lipgloss.Style
	dim    lipgloss.Style
	faint  lipgloss.Style
	sep    lipgloss.Style

	live   lipgloss.Style
	paused lipgloss.Style
	follow lipgloss.Style

	match    lipgloss.Style
	matchCur lipgloss.Style
	req      lipgloss.Style
	neutral  lipgloss.Style
	resp     lipgloss.Style
	respErr  lipgloss.Style
	slow     lipgloss.Style
	warn     lipgloss.Style
	invalid  lipgloss.Style
	notif    lipgloss.Style
	pending  lipgloss.Style
}

func newStyles(t theme) styles {
	fg := func(c lipgloss.TerminalColor) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	return styles{
		logo:        fg(t.blue).Bold(true),
		brand:       fg(t.blue).Bold(true),
		infoVal:     fg(t.bright).Bold(true),
		hintKey:     fg(t.blue),
		hintDesc:    fg(t.dim),
		tableHead:   fg(t.dim).Faint(true),
		sectionHead: fg(t.blue).Bold(true),
		crumbCur:    fg(t.bright).Bold(true),
		crumbPrev:   fg(t.dim),
		prompt:      fg(t.bright),
		panelTitle:  fg(t.blue).Bold(true),

		bright: fg(t.bright),
		dim:    fg(t.dim),
		faint:  fg(t.dim).Faint(true),
		sep:    fg(t.dim).Faint(true),

		live:   fg(t.green),
		paused: fg(t.yellow),
		follow: fg(t.cyan),

		match:    lipgloss.NewStyle().Background(t.surface).Foreground(t.yellow),
		matchCur: lipgloss.NewStyle().Background(t.yellow).Foreground(lipgloss.Color("0")),
		req:      fg(t.blue),   // request kind identity and marker
		neutral:  fg(t.fg),     // response, kind identity is neutral
		resp:     fg(t.green),  // ok verdict
		respErr:  fg(t.red),    // err verdict
		slow:     fg(t.yellow), // slow verdict
		warn:     fg(t.yellow), // protocol warning
		invalid:  fg(t.red),    // bad verdict, same red as err, told apart by the ! glyph
		notif:    fg(t.dim),    // notification, stderr, and invalid glyph, neutral comment
		pending:  fg(t.cyan),   // in-flight verdict, spinner, follow
	}
}
