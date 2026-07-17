package tui

import "github.com/charmbracelet/lipgloss"

// styles holds the concrete lipgloss styles the views render with, all built
// from the ANSI-bound theme. The color law has three layers. Chrome is blue and
// cyan, data identity is neutral (responses fg, notify and stderr comment), and
// verdict hues (green ok, yellow warn, red err, magenta bad, cyan pending)
// appear only in the STATUS column, the footer counters, and the dots. bad is
// magenta, not red, so a corrupt frame never reads as a call error, and the !
// glyph and label still tell them apart under NO_COLOR. No bold, since terminals
// render bold red as a brighter orange that reads as a second alarm hue.
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
	warn     lipgloss.Style
	invalid  lipgloss.Style
	notif    lipgloss.Style
	pending  lipgloss.Style
}

func newStyles(t theme) styles {
	fg := func(c lipgloss.TerminalColor) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	// The faint tier separates by its raised color when color is on. Applying the
	// faint attribute there too would re-dim that color on true-color terminals,
	// undoing the lift, so it is used only under NO_COLOR, where there is no color
	// to separate by and SGR 2 is the fallback.
	faintStyle := fg(t.faint)
	if t.noColor {
		faintStyle = faintStyle.Faint(true)
	}
	return styles{
		logo:        fg(t.blue).Bold(true),
		brand:       fg(t.blue).Bold(true),
		infoVal:     fg(t.bright).Bold(true),
		hintKey:     fg(t.blueKey),
		hintDesc:    fg(t.dim),
		tableHead:   faintStyle,
		sectionHead: fg(t.bright).Bold(true),
		crumbCur:    fg(t.bright).Bold(true),
		crumbPrev:   fg(t.dim),
		prompt:      fg(t.bright),
		panelTitle:  fg(t.bright).Bold(true),

		bright: fg(t.bright),
		dim:    fg(t.dim),
		faint:  faintStyle,
		sep:    faintStyle,

		live:   fg(t.green),
		paused: fg(t.yellow),
		follow: fg(t.cyan),

		match:    lipgloss.NewStyle().Background(t.selection).Foreground(t.yellow),
		matchCur: lipgloss.NewStyle().Background(t.yellow).Foreground(lipgloss.Color("0")),
		req:      fg(t.blue),    // request kind identity and marker
		neutral:  fg(t.fg),      // response, kind identity is neutral
		resp:     fg(t.green),   // ok verdict
		respErr:  fg(t.red),     // err verdict
		warn:     fg(t.yellow),  // protocol warning
		invalid:  fg(t.magenta), // bad verdict, magenta so it never reads as an err
		notif:    fg(t.dim),     // notification, stderr, and invalid glyph, neutral comment
		pending:  fg(t.cyan),    // in-flight verdict, spinner, follow
	}
}
