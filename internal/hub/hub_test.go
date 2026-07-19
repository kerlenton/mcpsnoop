package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func env(session string, seq uint64, method string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: session,
		Seq:       seq,
		TS:        time.Now(),
		Direction: proxy.ClientToServer,
		Transport: "stdio",
		Raw:       json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, seq, method)),
	}
}

// writeLog writes envelopes as JSONL to the session log for session.
func writeLog(t *testing.T, dir, session string, envs ...proxy.Envelope) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, session+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range envs {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHubBackfillLiveDedup verifies that file backfill and a live socket stream
// merge for the same session without duplicates, and that a fresh session over
// the socket comes through.
func TestHubBackfillLiveDedup(t *testing.T) {
	sessionsDir := t.TempDir()
	// Short socket path, macOS caps unix socket paths at ~104 bytes.
	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcpsnoop-test-%d.sock", os.Getpid()))
	defer os.Remove(sockPath)

	// History on disk for session s1, seq 1..3.
	writeLog(t, sessionsDir, "s1", env("s1", 1, "initialize"), env("s1", 2, "tools/list"), env("s1", 3, "tools/call"))

	got := make(chan proxy.Envelope, 64)
	h := New(sockPath, sessionsDir, func(e proxy.Envelope) { got <- e })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	// Live shim, re-sends s1 seq 2,3 (overlap → must dedup) and 4 (new), plus a
	// brand-new session s2.
	sink := proxy.NewSocketSink(sockPath, 0)
	defer sink.Close()
	// Give the hub a moment to finish backfill and start accepting.
	time.Sleep(300 * time.Millisecond)
	for _, e := range []proxy.Envelope{
		env("s1", 2, "tools/list"), env("s1", 3, "tools/call"), env("s1", 4, "ping"),
		env("s2", 1, "initialize"), env("s2", 2, "tools/list"),
	} {
		sink.Emit(e)
	}

	// Expect 6 unique envelopes, s1{1,2,3,4} + s2{1,2}.
	maxSeq := map[string]uint64{}
	count := 0
	deadline := time.After(3 * time.Second)
	for count < 6 {
		select {
		case e := <-got:
			count++
			if e.Seq <= maxSeq[e.SessionID] {
				t.Fatalf("duplicate/out-of-order frame: session=%s seq=%d (already saw %d)", e.SessionID, e.Seq, maxSeq[e.SessionID])
			}
			maxSeq[e.SessionID] = e.Seq
		case <-deadline:
			t.Fatalf("timed out: got %d/6 frames, maxSeq=%v", count, maxSeq)
		}
	}
	if maxSeq["s1"] != 4 || maxSeq["s2"] != 2 {
		t.Fatalf("unexpected high-water marks: %v", maxSeq)
	}

	// No extra frames should arrive (e.g. a leaked duplicate).
	select {
	case e := <-got:
		t.Fatalf("unexpected extra frame: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

// touchLog writes a session log and stamps it, so recency is set by the
// modification time rather than by the file name.
func touchLog(t *testing.T, dir, session string, modTime time.Time, envs ...proxy.Envelope) {
	t.Helper()
	writeLog(t, dir, session, envs...)
	path := filepath.Join(dir, session+".jsonl")
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func TestHubBackfillLimitReplaysNewestSessions(t *testing.T) {
	sessionsDir := t.TempDir()
	now := time.Now()
	// The names deliberately sort against the timestamps. A real log is named
	// <label>-<pid>.jsonl, so a name sort orders by server label, and the newest
	// session of an early-sorting label would otherwise be dropped in favour of a
	// stale one whose label sorts later.
	touchLog(t, sessionsDir, "zulu-oldest", now.Add(-3*time.Hour), env("zulu-oldest", 1, "initialize"))
	touchLog(t, sessionsDir, "mike-middle", now.Add(-2*time.Hour), env("mike-middle", 1, "initialize"))
	touchLog(t, sessionsDir, "alpha-newest", now.Add(-1*time.Hour), env("alpha-newest", 1, "initialize"))

	var got []string
	h := NewWithOptions("", sessionsDir, func(e proxy.Envelope) {
		got = append(got, e.SessionID)
	}, Options{BackfillLimit: 2})

	report := h.backfill(context.Background())

	// Oldest of the kept pair first, so replay order still tracks real time.
	want := []string{"mike-middle", "alpha-newest"}
	if !slices.Equal(got, want) {
		t.Fatalf("backfilled sessions = %v, want %v", got, want)
	}
	if report.Loaded != 2 || report.Total != 3 {
		t.Fatalf("backfill report = %+v, want loaded=2 total=3", report)
	}
	if _, ok := h.seen["zulu-oldest"]; ok {
		t.Fatal("out-of-bound session should not consume a seen entry")
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, "zulu-oldest.jsonl")); err != nil {
		t.Fatalf("out-of-bound session should remain openable on disk: %v", err)
	}
}

func TestHubBackfillLimitZeroReplaysEverything(t *testing.T) {
	sessionsDir := t.TempDir()
	now := time.Now()
	touchLog(t, sessionsDir, "zulu-oldest", now.Add(-2*time.Hour), env("zulu-oldest", 1, "initialize"))
	touchLog(t, sessionsDir, "alpha-newest", now.Add(-1*time.Hour), env("alpha-newest", 1, "initialize"))

	var got []string
	h := NewWithOptions("", sessionsDir, func(e proxy.Envelope) {
		got = append(got, e.SessionID)
	}, Options{BackfillLimit: 0})

	report := h.backfill(context.Background())

	want := []string{"zulu-oldest", "alpha-newest"}
	if !slices.Equal(got, want) {
		t.Fatalf("backfilled sessions = %v, want %v", got, want)
	}
	if report.Loaded != 2 || report.Total != 2 {
		t.Fatalf("backfill report = %+v, want loaded=2 total=2", report)
	}
}

// A cancelled backfill must report what it actually replayed, not what it
// intended to.
func TestHubBackfillReportCountsOnlyReplayedLogs(t *testing.T) {
	sessionsDir := t.TempDir()
	now := time.Now()
	touchLog(t, sessionsDir, "one", now.Add(-2*time.Hour), env("one", 1, "initialize"))
	touchLog(t, sessionsDir, "two", now.Add(-1*time.Hour), env("two", 1, "initialize"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := NewWithOptions("", sessionsDir, func(proxy.Envelope) {}, Options{})
	report := h.backfill(ctx)

	if report.Loaded != 0 || report.Total != 2 {
		t.Fatalf("backfill report = %+v, want loaded=0 total=2", report)
	}
}

func TestSweepSeenKeepsRecentDropsOldest(t *testing.T) {
	h := &Hub{seen: make(map[string]seenEntry)}
	base := time.Now()
	// Fill just past the cap with strictly increasing touch times, so the ordering
	// the sweep relies on is unambiguous.
	n := seenCap + 1
	for i := 0; i < n; i++ {
		h.seen[fmt.Sprintf("s%05d", i)] = seenEntry{seq: 1, touched: base.Add(time.Duration(i) * time.Millisecond)}
	}

	before := len(h.seen)
	h.sweepSeen()
	if got, want := before-len(h.seen), before/4; got != want {
		t.Fatalf("sweep dropped %d entries, want a quarter (%d)", got, want)
	}
	if _, ok := h.seen["s00000"]; ok {
		t.Fatal("the oldest session should have been swept")
	}
	if _, ok := h.seen[fmt.Sprintf("s%05d", n-1)]; !ok {
		t.Fatal("the most recently touched session should be kept")
	}
}

func TestEmitBoundsSeenMapAndKeepsLiveSession(t *testing.T) {
	h := &Hub{seen: make(map[string]seenEntry), handler: func(proxy.Envelope) {}}
	const live = "live-session"

	// Drive well past the cap, touching the live session throughout so it stays among
	// the most recently touched and is never swept.
	for i := 0; i < seenCap*2; i++ {
		h.emit(proxy.Envelope{SessionID: fmt.Sprintf("s%07d", i), Seq: 1})
		if i%16 == 0 {
			h.emit(proxy.Envelope{SessionID: live, Seq: uint64(i/16 + 1)})
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) > seenCap {
		t.Fatalf("emit should bound the seen map at the cap, got %d", len(h.seen))
	}
	if _, ok := h.seen[live]; !ok {
		t.Fatal("a session still receiving frames must survive sweeps")
	}
}
