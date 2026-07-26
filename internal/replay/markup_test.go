package replay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestWriteFramePreservesMarkupInParams. A replay sends captured params back to
// a live server and renders them in the replay overlay, so this is the one path
// where re-encoding leaves the tool actually being called with different bytes
// than the ones captured.
func TestWriteFramePreservesMarkupInParams(t *testing.T) {
	var buf bytes.Buffer
	params := json.RawMessage(`{"name":"render","arguments":{"q":"a < b & c"}}`)
	if err := writeRequest(&buf, 1, "tools/call", params); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `\u003c`) || strings.Contains(buf.String(), `\u0026`) {
		t.Fatalf("a replayed frame must carry the captured bytes: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"q":"a < b & c"`) {
		t.Fatalf("expected the arguments verbatim: %s", buf.String())
	}
}

// TestWithClientMetaPreservesMarkupInParams covers the stateless path, which
// re-marshals the params a second time on its way through _meta.
func TestWithClientMetaPreservesMarkupInParams(t *testing.T) {
	params := json.RawMessage(`{"name":"render","arguments":{"q":"a < b & c"}}`)
	got := string(withClientMeta(params))
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u0026`) {
		t.Fatalf("adding _meta must not rewrite the captured params: %s", got)
	}
	if !strings.Contains(got, `"q":"a < b & c"`) {
		t.Fatalf("expected the arguments verbatim: %s", got)
	}
}
