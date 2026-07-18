package proxy

import (
	"bytes"
	"testing"
)

// TestAsyncSinkEmitAfterCloseDoesNotPanic guards the shutdown race where a proxy
// goroutine still emits after Close. The channel must stay open, so a late emit
// drops into the default case instead of panicking on a send to a closed channel.
func TestAsyncSinkEmitAfterCloseDoesNotPanic(t *testing.T) {
	s := NewAsyncSink(&bytes.Buffer{}, 1)
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

// TestAsyncSinkCloseFlushesQueued checks that Close drains everything already
// queued rather than dropping it, so signalling via quit (not closing the
// channel) did not cost the flush guarantee.
func TestAsyncSinkCloseFlushesQueued(t *testing.T) {
	var buf bytes.Buffer
	s := NewAsyncSink(&buf, 16)
	const n = 8
	for i := range n {
		s.Emit(Envelope{SessionID: "s", Seq: uint64(i + 1)})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Encode writes one newline-terminated JSON object per envelope.
	if got := bytes.Count(buf.Bytes(), []byte("\n")); got != n {
		t.Fatalf("flushed %d envelopes, want %d", got, n)
	}
}
