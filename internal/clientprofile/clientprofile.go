// Package clientprofile folds captured sessions into what each MCP client did on
// the wire: the revision it spoke, how it opened a conversation, what it declared
// it could do, and how it behaved when the server needed something back.
//
// The client compatibility tables published elsewhere are built from client
// documentation and source. This is built from traffic, so a client that
// documents a capability and never exercises it, or does something its
// documentation never mentions, reads as what it actually did.
package clientprofile

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// The _meta keys 2026-07-28 moved the handshake into. Every request carries the
// protocol version and the client capabilities, and a client SHOULD identify
// itself on each one.
const (
	protocolVersionKey = "io.modelcontextprotocol/protocolVersion"
	clientInfoKey      = "io.modelcontextprotocol/clientInfo"
)

// Profile is one client at one version, across every session it was seen in.
type Profile struct {
	// Client and Version come from clientInfo, sent in initialize under
	// 2025-11-25 and in each request's _meta under 2026-07-28. Client is empty for
	// a client that never named itself.
	Client     string   `json:"client"`
	Version    string   `json:"version,omitempty"`
	Sessions   int      `json:"sessions"`
	Transports []string `json:"transports"`

	// Revisions are the protocol versions the client put on the wire, from
	// initialize, from _meta and from the MCP-Protocol-Version header.
	Revisions []string `json:"revisions"`
	// Handshakes says how the sessions opened, one entry per distinct way.
	Handshakes []string `json:"handshakes"`

	// Requests counts every request the client sent and Methods breaks them down.
	// Identified counts the ones carrying clientInfo in _meta, and
	// InitializeIdentified the initialize requests carrying it.
	Requests             int            `json:"requests"`
	Methods              map[string]int `json:"methods"`
	Identified           int            `json:"identified_requests"`
	InitializeIdentified int            `json:"initialize_identified"`

	// Capabilities are what the client declared, with the elicitation modes and
	// the extension ids spelled out.
	Capabilities []string `json:"capabilities"`

	// TraceContext counts requests carrying a well-formed W3C traceparent in _meta
	// (SEP-414), and TraceMalformed the ones carrying a broken one.
	TraceContext   int `json:"trace_context"`
	TraceMalformed int `json:"trace_malformed"`
	// ProgressTokens counts the tool calls that offered a progressToken.
	ToolCalls      int `json:"tool_calls"`
	ProgressTokens int `json:"progress_tokens"`
	// Effects counts the tool calls by what the called tool declares, read-only,
	// additive or destructive, or no hints for a tool that declares none or was
	// never listed. The client chooses none of this, but the two counts below
	// only read sensibly beside it.
	Effects map[string]int `json:"tool_call_effects"`
	// ReadOnlyAtOnce is the most calls to read-only tools the client had in flight
	// at the same time, and OthersAtOnce the most calls in flight at any moment one
	// of them went to a tool not marked read-only, both among calls the server
	// answered. A client that runs only read-only tools side by side shows 1 for
	// the second.
	ReadOnlyAtOnce int `json:"read_only_at_once"`
	OthersAtOnce   int `json:"others_at_once"`
	// Repeated counts the tool calls the client sent again before it could know
	// what became of an identical earlier one, and RanTwice the ones the server
	// answered both times.
	Repeated int `json:"repeated_calls"`
	RanTwice int `json:"ran_twice"`

	// InputRequired counts the interim results the server answered this client
	// with under multi round-trip requests, Retries the retries that continued
	// one, and StateIssues the retries whose requestState was changed, dropped or
	// invented.
	InputRequired int `json:"input_required"`
	Retries       int `json:"retries"`
	StateIssues   int `json:"state_issues"`

	// Answers counts elicitation outcomes by action through either channel, with
	// unanswered for a question no answer ever came back for.
	Answers map[string]int `json:"elicitation_answers"`
	// ServerRequests counts how the client answered each request a server sent it
	// the pre-2026-07-28 way, keyed by method and outcome.
	ServerRequests map[string]int `json:"server_requests"`

	Cancellations int `json:"cancellations"`
	// CacheRefetches counts list and read requests repeated inside the ttlMs
	// window the server declared on the previous answer (SEP-2549).
	CacheRefetches int `json:"cache_refetches"`

	// Deprecated counts the client's frames that use a feature 2026-07-28
	// deprecated, by feature, and Warnings the protocol warnings raised on them.
	Deprecated map[string]int `json:"deprecated"`
	Warnings   map[string]int `json:"warnings"`
}

func newProfile() Profile {
	return Profile{
		Transports:     []string{},
		Revisions:      []string{},
		Handshakes:     []string{},
		Methods:        map[string]int{},
		Effects:        map[string]int{},
		Capabilities:   []string{},
		Answers:        map[string]int{},
		ServerRequests: map[string]int{},
		Deprecated:     map[string]int{},
		Warnings:       map[string]int{},
	}
}

// Fold reads one session. It reports false when the client sent no request,
// because a session without client traffic says nothing about a client.
func Fold(st *store.Store, header store.SessionHeader) (Profile, bool) {
	p := newProfile()
	caps, _ := st.Capabilities(header.ID)
	p.Client, p.Version = identity(caps.ClientInfo)
	p.Capabilities = declared(caps.Client)
	p.Sessions = 1
	if header.Transport != "" {
		p.Transports = append(p.Transports, header.Transport)
	}

	definitions := listedTools(st, header.ID)
	revisions := map[string]struct{}{}
	discoverAt, initializeAt, stateless := -1, -1, 0
	for i, ev := range st.Timeline(header.ID) {
		if ev.Dir == proxy.ClientToServer {
			if ev.Deprecated != "" {
				p.Deprecated[deprecatedFeature(ev.Deprecated)]++
			}
			if ev.Warning != "" {
				p.Warnings[ev.Warning]++
			}
			if ev.CacheStaleRefetch != "" {
				p.CacheRefetches++
			}
		}
		switch {
		case ev.Dir == proxy.ClientToServer && ev.Kind == store.EventRequest:
			p.Requests++
			p.Methods[ev.Method]++
			req := parseRequest(ev.Raw)
			if v := req.metaString(protocolVersionKey); v != "" {
				revisions[v] = struct{}{}
				stateless++
			}
			if ev.MCPProtocolVersion != "" {
				revisions[ev.MCPProtocolVersion] = struct{}{}
			}
			if req.hasMeta(clientInfoKey) {
				p.Identified++
			}
			switch ev.Method {
			case "initialize":
				if initializeAt < 0 {
					initializeAt = i
				}
				if req.ProtocolVersion != "" {
					revisions[req.ProtocolVersion] = struct{}{}
				}
				if len(req.ClientInfo) > 0 && string(req.ClientInfo) != "null" {
					p.InitializeIdentified++
				}
			case "server/discover":
				if discoverAt < 0 {
					discoverAt = i
				}
			case "tools/call":
				p.ToolCalls++
				if ev.Call != nil {
					definition, listed := definitions[ev.Call.ToolName]
					p.Effects[effectOf(definition, listed)]++
				}
				if req.hasMeta("progressToken") {
					p.ProgressTokens++
				}
			}
			if raw, ok := req.Meta["traceparent"]; ok {
				var tp string
				if json.Unmarshal(raw, &tp) == nil && validTraceparent(tp) {
					p.TraceContext++
				} else {
					p.TraceMalformed++
				}
			}
			if ev.MRTRRoot != "" {
				p.Retries++
				if ev.MRTRStateIssue != "" {
					p.StateIssues++
				}
			}
		case ev.Dir == proxy.ClientToServer && ev.Kind == store.EventNotification:
			if ev.Method == "notifications/cancelled" {
				p.Cancellations++
			}
		case ev.Dir == proxy.ServerToClient && ev.Kind == store.EventResponse:
			if resultType(ev.Raw) == "input_required" {
				p.InputRequired++
			}
		case ev.Dir == proxy.ServerToClient && ev.Kind == store.EventRequest && ev.Call != nil:
			// Before 2026-07-28 a server asked through a request of its own, and how
			// the client answered it is the whole of what that channel says about it.
			outcome, action := serverRequestOutcome(*ev.Call)
			p.ServerRequests[ev.Method+" "+outcome]++
			if ev.Method == "elicitation/create" && action != "" {
				p.Answers[action]++
			}
		}
	}
	for _, el := range st.Elicitations(header.ID) {
		action := el.Action
		if action == "" {
			action = "unanswered"
		}
		p.Answers[action]++
	}

	p.ReadOnlyAtOnce, p.OthersAtOnce = callsAtOnce(st, header.ID, definitions)
	for _, d := range st.Duplicates(header.ID) {
		p.Repeated++
		if d.RanTwice {
			p.RanTwice++
		}
	}

	p.Revisions = slices.Sorted(maps.Keys(revisions))
	p.Handshakes = append(p.Handshakes, handshake(discoverAt, initializeAt, stateless))
	return p, p.Requests > 0
}

// Merge groups per-session profiles by client and version, summing the counts
// and joining the sets. Two versions of one client stay apart, since a version
// is exactly what a reader of a behaviour table needs to tell apart.
func Merge(in []Profile) []Profile {
	byKey := map[string]*Profile{}
	var keys []string
	for _, p := range in {
		key := p.Client + "\x00" + p.Version
		acc, ok := byKey[key]
		if !ok {
			cp := newProfile()
			cp.Client, cp.Version = p.Client, p.Version
			acc = &cp
			byKey[key] = acc
			keys = append(keys, key)
		}
		acc.Sessions += p.Sessions
		acc.Transports = union(acc.Transports, p.Transports)
		acc.Revisions = union(acc.Revisions, p.Revisions)
		acc.Handshakes = union(acc.Handshakes, p.Handshakes)
		acc.Capabilities = union(acc.Capabilities, p.Capabilities)
		acc.Requests += p.Requests
		acc.Identified += p.Identified
		acc.InitializeIdentified += p.InitializeIdentified
		acc.TraceContext += p.TraceContext
		acc.TraceMalformed += p.TraceMalformed
		acc.ToolCalls += p.ToolCalls
		acc.ProgressTokens += p.ProgressTokens
		acc.ReadOnlyAtOnce = max(acc.ReadOnlyAtOnce, p.ReadOnlyAtOnce)
		acc.OthersAtOnce = max(acc.OthersAtOnce, p.OthersAtOnce)
		acc.Repeated += p.Repeated
		acc.RanTwice += p.RanTwice
		acc.InputRequired += p.InputRequired
		acc.Retries += p.Retries
		acc.StateIssues += p.StateIssues
		acc.Cancellations += p.Cancellations
		acc.CacheRefetches += p.CacheRefetches
		addCounts(acc.Methods, p.Methods)
		addCounts(acc.Effects, p.Effects)
		addCounts(acc.Answers, p.Answers)
		addCounts(acc.ServerRequests, p.ServerRequests)
		addCounts(acc.Deprecated, p.Deprecated)
		addCounts(acc.Warnings, p.Warnings)
	}
	out := make([]Profile, 0, len(keys))
	for _, key := range keys {
		out = append(out, *byKey[key])
	}
	slices.SortFunc(out, func(a, b Profile) int {
		// An unnamed client sorts last, since it is the column a reader can do least with.
		if (a.Client == "") != (b.Client == "") {
			if a.Client == "" {
				return 1
			}
			return -1
		}
		if c := cmp.Compare(strings.ToLower(a.Client), strings.ToLower(b.Client)); c != 0 {
			return c
		}
		return cmp.Compare(a.Version, b.Version)
	})
	return out
}

// Name is how a profile is headed in a table: the client and its version, or a
// plain statement that it never named itself.
func (p Profile) Name() string {
	switch {
	case p.Client == "":
		return "unidentified client"
	case p.Version == "":
		return p.Client
	default:
		return p.Client + " " + p.Version
	}
}

// Row is one line of a comparison, a fact and its value for each client.
type Row struct {
	Label string
	Cells []string
}

// Table renders profiles as rows of facts, one cell per profile, in the order
// a reader comparing clients asks the questions.
func Table(profiles []Profile) []Row {
	rows := []Row{
		{Label: "sessions"},
		{Label: "revision"},
		{Label: "opens with"},
		{Label: "names itself"},
		{Label: "declares"},
		{Label: "lists"},
		{Label: "re-fetches inside ttlMs"},
		{Label: "trace context"},
		{Label: "tool calls"},
		{Label: "progress tokens"},
		{Label: "multi round-trip"},
		{Label: "elicitation answers"},
		{Label: "server requests"},
		{Label: "cancellations"},
		{Label: "calls in flight"},
		{Label: "repeated calls"},
		{Label: "deprecated in use"},
		{Label: "protocol warnings"},
	}
	for _, p := range profiles {
		cells := []string{
			sessionsCell(p),
			orNone(strings.Join(p.Revisions, ", "), "none sent"),
			strings.Join(p.Handshakes, "; "),
			namesItselfCell(p),
			orNone(strings.Join(p.Capabilities, ", "), "nothing"),
			listsCell(p.Methods),
			countOrNone(p.CacheRefetches),
			traceCell(p),
			toolCallsCell(p),
			progressCell(p),
			mrtrCell(p),
			orNone(counted(p.Answers, " "), "none"),
			orNone(counted(p.ServerRequests, " ×"), "none"),
			countOrNone(p.Cancellations),
			inFlightCell(p),
			repeatedCell(p),
			orNone(counted(p.Deprecated, " ×"), "none"),
			warningsCell(p.Warnings),
		}
		for i := range rows {
			rows[i].Cells = append(rows[i].Cells, cells[i])
		}
	}
	return rows
}

func sessionsCell(p Profile) string {
	out := fmt.Sprintf("%d", p.Sessions)
	if len(p.Transports) > 0 {
		out += " (" + strings.Join(p.Transports, ", ") + ")"
	}
	return out
}

// namesItselfCell answers where the client said who it was. Under 2026-07-28 a
// client SHOULD do that on every request, so a share is the honest answer; under
// 2025-11-25 it did it once, in initialize.
func namesItselfCell(p Profile) string {
	var parts []string
	if p.InitializeIdentified > 0 {
		parts = append(parts, "in initialize")
	}
	if p.Identified > 0 {
		parts = append(parts, fmt.Sprintf("on %d of %d requests", p.Identified, p.Requests))
	}
	return orNone(strings.Join(parts, ", and "), "never")
}

// listsCell picks the discovery calls out of the methods, since how often a
// client re-reads a server's catalog is the question the caching hints exist to
// answer.
func listsCell(methods map[string]int) string {
	lists := map[string]int{}
	for _, m := range []string{"tools/list", "prompts/list", "resources/list", "resources/templates/list"} {
		if n := methods[m]; n > 0 {
			lists[m] = n
		}
	}
	return orNone(counted(lists, " ×"), "none")
}

func traceCell(p Profile) string {
	if p.Requests == 0 {
		return "no requests"
	}
	out := fmt.Sprintf("%d of %d requests", p.TraceContext, p.Requests)
	if p.TraceMalformed > 0 {
		out += fmt.Sprintf(", %d malformed", p.TraceMalformed)
	}
	return out
}

func progressCell(p Profile) string {
	if p.ToolCalls == 0 {
		return "no tool calls"
	}
	return fmt.Sprintf("%d of %d tool calls", p.ProgressTokens, p.ToolCalls)
}

func mrtrCell(p Profile) string {
	if p.InputRequired == 0 && p.Retries == 0 {
		return "not exercised"
	}
	out := fmt.Sprintf("%d input_required, %d retried", p.InputRequired, p.Retries)
	if p.StateIssues > 0 {
		out += fmt.Sprintf(", %d with a bad requestState", p.StateIssues)
	}
	return out
}

// warningsCell gives the total and the commonest one, because a reader comparing
// clients needs to know whether there is anything to look at and where to start,
// and the full list is in the json.
func warningsCell(warnings map[string]int) string {
	total := 0
	for _, n := range warnings {
		total += n
	}
	if total == 0 {
		return "none"
	}
	top := byCount(warnings)[0]
	return fmt.Sprintf("%d, most often %q", total, clip(top, 80))
}

func countOrNone(n int) string {
	if n == 0 {
		return "none"
	}
	return fmt.Sprintf("%d", n)
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

// counted renders a tally commonest first, joining each key to its count with
// sep, so "accept 2" and "tools/list ×3" share one helper.
func counted(m map[string]int, sep string) string {
	keys := byCount(m)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s%s%d", k, sep, m[k]))
	}
	return strings.Join(parts, ", ")
}

func byCount(m map[string]int) []string {
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(m[b], m[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return keys
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// handshake names how a session opened, from the first appearance of each way of
// opening and whether requests carried their version in _meta.
func handshake(discoverAt, initializeAt, stateless int) string {
	switch {
	case discoverAt >= 0 && initializeAt >= 0 && discoverAt < initializeAt:
		// The backward-compatibility probe the specification describes for stdio,
		// answered by a server that did not speak 2026-07-28.
		return "server/discover, then initialize"
	case discoverAt >= 0 && initializeAt >= 0:
		return "initialize, then server/discover"
	case initializeAt >= 0 && stateless > 0:
		return "initialize and stateless requests"
	case initializeAt >= 0:
		return "initialize"
	case discoverAt >= 0:
		return "server/discover, then stateless"
	case stateless > 0:
		return "stateless, without server/discover"
	default:
		return "no handshake seen"
	}
}

// identity reads clientInfo. A name is what the specification makes the
// programmatic identifier, so that is what a column is keyed on, and title is
// left for display surfaces that are not this one.
func identity(raw json.RawMessage) (name, version string) {
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &info) != nil {
		return "", ""
	}
	return strings.TrimSpace(info.Name), strings.TrimSpace(info.Version)
}

// declared lists the client's capabilities. Elicitation modes and extension ids
// are spelled out, since "supports elicitation" without the mode and "has
// extensions" without the ids are the two answers that settle nothing.
func declared(raw json.RawMessage) []string {
	var caps map[string]json.RawMessage
	if json.Unmarshal(raw, &caps) != nil {
		return []string{}
	}
	out := []string{}
	for _, name := range slices.Sorted(maps.Keys(caps)) {
		switch name {
		case "experimental":
			continue
		case "extensions":
			var ids map[string]json.RawMessage
			if json.Unmarshal(caps[name], &ids) == nil {
				for _, id := range slices.Sorted(maps.Keys(ids)) {
					out = append(out, "extension "+id)
				}
			}
		case "elicitation":
			var modes map[string]json.RawMessage
			if json.Unmarshal(caps[name], &modes) == nil && len(modes) > 0 {
				out = append(out, "elicitation ("+strings.Join(slices.Sorted(maps.Keys(modes)), ", ")+")")
			} else {
				out = append(out, "elicitation")
			}
		default:
			out = append(out, name)
		}
	}
	return out
}

// request is the part of a client request this package reads.
type request struct {
	Meta            map[string]json.RawMessage
	ProtocolVersion string
	ClientInfo      json.RawMessage
}

func parseRequest(raw json.RawMessage) request {
	var frame struct {
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return request{}
	}
	var params struct {
		Meta            map[string]json.RawMessage `json:"_meta"`
		ProtocolVersion string                     `json:"protocolVersion"`
		ClientInfo      json.RawMessage            `json:"clientInfo"`
	}
	if json.Unmarshal(frame.Params, &params) != nil {
		return request{}
	}
	return request{Meta: params.Meta, ProtocolVersion: params.ProtocolVersion, ClientInfo: params.ClientInfo}
}

func (r request) hasMeta(key string) bool {
	raw, ok := r.Meta[key]
	return ok && string(raw) != "null"
}

func (r request) metaString(key string) string {
	var s string
	if json.Unmarshal(r.Meta[key], &s) != nil {
		return ""
	}
	return s
}

func resultType(raw json.RawMessage) string {
	var frame struct {
		Result struct {
			ResultType string `json:"resultType"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return ""
	}
	return frame.Result.ResultType
}

// serverRequestOutcome says how the client answered a server's request, and for
// an elicitation which action it reported.
func serverRequestOutcome(c store.CallView) (outcome, action string) {
	switch {
	case c.Err != nil:
		return fmt.Sprintf("error %d", c.Err.Code), ""
	case c.Result != nil:
		var res struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(c.Result, &res)
		return "answered", res.Action
	default:
		return "unanswered", ""
	}
}

// traceparentRE is the W3C Trace Context traceparent for version 00, the only
// one defined. A later version may append fields, which the format allows a
// reader to ignore, so anything past the four fields is accepted.
var traceparentRE = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(-.*)?$`)

func validTraceparent(s string) bool {
	m := traceparentRE.FindStringSubmatch(s)
	if m == nil || m[1] == "ff" {
		return false
	}
	if m[1] == "00" && m[5] != "" {
		return false
	}
	// An all-zero trace id or parent id is invalid by the specification.
	return strings.Trim(m[2], "0") != "" && strings.Trim(m[3], "0") != ""
}

// deprecatedFeature shortens the store's deprecation notice to the feature it
// names, which is the part a table has room for.
func deprecatedFeature(notice string) string {
	if feature, _, ok := strings.Cut(notice, " is deprecated"); ok {
		return feature
	}
	return notice
}

func union(a, b []string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		set[s] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

func addCounts(dst, src map[string]int) {
	for k, n := range src {
		dst[k] += n
	}
}
