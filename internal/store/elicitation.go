package store

import (
	"cmp"
	"encoding/json"
	"net/url"
	"slices"
	"time"
)

// elicitMethod is the only input request this ledger records. Sampling and roots
// travel through the same map and are deliberately not rows here: elicitation is
// the one path where a human types data into a server, which is what makes a
// record of what was asked worth keeping.
const elicitMethod = "elicitation/create"

// What one recorded question is allowed to cost. These are held on the call for
// the life of the session, outside the frame budget that releases bodies, so a
// server answering every call with an input request would otherwise grow the
// store by whatever it felt like writing.
//
// The limits are generous enough that no real question meets them. A message is
// prose for a person to read, a url has practical limits everywhere else it is
// handled, and the specification restricts a form schema to "flat objects with
// primitive properties only", which is not a thousand of them.
const (
	maxElicitMessage = 1 << 10
	maxElicitURL     = 2 << 10
	maxElicitFields  = 256
)

// clip bounds one recorded string and says so, rather than trailing off, so a
// reader can tell a truncated message from a server that wrote exactly that.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "… (truncated)"
}

// Elicitation modes. A request that names none is form mode, because the
// specification says clients "MUST treat requests without a mode field as form
// mode", so an absent field is a value rather than a gap.
const (
	ElicitModeForm = "form"
	ElicitModeURL  = "url"
)

// elicitation is one question a server put to the user through the client, and
// the answer that came back on the retry, if one ever did.
//
// It holds the shape of the question and never its answer. The submitted values
// live in the log for anyone who needs them, and keeping them out of here means
// no new field can leak what a redaction rule was set to hide, which matters
// most in url mode, where the specification puts credentials on purpose.
type elicitation struct {
	// key is the inputRequests entry this question was filed under, which is also
	// what pairs it with its inputResponses answer. One result may carry several.
	key     string
	mode    string
	message string
	// fields are the requestedSchema property names and declared types of a form
	// mode question. Names and types, never a value or a default.
	fields []elicitField
	// target is the url of a url mode question, kept whole because the
	// specification makes the client "show the full URL to the user for
	// examination before consent", and host is what it "SHOULD highlight" to
	// mitigate subdomain spoofing.
	target string
	host   string
	asked  time.Time
	// action is empty until a retry answers this key, and answered is when it did.
	action   string
	answered time.Time
}

// elicitField is one property of a form mode requestedSchema.
type elicitField struct {
	name string
	// typ is the declared type, or empty when the schema does not declare one.
	// Redaction can replace a whole subschema with its placeholder string, and a
	// placeholder is not a type, so this stays empty rather than reporting
	// mcpsnoop's own scrubbing as something the server said.
	typ string
}

// parseElicitations reads the elicitation questions out of one inputRequests
// map. Entries for any other method are skipped, so a result carrying a
// sampling/createMessage beside an elicitation/create produces one row.
//
// Every field is decoded on its own rather than through one typed struct. A
// struct fails whole on the first field whose JSON type does not match, and the
// fields here are not mcpsnoop's to shape: redaction replaces a matched value
// with a string, so a requestedSchema under a matching key becomes one, and a
// server is free to send something malformed. Losing the row for that means a
// capture taken with --redact-secrets can report no elicitation at all, which is
// a far worse answer than one field reading as unknown.
func parseElicitations(requests map[string]json.RawMessage, at time.Time) []*elicitation {
	out := make([]*elicitation, 0, len(requests))
	for key, raw := range requests {
		var entry struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Method != elicitMethod {
			continue
		}
		var params struct {
			Mode            json.RawMessage `json:"mode"`
			Message         json.RawMessage `json:"message"`
			URL             json.RawMessage `json:"url"`
			RequestedSchema json.RawMessage `json:"requestedSchema"`
		}
		// Params that are not an object at all still leave a row. The question was
		// asked, and a reader is better served by knowing that than by a gap.
		_ = json.Unmarshal(entry.Params, &params)

		e := &elicitation{
			key:     key,
			mode:    jsonString(params.Mode),
			message: clip(jsonString(params.Message), maxElicitMessage),
			asked:   at,
		}
		if e.mode == "" {
			e.mode = ElicitModeForm
		}
		if e.mode == ElicitModeURL {
			e.target = clip(jsonString(params.URL), maxElicitURL)
			e.host = urlHost(e.target)
		} else {
			e.fields = schemaFields(params.RequestedSchema)
		}
		out = append(out, e)
	}
	// Sorted, because a map iterates in no order and a ledger is a document people
	// diff.
	slices.SortFunc(out, func(a, b *elicitation) int { return cmp.Compare(a.key, b.key) })
	return out
}

// jsonString reads a value that should be a string, and answers with nothing
// when it is not one. A number where a url belongs is a server that is wrong,
// not a reason to lose the question it asked.
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// schemaFields lists a requestedSchema's property names and declared types.
//
// A schema that is not an object yields no fields rather than losing the row it
// belongs to. That is what a redacted requestedSchema looks like: --redact-secrets
// replaces the whole value under a matching key, so the schema arrives as the
// placeholder string.
func schemaFields(raw json.RawMessage) []elicitField {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) == 0 {
		return nil
	}
	fields := make([]elicitField, 0, min(len(schema.Properties), maxElicitFields))
	for name, sub := range schema.Properties {
		field := elicitField{name: name}
		var declared struct {
			Type json.RawMessage `json:"type"`
		}
		// A subschema that is not an object leaves the type empty, and so does a type
		// that is not a string. Both are the redacted case: the placeholder replaces
		// whatever it matched. The name survives, which is the half that carries the
		// meaning, and reporting the placeholder as a type would present mcpsnoop's
		// own scrubbing as the server's declaration.
		if json.Unmarshal(sub, &declared) == nil {
			field.typ = jsonString(declared.Type)
		}
		fields = append(fields, field)
	}
	slices.SortFunc(fields, func(a, b elicitField) int { return cmp.Compare(a.name, b.name) })
	if len(fields) > maxElicitFields {
		// Sorted first, so which ones survive is the same on every run rather than
		// whichever the map happened to yield.
		fields = fields[:maxElicitFields]
	}
	return fields
}

// urlHost is the host of a url mode target, empty when it does not parse. The
// specification tells a client to highlight it, so a reader gets it named rather
// than having to find it inside the whole address.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// answer records what a retry said about one key. An action mcpsnoop does not
// recognise is kept verbatim: the three the specification names are accept,
// decline and cancel, and a fourth is either a newer revision or a client that
// is wrong, and both are worth seeing rather than dropping.
func (e *elicitation) answer(action string, at time.Time) {
	if e.action != "" || action == "" {
		return
	}
	e.action, e.answered = action, at
}

// elicitActions reads the action of each inputResponses entry.
func elicitActions(responses map[string]json.RawMessage) map[string]string {
	if len(responses) == 0 {
		return nil
	}
	out := make(map[string]string, len(responses))
	for key, raw := range responses {
		var r struct {
			Action string `json:"action"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Action != "" {
			out[key] = r.Action
		}
	}
	return out
}

// ElicitationView is one row of the ledger: a question a server put to the user,
// and the answer that came back.
//
// It carries the shape of the question and never its content. A submitted value
// is in the log for whoever needs it, and keeping it out of here is what makes
// the ledger safe to export and pass around whatever redaction was set to.
type ElicitationView struct {
	// CallSeq, CallID, Method and ToolName name the operation the question
	// interrupted, so a row can be traced back to the call it belongs to. Every
	// round of one operation names the same one. CallSeq is the sequence of the
	// request frame that opened it, which is what the exporter indexes calls by,
	// since an id alone is only unique while a call is in flight.
	CallSeq  uint64
	CallID   string
	Method   string
	ToolName string
	// Key is the inputRequests entry the question was filed under, and what pairs
	// it with its answer.
	Key string
	// Mode is form or url. A request naming neither is form, which is what the
	// specification tells a client to assume.
	Mode    string
	Message string
	// Fields are the form mode requestedSchema properties, names and declared
	// types. Nil in url mode, where there is no form to describe.
	Fields []ElicitFieldView
	// URL and Host are the url mode target and the host to highlight. Empty in
	// form mode.
	URL  string
	Host string
	// Action is what the user did, one of accept, decline or cancel, or whatever
	// else a client sent. Empty means no retry ever answered this question.
	Action string
	// Asked is when the server put the question, Answered when the retry carrying
	// the answer arrived, and Elapsed the gap, which is roughly how long the human
	// took. Answered is zero and Elapsed is zero while the question is unanswered.
	Asked    time.Time
	Answered time.Time
	Elapsed  time.Duration
}

// Pending reports a question no retry ever answered.
func (e ElicitationView) Pending() bool { return e.Action == "" }

// ElicitFieldView is one requestedSchema property. Type is empty when the schema
// declared none, which includes a subschema redaction replaced with its
// placeholder, since a placeholder is not a type the server chose.
type ElicitFieldView struct {
	Name string
	Type string
}

func (e *elicitation) view(c *call) ElicitationView {
	out := ElicitationView{
		CallSeq: c.requestSeq, CallID: c.id, Method: c.method, ToolName: c.toolName,
		Key: e.key, Mode: e.mode, Message: e.message,
		URL: e.target, Host: e.host,
		Action: e.action, Asked: e.asked, Answered: e.answered,
	}
	if len(e.fields) > 0 {
		out.Fields = make([]ElicitFieldView, 0, len(e.fields))
		for _, f := range e.fields {
			out.Fields = append(out.Fields, ElicitFieldView{Name: f.name, Type: f.typ})
		}
	}
	if !e.answered.IsZero() {
		out.Elapsed = e.answered.Sub(e.asked)
	}
	return out
}

// sameElicitRound reports whether a freshly parsed round is the one already
// recorded, which is how a server resending its InputRequiredResult on one id is
// told apart from a genuine next round.
//
// Compared on what the question is rather than on when it arrived, since a
// resend carries the same question at a later timestamp.
func sameElicitRound(recorded, parsed []*elicitation) bool {
	if len(recorded) == 0 || len(recorded) != len(parsed) {
		return false
	}
	for i, e := range recorded {
		o := parsed[i]
		if e.key != o.key || e.mode != o.mode || e.message != o.message || e.target != o.target {
			return false
		}
		if len(e.fields) != len(o.fields) {
			return false
		}
		for j := range e.fields {
			if e.fields[j] != o.fields[j] {
				return false
			}
		}
	}
	return true
}
