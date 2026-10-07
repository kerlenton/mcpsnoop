package store

import (
	"encoding/json"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const (
	listAppend = `{"jsonrpc":"2.0","id":"l","result":{"tools":[` +
		`{"name":"append","inputSchema":{"type":"object"}},` +
		`{"name":"set","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false,"idempotentHint":true}},` +
		`{"name":"read","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]}}`
	callAppendA = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"append","arguments":{"text":"a","n":1}}}`
	cancel1     = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"timed out"}}`
	callAppendB = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"append","arguments":{"text":"a","n":1}}}`
	ok1         = `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`
	ok2         = `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`
	failed2     = `{"jsonrpc":"2.0","id":2,"result":{"content":[],"isError":true}}`
	error1      = `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"boom"}}`
)

// duplicatesOf runs one script of frames, each a direction and a raw frame,
// after the tool listing every case shares.
func duplicatesOf(t *testing.T, frames ...[2]any) []DuplicateCall {
	t.Helper()
	s := New()
	script := [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":"l","method":"tools/list"}`},
		{proxy.ServerToClient, 1, listAppend},
	}
	for i, f := range frames {
		script = append(script, [3]any{f[0], 10 * (i + 1), f[1]})
	}
	ingestFrames(t, s, script)
	return s.Duplicates("demo")
}

var (
	c2s = proxy.ClientToServer
	s2c = proxy.ServerToClient
)

func TestDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames [][2]any
		want   []DuplicateCall
	}{
		{
			// The case spec issue #3394 reproduces. The client gave up, the server
			// finished anyway, and the retry ran the tool a second time.
			name:   "cancelled, retried, and both answered",
			frames: [][2]any{{c2s, callAppendA}, {c2s, cancel1}, {c2s, callAppendB}, {s2c, ok1}, {s2c, ok2}},
			want:   []DuplicateCall{{Tool: "append", Seq: 5, ID: "2", EarlierSeq: 3, EarlierID: "1", AfterCancel: true, RanTwice: true}},
		},
		{
			// The spec tells the client to ignore an answer to a call it cancelled, so
			// one arriving before the retry has not told the client anything, and here
			// it says the work was done.
			name:   "cancelled, answered late, then retried",
			frames: [][2]any{{c2s, callAppendA}, {c2s, cancel1}, {s2c, ok1}, {c2s, callAppendB}, {s2c, ok2}},
			want:   []DuplicateCall{{Tool: "append", Seq: 6, ID: "2", EarlierSeq: 3, EarlierID: "1", AfterCancel: true, RanTwice: true}},
		},
		{
			name:   "cancelled, retried, and the first never answered",
			frames: [][2]any{{c2s, callAppendA}, {c2s, cancel1}, {c2s, callAppendB}, {s2c, ok2}},
			want:   []DuplicateCall{{Tool: "append", Seq: 5, ID: "2", EarlierSeq: 3, EarlierID: "1", AfterCancel: true}},
		},
		{
			// How a cancellation looks on Streamable HTTP, where closing the stream is
			// the signal and nothing arrives to say so.
			name:   "retried while the first was unanswered, which it stayed",
			frames: [][2]any{{c2s, callAppendA}, {c2s, callAppendB}, {s2c, ok2}},
			want:   []DuplicateCall{{Tool: "append", Seq: 4, ID: "2", EarlierSeq: 3, EarlierID: "1"}},
		},
		{
			// A hedged request, the copy sent while the first was slow and the first
			// cancelled once the copy was out. The first finished anyway.
			name:   "sent again while the first was in flight, then the first cancelled",
			frames: [][2]any{{c2s, callAppendA}, {c2s, callAppendB}, {c2s, cancel1}, {s2c, ok1}, {s2c, ok2}},
			want:   []DuplicateCall{{Tool: "append", Seq: 4, ID: "2", EarlierSeq: 3, EarlierID: "1", RanTwice: true}},
		},
		{
			name:   "two identical calls in flight and both answered",
			frames: [][2]any{{c2s, callAppendA}, {c2s, callAppendB}, {s2c, ok1}, {s2c, ok2}},
		},
		{
			name:   "the first was answered before the second was sent",
			frames: [][2]any{{c2s, callAppendA}, {s2c, ok1}, {c2s, callAppendB}, {s2c, ok2}},
		},
		{
			name:   "the retry failed, so it did no second piece of work",
			frames: [][2]any{{c2s, callAppendA}, {c2s, cancel1}, {c2s, callAppendB}, {s2c, failed2}},
		},
		{
			// An error after a cancellation is usually the cancellation reported back,
			// which says nothing about whether the work was done first.
			name:   "the first answered late with an error",
			frames: [][2]any{{c2s, callAppendA}, {c2s, cancel1}, {c2s, callAppendB}, {s2c, error1}, {s2c, ok2}},
			want:   []DuplicateCall{{Tool: "append", Seq: 5, ID: "2", EarlierSeq: 3, EarlierID: "1", AfterCancel: true}},
		},
		{
			name: "the first answered late with a tool error",
			frames: [][2]any{{c2s, callAppendA}, {c2s, cancel1}, {c2s, callAppendB},
				{s2c, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"context canceled"}],"isError":true}}`}, {s2c, ok2}},
			want: []DuplicateCall{{Tool: "append", Seq: 5, ID: "2", EarlierSeq: 3, EarlierID: "1", AfterCancel: true}},
		},
		{
			name: "an idempotent tool",
			frames: [][2]any{
				{c2s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set","arguments":{"k":"v"}}}`}, {c2s, cancel1},
				{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"set","arguments":{"k":"v"}}}`}, {s2c, ok2},
			},
		},
		{
			name: "a read-only tool",
			frames: [][2]any{
				{c2s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"k":"v"}}}`}, {c2s, cancel1},
				{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"k":"v"}}}`}, {s2c, ok2},
			},
		},
		{
			name: "different arguments",
			frames: [][2]any{
				{c2s, callAppendA}, {c2s, cancel1},
				{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"append","arguments":{"text":"b","n":1}}}`}, {s2c, ok2},
			},
		},
		{
			// Key order, spacing and _meta are how a request was written, not what it
			// asks for, so they do not make it a different call.
			name: "the same arguments spelled differently",
			frames: [][2]any{
				{c2s, callAppendA}, {c2s, cancel1},
				{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{"progressToken":9},"name":"append","arguments":{ "n":1 , "text":"a" }}}`},
				{s2c, ok2},
			},
			want: []DuplicateCall{{Tool: "append", Seq: 5, ID: "2", EarlierSeq: 3, EarlierID: "1", AfterCancel: true}},
		},
		{
			name: "a number written another way is another argument",
			frames: [][2]any{
				{c2s, callAppendA}, {c2s, cancel1},
				{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"append","arguments":{"text":"a","n":1.0}}}`}, {s2c, ok2},
			},
		},
		{
			// The server answered with an input request, which says it has not done the
			// work yet, so a client starting over is not repeating anything.
			name: "the first is waiting on the user",
			frames: [][2]any{
				{c2s, callAppendA},
				{s2c, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st","inputRequests":{"ok":{"method":"elicitation/create","params":{"message":"sure?","requestedSchema":{"type":"object","properties":{}}}}}}}`},
				{c2s, callAppendB}, {s2c, ok2},
			},
		},
		{
			// A task handle is the server accepting the work, so the client knows the
			// first one is running.
			name: "the first became a task",
			frames: [][2]any{
				{c2s, callAppendA},
				{s2c, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"task","taskId":"t1","status":"working"}}`},
				{c2s, callAppendB}, {s2c, ok2},
			},
		},
		{
			name: "a chain of retries",
			frames: [][2]any{
				{c2s, callAppendA}, {c2s, cancel1}, {c2s, callAppendB},
				{c2s, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`},
				{c2s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"append","arguments":{"text":"a","n":1}}}`},
				{s2c, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`},
			},
			want: []DuplicateCall{
				{Tool: "append", Seq: 5, ID: "2", EarlierSeq: 3, EarlierID: "1", AfterCancel: true},
				{Tool: "append", Seq: 7, ID: "3", EarlierSeq: 5, EarlierID: "2", AfterCancel: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := duplicatesOf(t, tc.frames...)
			if len(got) != len(tc.want) {
				t.Fatalf("duplicates = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("duplicate %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDuplicatesOverHTTP. A 401 refused the first attempt before it reached the
// tool, so re-sending it after authorizing again is what the spec tells a
// client to do. A 502 from something in front of the server says nothing about
// whether the server ran it, so re-sending after one may run the tool twice,
// unless it is mcpsnoop's own for a request it never delivered.
func TestDuplicatesOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		status      int
		undelivered bool
		want        int
	}{{401, false, 0}, {403, false, 0}, {502, false, 1}, {502, true, 0}} {
		s := New()
		frame := func(seq uint64, dir proxy.Direction, exchange uint64, status int, raw string) {
			e := proxy.Envelope{SessionID: "s", ServerLabel: "srv", Seq: seq, Direction: dir,
				Transport: proxy.TransportHTTP, Exchange: exchange, Status: status,
				Undelivered: dir == s2c && raw == "" && tc.undelivered}
			if raw != "" {
				e.Raw = json.RawMessage(raw)
			}
			s.Ingest(e)
		}
		frame(1, c2s, 1, 0, callAppendA)
		frame(2, s2c, 1, tc.status, "")
		frame(3, c2s, 2, 0, callAppendB)
		frame(4, s2c, 2, 200, ok2)
		if got := len(s.Duplicates("s")); got != tc.want {
			t.Errorf("after HTTP %d (undelivered %v), duplicates = %d, want %d", tc.status, tc.undelivered, got, tc.want)
		}
	}
}

// TestRedactedArgumentsAreNeverDuplicates. Two different secrets read the same
// once both are mcpsnoop's placeholder, so arguments the user asked mcpsnoop to
// scrub are not compared at all.
func TestRedactedArgumentsAreNeverDuplicates(t *testing.T) {
	s := New()
	for i, f := range []struct {
		dir      proxy.Direction
		raw      string
		redacted bool
	}{
		{c2s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pay","arguments":{"card":"[REDACTED]"}}}`, true},
		{c2s, cancel1, false},
		{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pay","arguments":{"card":"[REDACTED]"}}}`, true},
		{s2c, ok2, false},
	} {
		s.Ingest(proxy.Envelope{SessionID: "s", ServerLabel: "srv", Seq: uint64(i + 1), Direction: f.dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f.raw), Redacted: f.redacted})
	}
	if got := s.Duplicates("s"); len(got) != 0 {
		t.Fatalf("redacted arguments were compared: %+v", got)
	}
}

func TestDuplicatePhrase(t *testing.T) {
	for _, tc := range []struct {
		d    DuplicateCall
		want string
	}{
		{DuplicateCall{EarlierSeq: 3, AfterCancel: true, RanTwice: true},
			"was called again with the same arguments after the client cancelled the call on frame 3, and the server answered both, so it ran twice"},
		{DuplicateCall{EarlierSeq: 3},
			"was called again with the same arguments while the call on frame 3 was still unanswered, so it may have run twice"},
	} {
		if got := tc.d.Phrase(); got != tc.want {
			t.Errorf("Phrase() = %q, want %q", got, tc.want)
		}
	}
}

// TestABoundedStoreLetsGoOfRetriesItNoLongerShows. A live store keeps a window of
// frames, and a retry is reported by its request frame, so once that frame has
// left the window the retry has to leave with it, along with the calls it held,
// or a long-running session pins every retry it ever saw.
func TestABoundedStoreLetsGoOfRetriesItNoLongerShows(t *testing.T) {
	s := NewBounded(64<<10, 4)
	ingestFrames(t, s, [][3]any{
		{c2s, 0, callAppendA}, {c2s, 1, cancel1}, {c2s, 2, callAppendB}, {s2c, 3, ok2},
	})
	if got := len(s.Duplicates("demo")); got != 1 {
		t.Fatalf("duplicates inside the window = %d, want 1", got)
	}
	ingestFrames(t, s, [][3]any{
		{c2s, 4, `{"jsonrpc":"2.0","id":7,"method":"ping"}`},
		{s2c, 5, `{"jsonrpc":"2.0","id":7,"result":{}}`},
		{c2s, 6, `{"jsonrpc":"2.0","id":8,"method":"ping"}`},
	})
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess := s.sessions["demo"]
	if len(sess.retries) != 0 || len(sess.openAttempts) != 0 {
		t.Fatalf("retries = %d, attempts = %d after their frames left the window, want both empty", len(sess.retries), len(sess.openAttempts))
	}
}

// TestTheStreamMarksTheFrameThatRepeatedACall. The verdict is the report's own,
// so a frame shows it exactly when check would report it, and it can change as
// later frames arrive. The repeat is marked once the earlier call turns out to
// be one nobody will answer, and not while two parallel calls both get answers.
func TestTheStreamMarksTheFrameThatRepeatedACall(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{
		{c2s, 0, `{"jsonrpc":"2.0","id":"l","method":"tools/list"}`}, {s2c, 1, listAppend},
		{c2s, 2, callAppendA}, {c2s, 3, cancel1}, {c2s, 4, callAppendB}, {s2c, 5, ok1}, {s2c, 6, ok2},
	})
	events := s.Timeline("demo")
	if got, want := events[4].Observation, "repeats the call on frame 3, which the client had cancelled, and the server answered both, so it ran twice"; got != want {
		t.Fatalf("observation = %q, want %q", got, want)
	}
	if events[2].Observation != "" {
		t.Fatalf("the first attempt was marked: %q", events[2].Observation)
	}

	parallel := New()
	ingestFrames(t, parallel, [][3]any{
		{c2s, 0, `{"jsonrpc":"2.0","id":"l","method":"tools/list"}`}, {s2c, 1, listAppend},
		{c2s, 2, callAppendA}, {c2s, 3, callAppendB}, {s2c, 4, ok1}, {s2c, 5, ok2},
	})
	if got := parallel.Timeline("demo")[3].Observation; got != "" {
		t.Fatalf("two calls answered in parallel were marked: %q", got)
	}
}

// TestARefusalThatArrivesAfterTheRetryStillCounts. The retry can go out before
// the first attempt's HTTP failure comes back, so the verdict reads the failure
// whenever it arrives. A refusal and a request mcpsnoop never delivered both say
// the first attempt never ran, and a 502 from something in front of the server
// says nothing.
func TestARefusalThatArrivesAfterTheRetryStillCounts(t *testing.T) {
	for _, tc := range []struct {
		status      int
		undelivered bool
		want        int
	}{{401, false, 0}, {502, true, 0}, {502, false, 1}} {
		s := New()
		frame := func(seq uint64, dir proxy.Direction, exchange uint64, status int, raw string) {
			e := proxy.Envelope{SessionID: "s", ServerLabel: "srv", Seq: seq, Direction: dir,
				Transport: proxy.TransportHTTP, Exchange: exchange, Status: status}
			if raw != "" {
				e.Raw = json.RawMessage(raw)
			} else {
				e.Undelivered = tc.undelivered
			}
			s.Ingest(e)
		}
		frame(1, c2s, 1, 0, callAppendA)
		frame(2, c2s, 2, 0, callAppendB)
		frame(3, s2c, 1, tc.status, "")
		frame(4, s2c, 2, 200, ok2)
		if got := len(s.Duplicates("s")); got != tc.want {
			t.Errorf("HTTP %d (undelivered %v) after the retry: duplicates = %d, want %d", tc.status, tc.undelivered, got, tc.want)
		}
	}
}

// TestAbsentArgumentsAreTheEmptyObject. A tool with no parameters is called with
// the arguments left out or with {}, and the two ask for the same thing.
func TestAbsentArgumentsAreTheEmptyObject(t *testing.T) {
	got := duplicatesOf(t,
		[2]any{c2s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"append"}}`},
		[2]any{c2s, cancel1},
		[2]any{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"append","arguments":{}}}`},
		[2]any{s2c, ok2},
	)
	if len(got) != 1 {
		t.Fatalf("duplicates = %+v, want the empty object to repeat the absent one", got)
	}
}

// TestAttemptsAreHeldOnlyWhileUnanswered. The attempts a later call could repeat
// are kept for as long as nobody has answered them, so a session making many
// distinct calls holds only the ones still in flight. An answer, a task handle
// and a refusal each let go of theirs, and a bounded store lets go of one whose
// frames it has dropped.
func TestAttemptsAreHeldOnlyWhileUnanswered(t *testing.T) {
	held := func(s *Store) int {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return len(s.sessions["demo"].openAttempts)
	}
	s := New()
	ingestFrames(t, s, [][3]any{
		{c2s, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","arguments":{"n":1}}}`},
		{s2c, 1, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`},
		{c2s, 2, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"a","arguments":{"n":2}}}`},
		{s2c, 3, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"task","taskId":"t","status":"working"}}`},
	})
	if got := held(s); got != 0 {
		t.Fatalf("held attempts = %d after an answer and a task handle, want 0", got)
	}
	s.Ingest(proxy.Envelope{SessionID: "demo", ServerLabel: "srv", Seq: 5, Direction: c2s, Transport: proxy.TransportHTTP,
		Exchange: 9, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"a","arguments":{"n":3}}}`)})
	s.Ingest(proxy.Envelope{SessionID: "demo", ServerLabel: "srv", Seq: 6, Direction: s2c, Transport: proxy.TransportHTTP,
		Exchange: 9, Status: 403})
	if got := held(s); got != 0 {
		t.Fatalf("held attempts = %d after a refusal, want 0", got)
	}

	bounded := NewBounded(64<<10, 3)
	ingestFrames(t, bounded, [][3]any{{c2s, 0, callAppendA}, {c2s, 1, cancel1}})
	if got := held(bounded); got != 1 {
		t.Fatalf("held attempts = %d for a call cancelled and never answered, want 1", got)
	}
	ingestFrames(t, bounded, [][3]any{
		{c2s, 2, `{"jsonrpc":"2.0","id":7,"method":"ping"}`},
		{s2c, 3, `{"jsonrpc":"2.0","id":7,"result":{}}`},
		{c2s, 4, `{"jsonrpc":"2.0","id":8,"method":"ping"}`},
	})
	if got := held(bounded); got != 0 {
		t.Fatalf("held attempts = %d once the frames left the window, want 0", got)
	}
}
