// Package hub is the collector side of mcpsnoop. One hub runs in the user's
// terminal (`mcpsnoop` with no args). Shims stream envelopes to it over a unix
// socket, and it backfills history from the on-disk session logs so launching
// the TUI after traffic has happened still shows everything.
//
// Live (socket) and historical (file) sources can overlap — e.g. after a hub
// restart a shim reconnects while its file already holds the same frames. A
// single per-session high-water-mark on the monotonic Seq deduplicates all
// sources uniformly, so the rest of the system never sees a frame twice.
package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// Handler receives each unique envelope, in arrival order.
type Handler func(proxy.Envelope)

// Hub collects envelopes from shims (socket) and past sessions (files).
type Hub struct {
	socketPath  string
	sessionsDir string
	remote      RemoteConfig
	handler     Handler

	mu   sync.Mutex
	seen map[string]uint64 // session id -> highest seq forwarded
}

// RemoteConfig enables a TLS listener for shims running on other hosts.
type RemoteConfig struct {
	Listen   string
	Token    string
	CertFile string
	KeyFile  string
}

// Option configures optional hub inputs.
type Option func(*Hub)

// WithRemote enables a TLS remote ingest listener. The token authenticates
// shims after TLS is established.
func WithRemote(cfg RemoteConfig) Option {
	return func(h *Hub) { h.remote = cfg }
}

// New creates a hub. handler is invoked for every deduplicated envelope.
func New(socketPath, sessionsDir string, handler Handler, opts ...Option) *Hub {
	h := &Hub{
		socketPath:  socketPath,
		sessionsDir: sessionsDir,
		handler:     handler,
		seen:        make(map[string]uint64),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
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

	var remote net.Listener
	if h.remote.Listen != "" {
		remote, err = h.listenRemote()
		if err != nil {
			return err
		}
		defer remote.Close()
	}

	// Backfill BEFORE accepting: this primes the per-session high-water marks
	// from disk so a live shim that reconnects (e.g. after a hub restart) can't
	// race its high-Seq frames ahead of the file's history and cause the gate to
	// drop it. Shims keep retrying their connection until we accept.
	h.backfill(ctx)

	// Stop accepting when the context is done.
	go func() {
		<-ctx.Done()
		ln.Close()
		if remote != nil {
			remote.Close()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.accept(ctx, ln, false)
	}()
	if remote != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.accept(ctx, remote, true)
		}()
	}
	wg.Wait()
	return nil
}

// listen binds the unix socket, clearing a stale socket left by a dead hub but
// refusing to steal one from a live hub.
func (h *Hub) listen() (net.Listener, error) {
	if _, err := os.Stat(h.socketPath); err == nil {
		// Something is there. If we can dial it, a hub is alive.
		if c, derr := net.Dial("unix", h.socketPath); derr == nil {
			c.Close()
			return nil, ErrHubRunning
		}
		_ = os.Remove(h.socketPath) // stale; reclaim
	}
	return net.Listen("unix", h.socketPath)
}

func (h *Hub) listenRemote() (net.Listener, error) {
	if h.remote.Token == "" {
		return nil, errors.New("hub: --remote-token is required with --remote-listen")
	}
	if h.remote.CertFile == "" || h.remote.KeyFile == "" {
		return nil, errors.New("hub: --remote-cert and --remote-key are required with --remote-listen")
	}
	cert, err := tls.LoadX509KeyPair(h.remote.CertFile, h.remote.KeyFile)
	if err != nil {
		return nil, err
	}
	return tls.Listen("tcp", h.remote.Listen, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
}

func (h *Hub) accept(ctx context.Context, ln net.Listener, remote bool) {
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if remote {
				h.handleRemoteConn(conn)
				return
			}
			h.handleConn(conn)
		}()
	}
	wg.Wait()
}

type remoteHello struct {
	Version string `json:"mcpsnoop_remote"`
	Token   string `json:"token"`
}

// handleConn decodes a stream of newline/whitespace-separated envelopes.
func (h *Hub) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	h.decodeEnvelopes(dec)
}

func (h *Hub) handleRemoteConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	var hello remoteHello
	if err := dec.Decode(&hello); err != nil {
		return
	}
	if hello.Version != "1" || hello.Token != h.remote.Token {
		return
	}
	h.decodeEnvelopes(dec)
}

func (h *Hub) decodeEnvelopes(dec *json.Decoder) {
	for {
		var env proxy.Envelope
		if err := dec.Decode(&env); err != nil {
			return
		}
		h.emit(env)
	}
}

// backfill replays envelopes from every session log on disk.
func (h *Hub) backfill(ctx context.Context) {
	entries, err := os.ReadDir(h.sessionsDir)
	if err != nil {
		return
	}
	// Oldest first, so historical order roughly matches real time.
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".jsonl" {
			files = append(files, filepath.Join(h.sessionsDir, e.Name()))
		}
	}
	slices.Sort(files)
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		h.replayFile(f)
	}
}

func (h *Hub) replayFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var env proxy.Envelope
		if err := dec.Decode(&env); err != nil {
			return
		}
		h.emit(env)
	}
}

// emit deduplicates by per-session high-water-mark on Seq, then forwards.
func (h *Hub) emit(env proxy.Envelope) {
	h.mu.Lock()
	if env.Seq <= h.seen[env.SessionID] {
		h.mu.Unlock()
		return
	}
	h.seen[env.SessionID] = env.Seq
	h.mu.Unlock()
	h.handler(env)
}
