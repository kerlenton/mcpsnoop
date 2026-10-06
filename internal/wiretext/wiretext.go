// Package wiretext prints a value a peer chose, such as a tool name, on a line a
// person reads, so the value cannot pass for something else on that line.
package wiretext

import (
	"strconv"
	"strings"
)

// OneLine returns s as it is when every rune in it prints as itself, and quoted
// otherwise.
//
// A newline in a tool name ends the line it is printed on, and every line after
// it reads as a fresh row, so a report somebody acts on names things that never
// happened. An escape sequence in the same position drives the terminal.
// Quoting rather than dropping keeps the value recoverable.
//
// The test is everything strconv.Quote would escape, rather than
// unicode.IsControl. The runes between the two are the ones that lie without
// breaking a line. U+202E and the other bidi controls reorder the glyphs after
// them, so a tool can be named to read as a different tool, and the zero-width
// formatters let two different names print identically.
func OneLine(s string) string {
	if Unprintable(s) {
		return strconv.Quote(s)
	}
	return s
}

// Unprintable reports whether s holds a rune strconv.Quote would escape.
func Unprintable(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return !strconv.IsPrint(r) })
}
