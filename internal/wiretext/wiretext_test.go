package wiretext

import "testing"

func TestOneLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"search", "search"},
		{"recherche_ü", "recherche_ü"},
		{"a b", "a b"},
		{"x\nforged: row", `"x\nforged: row"`},
		{"\x1b[31mred", `"\x1b[31mred"`},
		// Neither breaks a line, and both lie about what the bytes are.
		{"admin\U0000202Etxt.exe", `"admin\u202etxt.exe"`},
		{"se\U0000200Barch", `"se\u200barch"`},
	} {
		if got := OneLine(tc.in); got != tc.want {
			t.Errorf("OneLine(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
