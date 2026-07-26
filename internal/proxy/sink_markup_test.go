package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAsyncSinkPreservesMarkupInPayloads. Raw is a json.RawMessage, and
// encoding/json escapes <, > and & inside one, so the trace file used to hold
// bytes the server never sent. This is the test that fails when only redactRaw
// is fixed: the sink escaped the result again on the way to the file.
func TestAsyncSinkPreservesMarkupInPayloads(t *testing.T) {
	const payload = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"<b>hi</b> & bye"}]}}`
	var buf bytes.Buffer
	sink := NewAsyncSink(&buf, 16)
	sink.Emit(Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: ServerToClient, Transport: TransportStdio,
		Raw: json.RawMessage(payload),
	})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `\u003c`) {
		t.Fatalf("the trace file must not rewrite markup:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "<b>hi</b> & bye") {
		t.Fatalf("the payload should survive verbatim:\n%s", buf.String())
	}
	// And it must still be valid JSON that round-trips to the same bytes.
	var env Envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if string(env.Raw) != payload {
		t.Fatalf("round-trip changed the payload:\n got %s\nwant %s", env.Raw, payload)
	}
}

// TestSocketSinkPreservesMarkupInPayloads covers the other writer on the shim
// side. The TUI reads this stream, so escaping here rewrote the payload for the
// live view even when nothing was ever written to disk.
func TestSocketSinkPreservesMarkupInPayloads(t *testing.T) {
	const payload = `{"jsonrpc":"2.0","id":1,"result":{"text":"a < b && c > d"}}`
	// Not t.TempDir: on macOS it sits under /var/folders/... and the whole path
	// overruns sun_path, which the repo already guards against in paths.
	dir, err := os.MkdirTemp("/tmp", "mcpsnoop")
	if err != nil {
		t.Skip("no short temp dir for a unix socket:", err)
	}
	defer os.RemoveAll(dir)
	addr := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			got <- "read: " + err.Error()
			return
		}
		got <- line
	}()

	sink := NewSocketSink(addr, 16)
	defer sink.Close()
	sink.Emit(Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: ServerToClient, Transport: TransportStdio,
		Raw: json.RawMessage(payload),
	})

	select {
	case line := <-got:
		if strings.Contains(line, `\u003c`) {
			t.Fatalf("the live stream must not rewrite markup: %s", line)
		}
		if !strings.Contains(line, `a < b && c > d`) {
			t.Fatalf("the payload should reach the TUI verbatim: %s", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no envelope reached the socket")
	}
}
