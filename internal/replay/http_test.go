package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// mcpEndpoint is a server that enforces what the transport makes mandatory, so a
// replay missing any of it gets the error a real server would send rather than a
// result.
func mcpEndpoint(t *testing.T, answer func(w http.ResponseWriter, seen http.Header, msg map[string]json.RawMessage)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		var method string
		_ = json.Unmarshal(msg["method"], &method)
		reject := func(message string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32020, "message": message}})
		}
		switch {
		case r.Header.Get("MCP-Protocol-Version") == "":
			reject("missing MCP-Protocol-Version")
		case r.Header.Get("Mcp-Method") != method:
			reject("Mcp-Method does not match the body")
		case !strings.Contains(r.Header.Get("Accept"), "application/json") ||
			!strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
			reject("Accept must list both types")
		default:
			answer(w, r.Header, msg)
		}
	}))
}

func okJSON(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
}

// TestReplayHTTPSendsWhatTheTransportRequires is the whole point: a POST of the
// bare captured body gets a -32020, not a result, so the replay carries the
// request metadata the specification makes mandatory.
func TestReplayHTTPSendsWhatTheTransportRequires(t *testing.T) {
	var seen http.Header
	srv := mcpEndpoint(t, func(w http.ResponseWriter, h http.Header, _ map[string]json.RawMessage) {
		seen = h.Clone()
		okJSON(w, "ok")
	})
	defer srv.Close()

	res, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
		json.RawMessage(`{"name":"echo","arguments":{"text":"hi"}}`),
		Routing{Name: "echo", ParamHeaders: []proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "us-west1"}}},
		5*time.Second)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("the server answered an error: %+v", res.Err)
	}
	for name, want := range map[string]string{
		"Mcp-Method":           "tools/call",
		"Mcp-Name":             "echo",
		"Mcp-Param-Region":     "us-west1",
		"Mcp-Protocol-Version": statelessProtocolVersion,
		"Content-Type":         "application/json",
	} {
		if got := seen.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if accept := seen.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		t.Fatalf("Accept = %q, want both types", accept)
	}
}

// TestReplayHTTPDeclaresTheRevisionItActuallySends covers the one header that
// cannot be copied from the capture. withClientMeta rewrites the body's
// protocolVersion, and the header "MUST match" it, so echoing what the capture
// declared would disagree with the body by construction.
func TestReplayHTTPDeclaresTheRevisionItActuallySends(t *testing.T) {
	var header, body string
	srv := mcpEndpoint(t, func(w http.ResponseWriter, h http.Header, msg map[string]json.RawMessage) {
		header = h.Get("MCP-Protocol-Version")
		var params struct {
			Meta struct {
				Version string `json:"io.modelcontextprotocol/protocolVersion"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(msg["params"], &params)
		body = params.Meta.Version
		okJSON(w, "ok")
	})
	defer srv.Close()

	// The captured body declares an older revision, which withClientMeta replaces.
	if _, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
		json.RawMessage(`{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}`),
		Routing{Name: "echo"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if header != body {
		t.Fatalf("header says %q and the body says %q; a server rejects that with -32020", header, body)
	}
	if header != statelessProtocolVersion {
		t.Fatalf("declared %q, want the revision the replay actually speaks", header)
	}
}

// TestReplayHTTPReadsAStreamedAnswer covers the other shape a server may choose,
// where request-scoped notifications precede the response.
func TestReplayHTTPReadsAStreamedAnswer(t *testing.T) {
	srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, _ map[string]json.RawMessage) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ":\r\n\r\n") // a keep-alive comment, which clients must ignore
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"streamed\"}]}}\n\n")
	})
	defer srv.Close()

	res, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
		json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatalf("error: %+v", res.Err)
	}
	if !strings.Contains(string(res.RPCResult), "streamed") {
		t.Fatalf("the answer was not read past the notifications: %s", res.RPCResult)
	}
}

// TestReplayHTTPNamesWhatWentWrong covers the failures a replay is most likely
// to cause, so a reader is told what to do rather than handed a status code.
func TestReplayHTTPNamesWhatWentWrong(t *testing.T) {
	t.Run("a credential is missing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://h/.well-known/x"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		_, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
			json.RawMessage(`{"name":"echo"}`), Routing{}, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "--replay-header") {
			t.Fatalf("err = %v, want it to name the way to send one", err)
		}
		if !strings.Contains(err.Error(), "Bearer") {
			t.Fatalf("err = %v, want the scheme the server demanded", err)
		}
	})

	t.Run("the address is not an MCP endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()
		_, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
			json.RawMessage(`{"name":"echo"}`), Routing{}, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "not a Streamable HTTP MCP endpoint") {
			t.Fatalf("err = %v, want it to say the address is wrong", err)
		}
	})

	t.Run("the server rejected the metadata", func(t *testing.T) {
		srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, _ map[string]json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32020, "message": "Mcp-Name does not match"}})
		})
		defer srv.Close()
		_, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
			json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "rejected the request metadata") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("no target", func(t *testing.T) {
		_, err := ReplayHTTP(context.Background(), HTTPTarget{}, "tools/call", nil, Routing{}, time.Second)
		if err == nil || !strings.Contains(err.Error(), "--replay-target") {
			t.Fatalf("err = %v, want it to name the flag", err)
		}
	})
}

// TestReplayHTTPRefusesToSendAPlaceholder keeps mcpsnoop's own scrubbing off a
// live server. A redacted header value is not the client's data, and sending it
// would put that placeholder on the server as though a user had typed it.
func TestReplayHTTPRefusesToSendAPlaceholder(t *testing.T) {
	srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, _ map[string]json.RawMessage) {
		t.Error("the request should never have been sent")
		okJSON(w, "ok")
	})
	defer srv.Close()

	_, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
		json.RawMessage(`{"name":"echo"}`),
		Routing{Name: "echo", ParamHeaders: []proxy.MCPParamHeader{{Name: "Mcp-Param-Token", Value: "[REDACTED]", Redacted: true}}},
		5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "scrubbed by redaction") {
		t.Fatalf("err = %v, want a refusal naming the reason", err)
	}
}

// TestReplayHTTPCarriesACallerHeader is how a credential reaches the server,
// since mcpsnoop records none and replays none.
func TestReplayHTTPCarriesACallerHeader(t *testing.T) {
	var auth string
	srv := mcpEndpoint(t, func(w http.ResponseWriter, h http.Header, _ map[string]json.RawMessage) {
		auth = h.Get("Authorization")
		okJSON(w, "ok")
	})
	defer srv.Close()

	if _, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL, Headers: []string{"Authorization: Bearer sk-test"}},
		"tools/call", json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", auth)
	}
}

// TestReplayHTTPSendsMcpNameOnlyWhereItIsRequired keeps a header off a request
// the transport does not ask it for.
func TestReplayHTTPSendsMcpNameOnlyWhereItIsRequired(t *testing.T) {
	var seen http.Header
	srv := mcpEndpoint(t, func(w http.ResponseWriter, h http.Header, _ map[string]json.RawMessage) {
		seen = h.Clone()
		okJSON(w, "ok")
	})
	defer srv.Close()

	if _, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/list",
		json.RawMessage(`{}`), Routing{Name: "leftover"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("Mcp-Name"); got != "" {
		t.Fatalf("Mcp-Name = %q on tools/list, which the transport does not require it for", got)
	}
	if got := seen.Get("Mcp-Method"); got != "tools/list" {
		t.Fatalf("Mcp-Method = %q", got)
	}
}

// TestReplayHTTPDeclaresEveryRequiredMetaField covers the body, which the header
// tests do not. From this revision clientCapabilities is required on every
// request and clientInfo is not, and a request missing a required field is
// malformed: the server "MUST reject it with JSON-RPC error code -32602" and
// answer 400. mcpsnoop's own checker reports a client that omits it, so leaving
// it out meant flagging its own replayed request.
func TestReplayHTTPDeclaresEveryRequiredMetaField(t *testing.T) {
	var meta map[string]json.RawMessage
	srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, msg map[string]json.RawMessage) {
		var params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		}
		_ = json.Unmarshal(msg["params"], &params)
		meta = params.Meta
		okJSON(w, "ok")
	})
	defer srv.Close()

	if _, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
		json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"io.modelcontextprotocol/protocolVersion",
		"io.modelcontextprotocol/clientCapabilities",
	} {
		if _, ok := meta[field]; !ok {
			t.Fatalf("_meta is missing %s, which the revision requires on every request: %v", field, meta)
		}
	}
	if got := string(meta["io.modelcontextprotocol/clientCapabilities"]); got != "{}" {
		t.Fatalf("clientCapabilities = %s, want an empty declaration; a one-shot replay can answer no input request", got)
	}
}

// TestReplayHTTPWillNotFollowARedirect is the safety property the whole design
// rests on. The address is the one the person replaying named and answered for,
// and following a redirect hands that choice back to the far end: a 307 resends
// the body, and on a hop that only changes the port Go forwards Authorization
// too, so a staging endpoint could deliver a mutating call and a credential to
// production while the overlay reported success.
func TestReplayHTTPWillNotFollowARedirect(t *testing.T) {
	var reached string
	var sawAuth string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.Host
		sawAuth = r.Header.Get("Authorization")
		okJSON(w, "reached the wrong host")
	}))
	defer second.Close()

	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			reached, sawAuth = "", ""
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", second.URL)
				w.WriteHeader(code)
			}))
			defer first.Close()

			_, err := ReplayHTTP(context.Background(),
				HTTPTarget{URL: first.URL, Headers: []string{"Authorization: Bearer sk-secret", "X-Api-Key: k-secret"}},
				"tools/call", json.RawMessage(`{"name":"echo","arguments":{"t":"private"}}`),
				Routing{Name: "echo"}, 5*time.Second)
			if err == nil {
				t.Fatal("a redirect was followed and reported as a success")
			}
			if reached != "" {
				t.Fatalf("the request reached %s, which nobody named; auth forwarded = %q", reached, sawAuth)
			}
			if !strings.Contains(err.Error(), "--replay-target") {
				t.Fatalf("err = %v, want it to name where it wanted to go and how to allow it", err)
			}
		})
	}
}

// TestReplayHTTPDerivesMcpNameFromTheBody covers the header the transport
// sources from "params.name or params.uri" and requires a server to reject when
// it disagrees with the body. Copying the captured one sent the old name against
// a body somebody had renamed, and sent nothing at all for a capture from a
// client that predates the header.
func TestReplayHTTPDerivesMcpNameFromTheBody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		params   string
		captured string
		want     string
	}{
		{"the body names the tool", "tools/call", `{"name":"renamed"}`, "captured", "renamed"},
		{"a capture that carried none", "tools/call", `{"name":"echo"}`, "", "echo"},
		{"a resource names its uri", "resources/read", `{"uri":"file:///a.txt"}`, "", "file:///a.txt"},
		{"a body naming nothing falls back", "tools/call", `{}`, "captured", "captured"},
		{"a name outside plain ascii is wrapped", "tools/call", `{"name":"Hello, 世界"}`, "", "=?base64?SGVsbG8sIOS4lueVjA==?="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			srv := mcpEndpoint(t, func(w http.ResponseWriter, h http.Header, _ map[string]json.RawMessage) {
				seen = h.Get("Mcp-Name")
				okJSON(w, "ok")
			})
			defer srv.Close()
			if _, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, tc.method,
				json.RawMessage(tc.params), Routing{Name: tc.captured}, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			if seen != tc.want {
				t.Fatalf("Mcp-Name = %q, want %q", seen, tc.want)
			}
		})
	}
}

// TestReplayHTTPOnlySetsTheParamFamilyFromACapture keeps a log from choosing the
// request's headers. A capture is a file people hand around, and setting
// whatever name it carries let a doctored one overwrite the mandatory headers or
// add a credential the operator never passed.
func TestReplayHTTPOnlySetsTheParamFamilyFromACapture(t *testing.T) {
	var seen http.Header
	srv := mcpEndpoint(t, func(w http.ResponseWriter, h http.Header, _ map[string]json.RawMessage) {
		seen = h.Clone()
		okJSON(w, "ok")
	})
	defer srv.Close()

	if _, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
		json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo", ParamHeaders: []proxy.MCPParamHeader{
			{Name: "Mcp-Param-Region", Value: "us-west1"},
			{Name: "Authorization", Value: "Bearer stolen"},
			{Name: "MCP-Protocol-Version", Value: "1999-01-01"},
			{Name: "Accept", Value: "text/plain"},
			{Name: "Mcp-Method", Value: "tools/list"},
		}}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("Mcp-Param-Region"); got != "us-west1" {
		t.Fatalf("the family that should be replayed was dropped: %q", got)
	}
	for name, notWant := range map[string]string{
		"Authorization":        "Bearer stolen",
		"MCP-Protocol-Version": "1999-01-01",
		"Accept":               "text/plain",
		"Mcp-Method":           "tools/list",
	} {
		if seen.Get(name) == notWant {
			t.Fatalf("a capture set %s to %q", name, notWant)
		}
	}
}

// TestReplayHTTPDropsMirroredHeadersOnAnEditedBody keeps a stale assertion off
// the request. An Mcp-Param-* mirrors a captured argument, so against a body
// somebody rewrote it claims something mcpsnoop cannot know is still true.
func TestReplayHTTPDropsMirroredHeadersOnAnEditedBody(t *testing.T) {
	var seen http.Header
	srv := mcpEndpoint(t, func(w http.ResponseWriter, h http.Header, _ map[string]json.RawMessage) {
		seen = h.Clone()
		okJSON(w, "ok")
	})
	defer srv.Close()

	if _, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
		json.RawMessage(`{"name":"echo","arguments":{"region":"eu-west1"}}`),
		Routing{Name: "echo", Edited: true,
			ParamHeaders: []proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "us-west1"}}},
		5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("Mcp-Param-Region"); got != "" {
		t.Fatalf("Mcp-Param-Region = %q, want none; the body was rewritten and the header mirrors the old one", got)
	}
	// The name is still derived, since it comes from the body being sent.
	if got := seen.Get("Mcp-Name"); got != "echo" {
		t.Fatalf("Mcp-Name = %q", got)
	}
}

// TestReplayHTTPNamesAnAnswerItCannotUse covers the shapes that used to be
// reported as a success or blamed on the wrong thing.
func TestReplayHTTPNamesAnAnswerItCannotUse(t *testing.T) {
	t.Run("valid json that is not a response", func(t *testing.T) {
		srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, _ map[string]json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","message":"proxy says hello"}`))
		})
		defer srv.Close()
		_, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
			json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "not a JSON-RPC response") {
			t.Fatalf("err = %v, want it to say the answer is not a response", err)
		}
	})

	t.Run("an empty body", func(t *testing.T) {
		srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, _ map[string]json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		})
		defer srv.Close()
		_, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
			json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "empty body") {
			t.Fatalf("err = %v, want it to name the empty body", err)
		}
	})

	t.Run("a stream whose media type is spelled differently", func(t *testing.T) {
		srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, _ map[string]json.RawMessage) {
			w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[]}}\n\n")
		})
		defer srv.Close()
		res, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
			json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 5*time.Second)
		if err != nil {
			t.Fatalf("a media type is case-insensitive: %v", err)
		}
		if res.Err != nil {
			t.Fatalf("error: %+v", res.Err)
		}
	})

	t.Run("an answer past the cap", func(t *testing.T) {
		srv := mcpEndpoint(t, func(w http.ResponseWriter, _ http.Header, _ map[string]json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"`))
			chunk := strings.Repeat("x", 64<<10)
			for range (maxReplayBody / len(chunk)) + 2 {
				_, _ = w.Write([]byte(chunk))
			}
			_, _ = w.Write([]byte(`"}]}}`))
		})
		defer srv.Close()
		_, err := ReplayHTTP(context.Background(), HTTPTarget{URL: srv.URL}, "tools/call",
			json.RawMessage(`{"name":"echo"}`), Routing{Name: "echo"}, 20*time.Second)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("err = %v, want the size named rather than the server blamed for a cut body", err)
		}
	})
}
