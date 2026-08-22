package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// elicitAsk is one round of an exchange: what the server asked, under which key.
type elicitAsk struct {
	key      string
	request  string // the whole inputRequests entry
	state    string
	answer   string // the whole inputResponses entry, empty when never answered
	askDelay time.Duration
}

// elicitSession ingests one operation and its rounds, returning the store and
// the session id. Each round is an InputRequiredResult answered by a retry under
// a new id, which is what MRTR requires.
func elicitSession(t *testing.T, tool string, rounds []elicitAsk) (*Store, string) {
	t.Helper()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := New()
	meta, err := json.Marshal(proxy.SessionMeta{Command: []string{"node", "s.js"}, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	seq := uint64(0)
	at := t0
	emit := func(dir proxy.Direction, raw string) {
		seq++
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: at,
			Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)})
	}
	seq++
	s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "demo", Seq: seq, TS: at,
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta})

	id := 1
	at = at.Add(time.Second)
	emit(proxy.ClientToServer, fmt.Sprintf(`{"jsonrpc":"2.0","id":"%d","method":"tools/call","params":{"name":%q}}`, id, tool))
	for _, round := range rounds {
		at = at.Add(time.Second)
		state := ""
		if round.state != "" {
			state = fmt.Sprintf(`,"requestState":%q`, round.state)
		}
		emit(proxy.ServerToClient, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":"%d","result":{"resultType":"input_required","inputRequests":{%s}%s}}`,
			id, round.request, state))
		if round.answer == "" {
			return s, "s1" // nobody ever retried
		}
		at = at.Add(round.askDelay)
		id++
		echo := ""
		if round.state != "" {
			echo = fmt.Sprintf(`,"requestState":%q`, round.state)
		}
		emit(proxy.ClientToServer, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":"%d","method":"tools/call","params":{"name":%q,"inputResponses":{%s}%s}}`,
			id, tool, round.answer, echo))
	}
	at = at.Add(time.Second)
	emit(proxy.ServerToClient, fmt.Sprintf(`{"jsonrpc":"2.0","id":"%d","result":{"content":[]}}`, id))
	return s, "s1"
}

func elicitReq(key, params string) string {
	return fmt.Sprintf(`%q:{"method":"elicitation/create","params":%s}`, key, params)
}

// TestElicitationLedgerPairsQuestionWithAnswer is the whole feature. Under MRTR
// the question and the answer are fields inside two different requests, and only
// the retry link ties them together.
func TestElicitationLedgerPairsQuestionWithAnswer(t *testing.T) {
	s, id := elicitSession(t, "create_account", []elicitAsk{{
		key:      "profile",
		state:    "st-1",
		request:  elicitReq("profile", `{"mode":"form","message":"Please provide your contact information","requestedSchema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"number"}},"required":["name"]}}`),
		answer:   `"profile":{"action":"accept","content":{"name":"Monalisa","age":30}}`,
		askDelay: 9 * time.Second,
	}})

	rows := s.Elicitations(id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Key != "profile" || row.Mode != ElicitModeForm {
		t.Fatalf("key %q mode %q", row.Key, row.Mode)
	}
	if row.Action != "accept" {
		t.Fatalf("action = %q, want accept", row.Action)
	}
	if row.Elapsed != 9*time.Second {
		t.Fatalf("elapsed = %v, want 9s of human time", row.Elapsed)
	}
	if row.ToolName != "create_account" || row.Method != "tools/call" {
		t.Fatalf("the row does not name the operation it interrupted: %+v", row)
	}
	want := []ElicitFieldView{{Name: "age", Type: "number"}, {Name: "name", Type: "string"}}
	if len(row.Fields) != 2 || row.Fields[0] != want[0] || row.Fields[1] != want[1] {
		t.Fatalf("fields = %+v, want %+v", row.Fields, want)
	}
	if row.Message != "Please provide your contact information" {
		t.Fatalf("message = %q", row.Message)
	}
}

// TestElicitationNeverCarriesASubmittedValue is the privacy boundary the ledger
// is built on. The values are in the capture for anyone who needs them, and a
// summary surface built to be pasted around must not repeat them.
func TestElicitationNeverCarriesASubmittedValue(t *testing.T) {
	const secret = "hunter2-do-not-repeat-me"
	s, id := elicitSession(t, "t", []elicitAsk{{
		key:     "k",
		state:   "st",
		request: elicitReq("k", `{"mode":"form","message":"who are you","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}`),
		answer:  fmt.Sprintf(`"k":{"action":"accept","content":{"name":%q}}`, secret),
	}})
	rendered, err := json.Marshal(s.Elicitations(id))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), secret) {
		t.Fatalf("a submitted value reached the ledger: %s", rendered)
	}
}

// TestElicitationDefaultsToFormMode covers the rule the specification states
// outright: clients "MUST treat requests without a mode field as form mode", so
// an absent field is a value rather than a gap.
func TestElicitationDefaultsToFormMode(t *testing.T) {
	s, id := elicitSession(t, "t", []elicitAsk{{
		key:     "k",
		state:   "st",
		request: elicitReq("k", `{"message":"no mode named","requestedSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}`),
		answer:  `"k":{"action":"accept"}`,
	}})
	rows := s.Elicitations(id)
	if len(rows) != 1 || rows[0].Mode != ElicitModeForm {
		t.Fatalf("mode = %q, want form for a request that named none", rows[0].Mode)
	}
}

// TestElicitationURLModeCarriesTheAddressAndHost covers what the specification
// makes a client show. The full url "for examination before consent", and the
// host it "SHOULD highlight" against subdomain spoofing.
func TestElicitationURLModeCarriesTheAddressAndHost(t *testing.T) {
	const target = "https://mcp.example.com/ui/set_api_key?flow=abc123"
	s, id := elicitSession(t, "sync", []elicitAsk{{
		key:     "auth",
		state:   "st",
		request: elicitReq("auth", fmt.Sprintf(`{"mode":"url","url":%q,"message":"Please provide your API key to continue."}`, target)),
		answer:  `"auth":{"action":"accept"}`,
	}})
	rows := s.Elicitations(id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Mode != ElicitModeURL || row.URL != target {
		t.Fatalf("mode %q url %q, want url mode carrying the address whole", row.Mode, row.URL)
	}
	if row.Host != "mcp.example.com" {
		t.Fatalf("host = %q, want the domain named on its own", row.Host)
	}
	if len(row.Fields) != 0 {
		t.Fatalf("a url mode question claimed a form field list: %+v", row.Fields)
	}
}

// TestElicitationRecordsEveryAction covers the three the specification names and
// one it does not. An unrecognised action is either a newer revision or a client
// that is wrong, and both are worth seeing rather than dropping.
func TestElicitationRecordsEveryAction(t *testing.T) {
	for _, action := range []string{"accept", "decline", "cancel", "something-else"} {
		t.Run(action, func(t *testing.T) {
			s, id := elicitSession(t, "t", []elicitAsk{{
				key:     "k",
				state:   "st",
				request: elicitReq("k", `{"message":"m","requestedSchema":{"type":"object","properties":{}}}`),
				answer:  fmt.Sprintf(`"k":{"action":%q}`, action),
			}})
			rows := s.Elicitations(id)
			if len(rows) != 1 || rows[0].Action != action {
				t.Fatalf("action = %q, want %q recorded verbatim", rows[0].Action, action)
			}
			if rows[0].Pending() {
				t.Fatal("an answered question reads as pending")
			}
		})
	}
}

// TestElicitationWithNoRetryIsPending covers the outcome MRTR makes ordinary:
// the specification tells servers they must not assume a client will retry.
func TestElicitationWithNoRetryIsPending(t *testing.T) {
	s, id := elicitSession(t, "abandoned", []elicitAsk{{
		key:     "q",
		state:   "st",
		request: elicitReq("q", `{"message":"Still there?","requestedSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}`),
	}})
	rows := s.Elicitations(id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the unanswered question still recorded", len(rows))
	}
	row := rows[0]
	if !row.Pending() || row.Action != "" {
		t.Fatalf("action = %q, want none", row.Action)
	}
	if !row.Answered.IsZero() || row.Elapsed != 0 {
		t.Fatalf("an unanswered question claims a time: answered %v elapsed %v", row.Answered, row.Elapsed)
	}
	if row.Message != "Still there?" {
		t.Fatalf("the question itself was lost: %+v", row)
	}
}

// TestElicitationIgnoresItsSiblings covers a result carrying more than one input
// request. Sampling and roots travel through the same map and are not questions
// put to a user, so they are not rows.
func TestElicitationIgnoresItsSiblings(t *testing.T) {
	request := strings.Join([]string{
		elicitReq("auth", `{"mode":"url","url":"https://h/ui","message":"m"}`),
		`"summarise":{"method":"sampling/createMessage","params":{"messages":[]}}`,
		`"where":{"method":"roots/list","params":{}}`,
	}, ",")
	s, id := elicitSession(t, "t", []elicitAsk{{
		key: "auth", state: "st", request: request,
		answer: `"auth":{"action":"accept"},"summarise":{"role":"assistant"},"where":{"roots":[]}`,
	}})
	rows := s.Elicitations(id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want only the elicitation: %+v", len(rows), rows)
	}
	if rows[0].Key != "auth" {
		t.Fatalf("the wrong entry became a row: %+v", rows[0])
	}
}

// TestElicitationChainKeepsEveryRoundOnOneCall covers an exchange whose retry is
// itself answered with another InputRequiredResult. Each round is its own
// question and all of them belong to the operation that started it.
func TestElicitationChainKeepsEveryRoundOnOneCall(t *testing.T) {
	s, id := elicitSession(t, "setup", []elicitAsk{
		{key: "one", state: "st-1", request: elicitReq("one", `{"message":"first","requestedSchema":{"type":"object","properties":{"a":{"type":"string"}}}}`),
			answer: `"one":{"action":"accept","content":{"a":"x"}}`, askDelay: 2 * time.Second},
		{key: "two", state: "st-2", request: elicitReq("two", `{"mode":"url","url":"https://h/second","message":"second"}`),
			answer: `"two":{"action":"decline"}`, askDelay: 5 * time.Second},
	})
	rows := s.Elicitations(id)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per round: %+v", len(rows), rows)
	}
	if rows[0].CallID != rows[1].CallID {
		t.Fatalf("the two rounds point at different calls: %q and %q", rows[0].CallID, rows[1].CallID)
	}
	if rows[0].Action != "accept" || rows[1].Action != "decline" {
		t.Fatalf("the rounds took each other's answers: %q then %q", rows[0].Action, rows[1].Action)
	}
	if rows[0].Elapsed != 2*time.Second || rows[1].Elapsed != 5*time.Second {
		t.Fatalf("elapsed = %v and %v, want 2s then 5s", rows[0].Elapsed, rows[1].Elapsed)
	}
	// One operation, still one call, which is what correlating MRTR is for.
	if calls := s.Calls(id); len(calls) != 1 {
		t.Fatalf("calls = %d, want the chain to remain one operation", len(calls))
	}
}

// TestElicitationRedactedSchemaHasNoType covers what a capture taken with
// --redact-secrets looks like. The key rules reach requestedSchema, so a matching
// property's whole subschema becomes the placeholder string. The name survives,
// which is the half that carries the meaning, and reporting the placeholder as a
// type would present mcpsnoop's own scrubbing as the server's declaration.
func TestElicitationRedactedSchemaHasNoType(t *testing.T) {
	s, id := elicitSession(t, "t", []elicitAsk{{
		key:     "creds",
		state:   "st",
		request: elicitReq("creds", `{"message":"m","requestedSchema":{"type":"object","properties":{"password":"[REDACTED]","user":{"type":"string"}},"required":["password"]}}`),
		answer:  `"creds":{"action":"decline"}`,
	}})
	rows := s.Elicitations(id)
	if len(rows) != 1 || len(rows[0].Fields) != 2 {
		t.Fatalf("fields = %+v, want both properties listed", rows[0].Fields)
	}
	byName := map[string]string{}
	for _, f := range rows[0].Fields {
		byName[f.Name] = f.Type
	}
	if byName["password"] != "" {
		t.Fatalf("the redacted property reports type %q, want none; a placeholder is not a type", byName["password"])
	}
	if byName["user"] != "string" {
		t.Fatalf("an untouched property lost its type: %q", byName["user"])
	}
}

// TestElicitationDoesNotDisturbTheRetryLink is the constraint the parsing change
// had to respect. matchRetry keys on the answered key set and the echoed
// requestState, and reading more out of the same bytes must not change either.
func TestElicitationDoesNotDisturbTheRetryLink(t *testing.T) {
	// Stateless, so the link rests entirely on the key set, which is the fragile
	// half.
	s, id := elicitSession(t, "t", []elicitAsk{{
		key:     "k",
		request: elicitReq("k", `{"message":"m","requestedSchema":{"type":"object","properties":{"a":{"type":"string"}}}}`),
		answer:  `"k":{"action":"accept"}`,
	}})
	calls := s.Calls(id)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want the retry linked to the operation it continues", len(calls))
	}
	if rows := s.Elicitations(id); len(rows) != 1 || rows[0].Action != "accept" {
		t.Fatalf("the answer did not reach the ledger: %+v", rows)
	}
}

// TestASessionWithoutElicitationHasNoLedger keeps an ordinary capture unchanged.
func TestASessionWithoutElicitationHasNoLedger(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := New()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/call", `{"name":"t"}`))
	s.Ingest(resp(2, t0.Add(time.Millisecond), proxy.ServerToClient, "1", `"result":{"content":[]}`))
	if rows := s.Elicitations("s1"); len(rows) != 0 {
		t.Fatalf("a session with no elicitation produced %d rows", len(rows))
	}
	if rows := s.Elicitations("nosuch"); rows != nil {
		t.Fatalf("an unknown session produced %+v", rows)
	}
}

// TestAChainReusingOneKeyKeepsEachRoundsOwnAnswer is why an answered round is
// not answered twice. A server is free to ask under the same inputRequests key
// on every round, and the retry that answers the second round carries that key
// too, so without a guard it would reach back and rewrite the first round's
// answer with the second one's.
func TestAChainReusingOneKeyKeepsEachRoundsOwnAnswer(t *testing.T) {
	s, id := elicitSession(t, "setup", []elicitAsk{
		{key: "k", state: "st-1", request: elicitReq("k", `{"message":"first","requestedSchema":{"type":"object","properties":{"a":{"type":"string"}}}}`),
			answer: `"k":{"action":"accept"}`, askDelay: 2 * time.Second},
		{key: "k", state: "st-2", request: elicitReq("k", `{"message":"second","requestedSchema":{"type":"object","properties":{"b":{"type":"string"}}}}`),
			answer: `"k":{"action":"cancel"}`, askDelay: 7 * time.Second},
	})
	rows := s.Elicitations(id)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per round: %+v", len(rows), rows)
	}
	if rows[0].Message != "first" || rows[1].Message != "second" {
		t.Fatalf("the rounds are out of order: %q then %q", rows[0].Message, rows[1].Message)
	}
	if rows[0].Action != "accept" {
		t.Fatalf("the first round's answer became %q; a later round under the same key rewrote it", rows[0].Action)
	}
	if rows[1].Action != "cancel" {
		t.Fatalf("the second round's answer = %q, want cancel", rows[1].Action)
	}
	if rows[0].Elapsed != 2*time.Second || rows[1].Elapsed != 7*time.Second {
		t.Fatalf("elapsed = %v and %v, want 2s then 7s", rows[0].Elapsed, rows[1].Elapsed)
	}
}

// TestALaterRoundDoesNotAnswerAnEarlierOne is the direction the chain test could
// not fail on. A retry is matched on the requestState and key set of the round it
// was issued from, so that is the round it answers.
//
// The case is the specification's own recovery path: MRTR says that when a client
// omits some of what was asked, the server "SHOULD respond with a new
// InputRequiredResult requesting the missing information again". So an earlier
// round holding one unanswered key beside an answered one is ordinary traffic,
// and reaching back to it reported a decline the user never gave.
func TestALaterRoundDoesNotAnswerAnEarlierOne(t *testing.T) {
	s, id := elicitSession(t, "checkout", []elicitAsk{
		{
			key:   "card",
			state: "A",
			request: strings.Join([]string{
				elicitReq("card", `{"mode":"url","url":"https://pay.example.com/x","message":"card"}`),
				elicitReq("who", `{"message":"who","requestedSchema":{"type":"object","properties":{"n":{"type":"string"}}}}`),
			}, ","),
			// The client answers only one of the two keys.
			answer:   `"who":{"action":"accept"}`,
			askDelay: 3 * time.Second,
		},
		{
			key:      "card",
			state:    "B",
			request:  elicitReq("card", `{"mode":"url","url":"https://pay.example.com/x","message":"card, again"}`),
			answer:   `"card":{"action":"cancel"}`,
			askDelay: 100 * time.Second,
		},
	})

	rows := s.Elicitations(id)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want two from the first round and one from the second: %+v", len(rows), rows)
	}
	byRound := map[string]ElicitationView{}
	for _, r := range rows {
		byRound[r.Message] = r
	}
	first := byRound["card"]
	if !first.Pending() {
		t.Fatalf("the first round's card was answered %q after %v; the client never answered it",
			first.Action, first.Elapsed)
	}
	if !first.Answered.IsZero() {
		t.Fatalf("an unanswered question carries a time: %v", first.Answered)
	}
	if got := byRound["who"]; got.Action != "accept" || got.Elapsed != 3*time.Second {
		t.Fatalf("the answered key of the first round = %q after %v, want accept after 3s", got.Action, got.Elapsed)
	}
	if got := byRound["card, again"]; got.Action != "cancel" || got.Elapsed != 100*time.Second {
		t.Fatalf("the second round = %q after %v, want cancel after 100s", got.Action, got.Elapsed)
	}
}

// TestAMalformedQuestionStillMakesARow covers what redaction and a wrong server
// both produce. Decoding through one typed struct lost the whole row on the
// first field whose JSON type did not match, so a capture taken with
// --redact-secrets could report no elicitation at all.
func TestAMalformedQuestionStillMakesARow(t *testing.T) {
	for _, tc := range []struct {
		name, params string
		wantMode     string
		wantFields   int
	}{
		{"redacted schema", `{"message":"password please","requestedSchema":"[REDACTED]"}`, ElicitModeForm, 0},
		{"redacted properties", `{"message":"m","requestedSchema":{"type":"object","properties":"[REDACTED]"}}`, ElicitModeForm, 0},
		{"boolean schema", `{"message":"m","requestedSchema":true}`, ElicitModeForm, 0},
		{"url is a number", `{"mode":"url","url":12345,"message":"m"}`, ElicitModeURL, 0},
		{"mode is a number", `{"mode":5,"message":"m","requestedSchema":{"type":"object","properties":{"a":{"type":"string"}}}}`, ElicitModeForm, 1},
		{"message is an object", `{"message":{"a":1},"requestedSchema":{"type":"object","properties":{"a":{"type":"string"}}}}`, ElicitModeForm, 1},
		{"params is an array", `[1,2,3]`, ElicitModeForm, 0},
		{"type is an array", `{"message":"m","requestedSchema":{"type":"object","properties":{"a":{"type":["string","null"]}}}}`, ElicitModeForm, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id := elicitSession(t, "t", []elicitAsk{{
				key: "k", state: "st", request: elicitReq("k", tc.params),
				answer: `"k":{"action":"decline"}`,
			}})
			rows := s.Elicitations(id)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want the question still recorded", len(rows))
			}
			if rows[0].Mode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", rows[0].Mode, tc.wantMode)
			}
			if len(rows[0].Fields) != tc.wantFields {
				t.Fatalf("fields = %+v, want %d", rows[0].Fields, tc.wantFields)
			}
			if rows[0].Action != "decline" {
				t.Fatalf("the answer was lost with the malformed half: %q", rows[0].Action)
			}
		})
	}
}

// TestAResentResultIsNotASecondRound covers a server repeating its
// InputRequiredResult on one id. Parking deliberately leaves the call Pending, so
// the duplicate guard cannot catch it, and every question would be recorded twice
// and one answer reported as two.
func TestAResentResultIsNotASecondRound(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := New()
	seq := uint64(0)
	emit := func(dir proxy.Direction, off time.Duration, raw string) {
		seq++
		s.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "d", Seq: seq, TS: t0.Add(off),
			Direction: dir, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw)})
	}
	const result = `{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"st","inputRequests":{"k":{"method":"elicitation/create","params":{"message":"m","requestedSchema":{"type":"object","properties":{"a":{"type":"string"}}}}}}}}`
	emit(proxy.ClientToServer, time.Second, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"t"}}`)
	emit(proxy.ServerToClient, 2*time.Second, result)
	emit(proxy.ServerToClient, 3*time.Second, result) // the same answer again
	emit(proxy.ClientToServer, 9*time.Second, `{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"t","inputResponses":{"k":{"action":"accept"}},"requestState":"st"}}`)

	rows := s.Elicitations("s1")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want one question however many times the result was sent: %+v", len(rows), rows)
	}
	if rows[0].Action != "accept" {
		t.Fatalf("action = %q", rows[0].Action)
	}
}

// TestARecordedQuestionIsBounded keeps one question's cost predictable. These
// strings are held for the life of the session, outside the frame budget that
// releases bodies, so a server is not free to make one arbitrarily expensive.
func TestARecordedQuestionIsBounded(t *testing.T) {
	huge := strings.Repeat("m", 200_000)
	properties := make([]string, 0, 1000)
	for i := range 1000 {
		properties = append(properties, fmt.Sprintf(`"f%04d":{"type":"string"}`, i))
	}
	s, id := elicitSession(t, "t", []elicitAsk{{
		key: "k", state: "st",
		request: elicitReq("k", fmt.Sprintf(`{"message":%q,"requestedSchema":{"type":"object","properties":{%s}}}`,
			huge, strings.Join(properties, ","))),
		answer: `"k":{"action":"accept"}`,
	}})
	rows := s.Elicitations(id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if n := len(rows[0].Message); n > maxElicitMessage+32 {
		t.Fatalf("message is %d bytes, want it bounded near %d", n, maxElicitMessage)
	}
	if !strings.HasSuffix(rows[0].Message, "(truncated)") {
		t.Fatal("a truncated message does not say it was truncated")
	}
	if n := len(rows[0].Fields); n != maxElicitFields {
		t.Fatalf("fields = %d, want the cap of %d", n, maxElicitFields)
	}
	// Sorted before the cap, so which survive is the same on every run.
	if rows[0].Fields[0].Name != "f0000" {
		t.Fatalf("the kept fields are not the first by name: %q", rows[0].Fields[0].Name)
	}
}
