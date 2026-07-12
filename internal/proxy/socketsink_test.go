package proxy

import (
	"path/filepath"
	"testing"
)

// TestSocketSinkEmitAfterCloseDoesNotPanic guards the shutdown race where a proxy
// goroutine still emits after Close. The channel must stay open, so a late emit
// drops into the default case instead of panicking on a send to a closed channel.
func TestSocketSinkEmitAfterCloseDoesNotPanic(t *testing.T) {
	s := NewSocketSink(filepath.Join(t.TempDir(), "no-such.sock"), 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// More emits than the buffer holds, so at least one exercises the drop path.
	for range 5 {
		s.Emit(Envelope{SessionID: "s", Seq: 1})
	}
	if s.Dropped() == 0 {
		t.Fatal("post-close emits beyond the buffer should be counted as dropped")
	}
}
