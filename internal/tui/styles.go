package tui

import "github.com/charmbracelet/lipgloss"

// palette — blue logo/keys, cyan crumbs & selection, with
// per-kind accents for the frame stream. Tuned for dark terminals.
var (
	colAccent = lipgloss.Color("39") // dodger blue — logo, hint keys, titles
	colCrumb  = lipgloss.Color("37") // cyan — current breadcrumb + selection bg
	colHeadCl = lipgloss.Color("73") // cadet blue — table column headers
	colBlack  = lipgloss.Color("16")
	colDim    = lipgloss.Color("244") // gray
	colFaint  = lipgloss.Color("240")
	colReq    = lipgloss.Color("111") // soft blue — request (distinct from cyan selection)
	colResp   = lipgloss.Color("114") // soft green — successful response
	colErr    = lipgloss.Color("203") // red — error
	colSlow   = lipgloss.Color("215") // warm amber — slow call
	colNotif  = lipgloss.Color("146") // muted lavender — notification
	colStderr = lipgloss.Color("216") // peach — server stderr
	colHeader = lipgloss.Color("231") // near-white
)

type styles struct {
	logo       lipgloss.Style
	infoKey    lipgloss.Style
	infoVal    lipgloss.Style
	hintKey    lipgloss.Style
	hintDesc   lipgloss.Style
	tableHead  lipgloss.Style
	crumbCur   lipgloss.Style
	crumbPrev  lipgloss.Style
	prompt     lipgloss.Style
	rule       lipgloss.Style
	panelTitle lipgloss.Style

	badgeErr lipgloss.Style

	rowSel   lipgloss.Style
	match    lipgloss.Style
	matchCur lipgloss.Style
	req      lipgloss.Style
	resp     lipgloss.Style
	respErr  lipgloss.Style
	slow     lipgloss.Style
	notif    lipgloss.Style
	stderr   lipgloss.Style
	pending  lipgloss.Style
	dim      lipgloss.Style
}

func newStyles() styles {
	return styles{
		logo:       lipgloss.NewStyle().Bold(true).Foreground(colAccent),
		infoKey:    lipgloss.NewStyle().Foreground(colCrumb),
		infoVal:    lipgloss.NewStyle().Foreground(colHeader).Bold(true),
		hintKey:    lipgloss.NewStyle().Foreground(colAccent),
		hintDesc:   lipgloss.NewStyle().Foreground(colDim),
		tableHead:  lipgloss.NewStyle().Bold(true).Foreground(colHeadCl),
		crumbCur:   lipgloss.NewStyle().Bold(true).Foreground(colBlack).Background(colCrumb).Padding(0, 1),
		crumbPrev:  lipgloss.NewStyle().Foreground(colHeader).Background(colFaint).Padding(0, 1),
		prompt:     lipgloss.NewStyle().Foreground(colHeader),
		rule:       lipgloss.NewStyle().Foreground(lipgloss.Color("238")),
		panelTitle: lipgloss.NewStyle().Bold(true).Foreground(colAccent),

		badgeErr: lipgloss.NewStyle().Foreground(colErr).Bold(true),

		rowSel:   lipgloss.NewStyle().Background(colCrumb).Foreground(colBlack).Bold(true),
		match:    lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(colHeader),
		matchCur: lipgloss.NewStyle().Background(colSlow).Foreground(colBlack).Bold(true),
		req:      lipgloss.NewStyle().Foreground(colReq),
		resp:     lipgloss.NewStyle().Foreground(colResp),
		respErr:  lipgloss.NewStyle().Foreground(colErr).Bold(true),
		slow:     lipgloss.NewStyle().Foreground(colSlow),
		notif:    lipgloss.NewStyle().Foreground(colNotif),
		stderr:   lipgloss.NewStyle().Foreground(colStderr),
		pending:  lipgloss.NewStyle().Foreground(lipgloss.Color("179")), // soft gold — in-flight
		dim:      lipgloss.NewStyle().Foreground(colFaint),
	}
}
