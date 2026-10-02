package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/sessiondiff"
)

// testFrame is one captured frame to fold into a test capture.
type testFrame struct {
	dir      proxy.Direction
	rpc      string // JSON-RPC bytes, "" for none
	redacted bool
}

func metaFrame() testFrame {
	return testFrame{dir: proxy.DirectionMeta, rpc: `{"command":["srv"],"cwd":"/tmp"}`}
}

func c2s(rpc string) testFrame { return testFrame{dir: proxy.ClientToServer, rpc: rpc} }
func s2c(rpc string) testFrame { return testFrame{dir: proxy.ServerToClient, rpc: rpc} }

// loadFrames builds a JSONL capture from frames and loads it, failing the test
// on any load error.
func loadFrames(t *testing.T, frames []testFrame) *Server {
	t.Helper()
	return loadEnvelopes(t, frameEnvelopes(t, "test-session", proxy.TransportStdio, frames))
}

// frameEnvelopes numbers frames into envelopes for one session and transport.
// A transport of "" marks nothing, which is what a legacy log looks like.
func frameEnvelopes(t *testing.T, session, transport string, frames []testFrame) []proxy.Envelope {
	t.Helper()
	out := make([]proxy.Envelope, 0, len(frames))
	for i, f := range frames {
		env := proxy.Envelope{
			SessionID:   session,
			ServerLabel: "test",
			Seq:         uint64(i + 1),
			TS:          time.Now(),
			Direction:   f.dir,
			Transport:   transport,
			Redacted:    f.redacted,
		}
		if f.rpc != "" {
			env.Raw = json.RawMessage(f.rpc)
		}
		out = append(out, env)
	}
	return out
}

// loadEnvelopes serves prebuilt envelopes as a JSONL capture.
func loadEnvelopes(t *testing.T, envs []proxy.Envelope) *Server {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	srv, err := Load(&buf, "test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return srv
}

// req builds one JSON-RPC request line.
func req(id, method, params string) string {
	if params == "" {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q}`, id, method)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, id, method, params)
}

// serveResult is one Serve run over a fixed input.
type serveResult struct {
	responses []proxy.RPCMessage
	raws      []string
	stderr    string
	code      int
}

func serveInput(t *testing.T, srv *Server, input string, policy Policy) serveResult {
	t.Helper()
	var out, errB bytes.Buffer
	code := srv.Serve(strings.NewReader(input), &out, &errB, policy)
	res := serveResult{stderr: errB.String(), code: code}
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Every stdout line must be a protocol frame. Parsing here keeps the
		// stdout-purity assertion in every test that serves.
		msg, ok := proxy.ParseRPC([]byte(line))
		if !ok {
			t.Fatalf("stdout line is not a JSON-RPC message: %q", line)
		}
		res.responses = append(res.responses, msg)
		res.raws = append(res.raws, line)
	}
	return res
}

func defaultPolicy() Policy { return Policy{} }

func canonical(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON %q: %v", raw, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExactReplay(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		metaFrame(),
		c2s(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"echo: hi"}]}}`),
	})
	res := serveInput(t, srv, req("9", "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`)+"\n", defaultPolicy())
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr = %q", res.code, res.stderr)
	}
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	got := res.responses[0]
	if string(got.ID) != "9" {
		t.Fatalf("response id = %s, want 9", got.ID)
	}
	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	if !strings.Contains(string(got.Result), "echo: hi") {
		t.Fatalf("result = %s, want the recorded echo", got.Result)
	}
}

func TestIncomingIDReplacesRecordedID(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	res := serveInput(t, srv, req(`"abc-1"`, "tools/list", `{}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	if string(res.responses[0].ID) != `"abc-1"` {
		t.Fatalf("response id = %s, want \"abc-1\"", res.responses[0].ID)
	}
	if strings.Contains(res.raws[0], `"id":1,`) || strings.Contains(res.raws[0], `"id":1}`) {
		t.Fatalf("recorded id leaked into response: %s", res.raws[0])
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	res := serveInput(t, srv,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+
			req("1", "tools/list", `{}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1 (notification draws no reply)", len(res.responses))
	}
}

func TestVolatileMetaDifferencesStillMatch(t *testing.T) {
	recorded := `{"name":"search","arguments":{"q":"kafka"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"driver","version":"0.1"},"io.modelcontextprotocol/clientCapabilities":{},"progressToken":"p-3","traceparent":"00-aaa-bbb-01","tracestate":"a=b","baggage":"x=y"}}`
	srv := loadFrames(t, []testFrame{
		c2s(req("3", "tools/call", recorded)),
		s2c(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"hit for kafka"}]}}`),
	})
	incoming := `{"name":"search","arguments":{"q":"kafka"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"other","version":"9.9"},"io.modelcontextprotocol/clientCapabilities":{"elicitation":{}},"progressToken":"p-9"}}`
	res := serveInput(t, srv, req("7", "tools/call", incoming)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1, stderr = %q", len(res.responses), res.stderr)
	}
	if res.responses[0].Error != nil {
		t.Fatalf("volatile _meta differences must still match: %+v", res.responses[0].Error)
	}
	if !strings.Contains(string(res.responses[0].Result), "hit for kafka") {
		t.Fatalf("result = %s, want the recorded kafka hit", res.responses[0].Result)
	}
}

// TestReservedMetaKeysSplitByWhetherTheyChangeTheAnswer pins the one judgement
// the matcher makes. The spec reserves a fixed set of _meta keys and they do
// not all mean the same thing here. Progress, client identity, logging level
// and the trace keys are per-request client preference that a recorded answer
// cannot depend on, so a replaying client which sets them differently has to
// match anyway, and under a strict matcher a miss is a hard failure rather than
// a worse answer. The protocol version is the era, which does change what an
// answer means, so it stays in the key.
func TestReservedMetaKeysSplitByWhetherTheyChangeTheAnswer(t *testing.T) {
	const base = `"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}`
	recorded := `{"name":"search","arguments":{"q":"kafka"},"_meta":{` + base + `}}`

	for _, tc := range []struct {
		name  string
		extra string
	}{
		{"progressToken", `"progressToken":"p-9"`},
		{"clientInfo", `"io.modelcontextprotocol/clientInfo":{"name":"other","version":"9.9"}`},
		{"logLevel", `"io.modelcontextprotocol/logLevel":"debug"`},
		{"traceparent", `"traceparent":"00-aaa-bbb-01"`},
		{"tracestate", `"tracestate":"a=b"`},
		{"baggage", `"baggage":"x=y"`},
	} {
		t.Run(tc.name+" does not change the answer", func(t *testing.T) {
			srv := loadFrames(t, []testFrame{
				c2s(req("3", "tools/call", recorded)),
				s2c(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"hit for kafka"}]}}`),
			})
			incoming := `{"name":"search","arguments":{"q":"kafka"},"_meta":{` + base + `,` + tc.extra + `}}`
			res := serveInput(t, srv, req("7", "tools/call", incoming)+"\n", defaultPolicy())
			if len(res.responses) != 1 {
				t.Fatalf("responses = %d, want 1, stderr = %q", len(res.responses), res.stderr)
			}
			if res.responses[0].Error != nil {
				t.Fatalf("a client setting %s must still match the recording: %+v", tc.name, res.responses[0].Error)
			}
		})
	}

	t.Run("protocolVersion does change the answer", func(t *testing.T) {
		srv := loadFrames(t, []testFrame{
			c2s(req("3", "tools/call", recorded)),
			s2c(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"hit for kafka"}]}}`),
		})
		incoming := `{"name":"search","arguments":{"q":"kafka"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18","io.modelcontextprotocol/clientCapabilities":{}}}`
		res := serveInput(t, srv, req("7", "tools/call", incoming)+"\n", defaultPolicy())
		if len(res.responses) != 1 {
			t.Fatalf("responses = %d, want 1", len(res.responses))
		}
		if res.responses[0].Error == nil {
			t.Fatalf("another era is another call, got result %s", res.responses[0].Result)
		}
	})
}

func TestSemanticParamDifferencesDoNotMatch(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("3", "tools/call", `{"name":"search","arguments":{"q":"kafka"}}`)),
		s2c(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"hit for kafka"}]}}`),
	})
	res := serveInput(t, srv, req("9", "tools/call", `{"name":"search","arguments":{"q":"redis"}}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	got := res.responses[0]
	if got.Error == nil {
		t.Fatalf("a semantic difference must not match, got result %s", got.Result)
	}
	if !strings.Contains(got.Error.Message, "tools/call") {
		t.Fatalf("unmatched error must name the method: %+v", got.Error)
	}
	if !strings.Contains(res.stderr, "tools/call") {
		t.Fatalf("the miss must be visible on stderr: %q", res.stderr)
	}
}

func TestCustomMetaIsSemantic(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"echo","arguments":{"text":"hi"},"_meta":{"custom":"keep"}}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hi"}]}}`),
	})
	res := serveInput(t, srv, req("2", "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error == nil {
		t.Fatalf("a dropped non-volatile _meta key must not match, got %+v", res.responses)
	}
}

func TestKeyOrderDoesNotAffectMatching(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"echo","arguments":{"b":2,"a":1}}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`),
	})
	res := serveInput(t, srv, req("2", "tools/call", `{"arguments":{"a":1,"b":2},"name":"echo"}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error != nil {
		t.Fatalf("key order must not affect matching, got %+v stderr %q", res.responses, res.stderr)
	}
}

func TestRepeatedIdenticalRequestsReplaySequentially(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"counter"}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"n":1}}`),
		c2s(req("2", "tools/call", `{"name":"counter"}`)),
		s2c(`{"jsonrpc":"2.0","id":2,"result":{"n":2}}`),
	})
	res := serveInput(t, srv,
		req("10", "tools/call", `{"name":"counter"}`)+"\n"+
			req("11", "tools/call", `{"name":"counter"}`)+"\n"+
			req("12", "tools/call", `{"name":"counter"}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 3 {
		t.Fatalf("responses = %d, want 3", len(res.responses))
	}
	if string(res.responses[0].Result) != `{"n":1}` || string(res.responses[1].Result) != `{"n":2}` {
		t.Fatalf("out of order: %s %s", res.responses[0].Result, res.responses[1].Result)
	}
	if string(res.responses[0].ID) != "10" || string(res.responses[1].ID) != "11" {
		t.Fatalf("ids not replaced: %s %s", res.responses[0].ID, res.responses[1].ID)
	}
	// The third identical call was never recorded twice over: an error, never a
	// silent repeat of an unrelated answer.
	if third := res.responses[2]; third.Error == nil {
		t.Fatalf("exhausted queue must error, got %s", third.Result)
	}
}

func TestUnmatchedMethodErrorsAndNeverServesUnrelatedData(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	res := serveInput(t, srv, req("5", "tools/call", `{"name":"echo"}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	got := res.responses[0]
	if got.Error == nil {
		t.Fatalf("unmatched method must error, got %s", got.Result)
	}
	if got.Error.Code != -32601 {
		t.Fatalf("code = %d, want -32601", got.Error.Code)
	}
	if string(got.ID) != "5" {
		t.Fatalf("error must carry the incoming id, got %s", got.ID)
	}
	if strings.Contains(res.raws[0], `"tools"`) {
		t.Fatalf("unrelated recorded data leaked: %s", res.raws[0])
	}
}

func TestJSONRPCErrorReplays(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"nope"}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad args","data":{"field":"q"}}}`),
	})
	res := serveInput(t, srv, req("8", "tools/call", `{"name":"nope"}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	got := res.responses[0]
	if got.Error == nil {
		t.Fatalf("recorded error must replay as an error, got %s", got.Result)
	}
	if got.Error.Code != -32602 || got.Error.Message != "bad args" {
		t.Fatalf("error not preserved: %+v", got.Error)
	}
	if string(got.ID) != "8" {
		t.Fatalf("id = %s, want 8", got.ID)
	}
}

// wireObject decodes one stdout line into its top-level members for
// fidelity assertions.
func wireObject(t *testing.T, line string) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("stdout is not a JSON object: %q", line)
	}
	return obj
}

// TestRecordedMissingJsonrpcPreserved: a response the server sent without
// jsonrpc replays without one. The mock reproduces, never repairs.
func TestRecordedMissingJsonrpcPreserved(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"id":1,"result":{"tools":[]}}`),
	})
	res := serveInput(t, srv, req("9", "tools/list", `{}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1, stderr %q", len(res.responses), res.stderr)
	}
	obj := wireObject(t, res.raws[0])
	if _, ok := obj["jsonrpc"]; ok {
		t.Fatalf("replay must not add a missing jsonrpc: %s", res.raws[0])
	}
	if string(obj["id"]) != "9" {
		t.Fatalf("id = %s, want 9", obj["id"])
	}
	if canonical(t, obj["result"]) != `{"tools":[]}` {
		t.Fatalf("result changed: %s", obj["result"])
	}
}

// TestRecordedWrongJsonrpcPreserved: a non-2.0 jsonrpc replays unchanged.
func TestRecordedWrongJsonrpcPreserved(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"1.0","id":1,"result":{"tools":[]}}`),
	})
	res := serveInput(t, srv, req("9", "tools/list", `{}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	obj := wireObject(t, res.raws[0])
	if string(obj["jsonrpc"]) != `"1.0"` {
		t.Fatalf("jsonrpc = %s, want the recorded \"1.0\"", obj["jsonrpc"])
	}
}

// TestRecordedUnknownTopLevelFieldPreserved: extension members ride through.
func TestRecordedUnknownTopLevelFieldPreserved(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]},"note":"extra"}`),
	})
	res := serveInput(t, srv, req("9", "tools/list", `{}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	obj := wireObject(t, res.raws[0])
	if string(obj["note"]) != `"extra"` {
		t.Fatalf("unknown top-level field lost: %s", res.raws[0])
	}
}

// TestRecordedErrorPreservedWhole: the error object replays with data and
// unknown members intact, under the incoming id.
func TestRecordedErrorPreservedWhole(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"nope"}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom","data":{"field":"q"},"mystery":"kept"}}`),
	})
	res := serveInput(t, srv, req("8", "tools/call", `{"name":"nope"}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	obj := wireObject(t, res.raws[0])
	if string(obj["id"]) != "8" {
		t.Fatalf("id = %s, want 8", obj["id"])
	}
	var errObj map[string]json.RawMessage
	if err := json.Unmarshal(obj["error"], &errObj); err != nil {
		t.Fatalf("error is not an object: %s", obj["error"])
	}
	if string(errObj["code"]) != "-32000" || string(errObj["message"]) != `"boom"` {
		t.Fatalf("code/message changed: %s", obj["error"])
	}
	if canonical(t, errObj["data"]) != `{"field":"q"}` {
		t.Fatalf("data changed: %s", errObj["data"])
	}
	if string(errObj["mystery"]) != `"kept"` {
		t.Fatalf("unknown error member lost: %s", obj["error"])
	}
}

// TestRecordedBothResultAndErrorPreserved: a response carrying both is not
// collapsed to one half.
func TestRecordedBothResultAndErrorPreserved(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"odd"}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"a":1},"error":{"code":-1,"message":"both"}}`),
	})
	res := serveInput(t, srv, req("8", "tools/call", `{"name":"odd"}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(res.responses))
	}
	obj := wireObject(t, res.raws[0])
	if canonical(t, obj["result"]) != `{"a":1}` {
		t.Fatalf("result changed: %s", res.raws[0])
	}
	if canonical(t, obj["error"]) != `{"code":-1,"message":"both"}` {
		t.Fatalf("error changed: %s", res.raws[0])
	}
}

// TestFIFOOutOfOrderResponsesDocumented: two identical requests whose
// responses arrive out of order replay in response-arrival order. The queue
// behind one match key follows the order the answers were recorded on the
// wire, not the order the requests were sent; neither the issue nor a
// maintainer has defined "recorded order" the other way, so this test pins
// the current rule rather than changing it.
func TestFIFOOutOfOrderResponsesDocumented(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"counter"}`)),
		c2s(req("2", "tools/call", `{"name":"counter"}`)),
		s2c(`{"jsonrpc":"2.0","id":2,"result":{"n":2}}`),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"n":1}}`),
	})
	res := serveInput(t, srv,
		req("10", "tools/call", `{"name":"counter"}`)+"\n"+
			req("11", "tools/call", `{"name":"counter"}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(res.responses))
	}
	if string(res.responses[0].Result) != `{"n":2}` || string(res.responses[1].Result) != `{"n":1}` {
		t.Fatalf("expected response-arrival order n=2,n=1, got %s %s",
			res.responses[0].Result, res.responses[1].Result)
	}
}

func TestMRTRChainReplays(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		metaFrame(),
		c2s(req("3", "tools/call", `{"name":"lookup","arguments":{"id":7}}`)),
		s2c(`{"jsonrpc":"2.0","id":3,"result":{"resultType":"input_required","requestState":"s-1","inputRequests":{"confirm":{"method":"elicitation/create","params":{}}}}}`),
		c2s(req("4", "tools/call", `{"name":"lookup","arguments":{"id":7},"requestState":"s-1","inputResponses":{"confirm":{"action":"accept"}}}`)),
		s2c(`{"jsonrpc":"2.0","id":4,"result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}}`),
	})
	if srv.Exchanges() != 2 {
		t.Fatalf("exchanges = %d, want 2 distinct hops", srv.Exchanges())
	}
	res := serveInput(t, srv,
		req("101", "tools/call", `{"name":"lookup","arguments":{"id":7}}`)+"\n"+
			req("102", "tools/call", `{"name":"lookup","arguments":{"id":7},"requestState":"s-1","inputResponses":{"confirm":{"action":"accept"}}}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 2 {
		t.Fatalf("responses = %d, want 2, stderr %q", len(res.responses), res.stderr)
	}
	first, second := res.responses[0], res.responses[1]
	if first.Error != nil || !strings.Contains(string(first.Result), "input_required") {
		t.Fatalf("first hop must replay the intermediate input_required: %+v %s", first.Error, first.Result)
	}
	if string(first.ID) != "101" {
		t.Fatalf("first id = %s, want 101", first.ID)
	}
	if second.Error != nil || !strings.Contains(string(second.Result), "done") {
		t.Fatalf("continuation must replay the final result: %+v %s", second.Error, second.Result)
	}
	if string(second.ID) != "102" {
		t.Fatalf("second id = %s, want 102", second.ID)
	}
}

// TestAmbiguousMRTRRetryIsNotGuessed covers the store refusing a link: two
// parked operations fit one retry, so matchRetry gives up and the retry stays
// its own call. The mock must replay each recorded hop as its own exchange and
// never fuse the retry into another interaction.
func TestAmbiguousMRTRRetryIsNotGuessed(t *testing.T) {
	parked := `{"resultType":"input_required","inputRequests":{"a":{"method":"elicitation/create","params":{}}}}`
	srv := loadFrames(t, []testFrame{
		c2s(req("10", "tools/call", `{"name":"echo","arguments":{"t":1}}`)),
		s2c(`{"jsonrpc":"2.0","id":10,"result":` + parked + `}`),
		c2s(req("11", "tools/call", `{"name":"echo","arguments":{"t":1}}`)),
		s2c(`{"jsonrpc":"2.0","id":11,"result":` + parked + `}`),
		c2s(req("12", "tools/call", `{"name":"echo","arguments":{"t":1},"inputResponses":{"a":{"action":"accept"}}}`)),
		s2c(`{"jsonrpc":"2.0","id":12,"result":{"resultType":"complete","content":[{"type":"text","text":"final"}]}}`),
	})
	if srv.Exchanges() != 3 {
		t.Fatalf("exchanges = %d, want 3", srv.Exchanges())
	}
	res := serveInput(t, srv,
		req("20", "tools/call", `{"name":"echo","arguments":{"t":1}}`)+"\n"+
			req("21", "tools/call", `{"name":"echo","arguments":{"t":1}}`)+"\n"+
			req("22", "tools/call", `{"name":"echo","arguments":{"t":1}}`)+"\n"+
			req("23", "tools/call", `{"name":"echo","arguments":{"t":1},"inputResponses":{"a":{"action":"accept"}}}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 4 {
		t.Fatalf("responses = %d, want 4", len(res.responses))
	}
	for i, want := range []string{"input_required", "input_required"} {
		if res.responses[i].Error != nil || !strings.Contains(string(res.responses[i].Result), want) {
			t.Fatalf("response %d = %+v %s, want %s", i, res.responses[i].Error, res.responses[i].Result, want)
		}
	}
	// The parked queue is spent: a third plain call errors rather than being
	// guessed into the retry's exchange.
	if res.responses[2].Error == nil {
		t.Fatalf("third plain call must error, got %s", res.responses[2].Result)
	}
	if last := res.responses[3]; last.Error != nil || !strings.Contains(string(last.Result), "final") {
		t.Fatalf("the retry replays its own recorded answer: %+v %s", last.Error, last.Result)
	}
}

func TestRedactedCaptureRefusedByDefault(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"echo"}`)),
		{dir: proxy.ServerToClient, rpc: `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[REDACTED]"}]}}`, redacted: true},
	})
	if !srv.Redacted() {
		t.Fatal("Redacted() = false, want true")
	}
	res := serveInput(t, srv, req("2", "tools/call", `{"name":"echo"}`)+"\n", defaultPolicy())
	if res.code != 1 {
		t.Fatalf("exit = %d, want 1 (refusal)", res.code)
	}
	if len(res.raws) != 0 {
		t.Fatalf("refusal must write nothing to stdout, got %v", res.raws)
	}
	if !strings.Contains(strings.ToLower(res.stderr), "redacted") {
		t.Fatalf("refusal must say redacted on stderr: %q", res.stderr)
	}
}

func TestRedactedCaptureServedWithExplicitOverride(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"echo"}`)),
		{dir: proxy.ServerToClient, rpc: `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[REDACTED]"}]}}`, redacted: true},
	})
	res := serveInput(t, srv, req("2", "tools/call", `{"name":"echo"}`)+"\n", Policy{AllowRedacted: true})
	if res.code != 0 {
		t.Fatalf("exit = %d, want 0, stderr %q", res.code, res.stderr)
	}
	if len(res.responses) != 1 || res.responses[0].Error != nil {
		t.Fatalf("override must serve the recorded exchange: %+v", res.responses)
	}
	if !strings.Contains(strings.ToLower(res.stderr), "redacted") {
		t.Fatalf("override must still announce it loudly: %q", res.stderr)
	}
}

// TestRedactedDetectedStructurally ensures the signal is the envelope mark, not
// the payload text: a client that literally sent "[REDACTED]" with no mark set
// is served normally.
func TestRedactedDetectedStructurally(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"echo"}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[REDACTED]"}]}}`),
	})
	if srv.Redacted() {
		t.Fatal("payload text alone must not count as redaction")
	}
	res := serveInput(t, srv, req("2", "tools/call", `{"name":"echo"}`)+"\n", defaultPolicy())
	if res.code != 0 || len(res.responses) != 1 || res.responses[0].Error != nil {
		t.Fatalf("unmarked capture must serve: %+v stderr %q", res.responses, res.stderr)
	}
}

func TestCaptureWithoutMetaFrame(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	res := serveInput(t, srv, req("2", "tools/list", `{}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error != nil {
		t.Fatalf("capture without a meta frame must still mock: %+v", res.responses)
	}
}

func TestModernDiscoverReplays(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "server/discover", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`),
	})
	res := serveInput(t, srv, req("7", "server/discover", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"new"},"io.modelcontextprotocol/clientCapabilities":{}}}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error != nil {
		t.Fatalf("recorded discover must replay: %+v stderr %q", res.responses, res.stderr)
	}
	if !strings.Contains(string(res.responses[0].Result), "2026-07-28") {
		t.Fatalf("result = %s", res.responses[0].Result)
	}
}

// TestLegacyCaptureDiscoverRule: a capture with no server/discover answers one
// with an error rather than a synthesised DiscoverResult, so a probing client
// falls back to the initialize handshake, which replays.
func TestLegacyCaptureDiscoverRule(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c"}}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"corpus"}}}`),
	})
	res := serveInput(t, srv,
		req("1", "server/discover", `{}`)+"\n"+
			req("2", "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"other"}}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(res.responses))
	}
	discover := res.responses[0]
	if discover.Error == nil {
		t.Fatalf("unrecorded discover must error, never synthesise: %s", discover.Result)
	}
	if discover.Error.Code != -32601 || !strings.Contains(discover.Error.Message, "server/discover") {
		t.Fatalf("discover error must name the method: %+v", discover.Error)
	}
	init := res.responses[1]
	if init.Error != nil || !strings.Contains(string(init.Result), "corpus") {
		t.Fatalf("initialize must replay after the discover miss: %+v %s", init.Error, init.Result)
	}
}

// TestInitializeCapabilitiesStrict: only clientInfo is identity on the legacy
// handshake. Different capabilities are a semantic difference and must not
// match, even with the client name changed too.
func TestInitializeCapabilitiesStrict(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c"}}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"corpus"}}}`),
	})
	res := serveInput(t, srv,
		req("2", "initialize", `{"protocolVersion":"2025-06-18","capabilities":{"sampling":{}},"clientInfo":{"name":"other"}}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error == nil {
		t.Fatalf("different capabilities must not match, got %+v", res.responses)
	}
	if !strings.Contains(res.responses[0].Error.Message, "initialize") {
		t.Fatalf("error must name the method: %+v", res.responses[0].Error)
	}
}

// TestSupersededIDReuse: the second request reuses id 1 while the first is
// still in flight, so the store marks the first Superseded and the later
// request owns the eventual response. The mock must index that response to
// the tools/list the store considers current, never to the orphaned call.
func TestSupersededIDReuse(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`)),
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	if srv.Exchanges() != 1 {
		t.Fatalf("exchanges = %d, want 1 (the superseded request owns nothing)", srv.Exchanges())
	}
	res := serveInput(t, srv,
		req("10", "tools/list", `{}`)+"\n"+
			req("11", "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(res.responses))
	}
	if res.responses[0].Error != nil {
		t.Fatalf("the current request must replay: %+v", res.responses[0].Error)
	}
	if second := res.responses[1]; second.Error == nil {
		t.Fatalf("the superseded request must not consume the response, got %s", second.Result)
	} else if !strings.Contains(second.Error.Message, "tools/call") {
		t.Fatalf("error must name the method: %+v", second.Error)
	}
}

// TestParamsAbsentNullEmptyDistinct: omitted params, explicit null and {}
// are three different calls. Volatile stripping may leave an explicit {},
// but that never becomes absent params.
// TestParamsAbsentNullEmptyDistinct keeps the three empty-ish param shapes as
// three match keys. Each one is recorded with its own answer and then asked for
// in the reverse order, because a capture holding a single exchange cannot tell
// a key that did not match from a key that matched an exhausted queue, and a
// test built that way passes even when all three collapse into one.
func TestParamsAbsentNullEmptyDistinct(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"shape":"empty-object"}}`),
		c2s(req("2", "tools/list", `null`)),
		s2c(`{"jsonrpc":"2.0","id":2,"result":{"shape":"explicit-null"}}`),
		c2s(req("3", "tools/list", "")),
		s2c(`{"jsonrpc":"2.0","id":3,"result":{"shape":"omitted"}}`),
	})
	res := serveInput(t, srv,
		req("10", "tools/list", "")+"\n"+
			req("11", "tools/list", `null`)+"\n"+
			req("12", "tools/list", `{}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 3 {
		t.Fatalf("responses = %d, want 3, stderr = %q", len(res.responses), res.stderr)
	}
	for i, want := range []string{"omitted", "explicit-null", "empty-object"} {
		if res.responses[i].Error != nil {
			t.Fatalf("response %d should have matched its own shape: %+v", i, res.responses[i].Error)
		}
		if !strings.Contains(string(res.responses[i].Result), want) {
			t.Fatalf("response %d = %s, want the %s answer", i, res.responses[i].Result, want)
		}
	}
}

// TestBigIntegersExact keeps neighbouring integers past float64 precision as
// two match keys. Both are recorded with their own answer and the later one is
// asked for first, since a single recorded exchange would let this pass with the
// two collapsed, and asking in recorded order would let the queue hand back the
// right answers by position rather than by key.
func TestBigIntegersExact(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"get","arguments":{"id":9007199254740992}}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"v":"even"}}`),
		c2s(req("2", "tools/call", `{"name":"get","arguments":{"id":9007199254740993}}`)),
		s2c(`{"jsonrpc":"2.0","id":2,"result":{"v":"odd"}}`),
	})
	res := serveInput(t, srv,
		req("10", "tools/call", `{"name":"get","arguments":{"id":9007199254740993}}`)+"\n"+
			req("11", "tools/call", `{"name":"get","arguments":{"id":9007199254740992}}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 2 {
		t.Fatalf("responses = %d, want 2, stderr = %q", len(res.responses), res.stderr)
	}
	for i, want := range []string{"odd", "even"} {
		if res.responses[i].Error != nil {
			t.Fatalf("response %d should have matched its own integer: %+v", i, res.responses[i].Error)
		}
		if !strings.Contains(string(res.responses[i].Result), want) {
			t.Fatalf("response %d = %s, want the %s answer, so the two integers are one key", i, res.responses[i].Result, want)
		}
	}
	// And the unrecorded neighbour still misses rather than borrowing either.
	res = serveInput(t, srv, req("12", "tools/call", `{"name":"get","arguments":{"id":9007199254740994}}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error == nil {
		t.Fatalf("an unrecorded integer must miss, got %+v", res.responses)
	}
}

// TestTruncatedCaptureFails: a torn final envelope is a corrupt cassette, not
// a clean end.
func TestTruncatedCaptureFails(t *testing.T) {
	envs := frameEnvelopes(t, "test-session", proxy.TransportStdio, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	raw := buf.String()
	cut := raw[:len(raw)-10] // inside the final envelope
	if _, err := Load(strings.NewReader(cut), "cut"); err == nil {
		t.Fatal("truncated capture must fail loading")
	} else if !strings.Contains(err.Error(), "invalid JSONL envelope") {
		t.Fatalf("error must name the cause: %v", err)
	}
}

// TestRedactionScopedToFirstSession: a redacted second session in a
// concatenated file must not make a clean first session unservable.
func TestRedactionScopedToFirstSession(t *testing.T) {
	first := frameEnvelopes(t, "first", proxy.TransportStdio, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	second := frameEnvelopes(t, "second", proxy.TransportStdio, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		{dir: proxy.ServerToClient, rpc: `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`, redacted: true},
	})
	srv := loadEnvelopes(t, append(first, second...))
	if srv.Redacted() {
		t.Fatal("a redacted later session must not taint the served first session")
	}
	if srv.SessionID() != "first" {
		t.Fatalf("session = %q, want first", srv.SessionID())
	}
	res := serveInput(t, srv, req("9", "tools/list", `{}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error != nil {
		t.Fatalf("first session must serve: %+v stderr %q", res.responses, res.stderr)
	}
}

// TestHTTPTransportRejected: an explicitly HTTP-captured session carries
// ConnID and transport semantics the stdio mock does not model.
func TestHTTPTransportRejected(t *testing.T) {
	envs := frameEnvelopes(t, "http-session", proxy.TransportHTTP, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	_, err := Load(&buf, "http")
	if err == nil {
		t.Fatal("http capture must be refused")
	}
	if !strings.Contains(err.Error(), "stdio") {
		t.Fatalf("refusal must say stdio-only: %v", err)
	}
}

// TestLegacyNoTransportAccepted: a log whose frames name no transport is the
// legacy shape and still mocks.
func TestLegacyNoTransportAccepted(t *testing.T) {
	srv := loadEnvelopes(t, frameEnvelopes(t, "legacy", "", []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	}))
	res := serveInput(t, srv, req("9", "tools/list", `{}`)+"\n", defaultPolicy())
	if len(res.responses) != 1 || res.responses[0].Error != nil {
		t.Fatalf("unmarked capture must serve: %+v stderr %q", res.responses, res.stderr)
	}
}

func TestTaskRequestsReplayAsRecorded(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/call", `{"name":"long"}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"task","taskId":"t-1","status":"working"}}`),
		c2s(req("2", "tasks/get", `{"taskId":"t-1"}`)),
		s2c(`{"jsonrpc":"2.0","id":2,"result":{"resultType":"task","taskId":"t-1","status":"completed","result":{"content":[{"type":"text","text":"done"}]}}}`),
	})
	res := serveInput(t, srv,
		req("10", "tools/call", `{"name":"long"}`)+"\n"+
			req("11", "tasks/get", `{"taskId":"t-1"}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 2 {
		t.Fatalf("responses = %d, want 2, stderr %q", len(res.responses), res.stderr)
	}
	if res.responses[0].Error != nil || !strings.Contains(string(res.responses[0].Result), "t-1") {
		t.Fatalf("task handle must replay: %+v %s", res.responses[0].Error, res.responses[0].Result)
	}
	if res.responses[1].Error != nil || !strings.Contains(string(res.responses[1].Result), "completed") {
		t.Fatalf("task poll must replay: %+v %s", res.responses[1].Error, res.responses[1].Result)
	}
}

func TestStdoutHoldsOnlyProtocolFrames(t *testing.T) {
	srv := loadFrames(t, []testFrame{
		c2s(req("1", "tools/list", `{}`)),
		s2c(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
	})
	res := serveInput(t, srv,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+
			"\n"+
			req("1", "tools/list", `{}`)+"\n"+
			req("2", "nope/missing", `{}`)+"\n",
		defaultPolicy())
	if len(res.responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(res.responses))
	}
	for _, line := range res.raws {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("stdout is not JSON: %q", line)
		}
		if v["jsonrpc"] != "2.0" {
			t.Fatalf("stdout frame is not JSON-RPC 2.0: %q", line)
		}
		if _, ok := v["method"]; ok {
			t.Fatalf("stdout must carry responses, not requests: %q", line)
		}
		if _, hasResult := v["result"]; !hasResult {
			if _, hasError := v["error"]; !hasError {
				t.Fatalf("stdout frame is neither result nor error: %q", line)
			}
		}
	}
}

func TestLoadRejectsBadCaptures(t *testing.T) {
	if _, err := Load(strings.NewReader(""), "empty"); err == nil {
		t.Fatal("empty input must error")
	}
	if _, err := Load(strings.NewReader("not json\n"), "bad"); err == nil {
		t.Fatal("malformed JSONL must error")
	}
	if _, err := LoadFile(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("missing file must error")
	}
}

// collectSink gathers proxied envelopes for the round-trip test.
type collectSink struct {
	mu   sync.Mutex
	envs []proxy.Envelope
}

func (c *collectSink) Emit(e proxy.Envelope) {
	c.mu.Lock()
	c.envs = append(c.envs, e)
	c.mu.Unlock()
}

func (c *collectSink) Close() error { return nil }

func (c *collectSink) jsonl() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var buf bytes.Buffer
	for _, e := range c.envs {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// deterministicServer is a tiny modern stdio MCP server: server/discover,
// tools/list and a tools/call echo with no timing or randomness, so two runs
// of one script produce comparable captures.
const deterministicServer = `package main

import ("bufio";"encoding/json";"os")

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
		Params json.RawMessage ` + "`json:\"params\"`" + `
	}
	if json.Unmarshal(line, &req) != nil || req.Method == "" || len(req.ID) == 0 {
		return
	}
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "server/discover":
		out["result"] = map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": "test server."}
	case "tools/list":
		out["result"] = map[string]any{"resultType": "complete", "tools": []map[string]any{{"name": "echo", "description": "echoes", "inputSchema": map[string]any{"type": "object"}}}}
	case "tools/call":
		var p struct {
			Name      string            ` + "`json:\"name\"`" + `
			Arguments map[string]string ` + "`json:\"arguments\"`" + `
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Name != "echo" {
			out["error"] = map[string]any{"code": -32601, "message": "unknown tool"}
			break
		}
		out["result"] = map[string]any{"resultType": "complete", "content": []map[string]string{{"type": "text", "text": "echo:" + p.Arguments["text"]}}}
	default:
		out["error"] = map[string]any{"code": -32601, "message": "method not found: " + req.Method}
	}
	b, _ := json.Marshal(out)
	os.Stdout.Write(append(b, '\n'))
}
`

var deterministicBin string

func TestMain(m *testing.M) {
	if os.Getenv("MCPSNOOP_MOCK_HELPER") == "1" {
		srv, err := LoadFile(os.Getenv("MCPSNOOP_MOCK_CAPTURE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "mock helper:", err)
			os.Exit(1)
		}
		os.Exit(srv.Serve(os.Stdin, os.Stdout, os.Stderr, Policy{}))
	}
	dir, err := os.MkdirTemp("", "mcpsnoop-mock")
	if err != nil {
		panic(err)
	}
	deterministicBin = buildDeterministicFixture(dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func buildDeterministicFixture(root string) string {
	dir := filepath.Join(root, "rtsrv")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(deterministicServer), 0o600); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module rtsrv\n\ngo 1.21\n"), 0o600); err != nil {
		panic(err)
	}
	bin := filepath.Join(dir, "rtsrv")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		panic("build rtsrv: " + err.Error() + "\n" + string(out))
	}
	return bin
}

// roundTripScript is the deterministic client session both runs execute.
func roundTripScript() string {
	return req("1", "server/discover", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`) + "\n" +
		req("2", "tools/list", `{}`) + "\n" +
		req("3", "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`) + "\n" +
		req("4", "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`) + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
}

// TestRoundTrip drives a deterministic server under the shim, serves that
// capture with the mock, drives the mock through the shim again, and verifies
// the resulting capture against the original with the diff machinery.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	script := roundTripScript()

	origSink := &collectSink{}
	var origOut bytes.Buffer
	if code, err := proxy.RunStdio(ctx, proxy.StdioConfig{
		Command: []string{deterministicBin}, Label: "rt", SessionID: "rt-orig",
		Sink: origSink, In: strings.NewReader(script), Out: &origOut, Err: &bytes.Buffer{},
	}); err != nil || code != 0 {
		t.Fatalf("original run: code=%d err=%v", code, err)
	}
	origJSONL := origSink.jsonl()
	if len(origJSONL) == 0 {
		t.Fatal("original run captured nothing")
	}
	capturePath := filepath.Join(t.TempDir(), "orig.jsonl")
	if err := os.WriteFile(capturePath, origJSONL, 0o600); err != nil {
		t.Fatal(err)
	}

	// The mock answers the same script directly.
	srv, err := LoadFile(capturePath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	direct := serveInput(t, srv, script, defaultPolicy())
	var origResponses []string
	for _, line := range strings.Split(strings.TrimSpace(origOut.String()), "\n") {
		origResponses = append(origResponses, strings.TrimSpace(line))
	}
	if len(direct.raws) != len(origResponses) {
		t.Fatalf("mock answered %d, server answered %d", len(direct.raws), len(origResponses))
	}
	for i := range origResponses {
		var want, got proxy.RPCMessage
		_ = json.Unmarshal([]byte(origResponses[i]), &want)
		_ = json.Unmarshal([]byte(direct.raws[i]), &got)
		if string(want.ID) != string(got.ID) || canonical(t, want.Result) != canonical(t, got.Result) {
			t.Fatalf("response %d differs:\n server %s\n mock   %s", i, origResponses[i], direct.raws[i])
		}
	}

	// The mock driven through the shim: the test binary re-executes as the
	// server, so the replayed capture comes off the real proxy path.
	t.Setenv("MCPSNOOP_MOCK_HELPER", "1")
	t.Setenv("MCPSNOOP_MOCK_CAPTURE", capturePath)
	replaySink := &collectSink{}
	var replayOut bytes.Buffer
	helper := []string{os.Args[0], "-test.run=TestMockHelperProcess"}
	if code, err := proxy.RunStdio(ctx, proxy.StdioConfig{
		Command: helper, Label: "rt", SessionID: "rt-replay",
		Sink: replaySink, In: strings.NewReader(script), Out: &replayOut, Err: &bytes.Buffer{},
	}); err != nil || code != 0 {
		t.Fatalf("replay run: code=%d err=%v", code, err)
	}

	before, err := buildExport(origJSONL, "orig")
	if err != nil {
		t.Fatalf("original export: %v", err)
	}
	after, err := buildExport(replaySink.jsonl(), "replay")
	if err != nil {
		t.Fatalf("replay export: %v", err)
	}
	report := sessiondiff.Compare(before, after, sessiondiff.Options{
		DurationThreshold: sessiondiff.DefaultDurationThreshold,
		DurationRatio:     sessiondiff.DefaultDurationRatio,
	})
	// Everything the protocol carried has to match. Durations cannot, and
	// asserting report.Empty() failed whenever the recorded server was slow
	// enough to clear the diff's threshold, which is the ordinary case and the
	// entire point of a mock. Assert the direction instead, since answering out
	// of a capture is never slower than the server that was captured.
	if report.Tools.Count() != 0 || len(report.CallChanges) != 0 {
		var buf bytes.Buffer
		_ = sessiondiff.WriteText(&buf, report)
		t.Fatalf("replayed capture differs in what the protocol carried:\n%s", buf.String())
	}
	for _, d := range report.DurationChanges {
		if d.After >= d.Before {
			t.Errorf("serving %s from the capture was not faster than the recorded server, %s -> %s",
				d.ToolName, d.Before, d.After)
		}
	}
}

// TestMockHelperProcess is the re-exec entry the round-trip test drives as its
// mock server. It never runs as a test: TestMain serves and exits first.
func TestMockHelperProcess(t *testing.T) {
	if os.Getenv("MCPSNOOP_MOCK_HELPER") != "1" {
		t.Skip("only the round-trip helper process runs this")
	}
	t.Fatal("TestMain should have served and exited")
}

func buildExport(raw []byte, source string) (exporter.SessionExport, error) {
	st, sessionID, err := exporter.Load(bytes.NewReader(raw), source)
	if err != nil {
		return exporter.SessionExport{}, err
	}
	return exporter.Build(st, sessionID)
}
