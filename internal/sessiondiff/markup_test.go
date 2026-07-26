package sessiondiff

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
)

// TestDiffPrintsArgumentsAsTheyWereSent. canonicalJSON is a comparison key, but
// callArguments feeds the same string into the report that WriteText prints to a
// terminal, so escaping there shows a person a call they never made.
func TestDiffPrintsArgumentsAsTheyWereSent(t *testing.T) {
	call := func(status string) exporter.SessionExport {
		return exporter.SessionExport{
			Session: exporter.SessionSummary{ID: "s"},
			Calls: []exporter.CallExport{{
				Index: 0, ID: "1", Method: "tools/call", IsTool: true, ToolName: "search",
				Status: status,
				Params: json.RawMessage(`{"name":"search","arguments":{"query":"a < b & c"}}`),
			}},
		}
	}
	report := Compare(call("ok"), call("error"), Options{})

	var buf bytes.Buffer
	if err := WriteText(&buf, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `\u003c`) {
		t.Fatalf("a diff read by a person must not escape the arguments:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `a < b & c`) {
		t.Fatalf("expected the arguments as sent:\n%s", buf.String())
	}
}
