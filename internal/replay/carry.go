package replay

import (
	"encoding/json"
	"strings"
	"unicode"
)

// carried is what the server handed out in the recording and hands out in its
// place now. A client sends some of what a server gave it straight back: the
// requestState of a multi round-trip result, a page's nextCursor, a task's id,
// the id of a record a tool just created. The recorded frames hold the values
// the server gave then, so a replay sending them as they stand asks a fresh
// server about a cursor, a state or a record it never issued.
//
// Every value is learned by comparing the recorded answer to a request with
// the replayed answer to the same request, place by place. Nothing is guessed
// from the frames alone, so a value only moves when the server itself moved
// it.
type carried struct {
	// values maps a whole string the server gave to the one it gives now.
	values map[string]string
	// tokens maps an identifier inside a longer string, the id in "Created
	// note n-12.", since a client reads those out of text and sends them on.
	tokens map[string]string
	// numbers maps a number under an id-like key, by its spelling.
	numbers map[string]string
}

func newCarried() *carried {
	return &carried{values: map[string]string{}, tokens: map[string]string{}, numbers: map[string]string{}}
}

// carriedKeys are members whose value is the server's to issue and the
// client's to echo, so a change in one is carried however it looks.
var carriedKeys = map[string]bool{
	"requestState":  true,
	"nextCursor":    true,
	"cursor":        true,
	"taskId":        true,
	"elicitationId": true,
	"uri":           true,
}

// learn records what changed between a recorded answer and the replayed one.
func (c *carried) learn(recorded, replayed *node) {
	c.learnAt(recorded, replayed, "")
}

// definitionKeys hold what a server declares about itself and its tools, never
// a value it issued for the client to send back. A newer server renames,
// rewords and re-dates these freely, and carrying one would turn a call to the
// recorded tool into a call to whatever replaced it, hiding the very change a
// replay is there to show.
var definitionKeys = map[string]bool{
	"name": true, "title": true, "description": true, "type": true, "method": true,
	"mimeType": true, "protocolVersion": true, "version": true, "uriTemplate": true,
	"role": true, "resultType": true, "status": true, "jsonrpc": true,
}

// definitionTrees are whole members of the same kind, schemas, hints,
// capabilities and metadata, which are not looked into at all.
var definitionTrees = map[string]bool{
	"inputSchema": true, "outputSchema": true, "annotations": true, "icons": true,
	"capabilities": true, "serverInfo": true, "clientInfo": true, "_meta": true,
}

func (c *carried) learnAt(rec, rep *node, key string) {
	if rec == nil || rep == nil || definitionTrees[key] {
		return
	}
	switch {
	case rec.kind == objectNode && rep.kind == objectNode:
		for _, m := range rec.members {
			c.learnAt(m.value, rep.get(m.key), m.key)
		}
	case rec.kind == arrayNode && rep.kind == arrayNode:
		c.learnList(rec, rep, key)
	case rec.kind == stringNode && rep.kind == stringNode && rec.str != rep.str && !definitionKeys[key]:
		if carriedKeys[key] || idKey(key) || identifier(rec.str) {
			c.values[rec.str] = rep.str
		}
		c.learnTokens(rec.str, rep.str)
	case rec.kind == numberNode && rep.kind == numberNode && rec.num != rep.num && idKey(key):
		c.numbers[rec.num.String()] = rep.num.String()
	}
}

// learnList pairs the items of two lists. Items that are known by a name, the
// tools, prompts and resources of a listing, pair by that name, since a newer
// server lists more of them, fewer, or in another order. Anything else pairs
// by position, and only when the lists are the same length, because a list
// that grew or shrank has no position that still means the same item.
func (c *carried) learnList(rec, rep *node, key string) {
	if named(rec) && named(rep) {
		byName := make(map[string]*node, len(rep.items))
		for _, item := range rep.items {
			byName[item.get("name").str] = item
		}
		for _, item := range rec.items {
			if match := byName[item.get("name").str]; match != nil {
				c.learnAt(item, match, key)
			}
		}
		return
	}
	if len(rec.items) != len(rep.items) {
		return
	}
	for i := range rec.items {
		c.learnAt(rec.items[i], rep.items[i], key)
	}
}

// named reports whether every item of a list is an object with a string name.
func named(list *node) bool {
	if len(list.items) == 0 {
		return false
	}
	for _, item := range list.items {
		if name := item.get("name"); name == nil || name.kind != stringNode {
			return false
		}
	}
	return true
}

// learnTokens pairs the identifiers of two strings that differ only in them.
// Strings that do not split into the same number of tokens are different text
// rather than the same text with a new id in it, and are left alone.
func (c *carried) learnTokens(rec, rep string) {
	a, b := tokenSpans(rec), tokenSpans(rep)
	if len(a) != len(b) {
		return
	}
	for i := range a {
		old, now := rec[a[i][0]:a[i][1]], rep[b[i][0]:b[i][1]]
		if old != now && identifierToken(old) {
			c.tokens[old] = now
		}
	}
}

// empty reports whether nothing has been learned yet.
func (c *carried) empty() bool {
	return len(c.values) == 0 && len(c.tokens) == 0 && len(c.numbers) == 0
}

// apply swaps every learned value inside a client frame, leaving the members
// that address the frame itself alone. It reports whether anything changed.
func (c *carried) apply(frame *node) bool {
	if c.empty() || frame == nil || frame.kind != objectNode {
		return false
	}
	changed := false
	for _, m := range frame.members {
		switch m.key {
		case "jsonrpc", "id", "method":
			continue
		}
		if c.applyAt(m.value, m.key) {
			changed = true
		}
	}
	return changed
}

func (c *carried) applyAt(n *node, key string) bool {
	switch n.kind {
	case objectNode:
		changed := false
		for _, m := range n.members {
			if c.applyAt(m.value, m.key) {
				changed = true
			}
		}
		return changed
	case arrayNode:
		changed := false
		for _, item := range n.items {
			if c.applyAt(item, key) {
				changed = true
			}
		}
		return changed
	case stringNode:
		if now, ok := c.values[n.str]; ok {
			n.str = now
			return true
		}
		if replaced := c.replaceTokens(n.str); replaced != n.str {
			n.str = replaced
			return true
		}
	case numberNode:
		if now, ok := c.numbers[n.num.String()]; ok && idKey(key) {
			n.num = json.Number(now)
			return true
		}
	}
	return false
}

func (c *carried) replaceTokens(s string) string {
	if len(c.tokens) == 0 {
		return s
	}
	spans := tokenSpans(s)
	var out strings.Builder
	last := 0
	for _, span := range spans {
		now, ok := c.tokens[s[span[0]:span[1]]]
		if !ok {
			continue
		}
		out.WriteString(s[last:span[0]])
		out.WriteString(now)
		last = span[1]
	}
	if last == 0 {
		return s
	}
	out.WriteString(s[last:])
	return out.String()
}

// idKey reports whether a member name says it holds an identifier.
func idKey(key string) bool {
	if key == "" {
		return false
	}
	lower := strings.ToLower(key)
	return lower == "id" || strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "ID") ||
		strings.HasSuffix(lower, "_id") || strings.HasSuffix(lower, "-id")
}

// identifier reports whether a whole string reads as something issued rather
// than written: no spaces, and a digit, a separator or enough length to be a
// generated token. A changed word such as a weather report's "Sunny" is the
// server's news, not an id the client is meant to send back.
func identifier(s string) bool {
	if len(s) < 4 || strings.ContainsFunc(s, unicode.IsSpace) {
		return false
	}
	return strings.ContainsFunc(s, unicode.IsDigit) || len(s) >= 16 || strings.ContainsAny(s, "-_:/.=+")
}

// identifierToken is the same judgement for a token taken out of text, where
// separators split tokens already, so only a digit or a generated length
// counts.
func identifierToken(t string) bool {
	return len(t) >= 4 && (strings.ContainsFunc(t, unicode.IsDigit) || len(t) >= 16)
}

// tokenSpans returns the identifier-shaped runs of s as byte offsets: letters,
// digits, '-', '_' and '.', with any '-' or '.' at either end trimmed off so the
// full stop ending a sentence is not part of the id before it.
func tokenSpans(s string) [][2]int {
	var spans [][2]int
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		for start < end && (s[start] == '-' || s[start] == '.') {
			start++
		}
		for end > start && (s[end-1] == '-' || s[end-1] == '.') {
			end--
		}
		if end > start {
			spans = append(spans, [2]int{start, end})
		}
		start = -1
	}
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(s))
	return spans
}
