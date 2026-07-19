package proxy

import (
	"path/filepath"
	"testing"
)

// fixedDropSink is a sink that reports a preset drop count, for exercising the
// optional DropCounter interface.
type fixedDropSink struct{ n uint64 }

func (fixedDropSink) Emit(Envelope)     {}
func (fixedDropSink) Close() error      { return nil }
func (s fixedDropSink) Dropped() uint64 { return s.n }

func TestMultiSinkTotalsDropsAcrossCountingSinks(t *testing.T) {
	// NopSink does not implement DropCounter, so it is skipped rather than failing.
	m := NewMultiSink(fixedDropSink{3}, NopSink(), fixedDropSink{4})
	if got := m.Dropped(); got != 7 {
		t.Fatalf("MultiSink.Dropped() = %d, want 7 (3 + 4)", got)
	}
}

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
