// Package hub is the collector side of mcpsnoop. One hub runs in the user's
// terminal (`mcpsnoop` with no args). Shims stream envelopes to it over a unix
// socket, and it backfills history from the on-disk session logs so launching
// the TUI after traffic has happened still shows everything.
//
// Live (socket) and historical (file) sources can overlap, for example after a
// hub restart a shim reconnects while its file already holds the same frames. A
// single per-session high-water-mark on the monotonic Seq deduplicates all
// sources uniformly, so the rest of the system never sees a frame twice.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// Handler receives each unique envelope, in arrival order.
type Handler func(proxy.Envelope)

// Hub collects envelopes from shims (socket) and past sessions (files).
type Hub struct {
	socketPath    string
	sessionsDir   string
	handler       Handler
	backfillLimit int
	onBackfill    func(BackfillReport)

	mu   sync.Mutex
	seen map[string]seenEntry // session id -> dedup high-water mark, with last touch
}

// seenEntry is one session's dedup high-water mark plus the last time it was
// touched, so the map can evict the least-recently-active sessions when it grows.
type seenEntry struct {
	seq     uint64
	touched time.Time
}

// DefaultBackfillLimit bounds startup work while keeping recent history useful.
const DefaultBackfillLimit = 100

// seenCap bounds the dedup map on a long-lived hub. It is far above the backfill
// limit, so ordinary use (with far fewer concurrent sessions) never sweeps.
const seenCap = 10 * DefaultBackfillLimit

// Options controls hub startup behavior. BackfillLimit 0 replays all history.
type Options struct {
	BackfillLimit int
	OnBackfill    func(BackfillReport)
}

// BackfillReport describes how much saved history was replayed at startup.
type BackfillReport struct {
	Loaded int
	Total  int
}

// New creates a hub. handler is invoked for every deduplicated envelope.
func New(socketPath, sessionsDir string, handler Handler) *Hub {
	return NewWithOptions(socketPath, sessionsDir, handler, Options{
		BackfillLimit: DefaultBackfillLimit,
	})
}

// NewWithOptions creates a hub with explicit startup behavior.
func NewWithOptions(socketPath, sessionsDir string, handler Handler, opts Options) *Hub {
	return &Hub{
		socketPath:    socketPath,
		sessionsDir:   sessionsDir,
		handler:       handler,
		backfillLimit: opts.BackfillLimit,
		onBackfill:    opts.OnBackfill,
		seen:          make(map[string]seenEntry),
	}
}

// ErrHubRunning means another hub already owns the socket.
var ErrHubRunning = errors.New("hub: another mcpsnoop hub is already running")

// Run backfills history, then accepts shim connections until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) error {
	ln, err := h.listen()
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(h.socketPath)

	// Backfill BEFORE accepting, this primes the per-session high-water marks
	// from disk so a live shim that reconnects (e.g. after a hub restart) can't
	// race its high-Seq frames ahead of the file's history and cause the gate to
	// drop it. Shims keep retrying their connection until we accept.
	report := h.backfill(ctx)
	if h.onBackfill != nil {
		h.onBackfill(report)
	}

	// Stop accepting when the context is done.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // clean shutdown
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.handleConn(conn)
		}()
	}
	wg.Wait()
	return nil
}

// listen binds the unix socket, clearing a stale socket left by a dead hub but
// refusing to steal one from a live hub.
func (h *Hub) listen() (net.Listener, error) {
	if err := paths.CheckSocketPath(h.socketPath); err != nil {
		return nil, err
	}
	if _, err := os.Stat(h.socketPath); err == nil {
		// Something is there. If we can dial it, a hub is alive.
		if c, derr := net.Dial("unix", h.socketPath); derr == nil {
			c.Close()
			return nil, ErrHubRunning
		}
		_ = os.Remove(h.socketPath) // stale, reclaim
	}
	return net.Listen("unix", h.socketPath)
}

// handleConn decodes a stream of newline/whitespace-separated envelopes.
func (h *Hub) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	for {
		var env proxy.Envelope
		if err := dec.Decode(&env); err != nil {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				return
			}
			return // malformed stream, drop the connection
		}
		h.emit(env)
	}
}

// backfill replays the most recently modified session logs on disk, oldest
// first so historical order roughly matches real time.
func (h *Hub) backfill(ctx context.Context) BackfillReport {
	entries, err := os.ReadDir(h.sessionsDir)
	if err != nil {
		return BackfillReport{}
	}

	// Order by modification time, not by name. A log is named <label>-<pid>.jsonl,
	// so sorting by name orders by server label first and would keep whichever
	// labels sort last rather than whichever sessions ran last. exporter's
	// newest-session lookup already resolves recency the same way.
	type sessionLog struct {
		path    string
		modTime time.Time
	}
	logs := make([]sessionLog, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // the file went away between the listing and the stat
		}
		logs = append(logs, sessionLog{
			path:    filepath.Join(h.sessionsDir, e.Name()),
			modTime: info.ModTime(),
		})
	}
	slices.SortFunc(logs, func(a, b sessionLog) int {
		if c := a.modTime.Compare(b.modTime); c != 0 {
			return c
		}
		return strings.Compare(a.path, b.path) // deterministic for equal timestamps
	})

	report := BackfillReport{Total: len(logs)}
	if h.backfillLimit > 0 && len(logs) > h.backfillLimit {
		logs = logs[len(logs)-h.backfillLimit:]
	}
	for _, l := range logs {
		if ctx.Err() != nil {
			return report // count only what was actually replayed
		}
		h.replayFile(l.path)
		report.Loaded++
	}
	return report
}

func (h *Hub) replayFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	_ = proxy.Decode(f, h.emit)
}

// emit deduplicates by per-session high-water-mark on Seq, then forwards.
func (h *Hub) emit(env proxy.Envelope) {
	h.mu.Lock()
	if env.Seq <= h.seen[env.SessionID].seq {
		h.mu.Unlock()
		return
	}
	h.seen[env.SessionID] = seenEntry{seq: env.Seq, touched: time.Now()}
	if len(h.seen) > seenCap {
		h.sweepSeen()
	}
	h.mu.Unlock()
	h.handler(env)
}

// sweepSeen drops the least-recently-touched quarter of the dedup map, keeping it
// bounded on a long-lived hub without a list to maintain. Caller holds h.mu.
//
// Least-recently-touched is the one safe policy, because a live session is touched
// on every frame, so it can never be among the oldest and is never evicted.
// Evicting a live session would drop its high-water mark, and a later duplicate
// frame would then pass the gate and reach the store as a new event. Dropping a
// quarter at once amortises the cost so a sweep is rare rather than per frame.
func (h *Hub) sweepSeen() {
	type aged struct {
		id      string
		touched time.Time
	}
	all := make([]aged, 0, len(h.seen))
	for id, e := range h.seen {
		all = append(all, aged{id: id, touched: e.touched})
	}
	slices.SortFunc(all, func(a, b aged) int { return a.touched.Compare(b.touched) })
	for _, a := range all[:len(all)/4] {
		delete(h.seen, a.id)
	}
}
