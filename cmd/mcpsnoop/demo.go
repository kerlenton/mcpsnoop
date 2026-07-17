package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/tui"
)

// runDemo plays a scripted MCP session into an isolated TUI, so a new user can
// see the live view in seconds without wiring up a client or a server. It runs
// on a throwaway home, so it never touches or shows the user's real sessions.
func runDemo() int {
	dir, err := os.MkdirTemp("", "mcpsnoop-demo-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "hub.sock")
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go playDemo(ctx, socket)

	if err := tui.Run(ctx, socket, sessions); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
		return 1
	}
	return 0
}

// demoFrame is one scripted observed frame plus the pause to wait after it, so
// the session animates into the TUI at a natural pace.
type demoFrame struct {
	dir     proxy.Direction
	raw     string // stdout frame, JSON-RPC or a stray non-protocol line, empty for a stderr line
	text    string // stderr line, empty for JSON-RPC frames
	invalid bool   // raw is deliberately not JSON-RPC, to show the invalid flag
	pause   time.Duration
}

// playDemo streams the scripted session to the hub over the socket, then keeps
// the session alive so the user can explore it (drill in, search, replay).
func playDemo(ctx context.Context, socket string) {
	sink := proxy.NewSocketSink(socket, 0)
	defer sink.Close()

	session := fmt.Sprintf("demo-%d", os.Getpid())
	var seq uint64

	// Give the hub a moment to bind. The socket sink also retries on its own.
	if !sleepCtx(ctx, 700*time.Millisecond) {
		return
	}
	for _, f := range demoScript() {
		if ctx.Err() != nil {
			return
		}
		seq++
		sink.Emit(demoEnvelope(session, seq, f))
		if !sleepCtx(ctx, f.pause) {
			return
		}
	}
	<-ctx.Done()
}

// demoEnvelope builds the observed envelope for one scripted frame. It routes a
// stray non-JSON line to Text exactly as the real shim does, so the frame
// survives the sink encoder. A json.RawMessage cannot carry non-JSON bytes.
func demoEnvelope(session string, seq uint64, f demoFrame) proxy.Envelope {
	env := proxy.Envelope{
		SessionID:   session,
		ServerLabel: "demo",
		Seq:         seq,
		TS:          time.Now(),
		Direction:   f.dir,
		Transport:   "stdio",
	}
	switch {
	case f.text != "":
		env.Text = f.text
	case json.Valid([]byte(f.raw)):
		env.Raw = []byte(f.raw)
	default:
		env.Text = f.raw
	}
	return env
}

// demoScript is the scripted session, a handshake, a few tool calls, a stray
// stdout line (flagged as invalid), a long-running call with progress, a large
// payload (shows the inspector's wrapping), and a tool-level error
// (result.isError). Pauses give the calls a range of visible latencies.
func demoScript() []demoFrame {
	bigValue := strings.Repeat("ZmFrZS1iYXNlNjQtcGF5bG9hZC0", 26) // ~700 chars, no spaces

	return []demoFrame{
		{dir: proxy.ServerStderr, text: "demo server ready (stdio)", pause: 300 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"roots":{"listChanged":true},"sampling":{}},"clientInfo":{"name":"mcpsnoop-demo","version":"1.0.0"}}}`, pause: 500 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true},"logging":{}},"serverInfo":{"name":"demo-server","version":"1.0.0"}}}`, pause: 400 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, pause: 500 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, pause: 400 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo","description":"Echo back a message"},{"name":"get_sum","description":"Add two numbers"},{"name":"slow_search","description":"A search that takes a while"},{"name":"big_payload","description":"Return a large value"},{"name":"flaky","description":"Sometimes fails"}]}}`, pause: 600 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello from the demo"}}}`, pause: 400 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"Echo: hello from the demo"}]}}`, pause: 500 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_sum","arguments":{"a":40,"b":2}}}`, pause: 350 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"42"}]}}`, pause: 600 * time.Millisecond},
		// A stray debug line the server left on stdout. It is not JSON-RPC, so on
		// stdio it corrupts the stream, and mcpsnoop flags it as invalid.
		{dir: proxy.ServerToClient, raw: `[debug] get_sum handler done`, invalid: true, pause: 500 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"slow_search","arguments":{"query":"everything"}}}`, pause: 500 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":4,"progress":1,"total":3}}`, pause: 600 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":4,"progress":2,"total":3}}`, pause: 600 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":4,"progress":3,"total":3}}`, pause: 600 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"found 3 results"}]}}`, pause: 600 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"big_payload","arguments":{}}}`, pause: 400 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":%q}]}}`, bigValue), pause: 600 * time.Millisecond},
		{dir: proxy.ClientToServer, raw: `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"flaky","arguments":{}}}`, pause: 400 * time.Millisecond},
		{dir: proxy.ServerToClient, raw: `{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"flaky tool failed: upstream timeout"}],"isError":true}}`, pause: 0},
	}
}

// sleepCtx waits for d or until ctx is cancelled. Returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
