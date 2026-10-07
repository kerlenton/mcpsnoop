package clientprofile

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// The effects a tool call is counted under, by what the called tool declares.
const (
	effectReadOnly    = "read-only"
	effectAdditive    = "additive"
	effectDestructive = "destructive"
	// effectNoHints is a tool that declares no behaviour hint or was never listed,
	// which every client reads as a destructive write.
	effectNoHints = "no hints"
)

// listedTools maps each tool of a session's complete listing to its definition.
// A listing still paginating says nothing about the tools it has not reached, so
// it maps nothing.
func listedTools(st *store.Store, sessionID string) map[string]store.ToolDefinition {
	definitions := map[string]store.ToolDefinition{}
	if listed, ok := st.ToolDefinitions(sessionID); ok {
		for _, d := range listed {
			definitions[d.Name] = d
		}
	}
	return definitions
}

// callsAtOnce reads the most tool calls one session had in flight at the same
// time, among calls to read-only tools and at any moment a call to another tool
// was among them.
//
// Only calls the server answered are measured. A client that gives up on a call
// and moves on has not run the two side by side, even though the next request
// can reach the wire before the cancellation does, and on Streamable HTTP a
// cancellation leaves no frame at all, so a call the client abandoned there would
// otherwise read as running for the rest of the capture.
func callsAtOnce(st *store.Store, sessionID string, definitions map[string]store.ToolDefinition) (readOnlyAtOnce, othersAtOnce int) {
	type edge struct {
		at       time.Time
		start    bool
		readOnly bool
	}
	var edges []edge
	for _, c := range st.Calls(sessionID) {
		if !c.IsTool || c.ReqDir != proxy.ClientToServer || c.State == store.Cancelled || !c.Done() || c.End.IsZero() {
			continue
		}
		definition, listed := definitions[c.ToolName]
		readOnly := effectOf(definition, listed) == effectReadOnly
		edges = append(edges, edge{c.Start, true, readOnly}, edge{c.End, false, readOnly})
	}
	// Ends before starts at the same instant, so a call sent the moment the one
	// before it was answered does not read as running beside it.
	slices.SortStableFunc(edges, func(a, b edge) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		if a.start == b.start {
			return 0
		}
		if a.start {
			return 1
		}
		return -1
	})
	readOnly, others := 0, 0
	for _, e := range edges {
		step := 1
		if !e.start {
			step = -1
		}
		if e.readOnly {
			readOnly += step
		} else {
			others += step
		}
		if !e.start {
			continue
		}
		readOnlyAtOnce = max(readOnlyAtOnce, readOnly)
		if others > 0 {
			othersAtOnce = max(othersAtOnce, readOnly+others)
		}
	}
	return readOnlyAtOnce, othersAtOnce
}

// effectOf names what a tool declares about its effects, the way clients read
// it through the defaults.
func effectOf(d store.ToolDefinition, listed bool) string {
	if !listed || slices.Contains(d.AnnotationFindings, store.AnnotationsMissing) {
		return effectNoHints
	}
	hints := store.ParseToolHints(d.Annotations)
	switch {
	case hints.Value(store.HintReadOnly):
		return effectReadOnly
	case hints.Value(store.HintDestructive):
		return effectDestructive
	}
	return effectAdditive
}

// toolCallsCell is the tool call count with its breakdown by effect, largest
// first. Both count requests, the way progress tokens does, so a multi
// round-trip retry is counted under the tool it continues.
func toolCallsCell(p Profile) string {
	if p.ToolCalls == 0 {
		return "none"
	}
	keys := byCount(p.Effects)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", p.Effects[k], k))
	}
	return fmt.Sprintf("%d (%s)", p.ToolCalls, strings.Join(parts, ", "))
}

// inFlightCell says how many tool calls the client ran side by side, apart for
// read-only tools and the rest, since that is the line clients draw.
func inFlightCell(p Profile) string {
	if p.ToolCalls == 0 {
		return "no tool calls"
	}
	atOnce := func(n int) string {
		if n == 1 {
			return "1 at a time"
		}
		return fmt.Sprintf("up to %d at once", n)
	}
	var parts []string
	if p.ReadOnlyAtOnce > 0 {
		parts = append(parts, "read-only "+atOnce(p.ReadOnlyAtOnce))
	}
	if p.OthersAtOnce > 0 {
		parts = append(parts, "others "+atOnce(p.OthersAtOnce))
	}
	return strings.Join(parts, ", ")
}

func repeatedCell(p Profile) string {
	if p.Repeated == 0 {
		return "none"
	}
	if p.RanTwice > 0 {
		return fmt.Sprintf("%d, %d of them ran twice", p.Repeated, p.RanTwice)
	}
	return fmt.Sprintf("%d, may have run twice", p.Repeated)
}
