package proxy

import (
	"strings"
	"testing"
)

func TestDecode_EmitsBeforeError(t *testing.T) {
	input := strings.Join([]string{
		`{"session_id":"1"}`,
		`{"session_id":"2"}`,
		`not json`,
	}, "\n")

	count := 0
	err := Decode(strings.NewReader(input), func(Envelope) {
		count++
	})

	if err == nil {
		t.Fatal("expected decode error")
	}
	if count != 2 {
		t.Fatalf("expected 2 envelopes, got %d", count)
	}
}
