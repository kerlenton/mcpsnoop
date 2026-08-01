package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// readerFunc adapts a function to an io.Reader.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

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

// TestStdioReportsExitCodeWhenServerExitsWithClientIdle guards the hang where the
// wrapped server exits on its own while the client keeps stdin open. The
// client->server pump blocks on that idle stdin forever, so RunStdio must await
// only the server-side pumps or it never reaps the process or reports the code.
func TestStdioReportsExitCodeWhenServerExitsWithClientIdle(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked) // release the detached pump at test end
	in := readerFunc(func([]byte) (int, error) { <-blocked; return 0, io.EOF })

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := RunStdio(context.Background(), StdioConfig{
			Command:   []string{"sh", "-c", "exit 3"},
			SessionID: "exit-test",
			Sink:      &captureSink{},
			In:        in,
			Out:       &bytes.Buffer{},
			Err:       &bytes.Buffer{},
		})
		done <- result{code, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("RunStdio: %v", r.err)
		}
		if r.code != 3 {
			t.Fatalf("exit code = %d, want 3", r.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunStdio hung waiting on the idle client->server pump")
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

// TestPumpFramesFlagsTruncated checks that stdio observation marks an oversized
// line as truncated, so the store reports a capped copy rather than diagnosing
// the server's stream as corrupted.
//
// The cap is an argument rather than a package variable a test lowers: mutating
// shared state to reach a branch leaves the branch reachable from anywhere, and
// this package runs its taps from goroutines.
func TestPumpFramesFlagsTruncated(t *testing.T) {
	const observeCap = 32
	line := strings.Repeat("x", 48) + "\n"

	var out bytes.Buffer
	var truncated bool
	var observed []byte
	pumpFrames(strings.NewReader(line), &out, observeCap, func(l []byte, tr bool) {
		observed = append([]byte(nil), l...)
		truncated = tr
	})

	// Forwarding first: the observer's cap must never reach the data path.
	if out.String() != line {
		t.Fatalf("passthrough mismatch: got %q want %q", out.String(), line)
	}
	if !truncated {
		t.Fatal("expected truncated=true for an oversized stdio line")
	}
	if len(observed) != observeCap {
		t.Fatalf("observed len = %d, want %d", len(observed), observeCap)
	}
	// The copy stops short of the terminator, so nothing may be stripped as one.
	// Every byte here is content, and a line that lost one to terminator handling
	// would come back shorter than the cap.
	if strings.Trim(string(observed), "x") != "" {
		t.Fatalf("a truncated copy lost content to terminator stripping: %q", observed)
	}
}

// TestPumpFramesLeavesAWholeLineUnflagged pins the other side. A cap that always
// trips would satisfy the test above while flagging every frame of a real
// session, and the terminator must still be stripped when it is actually there.
func TestPumpFramesLeavesAWholeLineUnflagged(t *testing.T) {
	const frame = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	var out bytes.Buffer
	var truncated bool
	var observed []byte
	pumpFrames(strings.NewReader(frame+"\r\n"), &out, maxFrameBytes, func(l []byte, tr bool) {
		observed = append([]byte(nil), l...)
		truncated = tr
	})

	if truncated {
		t.Fatal("a line well inside the cap must not be flagged")
	}
	if string(observed) != frame {
		t.Fatalf("observed %q, want the frame without its CRLF: %q", observed, frame)
	}
}

// TestTruncatedFrameSurvivesTheSink is the regression the truncation flag turns
// on. Envelope.Raw is a json.RawMessage, and both sinks encode the envelope with
// encoding/json, which validates it. A truncated frame is a fragment and so is
// never valid JSON, so putting one in Raw makes the whole envelope fail to
// marshal. The two sinks answer that differently and both badly: AsyncSink
// discards the write, so the frame never reaches the trace file, and SocketSink
// reads the error as the hub having gone away and drops the connection.
//
// splitObserved is what keeps that from happening: a fragment goes to Text,
// which encodes fine, and the flag is what says the copy is short.
func TestTruncatedFrameSurvivesTheSink(t *testing.T) {
	// Valid JSON in full, a fragment once capped, which is the shape of every
	// frame this path exists for.
	full := `{"jsonrpc":"2.0","id":1,"result":{"content":"` + strings.Repeat("y", 64) + `"}}`
	fragment := full[:40]
	if json.Valid([]byte(fragment)) {
		t.Fatal("this test needs a prefix that is not valid JSON on its own")
	}

	var buf bytes.Buffer
	sink := NewAsyncSink(&buf, 16)
	raw, text := splitObserved([]byte(fragment))
	sink.Emit(Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: ServerToClient, Transport: TransportStdio,
		Raw: raw, Text: text, Truncated: true,
	})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	var env Envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("the truncated frame never reached the sink: %v (wrote %q)", err, buf.String())
	}
	if !env.Truncated {
		t.Fatal("the frame should arrive flagged truncated")
	}
	if len(env.Raw) != 0 {
		t.Fatalf("a fragment must not travel in Raw, it is not valid JSON: %s", env.Raw)
	}
	if env.Text != fragment {
		t.Fatalf("the fragment should survive in Text, got %q", env.Text)
	}
}

// TestStdioSeqReachesTheSinkInOrder. Three pumps call emit at once, the client,
// the server and stderr. An atomic counter only makes each number unique, not the
// order they reach the sink in, and the store infers a dropped frame from a
// forward jump in Seq, so a pair that arrived swapped was counted as loss on a
// capture that had lost nothing. That invented figure reached
// `check --fail-on incomplete`, the JSON export, the HAR comment, the OTLP
// attributes and the TUI banner alike.
func TestStdioSeqReachesTheSinkInOrder(t *testing.T) {
	const frames = 8000
	var input strings.Builder
	for i := range frames {
		fmt.Fprintf(&input, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"ping"}}`+"\n", i+1)
	}
	// A wrapped server that echoes every line and writes to stderr at the same
	// time, so all three pumps are live together.
	script := `while IFS= read -r line; do printf '%s\n' "$line"; printf 'noise\n' >&2; done`

	sink := &orderedSink{}
	code, err := RunStdio(context.Background(), StdioConfig{
		Command:   []string{"/bin/sh", "-c", script},
		Label:     "test",
		SessionID: "order-1",
		Sink:      sink,
		In:        strings.NewReader(input.String()),
		Out:       io.Discard,
		Err:       io.Discard,
	})
	if err != nil {
		t.Fatalf("RunStdio: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if sink.outOfOrder > 0 {
		t.Fatalf("%d of %d envelopes reached the sink out of Seq order, which the store counts as dropped frames",
			sink.outOfOrder, sink.count)
	}
	if sink.count < frames {
		t.Fatalf("only %d envelopes observed, want at least %d", sink.count, frames)
	}
}

type orderedSink struct {
	mu         sync.Mutex
	last       uint64
	count      int
	outOfOrder int
}

func (s *orderedSink) Emit(e Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Seq <= s.last {
		s.outOfOrder++
	}
	s.last = e.Seq
	s.count++
}

func (s *orderedSink) Close() error { return nil }
