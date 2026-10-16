package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// sessionServer is a stateful fixture on 2025-11-25 that hands out something
// new on every run: the cursor of its second tools/list page, the id of every
// note it creates, and the id of every request it sends the client. A replay
// that sent the recorded values back would ask it about a page, a note and a
// question it never issued, so the tests below only pass when those values are
// carried.
const sessionServer = `package main

import ("bufio";"encoding/json";"fmt";"os";"strings";"sync";"time")

var (
	mu      sync.Mutex
	run     = fmt.Sprintf("%x", time.Now().UnixNano()%1000000007)
	notes   = map[string]string{}
	waiting = map[string]chan string{}
	count   int
	out     = json.NewEncoder(os.Stdout)
)

func send(v any) { mu.Lock(); defer mu.Unlock(); out.Encode(v) }

func next() string { mu.Lock(); defer mu.Unlock(); count++; return fmt.Sprintf("%s%d", run, count) }

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

type msg struct {
	ID     json.RawMessage ` + "`json:\"id\"`" + `
	Method string          ` + "`json:\"method\"`" + `
	Params struct {
		Name      string            ` + "`json:\"name\"`" + `
		Cursor    string            ` + "`json:\"cursor\"`" + `
		Arguments map[string]any    ` + "`json:\"arguments\"`" + `
	} ` + "`json:\"params\"`" + `
	Result struct {
		Action string ` + "`json:\"action\"`" + `
	} ` + "`json:\"result\"`" + `
}

func text(s string) map[string]any { return map[string]any{"content": []map[string]string{{"type": "text", "text": s}}} }

func handle(line []byte) {
	var m msg
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if m.Method == "" && len(m.ID) > 0 {
		mu.Lock()
		ch := waiting[string(m.ID)]
		mu.Unlock()
		if ch != nil {
			ch <- m.Result.Action
		}
		return
	}
	if len(m.ID) == 0 {
		return
	}
	reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) }
	switch m.Method {
	case "initialize":
		reply(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "notes"}})
	case "tools/list":
		ro := map[string]any{"readOnlyHint": true}
		rw := map[string]any{"readOnlyHint": false, "destructiveHint": false}
		if m.Params.Cursor == "" {
			mu.Lock(); cursor := "page2-" + run; mu.Unlock()
			reply(map[string]any{"tools": []map[string]any{{"name": "create_note", "inputSchema": map[string]any{"type": "object"}, "annotations": rw}, {"name": "get_note", "inputSchema": map[string]any{"type": "object"}, "annotations": ro}}, "nextCursor": cursor})
		} else if m.Params.Cursor == "page2-"+run {
			reply(map[string]any{"tools": []map[string]any{{"name": "ask", "inputSchema": map[string]any{"type": "object"}, "annotations": rw}, {"name": "slow", "inputSchema": map[string]any{"type": "object"}, "annotations": ro}}})
		} else {
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32602, "message": "unknown cursor " + m.Params.Cursor}})
		}
	case "tools/call":
		switch m.Params.Name {
		case "create_note":
			id := "n-" + next()
			mu.Lock(); notes[id] = fmt.Sprint(m.Params.Arguments["text"]); mu.Unlock()
			res := text("Created note " + id + ".")
			if m.Params.Arguments["structured"] == true {
				res["structuredContent"] = map[string]any{"id": id}
			}
			reply(res)
		case "get_note":
			id := fmt.Sprint(m.Params.Arguments["id"])
			mu.Lock(); body, ok := notes[id]; mu.Unlock()
			if !ok {
				res := text("no note " + id)
				res["isError"] = true
				reply(res)
				return
			}
			reply(text(body))
		case "ask":
			go func() {
				id := "q-" + next()
				ch := make(chan string, 1)
				raw, _ := json.Marshal(id)
				mu.Lock(); waiting[string(raw)] = ch; mu.Unlock()
				send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "elicitation/create", "params": map[string]any{"message": "sure?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}}}})
				select {
				case action := <-ch:
					reply(text("the user said " + action))
				case <-time.After(5 * time.Second):
					reply(text("nobody answered"))
				}
			}()
		case "slow":
			go func() {
				ms, _ := m.Params.Arguments["ms"].(float64)
				time.Sleep(time.Duration(ms) * time.Millisecond)
				reply(text("slept"))
			}()
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32602, "message": "unknown tool " + strings.TrimSpace(m.Params.Name)}})
		}
	default:
		send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}
`

// recording builds a capture from frames written the way a client and this
// server exchanged them once, with the values that run handed out.
type recording struct {
	t      *testing.T
	frames []string
	start  time.Time
}

func newRecording(t *testing.T) *recording {
	return &recording{t: t, start: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)}
}

func (r *recording) add(dir proxy.Direction, after time.Duration, raw string) *recording {
	env := proxy.Envelope{
		SessionID: "rec", ServerLabel: "notes", Seq: uint64(len(r.frames) + 2),
		TS: r.start.Add(after), Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw),
	}
	b, err := json.Marshal(env)
	if err != nil {
		r.t.Fatal(err)
	}
	r.frames = append(r.frames, string(b))
	return r
}

func (r *recording) client(after time.Duration, raw string) *recording {
	return r.add(proxy.ClientToServer, after, raw)
}

func (r *recording) server(after time.Duration, raw string) *recording {
	return r.add(proxy.ServerToClient, after, raw)
}

func (r *recording) load() *Recording {
	r.t.Helper()
	rec, err := LoadRecording(strings.NewReader(strings.Join(r.frames, "\n")+"\n"), "")
	if err != nil {
		r.t.Fatal(err)
	}
	return rec
}

// collected is a sink that keeps the replayed capture in memory.
type collected struct {
	mu   sync.Mutex
	envs []proxy.Envelope
}

func (c *collected) Emit(e proxy.Envelope) { c.mu.Lock(); c.envs = append(c.envs, e); c.mu.Unlock() }
func (c *collected) Close() error          { return nil }

// response returns the replayed server frame answering the client request
// whose method and tool match, as text.
func (c *collected) answerTo(t *testing.T, method, tool string) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var id string
	for _, e := range c.envs {
		msg, ok := proxy.ParseRPC(e.Raw)
		if !ok || e.Direction != proxy.ClientToServer || msg.Method != method {
			continue
		}
		if tool != "" && !strings.Contains(string(msg.Params), `"name":"`+tool+`"`) {
			continue
		}
		id = string(msg.ID)
	}
	if id == "" {
		t.Fatalf("the replay sent no %s %s", method, tool)
	}
	for _, e := range c.envs {
		msg, ok := proxy.ParseRPC(e.Raw)
		if ok && e.Direction == proxy.ServerToClient && msg.Method == "" && string(msg.ID) == id {
			return string(e.Raw)
		}
	}
	t.Fatalf("the replayed %s %s got no answer", method, tool)
	return ""
}

func (c *collected) clientFrames(method string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.envs {
		if msg, ok := proxy.ParseRPC(e.Raw); ok && e.Direction == proxy.ClientToServer && (method == "" || msg.Method == method) {
			out = append(out, string(e.Raw))
		}
	}
	return out
}

func replayAgainst(t *testing.T, rec *Recording, opts SessionOptions) (*collected, SessionReport) {
	t.Helper()
	sink := &collected{}
	opts.Command = []string{sessionBin}
	opts.Sink = sink
	opts.SessionID = "replayed"
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

const handshake = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{}},"clientInfo":{"name":"rec-client","version":"1"}}}`

func baseRecording(t *testing.T) *recording {
	return newRecording(t).
		client(0, handshake).
		server(10*time.Millisecond, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"notes"}}}`).
		client(11*time.Millisecond, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// TestReplayCarriesWhatTheServerHandedOut is the point of the feature. The
// recorded client paged with a cursor, read back a note by the id it was
// given, and answered the server's question under the id the server chose.
// None of those values exist in a fresh server, and the replay still works,
// because each one is learned from the server's own answers as they come.
func TestReplayCarriesWhatTheServerHandedOut(t *testing.T) {
	for _, structured := range []bool{true, false} {
		t.Run(fmt.Sprintf("structured=%v", structured), func(t *testing.T) {
			created := `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"Created note n-old7."}]}}`
			if structured {
				created = `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"Created note n-old7."}],"structuredContent":{"id":"n-old7"}}}`
			}
			rec := baseRecording(t).
				client(1*time.Second, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`).
				server(1010*time.Millisecond, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"create_note","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false,"destructiveHint":false}},{"name":"get_note","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}],"nextCursor":"page2-0ld9"}}`).
				client(1020*time.Millisecond, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"cursor":"page2-0ld9"}}`).
				server(1030*time.Millisecond, `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"ask","inputSchema":{"type":"object"}},{"name":"slow","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]}}`).
				client(5*time.Second, fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_note","arguments":{"text":"hello","structured":%v}}}`, structured)).
				server(5010*time.Millisecond, created).
				client(9*time.Second, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_note","arguments":{"id":"n-old7"}}}`).
				server(9010*time.Millisecond, `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"hello"}]}}`).
				client(12*time.Second, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"ask","arguments":{}}}`).
				server(12010*time.Millisecond, `{"jsonrpc":"2.0","id":"q-old3","method":"elicitation/create","params":{"message":"sure?","requestedSchema":{"type":"object","properties":{}}}}`).
				client(15*time.Second, `{"jsonrpc":"2.0","id":"q-old3","result":{"action":"accept","content":{}}}`).
				server(15010*time.Millisecond, `{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"the user said accept"}]}}`).
				load()

			started := time.Now()
			sink, report := replayAgainst(t, rec, SessionOptions{})
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("the replay took %v, so it waited out the recording's pauses", elapsed)
			}
			if len(report.Missing) != 0 || len(report.Skipped) != 0 {
				t.Fatalf("missing %v, skipped %v", report.Missing, report.Skipped)
			}
			if page2 := sink.answerTo(t, "tools/list", ""); strings.Contains(page2, "unknown cursor") || !strings.Contains(page2, `"slow"`) {
				t.Fatalf("the second page was not reached with the server's own cursor: %s", page2)
			}
			if got := sink.answerTo(t, "tools/call", "get_note"); strings.Contains(got, "isError") || !strings.Contains(got, "hello") {
				t.Fatalf("get_note did not find the note the replay created: %s", got)
			}
			if got := sink.answerTo(t, "tools/call", "ask"); !strings.Contains(got, "the user said accept") {
				t.Fatalf("the server's question was not answered under its new id: %s", got)
			}
		})
	}
}

// TestReplayTakesAnAnswerRecordedAheadOfItsRequest. A stdio capture recorded
// before 0.28.0 can hold a quick client's answer ahead of the server request it
// answers, and the server's new id for that request is only known once the
// request arrives. The answer goes out behind it, under that id.
func TestReplayTakesAnAnswerRecordedAheadOfItsRequest(t *testing.T) {
	rec := baseRecording(t).
		client(1*time.Second, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"ask","arguments":{}}}`).
		client(1010*time.Millisecond, `{"jsonrpc":"2.0","id":"q-old3","result":{"action":"accept","content":{}}}`).
		server(1012*time.Millisecond, `{"jsonrpc":"2.0","id":"q-old3","method":"elicitation/create","params":{"message":"sure?","requestedSchema":{"type":"object","properties":{}}}}`).
		server(1020*time.Millisecond, `{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"the user said accept"}]}}`).
		load()

	sink, report := replayAgainst(t, rec, SessionOptions{})
	if len(report.Missing) != 0 || len(report.Skipped) != 0 || len(report.Unanswered) != 0 {
		t.Fatalf("missing %v, skipped %v, unanswered %v", report.Missing, report.Skipped, report.Unanswered)
	}
	if got := sink.answerTo(t, "tools/call", "ask"); !strings.Contains(got, "the user said accept") {
		t.Fatalf("the early answer did not reach the server's question: %s", got)
	}
}

func TestAnswersAfterRequestsLeavesAnOrderedCaptureAlone(t *testing.T) {
	env := func(dir proxy.Direction, raw string) proxy.Envelope {
		return proxy.Envelope{Direction: dir, Raw: json.RawMessage(raw)}
	}
	// A server that reuses an id once its first request is answered, which is
	// legal, and an answer to the first must stay where it is.
	frames := []proxy.Envelope{
		env(proxy.ServerToClient, `{"jsonrpc":"2.0","id":0,"method":"ping"}`),
		env(proxy.ClientToServer, `{"jsonrpc":"2.0","id":0,"result":{}}`),
		env(proxy.ServerToClient, `{"jsonrpc":"2.0","id":0,"method":"roots/list"}`),
		env(proxy.ClientToServer, `{"jsonrpc":"2.0","id":0,"result":{"roots":[]}}`),
	}
	if got := answersAfterRequests(frames); fmt.Sprint(got) != fmt.Sprint(frames) {
		t.Fatalf("an ordered capture was reordered: %v", got)
	}
	// An answer ahead of its request moves just behind it, and nothing else does.
	early := []proxy.Envelope{frames[1], frames[0], frames[2], frames[3]}
	want := []proxy.Envelope{frames[0], frames[1], frames[2], frames[3]}
	if got := answersAfterRequests(early); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestReplayKeepsTheClientsOrderButNotItsPauses. Two calls the client sent at
// once go out at once, and a call the client sent only after an answer waits
// for that answer, however long the server takes. The recorded gaps between
// them, which are a model thinking, are not waited out.
func TestReplayKeepsTheClientsOrderButNotItsPauses(t *testing.T) {
	rec := baseRecording(t).
		client(1*time.Second, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow","arguments":{"ms":400}}}`).
		client(1*time.Second, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow","arguments":{"ms":400}}}`).
		server(1400*time.Millisecond, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"slept"}]}}`).
		server(1401*time.Millisecond, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"slept"}]}}`).
		client(30*time.Second, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"slow","arguments":{"ms":10}}}`).
		server(30010*time.Millisecond, `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"slept"}]}}`).
		load()
	started := time.Now()
	sink, report := replayAgainst(t, rec, SessionOptions{})
	elapsed := time.Since(started)
	if len(report.Missing) != 0 {
		t.Fatalf("missing %v", report.Missing)
	}
	// Run one after the other the first two would take 800 ms. At once they
	// take 400, and the recorded half minute is not spent at all.
	if elapsed < 400*time.Millisecond || elapsed > 790*time.Millisecond+2*time.Second {
		t.Fatalf("elapsed %v", elapsed)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var thirdSent, firstAnswered time.Time
	for _, e := range sink.envs {
		msg, _ := proxy.ParseRPC(e.Raw)
		switch {
		case e.Direction == proxy.ClientToServer && string(msg.ID) == "4":
			thirdSent = e.TS
		case e.Direction == proxy.ServerToClient && msg.Method == "" && (string(msg.ID) == "2" || string(msg.ID) == "3") && firstAnswered.IsZero():
			firstAnswered = e.TS
		}
	}
	if thirdSent.IsZero() || firstAnswered.IsZero() || thirdSent.Before(firstAnswered) {
		t.Fatalf("the third call went out at %v, before the answers it followed (%v)", thirdSent, firstAnswered)
	}
}

// TestReplayCancelsAfterTheRecordedDelay. The client cancelled a call it had
// waited 150 ms for. The replay waits the same 150 ms and cancels too, unless
// the server answers first, in which case the client had no reason to.
func TestReplayCancelsAfterTheRecordedDelay(t *testing.T) {
	cancelled := func(ms int) *Recording {
		return baseRecording(t).
			client(1*time.Second, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow","arguments":{"ms":%d}}}`, ms)).
			client(1150*time.Millisecond, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2,"reason":"timeout"}}`).
			load()
	}

	sink, report := replayAgainst(t, cancelled(1000), SessionOptions{Wait: 2 * time.Second})
	if got := sink.clientFrames("notifications/cancelled"); len(got) != 1 {
		t.Fatalf("a call still running after the recorded delay should be cancelled, sent %v (skipped %v)", got, report.Skipped)
	}

	sink, report = replayAgainst(t, cancelled(10), SessionOptions{Wait: 2 * time.Second})
	if got := sink.clientFrames("notifications/cancelled"); len(got) != 0 {
		t.Fatalf("a call answered in time should not be cancelled, sent %v", got)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "answered before the client would have given up") {
		t.Fatalf("skipped = %v", report.Skipped)
	}
}

// TestReplaySkipsWhatItIsToldTo. --read-only holds back every call the
// recording's own listing did not mark read-only, along with anything that
// only makes sense after one, and says so.
func TestReplaySkipsWhatItIsToldTo(t *testing.T) {
	rec := baseRecording(t).
		client(1*time.Second, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`).
		server(1010*time.Millisecond, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"create_note","annotations":{"readOnlyHint":false}},{"name":"get_note","annotations":{"readOnlyHint":true}}]}}`).
		client(2*time.Second, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_note","arguments":{"text":"x"}}}`).
		server(2010*time.Millisecond, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"Created note n-old1."}]}}`).
		client(3*time.Second, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_note","arguments":{"id":"n-old1"}}}`).
		server(3010*time.Millisecond, `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"x"}]}}`).
		load()

	for _, tc := range []struct {
		name string
		opts SessionOptions
	}{
		{"read-only", SessionOptions{ReadOnly: true}},
		{"a named tool", SessionOptions{SkipTools: []string{"create_note"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, report := replayAgainst(t, rec, tc.opts)
			for _, frame := range sink.clientFrames("tools/call") {
				if strings.Contains(frame, "create_note") {
					t.Fatalf("a skipped tool was called: %s", frame)
				}
			}
			if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "skipped tool call") {
				t.Fatalf("skipped = %v", report.Skipped)
			}
			// get_note still ran, and failed, which is the replay telling the truth
			// about a session that depended on a write it was told not to make.
			if got := sink.answerTo(t, "tools/call", "get_note"); !strings.Contains(got, "isError") {
				t.Fatalf("get_note = %s", got)
			}
		})
	}
}

// TestReplayReportsAnswersThatNeverCame. A call the recording got an answer to
// and the server under test never answers is reported once, after one wait,
// and the replay goes on.
func TestReplayReportsAnswersThatNeverCame(t *testing.T) {
	rec := baseRecording(t).
		client(1*time.Second, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow","arguments":{"ms":5000}}}`).
		server(1010*time.Millisecond, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"slept"}]}}`).
		client(2*time.Second, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`).
		server(2010*time.Millisecond, `{"jsonrpc":"2.0","id":3,"result":{"tools":[]}}`).
		load()
	started := time.Now()
	sink, report := replayAgainst(t, rec, SessionOptions{Wait: 300 * time.Millisecond})
	if len(report.Missing) != 1 || !strings.Contains(report.Missing[0], `no answer to tools/call "slow" (id 2) within 300ms`) {
		t.Fatalf("missing = %v", report.Missing)
	}
	if len(sink.clientFrames("tools/list")) != 1 {
		t.Fatal("the replay should go on after giving up on one answer")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("elapsed %v, the wait should be paid once", elapsed)
	}
}

// TestReplayAnswersPingsItself. A ping is about the moment, so the replay
// answers whatever the server sends rather than replaying the recorded pong.
func TestReplayAnswersPingsItself(t *testing.T) {
	rec := baseRecording(t).
		server(20*time.Millisecond, `{"jsonrpc":"2.0","id":"p1","method":"ping"}`).
		client(25*time.Millisecond, `{"jsonrpc":"2.0","id":"p1","result":{}}`).
		load()
	_, report := replayAgainst(t, rec, SessionOptions{})
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "pings") {
		t.Fatalf("skipped = %v", report.Skipped)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("a ping is not something to wait for, missing = %v", report.Missing)
	}
}

// TestReplayOfAServerThatWillNotStart says so rather than reporting a
// replay that sent nothing.
func TestReplayOfAServerThatWillNotStart(t *testing.T) {
	rec := baseRecording(t).load()
	_, err := ReplaySession(context.Background(), rec, SessionOptions{
		Command: []string{"/nonexistent/mcpsnoop-no-such-server"}, Sink: &collected{}, Wait: time.Second,
	})
	if err == nil {
		t.Fatal("want an error for a server that cannot start")
	}
}

// TestLoadRecordingPicksTheSession. A file can hold several sessions, and the
// first is the default, the way check and export read one.
func TestLoadRecordingPicksTheSession(t *testing.T) {
	line := func(session string, seq uint64) string {
		b, _ := json.Marshal(proxy.Envelope{SessionID: session, ServerLabel: session + "-label", Seq: seq, TS: time.Now(),
			Direction: proxy.ClientToServer, Transport: proxy.TransportStdio, Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"x"}`)})
		return string(b)
	}
	file := strings.Join([]string{line("a", 1), line("b", 1), line("b", 2)}, "\n")
	first, err := LoadRecording(strings.NewReader(file), "")
	if err != nil || first.SessionID != "a" || first.ClientFrames() != 1 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	named, err := LoadRecording(strings.NewReader(file), "b")
	if err != nil || named.Label != "b-label" || named.ClientFrames() != 2 {
		t.Fatalf("named = %+v, %v", named, err)
	}
	if _, err := LoadRecording(strings.NewReader(file), "c"); err == nil {
		t.Fatal("a session that is not there should be an error")
	}
}
