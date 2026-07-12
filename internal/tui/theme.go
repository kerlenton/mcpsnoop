package tui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

// theme binds each UI role to a terminal color. Roles map to the 16 ANSI colors
// (indices 0..15) so the UI inherits whatever theme the user's terminal runs and
// reads like tig or htop rather than a fixed neon palette. surface and border use
// 256-color grays for subtle fills. The reference hexes are base16 Tomorrow
// Night and exist only for the design screenshots, the code always binds the
// index. NO_COLOR drops every hue and leans on weight, italic, and glyphs.
type theme struct {
	fg      lipgloss.TerminalColor // default text
	bright  lipgloss.TerminalColor // titles, tool names, the selected primary cell
	dim     lipgloss.TerminalColor // secondary text, timestamps, notifications, stderr
	blue    lipgloss.TerminalColor // brand, requests, key hints, borders, JSON keys, one bright blue
	cyan    lipgloss.TerminalColor // follow, pending, spinner
	green   lipgloss.TerminalColor // ok verdict, live dot
	yellow  lipgloss.TerminalColor // slow and warn verdicts
	red     lipgloss.TerminalColor // err and bad verdicts
	surface lipgloss.TerminalColor // overlay and selection background
	border  lipgloss.TerminalColor // panel borders
	noColor bool                   // distinguish by weight and glyph only
}

// newTheme builds the ANSI-bound palette, honoring NO_COLOR.
func newTheme() theme {
	if noColorSet() {
		n := lipgloss.NoColor{}
		return theme{
			fg: n, bright: n, dim: n, blue: n, cyan: n,
			green: n, yellow: n, red: n, surface: n, border: n, noColor: true,
		}
	}
	return theme{
		fg:      lipgloss.Color("7"),   // #C5C8C6
		bright:  lipgloss.Color("15"),  // #E4E6E3
		dim:     lipgloss.Color("8"),   // #969896
		blue:    lipgloss.Color("12"),  // bright blue, index 4 read too dark in dark themes
		cyan:    lipgloss.Color("6"),   // #8ABEB7
		green:   lipgloss.Color("2"),   // #B5BD68
		yellow:  lipgloss.Color("3"),   // #F0C674
		red:     lipgloss.Color("1"),   // #CC6666
		surface: lipgloss.Color("235"), // #282A2E
		border:  lipgloss.Color("237"), // #34373B
	}
}

// noColorSet reports whether NO_COLOR is present and non-empty, per no-color.org.
func noColorSet() bool { return os.Getenv("NO_COLOR") != "" }
