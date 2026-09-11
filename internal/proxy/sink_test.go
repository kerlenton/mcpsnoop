package proxy

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// TestAsyncSinkEmitAfterCloseDoesNotPanic guards the shutdown race where a proxy
// goroutine still emits after Close, for example an SSE tap draining after the
// HTTP grace period. Close must leave s.ch open, since a send on a closed
// channel panics even inside a select.
//
// The emits no longer reach that send at all, because Emit now turns back at the
// closed check, so this no longer exercises the default case the way it once
// did. It still pins the invariant, which is that nothing here panics however
// late an emit arrives, and TestAsyncSinkDropsWhenTheBufferIsFull covers the
// default case on a sink that is still open.
func TestAsyncSinkEmitAfterCloseDoesNotPanic(t *testing.T) {
	s := NewAsyncSink(&bytes.Buffer{}, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		s.Emit(Envelope{SessionID: "s", Seq: 1})
	}
	if s.Dropped() == 0 {
		t.Fatal("post-close emits should be counted as dropped")
	}
}

func TestAsyncSinkCountsFirstEmitAfterCloseAsDropped(t *testing.T) {
	s := NewAsyncSink(&bytes.Buffer{}, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s.Emit(Envelope{SessionID: "s", Seq: 1})

	if got := s.Dropped(); got != 1 {
		t.Fatalf("post-close dropped count = %d, want 1", got)
	}
}

func TestAsyncSinkEmitDoesNotWaitForLifecycleLock(t *testing.T) {
	s := NewAsyncSink(&bytes.Buffer{}, 1)
	s.mu.Lock()
	emitted := make(chan struct{})
	go func() {
		s.Emit(Envelope{SessionID: "s", Seq: 1})
		close(emitted)
	}()

	select {
	case <-emitted:
	case <-time.After(time.Second):
		s.mu.Unlock()
		t.Fatal("Emit blocked on the lifecycle lock")
	}
	s.mu.Unlock()

	if got := s.Dropped(); got != 1 {
		t.Fatalf("contended dropped count = %d, want 1", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
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

// TestAsyncSinkDropsWhenTheBufferIsFull covers the default case on a sink that is
// still open, which is the ordinary production drop and which nothing covered.
// The post-close test used to reach it by accident and no longer does, so
// deleting Emit's default arm, or turning it into a panic, stopped being caught.
//
// A sink is never allowed to wait here. The interface contract says so because
// the queue sits on the proxied path, and a full queue is exactly when waiting
// would be most tempting and most damaging.
func TestAsyncSinkDropsWhenTheBufferIsFull(t *testing.T) {
	// A writer the test holds shut, so the loop takes one envelope and parks
	// inside Encode while the buffer behind it fills.
	w := &blockingWriter{released: make(chan struct{})}
	s := NewAsyncSink(w, 2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 32 {
			s.Emit(Envelope{SessionID: "s", Seq: 1})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(w.released)
		t.Fatal("Emit waited on a full buffer instead of dropping")
	}
	if s.Dropped() == 0 {
		t.Fatal("a full buffer dropped nothing, so the default case never ran")
	}

	// Release the writer before Close, which waits for the loop to finish.
	close(w.released)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// blockingWriter parks the first write until released, then accepts everything.
type blockingWriter struct {
	released chan struct{}
	once     sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { <-b.released })
	return len(p), nil
}

// TestAsyncSinkCountsEveryLateEmitExactlyOnce. Counted is not the same as
// accounted. An implementation that counts a late envelope and then queues it
// anyway satisfies a test that only reads Dropped, while leaving the envelope in
// a channel no goroutine will ever read. The count would then be right and the
// buffer would still be holding what it claimed to have thrown away.
//
// On main this loses every one of them silently: five emits after Close on a
// buffer of eight reported nothing dropped and wrote nothing.
func TestAsyncSinkCountsEveryLateEmitExactlyOnce(t *testing.T) {
	var buf bytes.Buffer
	s := NewAsyncSink(&buf, 8)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	const late = 5
	for range late {
		s.Emit(Envelope{SessionID: "s", Seq: 1})
	}
	if got := s.Dropped(); got != late {
		t.Errorf("dropped = %d, want %d", got, late)
	}
	if n := len(s.ch); n != 0 {
		t.Errorf("%d envelope(s) were counted as dropped and queued anyway, where nothing will read them", n)
	}
	if buf.Len() != 0 {
		t.Errorf("a closed sink wrote %d bytes", buf.Len())
	}
}

// TestAsyncSinkAccountsForEveryEnvelopeWhileClosing is the reason the lifecycle
// lock exists. Every envelope handed to Emit must end up written or counted,
// exactly one of the two, even when Close lands in the middle of a burst.
//
// The race detector is what makes this sharp, so CI is where it earns its keep.
// Without it the flaw this pins takes thousands of rounds to surface.
func TestAsyncSinkAccountsForEveryEnvelopeWhileClosing(t *testing.T) {
	for round := range 50 {
		counter := &lineCounter{}
		s := NewAsyncSink(counter, 4)
		const workers, each = 8, 20

		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range each {
					s.Emit(Envelope{SessionID: "s", Seq: 1})
				}
			}()
		}
		// Vary where Close lands inside the burst rather than always at the end.
		time.Sleep(time.Duration(round%5) * 20 * time.Microsecond)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()

		if got := counter.count() + int(s.Dropped()); got != workers*each {
			t.Fatalf("round %d: %d envelopes written plus dropped, want %d, so %d went missing from both",
				round, got, workers*each, workers*each-got)
		}
	}
}

// lineCounter counts records rather than bytes, since one Write carries one
// newline-delimited envelope.
type lineCounter struct {
	mu sync.Mutex
	n  int
}

func (c *lineCounter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n += bytes.Count(p, []byte("\n"))
	c.mu.Unlock()
	return len(p), nil
}

func (c *lineCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
