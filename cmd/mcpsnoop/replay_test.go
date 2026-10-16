package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestReplayHelperServer is the server the replay tests drive, run as a child
// of the test binary. It never runs as a test of its own: without the
// variable it returns at once. MCPSNOOP_REPLAY_BROKEN makes its one tool fail,
// which is the regression a replay is there to catch.
func TestReplayHelperServer(t *testing.T) {
	if os.Getenv("MCPSNOOP_REPLAY_HELPER") != "1" {
		return
	}
	broken := os.Getenv("MCPSNOOP_REPLAY_BROKEN") == "1"
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		var result string
		switch msg.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"helper","version":"1"}}`
		case "tools/list":
			result = `{"tools":[{"name":"search","description":"Search","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"found"}]}`
			if broken {
				result = `{"content":[{"type":"text","text":"index missing"}],"isError":true}`
			}
		default:
			result = `{}`
		}
		os.Stdout.WriteString(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":` + result + "}\n")
	}
	os.Exit(0)
}

// replayFixture writes a recording of one session against the helper, the way
// it answers when it works, and returns its path and the helper's command.
func replayFixture(t *testing.T) (recording string, server []string) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "msr")
	if err != nil {
		t.Skip("no short temp dir for the state directory:", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("MCPSNOOP_HOME", home)
	t.Setenv("MCPSNOOP_REPLAY_HELPER", "1")

	recording = filepath.Join(t.TempDir(), "recording.jsonl")
	at := func(seconds int) time.Time { return time.Unix(1_700_000_000+int64(seconds), 0) }
	writeDiffLog(t, recording,
		diffEnvelope("rec", 2, at(0), proxy.ClientToServer, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"cli","version":"1"}}}`),
		diffEnvelope("rec", 3, at(0).Add(time.Millisecond), proxy.ServerToClient, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"helper","version":"1"}}}`),
		diffEnvelope("rec", 4, at(2), proxy.ClientToServer, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		diffEnvelope("rec", 5, at(2).Add(time.Millisecond), proxy.ServerToClient, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search","inputSchema":{"type":"object"}}]}}`),
		diffEnvelope("rec", 6, at(60), proxy.ClientToServer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"replay"}}}`),
		diffEnvelope("rec", 7, at(60).Add(time.Millisecond), proxy.ServerToClient, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"found"}]}}`),
	)
	return recording, []string{os.Args[0], "-test.run=^TestReplayHelperServer$"}
}

func executeReplay(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	cmd := newReplayCmd()
	cmd.SetArgs(args)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var code exitCode
	if !errors.As(err, &code) {
		t.Fatalf("unexpected command error: %v", err)
	}
	return int(code), stdout.String(), stderr.String()
}

func TestReplayCommandWritesACaptureAndCompares(t *testing.T) {
	recording, server := replayFixture(t)
	out := filepath.Join(t.TempDir(), "replayed.jsonl")
	code, stdout, stderr := executeReplay(t, append([]string{recording, "--out", out, "--exit-code", "--"}, server...))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	for _, want := range []string{"replayed 3 of 3 client frames from session rec", "capture: " + out, "no differences found"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
	// The capture is an ordinary one, so check reads it and finds the call.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name":"search"`) || !strings.Contains(string(data), `"direction":"meta"`) {
		t.Fatalf("the replayed capture is not a session log:\n%s", data)
	}
}

func TestReplayCommandExitCodeGatesOnRegression(t *testing.T) {
	recording, server := replayFixture(t)
	t.Setenv("MCPSNOOP_REPLAY_BROKEN", "1")
	out := filepath.Join(t.TempDir(), "replayed.jsonl")

	code, stdout, _ := executeReplay(t, append([]string{recording, "--out", out, "--"}, server...))
	if code != 0 || !strings.Contains(stdout, `status changed: search {"q":"replay"} ok -> error`) {
		t.Fatalf("without --exit-code: exit = %d, stdout = %q", code, stdout)
	}
	if code, stdout, _ := executeReplay(t, append([]string{recording, "--out", out, "--exit-code", "--"}, server...)); code != 1 {
		t.Fatalf("with --exit-code: exit = %d, want 1, stdout = %q", code, stdout)
	}
}

func TestReplayCommandRefusesWhatItCannotDo(t *testing.T) {
	recording, server := replayFixture(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no server", []string{recording, "extra"}, "give one capture, then --"},
		{"no capture", append([]string{filepath.Join(t.TempDir(), "missing.jsonl"), "--"}, server...), "missing.jsonl"},
		{"the recording as the output", append([]string{recording, "--out", recording, "--"}, server...), "--out names the recording itself"},
		{"a server that will not start", []string{recording, "--out", filepath.Join(t.TempDir(), "o.jsonl"), "--", "/nonexistent/mcpsnoop-no-server"}, "replay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := executeReplay(t, tc.args)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d, stderr = %q, want 2 and %q", code, stderr, tc.want)
			}
		})
	}
	// The recording survives an attempt to write over it.
	if data, err := os.ReadFile(recording); err != nil || !strings.Contains(string(data), "tools/call") {
		t.Fatalf("the recording was damaged: %v", err)
	}
}

func TestReplayCommandWillNotSendPlaceholdersUnasked(t *testing.T) {
	_, server := replayFixture(t)
	recording := filepath.Join(t.TempDir(), "redacted.jsonl")
	redacted := diffEnvelope("rec", 2, time.Unix(1, 0), proxy.ClientToServer,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"token":"[REDACTED]"}}}`)
	redacted.Redacted = true
	writeDiffLog(t, recording, redacted)

	out := filepath.Join(t.TempDir(), "o.jsonl")
	if code, _, stderr := executeReplay(t, append([]string{recording, "--out", out, "--"}, server...)); code != 2 || !strings.Contains(stderr, "were redacted when recorded") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if code, _, stderr := executeReplay(t, append([]string{recording, "--out", out, "--allow-redacted", "--"}, server...)); code != 0 {
		t.Fatalf("with --allow-redacted: exit = %d, stderr = %q", code, stderr)
	}
}

// helperHTTP answers the same three methods as the stdio helper, one JSON
// object per POST, the way a stateless Streamable HTTP server does.
func helperHTTP(broken bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &msg) != nil || len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := `{}`
		switch msg.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"helper","version":"1"}}`
		case "tools/list":
			result = `{"tools":[{"name":"search","description":"Search","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"found"}]}`
			if broken {
				result = `{"content":[{"type":"text","text":"index missing"}],"isError":true}`
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":`+result+`}`)
	})
}

// TestReplayHelperHTTPServer is the HTTP server a replay test starts as a
// child of the test binary, listening on the port it is given.
func TestReplayHelperHTTPServer(t *testing.T) {
	addr := os.Getenv("MCPSNOOP_REPLAY_HTTP_HELPER")
	if addr == "" {
		return
	}
	_ = http.ListenAndServe(addr, helperHTTP(false))
	os.Exit(0)
}

func TestReplayCommandOverHTTP(t *testing.T) {
	recording, _ := replayFixture(t)

	t.Run("against a server that is already running", func(t *testing.T) {
		good := httptest.NewServer(helperHTTP(false))
		defer good.Close()
		out := filepath.Join(t.TempDir(), "replayed.jsonl")
		code, stdout, stderr := executeReplay(t, []string{recording, "--target", good.URL + "/mcp", "--out", out, "--exit-code"})
		if code != 0 || !strings.Contains(stdout, "replayed 3 of 3 client frames") || !strings.Contains(stdout, "no differences found") {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "captured over stdio, and its frames were replayed over HTTP") {
			t.Fatalf("the report should say the transport changed: %q", stdout)
		}
		data, _ := os.ReadFile(out)
		if !strings.Contains(string(data), `"transport":"http"`) || !strings.Contains(string(data), `"status":200`) {
			t.Fatalf("the capture is not an HTTP one:\n%s", data)
		}

		broken := httptest.NewServer(helperHTTP(true))
		defer broken.Close()
		code, stdout, _ = executeReplay(t, []string{recording, "--target", broken.URL + "/mcp", "--out", out, "--exit-code"})
		if code != 1 || !strings.Contains(stdout, `status changed: search {"q":"replay"} ok -> error`) {
			t.Fatalf("exit = %d, stdout = %q", code, stdout)
		}
	})

	t.Run("starting the server itself", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		t.Setenv("MCPSNOOP_REPLAY_HTTP_HELPER", addr)
		out := filepath.Join(t.TempDir(), "replayed.jsonl")
		code, stdout, stderr := executeReplay(t, []string{recording, "--target", "http://" + addr + "/mcp", "--out", out, "--exit-code",
			"--", os.Args[0], "-test.run=^TestReplayHelperHTTPServer$"})
		if code != 0 || !strings.Contains(stdout, "no differences found") {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		// The server was stopped with the replay, so the port is free again.
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				break
			}
			_ = conn.Close()
			if time.Now().After(deadline) {
				t.Fatal("the server the replay started is still listening after it finished")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("arguments that do not make sense", func(t *testing.T) {
		for name, args := range map[string][]string{
			"a header without a target":  {recording, "--header", "Authorization: Bearer x", "--", "node"},
			"a header that is not one":   {recording, "--target", "http://127.0.0.1:1/mcp", "--header", "nonsense"},
			"a target and a stray word":  {recording, "--target", "http://127.0.0.1:1/mcp", "stray"},
			"a target that is not a URL": {recording, "--target", "ftp://example.com/mcp"},
		} {
			t.Run(name, func(t *testing.T) {
				if code, _, stderr := executeReplay(t, args); code != 2 {
					t.Fatalf("exit = %d, stderr = %q, want 2", code, stderr)
				}
			})
		}
	})
}
