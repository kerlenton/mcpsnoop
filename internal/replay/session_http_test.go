package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// legacyServer is a 2025-11-25 Streamable HTTP server with sessions. It hands
// out a session id, a note id and a question id that differ on every run, asks
// its question inside the event stream of the call that needs it, keeps a GET
// stream on which it pings the client, and refuses any POST after the handshake
// that does not carry its session. It answers its first call only once its ping
// has been answered, which is the order the recording has, since a replay that
// had nothing left to wait for would otherwise be free to end first.
type legacyServer struct {
	mu       sync.Mutex
	run      string
	session  string
	notes    map[string]string
	waiting  map[string]chan string
	pongs    int
	ponged   chan struct{}
	deleted  bool
	refusals []string
}

func newLegacyServer() *legacyServer {
	return &legacyServer{run: fmt.Sprintf("%x", time.Now().UnixNano()), notes: map[string]string{},
		waiting: map[string]chan string{}, ponged: make(chan struct{})}
}

func (s *legacyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		s.mu.Lock()
		s.deleted = r.Header.Get("Mcp-Session-Id") == s.session
		s.mu.Unlock()
		return
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"ping-%s\",\"method\":\"ping\"}\n\n", s.run)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	body, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		} `json:"params"`
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &msg)
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	if msg.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != session {
		s.mu.Lock()
		s.refusals = append(s.refusals, string(body))
		s.mu.Unlock()
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	if msg.Method == "" {
		// The client answering one of the server's own requests.
		s.mu.Lock()
		if strings.HasPrefix(strings.Trim(string(msg.ID), `"`), "ping-") {
			s.pongs++
			if s.pongs == 1 {
				close(s.ponged)
			}
		}
		ch := s.waiting[string(msg.ID)]
		s.mu.Unlock()
		if ch != nil {
			ch <- msg.Result.Action
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
	text := func(t string) map[string]any {
		return map[string]any{"content": []map[string]string{{"type": "text", "text": t}}}
	}
	switch msg.Method {
	case "initialize":
		s.mu.Lock()
		s.session = "sess-" + s.run
		w.Header().Set("Mcp-Session-Id", s.session)
		s.mu.Unlock()
		reply(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "notes"}})
	case "tools/list":
		reply(map[string]any{"tools": []map[string]any{{"name": "create_note"}, {"name": "get_note"}, {"name": "ask"}}})
	case "tools/call":
		switch msg.Params.Name {
		case "create_note":
			select {
			case <-s.ponged:
			case <-time.After(2 * time.Second):
			}
			id := "n-" + s.run
			s.mu.Lock()
			s.notes[id] = msg.Params.Arguments["text"]
			s.mu.Unlock()
			res := text("Created note " + id + ".")
			res["structuredContent"] = map[string]string{"id": id}
			reply(res)
		case "get_note":
			s.mu.Lock()
			body, ok := s.notes[msg.Params.Arguments["id"]]
			s.mu.Unlock()
			if !ok {
				res := text("no note " + msg.Params.Arguments["id"])
				res["isError"] = true
				reply(res)
				return
			}
			reply(text(body))
		case "ask":
			question := "q-" + s.run
			raw, _ := json.Marshal(question)
			ch := make(chan string, 1)
			s.mu.Lock()
			s.waiting[string(raw)] = ch
			s.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"method\":\"elicitation/create\",\"params\":{\"message\":\"sure?\",\"requestedSchema\":{\"type\":\"object\",\"properties\":{}}}}\n\n", raw)
			w.(http.Flusher).Flush()
			action := "nobody answered"
			select {
			case action = <-ch:
			case <-time.After(3 * time.Second):
			}
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": text("the user said " + action)})
			fmt.Fprintf(w, "data: %s\n\n", out)
		}
	}
}

// httpRecording writes frames the way mcpsnoop http records them, one
// exchange per POST.
type httpRecording struct {
	t      *testing.T
	frames []string
}

func (r *httpRecording) add(dir proxy.Direction, exchange uint64, raw string, edit func(*proxy.Envelope)) *httpRecording {
	env := proxy.Envelope{
		SessionID: "rec", ServerLabel: "notes", Seq: uint64(len(r.frames) + 2),
		TS:        time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC).Add(time.Duration(len(r.frames)) * time.Second),
		Direction: dir, Transport: proxy.TransportHTTP, Exchange: exchange, Raw: json.RawMessage(raw),
	}
	if dir == proxy.ServerToClient {
		env.Status = 200
	}
	if edit != nil {
		edit(&env)
	}
	b, err := json.Marshal(env)
	if err != nil {
		r.t.Fatal(err)
	}
	r.frames = append(r.frames, string(b))
	return r
}

func (r *httpRecording) load() *Recording {
	r.t.Helper()
	rec, err := LoadRecording(strings.NewReader(strings.Join(r.frames, "\n")+"\n"), "")
	if err != nil {
		r.t.Fatal(err)
	}
	return rec
}

func replayHTTP(t *testing.T, rec *Recording, target string, opts SessionOptions) (*collected, SessionReport) {
	t.Helper()
	sink := &collected{}
	opts.Target, opts.Sink, opts.SessionID = target, sink, "replayed"
	if opts.Wait == 0 {
		opts.Wait = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := ReplaySession(ctx, rec, opts)
	if err != nil {
		t.Fatalf("ReplaySession: %v", err)
	}
	return sink, report
}

// TestReplayOverHTTPKeepsA2025Session. The server's session id, the note it
// created and the question it asked all have new values on the new run, and the
// question arrives inside the event stream of the call that needs it. The
// replay carries the session onto every POST, carries the note's id into the
// call that reads it back, answers the question under its new id with what the
// recorded user said, answers the pings on the GET stream it opens because the
// recording had one, and ends the session when it is done.
func TestReplayOverHTTPKeepsA2025Session(t *testing.T) {
	server := newLegacyServer()
	ts := httptest.NewServer(server)
	defer ts.Close()

	rec := (&httpRecording{t: t}).
		add(proxy.ClientToServer, 1, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{}},"clientInfo":{"name":"rec","version":"1"}}}`, nil).
		add(proxy.ServerToClient, 1, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"notes"}}}`, nil).
		add(proxy.ClientToServer, 2, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, func(e *proxy.Envelope) { e.MCPProtocolVersion = "2025-11-25" }).
		add(proxy.ServerToClient, 9, `{"jsonrpc":"2.0","id":"ping-old","method":"ping"}`, nil).
		add(proxy.ClientToServer, 10, `{"jsonrpc":"2.0","id":"ping-old","result":{}}`, func(e *proxy.Envelope) { e.MCPProtocolVersion = "2025-11-25" }).
		add(proxy.ClientToServer, 3, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_note","arguments":{"text":"hello"}}}`, func(e *proxy.Envelope) { e.MCPProtocolVersion = "2025-11-25" }).
		add(proxy.ServerToClient, 3, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Created note n-old."}],"structuredContent":{"id":"n-old"}}}`, nil).
		add(proxy.ClientToServer, 4, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_note","arguments":{"id":"n-old"}}}`, func(e *proxy.Envelope) { e.MCPProtocolVersion = "2025-11-25" }).
		add(proxy.ServerToClient, 4, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hello"}]}}`, nil).
		add(proxy.ClientToServer, 5, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{}}}`, func(e *proxy.Envelope) { e.MCPProtocolVersion = "2025-11-25" }).
		add(proxy.ServerToClient, 5, `{"jsonrpc":"2.0","id":"q-old","method":"elicitation/create","params":{"message":"sure?","requestedSchema":{"type":"object","properties":{}}}}`, nil).
		add(proxy.ClientToServer, 6, `{"jsonrpc":"2.0","id":"q-old","result":{"action":"accept","content":{}}}`, func(e *proxy.Envelope) { e.MCPProtocolVersion = "2025-11-25" }).
		add(proxy.ServerToClient, 5, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"the user said accept"}]}}`, nil).
		load()
	if !rec.listens {
		t.Fatal("a server frame on an exchange no client frame opened is a GET stream")
	}

	sink, report := replayHTTP(t, rec, ts.URL+"/mcp", SessionOptions{})
	if len(report.Missing) != 0 {
		t.Fatalf("missing %v, skipped %v", report.Missing, report.Skipped)
	}
	server.mu.Lock()
	refusals, pongs, deleted := server.refusals, server.pongs, server.deleted
	server.mu.Unlock()
	if len(refusals) != 0 {
		t.Fatalf("POSTs went without the session: %v", refusals)
	}
	if got := sink.answerTo(t, "tools/call", "get_note"); strings.Contains(got, "isError") || !strings.Contains(got, "hello") {
		t.Fatalf("get_note did not find the note the replay created: %s", got)
	}
	if got := sink.answerTo(t, "tools/call", "ask"); !strings.Contains(got, "the user said accept") {
		t.Fatalf("the question in the stream was not answered under its new id: %s", got)
	}
	if pongs != 1 {
		t.Fatalf("pongs = %d, want the replay to answer the ping on its GET stream", pongs)
	}
	if !deleted {
		t.Fatal("the replay should end the session it was given")
	}
	// The capture is the proxy's own, with the HTTP fields a live one has.
	sink.mu.Lock()
	defer sink.mu.Unlock()
	statuses := 0
	for _, e := range sink.envs {
		if e.Transport != proxy.TransportHTTP {
			t.Fatalf("an envelope that is not HTTP: %+v", e)
		}
		if e.Status != 0 {
			statuses++
		}
	}
	if statuses == 0 {
		t.Fatal("the replayed capture carries no HTTP statuses, so it was not written by the proxy")
	}
}

// strictServer is a 2026-07-28 server that rejects any request whose headers
// disagree with its body, the way the transport requires, and answers a
// confirmation through MRTR with a requestState that differs on every run. Its
// move tool mirrors its note argument in an Mcp-Param header, which its listing
// declares. It also refuses a credential, since none of these tests pass one,
// so one that arrives came out of a capture.
type strictServer struct {
	mu        sync.Mutex
	run       string
	bad       []string
	seen      []string
	cancelled int
}

func (s *strictServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name         string            `json:"name"`
			Arguments    map[string]string `json:"arguments"`
			RequestState string            `json:"requestState"`
			Meta         struct {
				Version string `json:"io.modelcontextprotocol/protocolVersion"`
			} `json:"_meta"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &msg)
	mismatch := func(why string) {
		s.mu.Lock()
		s.bad = append(s.bad, why)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32020, "message": why}})
	}
	switch {
	case r.Header.Get("Authorization") != "":
		mismatch("a credential nobody passed: " + r.Header.Get("Authorization"))
		return
	case r.Header.Get("MCP-Protocol-Version") != msg.Params.Meta.Version:
		mismatch("protocol version " + r.Header.Get("MCP-Protocol-Version"))
		return
	case r.Header.Get("Mcp-Method") != msg.Method:
		mismatch("method " + r.Header.Get("Mcp-Method"))
		return
	case msg.Method == "tools/call" && r.Header.Get("Mcp-Name") != msg.Params.Name:
		mismatch("name " + r.Header.Get("Mcp-Name"))
		return
	case msg.Params.Name == "move" && r.Header.Get("Mcp-Param-Note") != msg.Params.Arguments["note"]:
		mismatch("Mcp-Param-Note " + r.Header.Get("Mcp-Param-Note") + " for " + msg.Params.Arguments["note"])
		return
	}
	s.mu.Lock()
	s.seen = append(s.seen, msg.Method+" "+msg.Params.Name)
	s.mu.Unlock()
	reply := func(result map[string]any) {
		result["resultType"] = "complete"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
	switch {
	case msg.Method == "server/discover":
		reply(map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}})
	case msg.Method == "tools/list":
		reply(map[string]any{"tools": []any{
			map[string]any{"name": "create", "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "move", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"note": map[string]any{"type": "string", "x-mcp-header": "Note"},
			}}},
		}})
	case msg.Params.Name == "create":
		reply(map[string]any{"content": []any{}, "structuredContent": map[string]string{"id": "note-" + s.run}})
	case msg.Params.Name == "move" && msg.Params.Arguments["note"] == "note-"+s.run:
		reply(map[string]any{"content": []map[string]string{{"type": "text", "text": "moved"}}})
	case msg.Params.Name == "move":
		reply(map[string]any{"content": []map[string]string{{"type": "text", "text": "no such note"}}, "isError": true})
	case msg.Params.Name == "slow":
		// Answers after a while, unless the client closes the stream first, which
		// is the only cancellation this revision has over HTTP.
		select {
		case <-r.Context().Done():
			s.mu.Lock()
			s.cancelled++
			s.mu.Unlock()
		case <-time.After(5 * time.Second):
			reply(map[string]any{"content": []map[string]string{{"type": "text", "text": "finished"}}})
		}
	case msg.Params.Name == "confirm" && msg.Params.RequestState == "":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
			"resultType": "input_required", "requestState": "state-" + s.run,
			"inputRequests": map[string]any{"ok": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "sure?"}}},
		}})
	case msg.Params.Name == "confirm" && msg.Params.RequestState == "state-"+s.run:
		reply(map[string]any{"content": []map[string]string{{"type": "text", "text": "confirmed"}}})
	default:
		reply(map[string]any{"content": []map[string]string{{"type": "text", "text": "unexpected state " + msg.Params.RequestState}}, "isError": true})
	}
}

// TestReplayOverHTTPSendsHeadersThatMatchWhatIsSent. A 2026-07-28 server
// rejects a request whose headers disagree with its body, and the replay
// changes bodies when it carries a value. So the routing headers follow the
// body actually sent: the note id a create handed out moves into the move
// call's argument and into the Mcp-Param header that mirrors it, and the
// requestState of the confirmation moves into its retry. The recording never
// lists the tools, so the recorded Mcp-Param headers are all there is to go
// on, and a header outside that family, which only a doctored capture holds,
// is not sent.
func TestReplayOverHTTPSendsHeadersThatMatchWhatIsSent(t *testing.T) {
	server := &strictServer{run: fmt.Sprintf("%x", time.Now().UnixNano())}
	ts := httptest.NewServer(server)
	defer ts.Close()

	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	headers := func(method, name string, params ...proxy.MCPParamHeader) func(*proxy.Envelope) {
		return func(e *proxy.Envelope) {
			e.MCPProtocolVersion, e.MCPMethod, e.MCPName, e.MCPParamHeaders = "2026-07-28", method, name, params
		}
	}
	rec := (&httpRecording{t: t}).
		add(proxy.ClientToServer, 1, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`, headers("server/discover", "")).
		add(proxy.ServerToClient, 1, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["2026-07-28"]}}`, nil).
		add(proxy.ClientToServer, 2, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create","arguments":{},`+meta+`}}`,
			headers("tools/call", "create", proxy.MCPParamHeader{Name: "Authorization", Value: "Bearer doctored"})).
		add(proxy.ServerToClient, 2, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"complete","content":[],"structuredContent":{"id":"note-0ld1"}}}`, nil).
		add(proxy.ClientToServer, 3, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"move","arguments":{"note":"note-0ld1"},`+meta+`}}`,
			headers("tools/call", "move", proxy.MCPParamHeader{Name: "Mcp-Param-Note", Value: "note-0ld1"})).
		add(proxy.ServerToClient, 3, `{"jsonrpc":"2.0","id":3,"result":{"resultType":"complete","content":[{"type":"text","text":"moved"}]}}`, nil).
		add(proxy.ClientToServer, 4, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"confirm","arguments":{},`+meta+`}}`, headers("tools/call", "confirm")).
		add(proxy.ServerToClient, 4, `{"jsonrpc":"2.0","id":4,"result":{"resultType":"input_required","requestState":"state-0ld2","inputRequests":{"ok":{"method":"elicitation/create","params":{"message":"sure?"}}}}}`, nil).
		add(proxy.ClientToServer, 5, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"confirm","arguments":{},"requestState":"state-0ld2","inputResponses":{"ok":{"action":"accept"}},`+meta+`}}`, headers("tools/call", "confirm")).
		add(proxy.ServerToClient, 5, `{"jsonrpc":"2.0","id":5,"result":{"resultType":"complete","content":[{"type":"text","text":"confirmed"}]}}`, nil).
		load()

	sink, report := replayHTTP(t, rec, ts.URL+"/mcp", SessionOptions{})
	server.mu.Lock()
	bad := server.bad
	server.mu.Unlock()
	if len(bad) != 0 {
		t.Fatalf("the server rejected headers that disagreed with the body: %v", bad)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing %v", report.Missing)
	}
	if got := sink.answerTo(t, "tools/call", "move"); !strings.Contains(got, "moved") || strings.Contains(got, "isError") {
		t.Fatalf("move = %s", got)
	}
	var retry string
	for _, frame := range sink.clientFrames("tools/call") {
		if strings.Contains(frame, "requestState") {
			retry = frame
		}
	}
	if !strings.Contains(retry, `"requestState":"state-`+server.run+`"`) {
		t.Fatalf("the retry carries the recorded state: %s", retry)
	}
}

// TestReplayOverHTTPReadsMcpParamFromTheListing. A client over HTTP reads the
// Mcp-Param headers a call needs from the definition the server gave, and the
// replay does the same. A 2026 session recorded over stdio has no headers at
// all, and one recorded over HTTP has the headers of the definition it was
// recorded against, which the server may have renamed since. Both reach a
// server that requires the header with the one its listing declares, mirroring
// the note id carried into the call.
func TestReplayOverHTTPReadsMcpParamFromTheListing(t *testing.T) {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	session := func(t *testing.T, edit func(method, name string, params ...proxy.MCPParamHeader) func(*proxy.Envelope)) *Recording {
		return (&httpRecording{t: t}).
			add(proxy.ClientToServer, 1, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta+`}}`, edit("tools/list", "")).
			add(proxy.ServerToClient, 1, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","tools":[{"name":"create","inputSchema":{"type":"object"}},`+
				`{"name":"move","inputSchema":{"type":"object","properties":{"note":{"type":"string","x-mcp-header":"Note-Id"}}}}]}}`, edit("", "")).
			add(proxy.ClientToServer, 2, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create","arguments":{},`+meta+`}}`, edit("tools/call", "create")).
			add(proxy.ServerToClient, 2, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"complete","content":[],"structuredContent":{"id":"note-0ld1"}}}`, edit("", "")).
			add(proxy.ClientToServer, 3, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"move","arguments":{"note":"note-0ld1"},`+meta+`}}`,
				edit("tools/call", "move", proxy.MCPParamHeader{Name: "Mcp-Param-Note-Id", Value: "note-0ld1"})).
			add(proxy.ServerToClient, 3, `{"jsonrpc":"2.0","id":3,"result":{"resultType":"complete","content":[{"type":"text","text":"moved"}]}}`, edit("", "")).
			load()
	}
	recordings := map[string]func(t *testing.T) *Recording{
		"recorded over stdio": func(t *testing.T) *Recording {
			return session(t, func(string, string, ...proxy.MCPParamHeader) func(*proxy.Envelope) {
				return func(e *proxy.Envelope) { e.Transport, e.Exchange, e.Status = proxy.TransportStdio, 0, 0 }
			})
		},
		"recorded over HTTP against an older definition": func(t *testing.T) *Recording {
			return session(t, func(method, name string, params ...proxy.MCPParamHeader) func(*proxy.Envelope) {
				return func(e *proxy.Envelope) {
					if method != "" {
						e.MCPProtocolVersion, e.MCPMethod, e.MCPName, e.MCPParamHeaders = "2026-07-28", method, name, params
					}
				}
			})
		},
	}
	for name, recording := range recordings {
		t.Run(name, func(t *testing.T) {
			server := &strictServer{run: fmt.Sprintf("%x", time.Now().UnixNano())}
			ts := httptest.NewServer(server)
			defer ts.Close()

			sink, report := replayHTTP(t, recording(t), ts.URL+"/mcp", SessionOptions{})
			server.mu.Lock()
			bad := server.bad
			server.mu.Unlock()
			if len(bad) != 0 || len(report.Missing) != 0 {
				t.Fatalf("rejected %v, missing %v", bad, report.Missing)
			}
			if got := sink.answerTo(t, "tools/call", "move"); !strings.Contains(got, "moved") || strings.Contains(got, "isError") {
				t.Fatalf("move = %s", got)
			}
			// The replayed capture shows what went over the wire: the header the
			// listing declares, and not the one the recording carried.
			sink.mu.Lock()
			defer sink.mu.Unlock()
			for _, e := range sink.envs {
				if e.Direction != proxy.ClientToServer || e.MCPName != "move" {
					continue
				}
				want := []proxy.MCPParamHeader{{Name: "Mcp-Param-Note", Value: "note-" + server.run}}
				if fmt.Sprint(e.MCPParamHeaders) != fmt.Sprint(want) {
					t.Fatalf("move went out with %+v, want %+v", e.MCPParamHeaders, want)
				}
				return
			}
			t.Fatal("the replayed capture has no move call")
		})
	}
}

// TestReplayOverHTTPCancelsByClosingTheStream. Over stdio a 2026 client
// cancels with notifications/cancelled, and over HTTP that revision has it close
// the stream the answer would come on and send nothing. A session recorded
// over stdio crosses to HTTP the same way, and an answer the recording got
// anyway, because it crossed the cancellation on the wire, is not waited for,
// since nothing comes on a closed stream.
func TestReplayOverHTTPCancelsByClosingTheStream(t *testing.T) {
	server := &strictServer{run: fmt.Sprintf("%x", time.Now().UnixNano())}
	ts := httptest.NewServer(server)
	defer ts.Close()

	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	stdio := func(e *proxy.Envelope) { e.Transport, e.Exchange, e.Status = proxy.TransportStdio, 0, 0 }
	rec := (&httpRecording{t: t}).
		add(proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow","arguments":{},`+meta+`}}`, stdio).
		add(proxy.ClientToServer, 0, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"the user gave up"}}`, stdio).
		add(proxy.ServerToClient, 0, `{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","content":[{"type":"text","text":"finished"}]}}`, stdio).
		load()

	start := time.Now()
	_, report := replayHTTP(t, rec, ts.URL+"/mcp", SessionOptions{Wait: 4 * time.Second})
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the replay waited %s for an answer to a call it cancelled", took)
	}
	// The server learns of a closed stream from its connection, a moment after
	// the replay closed it.
	cancelled := server.waitCancelled(2 * time.Second)
	server.mu.Lock()
	bad := server.bad
	server.mu.Unlock()
	if len(bad) != 0 {
		t.Fatalf("the server got what a 2026 client over HTTP never sends: %v", bad)
	}
	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want the stream closed under the call", cancelled)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing %v", report.Missing)
	}
}

// waitCancelled returns how many calls the server saw cancelled, once it has
// seen one or patience has passed.
func (s *strictServer) waitCancelled(patience time.Duration) int {
	deadline := time.Now().Add(patience)
	for {
		s.mu.Lock()
		n := s.cancelled
		s.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReplayOfAClientThatLeftItsExchange. A 2026 client recorded over HTTP
// cancels by leaving the exchange, which the capture records with no frame of
// its own. Replayed over HTTP the replay leaves the same way, and over stdio it
// sends the notification a client there cancels with. A 2025 client that left
// only disconnected, which stdio has no way to say, so that is reported rather
// than turned into a cancellation it never was.
func TestReplayOfAClientThatLeftItsExchange(t *testing.T) {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	left := func(e *proxy.Envelope) { e.Raw, e.Abandoned = nil, true }
	recording := func(t *testing.T, call string, edit func(*proxy.Envelope)) *Recording {
		return (&httpRecording{t: t}).
			add(proxy.ClientToServer, 1, call, edit).
			add(proxy.ClientToServer, 1, `{}`, left).
			load()
	}

	t.Run("over HTTP", func(t *testing.T) {
		server := &strictServer{run: fmt.Sprintf("%x", time.Now().UnixNano())}
		ts := httptest.NewServer(server)
		defer ts.Close()
		rec := recording(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow","arguments":{},`+meta+`}}`,
			func(e *proxy.Envelope) {
				e.MCPProtocolVersion, e.MCPMethod, e.MCPName = "2026-07-28", "tools/call", "slow"
			})

		_, report := replayHTTP(t, rec, ts.URL+"/mcp", SessionOptions{Wait: 4 * time.Second})
		cancelled := server.waitCancelled(2 * time.Second)
		server.mu.Lock()
		bad := server.bad
		server.mu.Unlock()
		if len(bad) != 0 || cancelled != 1 {
			t.Fatalf("rejected %v, cancelled %d, want the stream closed under the call", bad, cancelled)
		}
		if report.Sent != 2 || len(report.Skipped) != 0 || len(report.Missing) != 0 {
			t.Fatalf("sent %d, skipped %v, missing %v", report.Sent, report.Skipped, report.Missing)
		}
	})

	t.Run("over stdio", func(t *testing.T) {
		rec := recording(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow","arguments":{"ms":3000},`+meta+`}}`, nil)
		sink, report := replayAgainst(t, rec, SessionOptions{Wait: 2 * time.Second})
		got := sink.clientFrames("notifications/cancelled")
		if len(got) != 1 || !strings.Contains(got[0], `"requestId":7`) {
			t.Fatalf("sent %v, skipped %v, want the call cancelled the stdio way", got, report.Skipped)
		}
	})

	t.Run("a 2025 client over stdio", func(t *testing.T) {
		rec := recording(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow","arguments":{"ms":3000}}}`,
			func(e *proxy.Envelope) { e.MCPProtocolVersion = "2025-11-25" })
		sink, report := replayAgainst(t, rec, SessionOptions{Wait: 4 * time.Second})
		if got := sink.clientFrames("notifications/cancelled"); len(got) != 0 {
			t.Fatalf("a disconnect was replayed as a cancellation: %v", got)
		}
		if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "2025-11-25 client leaving an HTTP exchange disconnected") {
			t.Fatalf("skipped = %v", report.Skipped)
		}
	})
}

// TestReplayOverHTTPStartsTheServer. With a command beside the target, the
// replay starts the server, waits for it to listen, and stops it afterwards,
// which is what a CI job needs from one line. A server that never listens is
// reported, not waited on forever.
func TestReplayOverHTTPStartsTheServer(t *testing.T) {
	rec := (&httpRecording{t: t}).
		add(proxy.ClientToServer, 1, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"rec","version":"1"}}}`, nil).
		add(proxy.ServerToClient, 1, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"notes"}}}`, nil).
		load()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := ReplaySession(ctx, rec, SessionOptions{
		Target: "http://127.0.0.1:1/mcp", Command: []string{"sleep", "30"},
		Sink: &collected{}, StartWait: 500 * time.Millisecond, Wait: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "nothing listened on 127.0.0.1:1 within 500ms") {
		t.Fatalf("err = %v", err)
	}

	// A target with no command is a server an earlier step started, and one
	// that never comes up means nothing was replayed, not that every call failed.
	_, err = ReplaySession(ctx, rec, SessionOptions{
		Target: "http://127.0.0.1:1/mcp", Sink: &collected{}, StartWait: 300 * time.Millisecond, Wait: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "nothing listened on 127.0.0.1:1") {
		t.Fatalf("err = %v", err)
	}

	_, err = ReplaySession(ctx, rec, SessionOptions{
		Target: "http://127.0.0.1:1/mcp", Command: []string{"true"},
		Sink: &collected{}, StartWait: 5 * time.Second, Wait: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "exited before it listened") {
		t.Fatalf("err = %v", err)
	}
}
