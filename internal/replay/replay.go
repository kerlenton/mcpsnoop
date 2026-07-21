// Package replay re-runs a captured request against a fresh, isolated copy of
// the server. It spawns the server command itself, negotiates whatever protocol
// revision that server speaks, and sends the request, so it never touches or
// corrupts the live client session being observed.
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
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

const (
	clientName = "mcpsnoop-replay"

	// legacyProtocolVersion is announced in the initialize handshake, which the
	// 2026-07-28 revision removed (SEP-2575, SEP-2567).
	legacyProtocolVersion = "2025-06-18"

	// statelessProtocolVersion is announced per request instead, since a
	// stateless server negotiates nothing up front and every request has to
	// describe itself.
	statelessProtocolVersion = "2026-07-28"
)

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
		// Close stdin for a graceful stop, then cancel to SIGKILL a server that
		// ignores EOF, so teardown never blocks on Wait until the timeout fires.
		// cancel is idempotent, the outer defer calls it too.
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()

	r := bufio.NewReaderSize(stdout, 1<<20)

	// 1. Try the legacy handshake first and let the answer decide the revision.
	// A server on 2026-07-28 or later does not implement initialize and answers
	// with an error, which is the signal that it is stateless. Probing the other
	// way round, as the SDK clients do, would mean sending server/discover to a
	// server that may silently drop unknown methods, and bounding that read
	// needs a second reader on the same stream. Going legacy-first keeps the
	// path that works today byte for byte unchanged, at the cost of one doomed
	// request against a new server, which is cheap for a one-shot replay.
	initParams := json.RawMessage(fmt.Sprintf(
		`{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":%q,"version":"dev"}}`,
		legacyProtocolVersion, clientName))
	if err := writeRequest(stdin, 1, "initialize", initParams); err != nil {
		return Result{}, err
	}
	initResp, err := readResponse(r, "1")
	if err != nil {
		return Result{}, fmt.Errorf("replay: initialize: %w", err)
	}

	stateless := false
	if msg, ok := proxy.ParseRPC(initResp); ok && msg.Error != nil {
		stateless = true
	}
	if !stateless {
		// 2. initialized notification, only meaningful once a handshake happened
		if err := writeNotification(stdin, "notifications/initialized", nil); err != nil {
			return Result{}, err
		}
	} else {
		// Every stateless request carries its own client identity instead.
		params = withClientMeta(params)
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

// withClientMeta adds the self-describing _meta a stateless server expects,
// merging into whatever the captured request already carried so a progress
// token or anything else survives. The key names match the ones the store
// parses, so a replayed frame reads the same as a live one.
func withClientMeta(params json.RawMessage) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if len(params) > 0 {
		if json.Unmarshal(params, &obj) != nil {
			return params // not a JSON object, nothing safe to merge into
		}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := obj["_meta"]; ok {
		if json.Unmarshal(raw, &meta) != nil {
			return params // a _meta we do not understand, leave the request alone
		}
	}
	meta["io.modelcontextprotocol/protocolVersion"] = json.RawMessage(strconv.Quote(statelessProtocolVersion))
	meta["io.modelcontextprotocol/clientInfo"] = json.RawMessage(
		fmt.Sprintf(`{"name":%q,"version":"dev"}`, clientName))

	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return params
	}
	obj["_meta"] = encodedMeta
	encoded, err := json.Marshal(obj)
	if err != nil {
		return params
	}
	return encoded
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
