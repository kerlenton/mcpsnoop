package store

import (
	"strings"
	"testing"
)

func TestDeprecatedMethodNote(t *testing.T) {
	cases := []struct {
		method string
		want   string
	}{
		{"roots/list", "roots is deprecated"},
		{"sampling/createMessage", "sampling is deprecated"},
		{"logging/setLevel", "logging is deprecated"},
		{"notifications/roots/list_changed", "roots is deprecated"},
		{"notifications/message", "logging notifications/message is deprecated"},
		{"tools/list", ""},
		{"notifications/progress", ""},
	}
	for _, tc := range cases {
		got := deprecatedMethodNote(tc.method)
		if tc.want == "" {
			if got != "" {
				t.Fatalf("deprecatedMethodNote(%q) = %q, want empty", tc.method, got)
			}
			continue
		}
		if got == "" || !strings.Contains(got, tc.want) {
			t.Fatalf("deprecatedMethodNote(%q) = %q, want substring %q", tc.method, got, tc.want)
		}
	}
}

func TestDeprecatedCapabilityNote(t *testing.T) {
	if got := DeprecatedCapabilityNote("roots"); got == "" {
		t.Fatal("roots capability should be deprecated")
	}
	if got := DeprecatedCapabilityNote("tools"); got != "" {
		t.Fatalf("tools capability should not be deprecated, got %q", got)
	}
}
