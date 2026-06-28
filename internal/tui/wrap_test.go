package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestSoftWrap(t *testing.T) {
	// A short line is left untouched; a long unbroken value is hard-wrapped to
	// width, and nothing ends up wider than the limit.
	in := "short\n" + strings.Repeat("x", 25)
	out := softWrap(in, 10)

	lines := strings.Split(out, "\n")
	if lines[0] != "short" {
		t.Fatalf("short line was altered: %q", lines[0])
	}
	for _, l := range lines {
		if w := lipgloss.Width(l); w > 10 {
			t.Fatalf("line wider than limit: %q (width %d)", l, w)
		}
	}
	if got := strings.Count(out, "x"); got != 25 {
		t.Fatalf("content lost in wrap: %d of 25 x's survived\n%q", got, out)
	}
}

func TestSoftWrapNoopWidth(t *testing.T) {
	in := "anything goes here"
	if out := softWrap(in, 0); out != in {
		t.Fatalf("non-positive width should be a no-op, got %q", out)
	}
}
