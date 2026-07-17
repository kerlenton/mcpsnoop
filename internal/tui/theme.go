package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// theme binds each UI role to a color. Every role is a CompleteAdaptiveColor, so
// it picks a light or dark ramp from the terminal's own background AND degrades
// from true color to an ANSI-256 or base-16 index by the terminal's color
// profile. The tiers are spaced to separate cleanly against that inherited
// background, which the UI never paints over, it only fills the selection band,
// overlay/toast surfaces, and the current search match. NO_COLOR drops every hue
// and leans on weight and glyphs.
type theme struct {
	fg        lipgloss.TerminalColor // default body text
	bright    lipgloss.TerminalColor // titles, tool names, the selected primary cell
	dim       lipgloss.TerminalColor // secondary text, hint labels, timestamps
	faint     lipgloss.TerminalColor // column headers, DETAIL, suffixes, punctuation, counters
	blue      lipgloss.TerminalColor // denim accent, brand, requests, borders, JSON keys
	blueKey   lipgloss.TerminalColor // brighter denim, key hints
	cyan      lipgloss.TerminalColor // follow, pending, spinner
	green     lipgloss.TerminalColor // ok verdict, live dot
	yellow    lipgloss.TerminalColor // warn verdict
	red       lipgloss.TerminalColor // err verdict
	magenta   lipgloss.TerminalColor // bad verdict, invalid/corrupt frames
	surface   lipgloss.TerminalColor // overlay, toast, and match background
	selection lipgloss.TerminalColor // selection band background
	border    lipgloss.TerminalColor // panel borders
	noColor   bool                   // distinguish by weight and glyph only
}

// newTheme builds the adaptive palette, honoring NO_COLOR.
func newTheme() theme {
	if noColorSet() {
		n := lipgloss.NoColor{}
		return theme{
			fg: n, bright: n, dim: n, faint: n, blue: n, blueKey: n, cyan: n,
			green: n, yellow: n, red: n, magenta: n, surface: n, selection: n, border: n, noColor: true,
		}
	}
	resolveBackground()

	// ac pairs a light-background ramp value with a dark one. Each is carried as a
	// CompleteColor (true color, then ANSI-256, then base-16), so it also degrades
	// with the terminal's color profile. Light tiers are dark ink on a light page,
	// dark tiers are light ink on a dark page, mirrored so each level separates.
	ac := func(lHex, l256, lANSI, dHex, d256, dANSI string) lipgloss.CompleteAdaptiveColor {
		return lipgloss.CompleteAdaptiveColor{
			Light: lipgloss.CompleteColor{TrueColor: lHex, ANSI256: l256, ANSI: lANSI},
			Dark:  lipgloss.CompleteColor{TrueColor: dHex, ANSI256: d256, ANSI: dANSI},
		}
	}
	return theme{
		//         light bg (dark ink)          dark bg (light ink)
		fg:        ac("#33373C", "237", "0", "#CDD0D2", "252", "7"),
		bright:    ac("#16181B", "234", "0", "#EDEEEF", "255", "15"),
		dim:       ac("#5E636B", "241", "8", "#9DA1A7", "247", "8"),
		faint:     ac("#868B93", "245", "8", "#767B83", "243", "8"),
		blue:      ac("#35678F", "25", "4", "#83A6C2", "110", "4"),
		blueKey:   ac("#35678F", "25", "4", "#83A6C2", "110", "12"),
		cyan:      ac("#2A7C74", "30", "6", "#8ABEB7", "115", "6"),
		green:     ac("#55781F", "64", "2", "#B9C06C", "143", "2"),
		yellow:    ac("#916A12", "136", "3", "#E9C46C", "179", "3"),
		red:       ac("#B23A36", "124", "1", "#D0726E", "167", "1"),
		magenta:   ac("#9F3D7C", "132", "5", "#CE89B0", "175", "5"),
		surface:   ac("#EDEFF2", "254", "7", "#131417", "233", "0"),
		selection: ac("#DCE0E6", "253", "7", "#21252B", "235", "8"),
		border:    ac("#C9CDD3", "251", "7", "#34383E", "237", "8"),
	}
}

// resolveBackground honors an explicit MCPSNOOP_THEME override (light or dark),
// otherwise it triggers the terminal background probe once now, at model build
// time before the Bubble Tea input loop starts, so the adaptive colors do not
// race the input reader when they resolve during the first render. Detection
// falls back to a dark background when the terminal does not answer.
func resolveBackground() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCPSNOOP_THEME"))) {
	case "light":
		lipgloss.SetHasDarkBackground(false)
	case "dark":
		lipgloss.SetHasDarkBackground(true)
	default:
		_ = lipgloss.HasDarkBackground()
	}
}

// noColorSet reports whether NO_COLOR is present and non-empty, per no-color.org.
func noColorSet() bool { return os.Getenv("NO_COLOR") != "" }
