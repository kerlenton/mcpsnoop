package proxy

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestStdioMetaFramePreservesMarkupInTheCommand. The meta frame is the only
// Envelope.Raw the proxy builds rather than copies, so without the same encoder
// as the sinks a command or cwd containing & is the one escaped frame in an
// otherwise verbatim log.
func TestStdioMetaFramePreservesMarkupInTheCommand(t *testing.T) {
	sink := &captureSink{}
	_, err := RunStdio(context.Background(), StdioConfig{
		Command:   []string{"cat", "--url=https://h/mcp?a=1&b=2"},
		Label:     "test",
		SessionID: "meta-1",
		Sink:      sink,
		In:        strings.NewReader(""),
		Out:       &bytes.Buffer{},
		Err:       &bytes.Buffer{},
	})
	// cat rejects the flag, which is fine: the meta frame is emitted before the
	// server is ever read from, and a non-zero exit is not an error here.
	if err != nil {
		t.Fatal(err)
	}

	meta := sink.byDir(DirectionMeta)
	if len(meta) != 1 {
		t.Fatalf("expected exactly one meta frame, got %d", len(meta))
	}
	got := string(meta[0].Raw)
	if strings.Contains(got, `\u0026`) {
		t.Fatalf("the meta frame must not be the one escaped frame in the log: %s", got)
	}
	if !strings.Contains(got, "a=1&b=2") {
		t.Fatalf("expected the command verbatim: %s", got)
	}
}
