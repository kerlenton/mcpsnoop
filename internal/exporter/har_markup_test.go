package exporter

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestHARErrorBodyPreservesMarkup covers the branch the top-level encoder does
// not reach. harResponseBody flattens the error object into content.text with
// its own Marshal, so a failed call could still be re-escaped in a file whose
// successful calls are verbatim, and RPCError.Data is captured wire bytes.
func TestHARErrorBodyPreservesMarkup(t *testing.T) {
	data := SessionExport{
		Session: SessionSummary{ID: "s1", Label: "srv"},
		Calls: []CallExport{{
			Index: 0, ID: "1", Method: "tools/call", Status: "error", StartedAt: time.Now(),
			Error: &proxy.RPCError{
				Code:    -32602,
				Message: "bad <tag> & co",
				Data:    json.RawMessage(`{"detail":"<b>hi</b>"}`),
			},
		}},
	}
	var buf bytes.Buffer
	if err := WriteHAR(&buf, data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `\u003c`) {
		t.Fatalf("the error body must not be re-escaped:\n%s", buf.String())
	}
	for _, want := range []string{"bad <tag> & co", "<b>hi</b>"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("expected %q verbatim in content.text:\n%s", want, buf.String())
		}
	}
}
