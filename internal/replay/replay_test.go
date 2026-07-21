package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// demoBin is a tiny stdio MCP server built once for these tests, self-contained
// so the suite has no dependency on any examples/ directory.
var demoBin string

// tinyServer is a minimal MCP stdio server, handling initialize, and tools/call where
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

// statelessServer is a fixture on the 2026-07-28 revision. It has no handshake,
// so initialize is method-not-found, and it rejects a tools/call that does not
// carry the self-describing _meta, which is what proves replay sends it.
const statelessServer = `package main

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
	var req map[string]any
	if json.Unmarshal(line, &req) != nil {
		return
	}
	id, ok := req["id"]
	if !ok {
		return
	}
	method, _ := req["method"].(string)
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	switch method {
	case "initialize":
		out["error"] = map[string]any{"code": -32601, "message": "method not found: initialize"}
	case "tools/call":
		params, _ := req["params"].(map[string]any)
		meta, _ := params["_meta"].(map[string]any)
		version, _ := meta["io.modelcontextprotocol/protocolVersion"].(string)
		if version == "" {
			out["error"] = map[string]any{"code": -32602, "message": "request is not self-describing"}
			break
		}
		name, _ := params["name"].(string)
		if name != "echo" {
			out["error"] = map[string]any{"code": -32601, "message": "unknown tool: " + name}
			break
		}
		time.Sleep(120 * time.Millisecond)
		token, _ := meta["progressToken"].(string)
		out["result"] = map[string]any{"content": []map[string]string{
			{"type": "text", "text": "stateless echo " + version + " token=" + token},
		}}
	default:
		out["error"] = map[string]any{"code": -32601, "message": "method not found: " + method}
	}
	b, _ := json.Marshal(out)
	os.Stdout.Write(append(b, '\n'))
}
`

// statelessBin is the fixture above, built alongside demoBin.
var statelessBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcpsnoop-replay")
	if err != nil {
		panic(err)
	}
	demoBin = buildFixture(dir, "tinyserver", tinyServer)
	statelessBin = buildFixture(dir, "statelessserver", statelessServer)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// buildFixture compiles one fixture server into its own temp module, so the
// suite never builds against the mcpsnoop module.
func buildFixture(root, name, source string) string {
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+name+"\n\ngo 1.21\n"), 0o600); err != nil {
		panic(err)
	}
	bin := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		panic("build " + name + ": " + err.Error() + "\n" + string(out))
	}
	return bin
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

func TestReadResponseSkipsIntermediateAndMatchesByID(t *testing.T) {
	stream := `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":1,"result":{"other":true}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"result":{"ok":true}}` + "\n"
	resp, err := readResponse(bufio.NewReader(strings.NewReader(stream)), "2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), `"ok":true`) {
		t.Fatalf("matched the wrong response: %s", resp)
	}
}

func TestReadResponseRejectsMalformedResponse(t *testing.T) {
	// A response for our id with neither result nor error must error promptly,
	// not spin until the reader hits EOF or the deadline.
	stream := `{"jsonrpc":"2.0","id":2}` + "\n"
	if _, err := readResponse(bufio.NewReader(strings.NewReader(stream)), "2"); err == nil {
		t.Fatal("malformed response should error")
	}
}

// A server on the new revision has no handshake at all, so replay has to notice
// the initialize error and carry on rather than giving up or hanging.
func TestReplayAgainstStatelessServer(t *testing.T) {
	res, err := Replay(context.Background(), []string{statelessBin}, "",
		"tools/call", json.RawMessage(`{"name":"echo","arguments":{"text":"hi"}}`), 10*time.Second)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("unexpected rpc error: %+v", res.Err)
	}
	// The fixture refuses a request without _meta, so reaching a result at all
	// proves the request described itself.
	if !strings.Contains(string(res.RPCResult), "stateless echo "+statelessProtocolVersion) {
		t.Fatalf("result missing the self-describing metadata: %s", res.RPCResult)
	}
}

// The legacy path has to keep working unchanged, since servers on the older
// revision stay supported for at least a year after 2026-07-28.
func TestReplayStillHandshakesWithLegacyServer(t *testing.T) {
	res, err := Replay(context.Background(), []string{demoBin}, "",
		"tools/call", json.RawMessage(`{"name":"echo","arguments":{"text":"hi"}}`), 10*time.Second)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("unexpected rpc error: %+v", res.Err)
	}
	if !strings.Contains(string(res.RPCResult), "echo: hi") {
		t.Fatalf("legacy replay broke: %s", res.RPCResult)
	}
}

func TestWithClientMetaMergesAndPreserves(t *testing.T) {
	got := withClientMeta(json.RawMessage(`{"name":"echo","_meta":{"progressToken":"abc"}}`))

	var obj struct {
		Name string `json:"name"`
		Meta struct {
			ProtocolVersion string          `json:"io.modelcontextprotocol/protocolVersion"`
			ClientInfo      json.RawMessage `json:"io.modelcontextprotocol/clientInfo"`
			ProgressToken   string          `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("result is not valid JSON: %v (%s)", err, got)
	}
	if obj.Name != "echo" {
		t.Fatalf("the original params were lost: %s", got)
	}
	if obj.Meta.ProtocolVersion != statelessProtocolVersion {
		t.Fatalf("protocol version = %q, want %q", obj.Meta.ProtocolVersion, statelessProtocolVersion)
	}
	if len(obj.Meta.ClientInfo) == 0 {
		t.Fatal("client info missing")
	}
	if obj.Meta.ProgressToken != "abc" {
		t.Fatal("an existing _meta entry must survive the merge")
	}
}

func TestWithClientMetaHandlesEmptyAndNonObjectParams(t *testing.T) {
	var empty struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(withClientMeta(nil), &empty); err != nil {
		t.Fatalf("empty params should still produce a valid object: %v", err)
	}
	if len(empty.Meta) == 0 {
		t.Fatal("empty params should still get the self-describing metadata")
	}
	// An array is not something we can merge into, so it has to pass through.
	array := json.RawMessage(`[1,2,3]`)
	if got := withClientMeta(array); string(got) != string(array) {
		t.Fatalf("non-object params should pass through unchanged, got %s", got)
	}
}
