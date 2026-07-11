// Package replay re-runs a captured request against a fresh, isolated copy of
// the server. It spawns the server command itself, performs its own MCP
// handshake, and sends the request, so it never touches or corrupts the live
// client session being observed.
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// Result is the outcome of a replay.
type Result struct {
	Method    string
	Params    json.RawMessage
	Response  json.RawMessage // raw response frame
	RPCResult json.RawMessage
	Err       *proxy.RPCError
	Duration  time.Duration
}

const clientName = "mcpsnoop-replay"

// Replay spawns command, handshakes, and sends one request (method+params),
// returning the correlated response and its latency. The server is always shut
// down before returning.
func Replay(ctx context.Context, command []string, cwd, method string, params json.RawMessage, timeout time.Duration) (Result, error) {
	if len(command) == 0 {
		return Result{}, fmt.Errorf("replay: empty command")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	cmd.Stderr = nil // discard the replayed server's logs

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("replay: start: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	r := bufio.NewReaderSize(stdout, 1<<20)

	// 1. initialize
	initParams := json.RawMessage(fmt.Sprintf(
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":%q,"version":"dev"}}`, clientName))
	if err := writeRequest(stdin, 1, "initialize", initParams); err != nil {
		return Result{}, err
	}
	if _, err := readResponse(r, "1"); err != nil {
		return Result{}, fmt.Errorf("replay: initialize: %w", err)
	}
	// 2. initialized notification
	if err := writeNotification(stdin, "notifications/initialized", nil); err != nil {
		return Result{}, err
	}

	// 3. the actual request, timed
	start := time.Now()
	if err := writeRequest(stdin, 2, method, params); err != nil {
		return Result{}, err
	}
	resp, err := readResponse(r, "2")
	if err != nil {
		return Result{}, fmt.Errorf("replay: %s: %w", method, err)
	}
	dur := time.Since(start)

	msg, _ := proxy.ParseRPC(resp)
	return Result{
		Method:    method,
		Params:    params,
		Response:  resp,
		RPCResult: msg.Result,
		Err:       msg.Error,
		Duration:  dur,
	}, nil
}

func writeRequest(w io.Writer, id int, method string, params json.RawMessage) error {
	return writeFrame(w, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func writeNotification(w io.Writer, method string, params json.RawMessage) error {
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		frame["params"] = params
	}
	return writeFrame(w, frame)
}

func writeFrame(w io.Writer, frame map[string]any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// readResponse reads frames until it finds a response whose id matches wantID,
// ignoring notifications and server-initiated requests.
func readResponse(r *bufio.Reader, wantID string) (json.RawMessage, error) {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			// A response for our id has an empty method and a matching id.
			// Everything else (notifications, log lines, unrelated responses)
			// is skipped rather than treated as ours.
			if msg, ok := proxy.ParseRPC(line); ok && msg.Method == "" && string(msg.ID) == wantID {
				if !msg.IsResponse() {
					return nil, fmt.Errorf("malformed response for id %s: neither result nor error", wantID)
				}
				return append(json.RawMessage(nil), trimNewline(line)...), nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
