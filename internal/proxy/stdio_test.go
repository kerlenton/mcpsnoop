package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

// captureSink collects envelopes for assertions.
type captureSink struct {
	mu   sync.Mutex
	envs []Envelope
}

func (c *captureSink) Emit(e Envelope) {
	c.mu.Lock()
	c.envs = append(c.envs, e)
	c.mu.Unlock()
}
func (c *captureSink) Close() error { return nil }

func (c *captureSink) byDir(d Direction) []Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Envelope
	for _, e := range c.envs {
		if e.Direction == d {
			out = append(out, e)
		}
	}
	return out
}

// TestStdioTransparency uses `cat` as the wrapped "server", it echoes stdin to
// stdout. The proxy must pass bytes through verbatim and observe both
// directions.
func TestStdioTransparency(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ping"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"

	var out bytes.Buffer
	sink := &captureSink{}

	code, err := RunStdio(context.Background(), StdioConfig{
		Command:   []string{"cat"},
		Label:     "test",
		SessionID: "test-1",
		Sink:      sink,
		In:        strings.NewReader(input),
		Out:       &out,
		Err:       &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("RunStdio: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// Passthrough must be byte-identical (cat echoes input to output).
	if out.String() != input {
		t.Fatalf("passthrough mismatch:\n got: %q\nwant: %q", out.String(), input)
	}

	c2s := sink.byDir(ClientToServer)
	s2c := sink.byDir(ServerToClient)
	if len(c2s) != 2 {
		t.Fatalf("c2s frames = %d, want 2", len(c2s))
	}
	if len(s2c) != 2 {
		t.Fatalf("s2c frames = %d, want 2", len(s2c))
	}

	// The first c2s frame should parse as a tools/call request.
	msg, ok := ParseRPC(c2s[0].Raw)
	if !ok {
		t.Fatalf("first c2s frame did not parse as JSON-RPC: %q", c2s[0].Raw)
	}
	if !msg.IsRequest() || msg.Method != "tools/call" {
		t.Fatalf("first frame: method=%q isRequest=%v, want tools/call request", msg.Method, msg.IsRequest())
	}
	// The second is a notification.
	msg2, _ := ParseRPC(c2s[1].Raw)
	if !msg2.IsNotification() {
		t.Fatalf("second frame should be a notification, got %+v", msg2)
	}
}

// TestParseRPCResponse checks response classification.
func TestParseRPCResponse(t *testing.T) {
	msg, ok := ParseRPC([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	if !ok || !msg.IsResponse() {
		t.Fatalf("expected a response, got ok=%v msg=%+v", ok, msg)
	}
	emsg, ok := ParseRPC([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no"}}`))
	if !ok || !emsg.IsResponse() || emsg.Error == nil {
		t.Fatalf("expected an error response, got ok=%v msg=%+v", ok, emsg)
	}
}

// TestStdioNonJSONLine feeds a stray non-JSON line through `cat`, which echoes
// it, so the line appears in both directions. A json.RawMessage cannot hold
// non-JSON bytes, so the shim must carry the line as Text, otherwise the
// envelope fails to encode and the frame is silently dropped before the hub.
func TestStdioNonJSONLine(t *testing.T) {
	const stray = "[debug] not json-rpc"
	input := stray + "\n" +
		`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

	var out bytes.Buffer
	sink := &captureSink{}
	code, err := RunStdio(context.Background(), StdioConfig{
		Command:   []string{"cat"},
		Label:     "test",
		SessionID: "test-nonjson",
		Sink:      sink,
		In:        strings.NewReader(input),
		Out:       &out,
		Err:       &bytes.Buffer{},
	})
	if err != nil || code != 0 {
		t.Fatalf("RunStdio: err=%v code=%d", err, code)
	}
	if out.String() != input {
		t.Fatalf("passthrough mismatch:\n got: %q\nwant: %q", out.String(), input)
	}

	for _, dir := range []Direction{ClientToServer, ServerToClient} {
		envs := sink.byDir(dir)
		if len(envs) != 2 {
			t.Fatalf("%s frames = %d, want 2", dir, len(envs))
		}
		got := envs[0]
		if len(got.Raw) != 0 || got.Text != stray {
			t.Fatalf("%s stray line: raw=%q text=%q, want it carried as Text %q", dir, got.Raw, got.Text, stray)
		}
		// The real failure mode this guards, the envelope must actually encode.
		if err := json.NewEncoder(io.Discard).Encode(got); err != nil {
			t.Fatalf("%s stray envelope failed to encode: %v", dir, err)
		}
		// The following JSON-RPC line still travels in Raw and parses.
		if _, ok := ParseRPC(envs[1].Raw); !ok {
			t.Fatalf("%s second frame should parse as JSON-RPC, raw=%q", dir, envs[1].Raw)
		}
	}
}
