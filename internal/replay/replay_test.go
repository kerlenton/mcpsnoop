package replay

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// demoBin is a tiny stdio MCP server built once for these tests — self-contained
// so the suite has no dependency on any examples/ directory.
var demoBin string

// tinyServer is a minimal MCP stdio server: initialize, and tools/call where
// "echo" succeeds (after a short delay) and anything else is a JSON-RPC error.
const tinyServer = `package main

import ("bufio";"encoding/json";"os";"time")

func main() {
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			handle(line)
		}
		if err != nil {
			return
		}
	}
}

func handle(line []byte) {
	var req struct {
		ID     json.RawMessage ` + "`json:\"id\"`" + `
		Method string          ` + "`json:\"method\"`" + `
		Params struct {
			Name      string ` + "`json:\"name\"`" + `
			Arguments struct{ Text string ` + "`json:\"text\"`" + ` } ` + "`json:\"arguments\"`" + `
		} ` + "`json:\"params\"`" + `
	}
	if json.Unmarshal(line, &req) != nil || req.Method == "" || len(req.ID) == 0 {
		return
	}
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "tools/call":
		if req.Params.Name == "echo" {
			time.Sleep(120 * time.Millisecond)
			out["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": "echo: " + req.Params.Arguments.Text}}}
		} else {
			out["error"] = map[string]any{"code": -32601, "message": "unknown tool: " + req.Params.Name}
		}
	default:
		out["result"] = map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]string{"name": "tiny"}}
	}
	b, _ := json.Marshal(out)
	os.Stdout.Write(append(b, '\n'))
}
`

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcpsnoop-replay")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(tinyServer), 0o600); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tinyserver\n\ngo 1.21\n"), 0o600); err != nil {
		panic(err)
	}
	demoBin = filepath.Join(dir, "tinyserver")
	build := exec.Command("go", "build", "-o", demoBin, ".")
	build.Dir = dir // build the standalone temp module, not the mcpsnoop module
	if out, err := build.CombinedOutput(); err != nil {
		panic("build tiny server: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestReplaySuccess(t *testing.T) {
	res, err := Replay(context.Background(), []string{demoBin}, "",
		"tools/call", json.RawMessage(`{"name":"echo","arguments":{"text":"hi"}}`), 10*time.Second)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("unexpected rpc error: %+v", res.Err)
	}
	if !strings.Contains(string(res.RPCResult), "echo: hi") {
		t.Fatalf("result missing echo payload: %s", res.RPCResult)
	}
	// demo-server sleeps 120ms inside tools/call.
	if res.Duration < 100*time.Millisecond {
		t.Fatalf("duration = %v, want >= ~120ms", res.Duration)
	}
}

func TestReplayToolError(t *testing.T) {
	res, err := Replay(context.Background(), []string{demoBin}, "",
		"tools/call", json.RawMessage(`{"name":"does-not-exist"}`), 10*time.Second)
	if err != nil {
		t.Fatalf("Replay transport error: %v", err)
	}
	if res.Err == nil {
		t.Fatalf("expected an rpc error for unknown tool, got result %s", res.RPCResult)
	}
}

func TestReplayEmptyCommand(t *testing.T) {
	if _, err := Replay(context.Background(), nil, "", "tools/list", nil, time.Second); err == nil {
		t.Fatal("expected error for empty command")
	}
}
