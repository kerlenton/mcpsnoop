package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/kerlenton/mcpsnoop/internal/store"
)

// TestStatusStyleWarnsOnTruncated locks the row colour to the "warn" text a
// truncated frame shows. The marker moved off the Warning field, so statusStyle
// must check the flag or the cell falls through to the muted style.
func TestStatusStyleWarnsOnTruncated(t *testing.T) {
	m := New(store.New())
	fg := m.statusStyle(store.EventView{Kind: store.EventOther, Truncated: true}).GetForeground()
	if fg != m.styles.warn.GetForeground() {
		t.Fatal("a truncated event should render its status in the warn style")
	}
	if fg == m.styles.dim.GetForeground() {
		t.Fatal("a truncated event must not fall through to the muted style")
	}
}

// TestTruncateMeasuresInCells locks the fix for the panic where truncate mixed
// cell width with rune count. Every assertion is on lipgloss.Width of the result,
// never its rune or byte length, so the test cannot repeat the bug it guards.
func TestTruncateMeasuresInCells(t *testing.T) {
	cjk := strings.Repeat("あ", 20)  // 20 runes, 40 cells
	emoji := strings.Repeat("😀", 3) // 3 runes, 6 cells
	mixed := "abcあいうdef漢字"          // mix of one- and two-cell runes

	cases := []struct {
		name string
		s    string
		w    int
		want string // exact result to assert, empty means only bound the width
	}{
		{"ascii longer than w", strings.Repeat("a", 30), 10, strings.Repeat("a", 9) + "…"},
		{"exact fit unchanged", "hello", 5, "hello"},
		{"twenty cjk at w=30 (old panic)", cjk, 30, ""},
		{"three emoji at w=5 (small panic)", emoji, 5, ""},
		{"wide runes w=0", cjk, 0, ""},
		{"wide runes w=1", cjk, 1, ""},
		{"wide runes w=2", cjk, 2, ""},
		{"mixed ascii and cjk", mixed, 9, ""},
		// An invalid byte (stderr is raw server bytes) decodes to U+FFFD; the offset
		// must advance by one byte, not the three of the re-encoded form.
		{"invalid utf-8 byte", "ab\xffcd", 3, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.w) // must not panic
			if w := lipgloss.Width(got); w > tc.w {
				t.Fatalf("truncate(%q, %d) width = %d cells, want at most %d (%q)", tc.s, tc.w, w, tc.w, got)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.s, tc.w, got, tc.want)
			}
		})
	}
}

func TestTruncateAppendsEllipsisWhenItCuts(t *testing.T) {
	got := truncate(strings.Repeat("a", 30), 10)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a cut result should end with an ellipsis, got %q", got)
	}
}
