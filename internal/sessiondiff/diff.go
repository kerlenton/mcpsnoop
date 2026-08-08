// Package sessiondiff compares two exported MCP sessions.
package sessiondiff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

const (
	DefaultDurationThreshold = 100 * time.Millisecond
	DefaultDurationRatio     = 2.0
)

type Options struct {
	DurationThreshold time.Duration
	DurationRatio     float64
}

type Report struct {
	BeforeSession string
	AfterSession  string
	// Tools carries every definition difference, keyed by kind. One value rather
	// than a slice per kind, so adding a kind cannot leave a renderer, a count or
	// a gate silently behind.
	Tools           store.ToolDrift
	CallChanges     []CallChange
	DurationChanges []DurationChange
}

// ToolDefinition is the behavior-affecting contract advertised for one MCP tool.
// Title, OutputSchema, Annotations and Icons are here because each is something
// a server can change after the user approved the tool: Title outranks both
// annotations.title and Name as the displayed name, OutputSchema is the promise
// about structuredContent, Annotations carries the behaviour hints the spec
// tells clients to distrust, and Icons is rendered beside the tool.
type ToolDefinition struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	Title        string
	OutputSchema json.RawMessage
	Annotations  json.RawMessage
	Icons        json.RawMessage
}

type CallChange struct {
	ToolName  string
	Arguments string
	Before    string
	After     string
}

type DurationChange struct {
	ToolName  string
	Arguments string
	Before    time.Duration
	After     time.Duration
}

func (r Report) Empty() bool {
	return r.Tools.Count() == 0 &&
		len(r.CallChanges) == 0 &&
		len(r.DurationChanges) == 0
}

// regressionKinds are the definition changes that make the after session worse
// rather than merely different. A removed tool, a changed contract, and a
// changed displayed name can all break or mislead a caller that relied on the
// before session. Added tools do not, and neither do icons, which change how a
// tool looks without changing what it does or promises.
var regressionKinds = []store.ToolDriftKind{
	store.DriftToolRemoved,
	store.DriftDescription,
	store.DriftInputSchema,
	store.DriftTitle,
	store.DriftOutputSchema,
	store.DriftAnnotations,
}

// HasRegression reports whether the after session is worse than the before one:
// a removed tool, a changed description, title, input or output schema, or
// changed annotations (all potentially breaking or misleading contract changes),
// a call whose status got worse, or a call that got notably slower.
// Improvements, added tools, fixed calls, speedups, and icon changes do not count.
func (r Report) HasRegression() bool {
	for _, kind := range regressionKinds {
		if len(r.Tools.Names(kind)) > 0 {
			return true
		}
	}
	for _, change := range r.CallChanges {
		if statusRank(change.After) > statusRank(change.Before) {
			return true
		}
	}
	for _, change := range r.DurationChanges {
		if change.After > change.Before {
			return true
		}
	}
	return false
}

// statusRank orders call outcomes worst-last, so a status change reads as a
// regression when the rank rises and an improvement when it falls.
func statusRank(status string) int {
	switch status {
	case "error":
		return 2
	case "pending", "call_cancelled", "late_result":
		// A call the client gave up on, and a result that arrived after it did,
		// both end without the answer the caller wanted. Ranking them with ok made
		// a run that went from ok to late_result print the change and still exit 0,
		// so the line a human reads and the code a CI job reads disagreed.
		return 1
	default: // ok, and anything unexpected
		return 0
	}
}

func Compare(before, after exporter.SessionExport, opts Options) Report {
	if opts.DurationThreshold < 0 {
		opts.DurationThreshold = DefaultDurationThreshold
	}
	if opts.DurationRatio < 1 || math.IsNaN(opts.DurationRatio) || math.IsInf(opts.DurationRatio, 0) {
		opts.DurationRatio = DefaultDurationRatio
	}

	report := Report{
		BeforeSession: before.Session.ID,
		AfterSession:  after.Session.ID,
	}
	report.Tools = CompareToolDefinitions(listedTools(before), listedTools(after))

	beforeCalls := callsBySignature(before)
	afterCalls := callsBySignature(after)
	var signatures []string
	for signature := range beforeCalls {
		if _, ok := afterCalls[signature]; ok {
			signatures = append(signatures, signature)
		}
	}
	slices.Sort(signatures)
	for _, signature := range signatures {
		beforeMatches := beforeCalls[signature]
		afterMatches := afterCalls[signature]
		for i := range min(len(beforeMatches), len(afterMatches)) {
			beforeCall := beforeMatches[i]
			afterCall := afterMatches[i]
			if beforeCall.status != afterCall.status {
				report.CallChanges = append(report.CallChanges, CallChange{
					ToolName: beforeCall.toolName, Arguments: beforeCall.arguments,
					Before: beforeCall.status, After: afterCall.status,
				})
			}
			if beforeCall.duration == nil || afterCall.duration == nil {
				continue
			}
			if notableDurationChange(*beforeCall.duration, *afterCall.duration, opts) {
				report.DurationChanges = append(report.DurationChanges, DurationChange{
					ToolName: beforeCall.toolName, Arguments: beforeCall.arguments,
					Before: *beforeCall.duration, After: *afterCall.duration,
				})
			}
		}
	}
	return report
}

func WriteText(w io.Writer, report Report) error {
	if _, err := fmt.Fprintf(w, "mcpsnoop diff %s -> %s\n", report.BeforeSession, report.AfterSession); err != nil {
		return err
	}
	if report.Empty() {
		_, err := fmt.Fprintln(w, "no differences found")
		return err
	}
	if report.Tools.Count() > 0 {
		if _, err := fmt.Fprintln(w, "tools:"); err != nil {
			return err
		}
		// Driven by store.ToolDriftKinds rather than a hand-written list, so a kind
		// added later cannot be counted here and then never printed.
		for _, kind := range store.ToolDriftKinds {
			for _, name := range report.Tools.Names(kind) {
				if _, err := fmt.Fprintf(w, "  %s: %s\n", driftLabel(kind), name); err != nil {
					return err
				}
			}
		}
	}
	if len(report.CallChanges) > 0 {
		if _, err := fmt.Fprintln(w, "calls:"); err != nil {
			return err
		}
		for _, change := range report.CallChanges {
			if _, err := fmt.Fprintf(w, "  status changed: %s %s %s -> %s\n",
				change.ToolName, change.Arguments, change.Before, change.After); err != nil {
				return err
			}
		}
	}
	if len(report.DurationChanges) > 0 {
		if _, err := fmt.Fprintln(w, "durations:"); err != nil {
			return err
		}
		for _, change := range report.DurationChanges {
			direction := "slower"
			if change.After < change.Before {
				direction = "faster"
			}
			if _, err := fmt.Fprintf(w, "  %s: %s %s %s -> %s\n",
				direction, change.ToolName, change.Arguments, change.Before, change.After); err != nil {
				return err
			}
		}
	}
	return nil
}

// CompareToolDefinitions reports behavior-affecting differences between two
// complete tool lists. Schema JSON is canonicalized before comparison.
//
// skip names kinds the trusted side never recorded, which happens when a
// baseline predates mcpsnoop tracking that field. Those arms are not run at all
// rather than compared against nothing, since an absent record is not evidence
// that the server changed anything.
func CompareToolDefinitions(before, after []ToolDefinition, skip ...store.ToolDriftKind) store.ToolDrift {
	beforeTools := toolDefinitionsByName(before)
	afterTools := toolDefinitionsByName(after)
	skipped := make(map[store.ToolDriftKind]bool, len(skip))
	for _, kind := range skip {
		skipped[kind] = true
	}

	var drift store.ToolDrift
	drift.Unverified = append(drift.Unverified, skip...)
	compare := func(kind store.ToolDriftKind, name string, differs func() bool) {
		if !skipped[kind] && differs() {
			drift.Add(kind, name)
		}
	}

	for name, trusted := range beforeTools {
		observed, ok := afterTools[name]
		if !ok {
			drift.Add(store.DriftToolRemoved, name)
			continue
		}
		compare(store.DriftDescription, name, func() bool {
			return trusted.Description != observed.Description
		})
		compare(store.DriftInputSchema, name, func() bool {
			return comparableSchema(trusted.InputSchema) != comparableSchema(observed.InputSchema)
		})
		compare(store.DriftTitle, name, func() bool {
			return trusted.Title != observed.Title
		})
		compare(store.DriftOutputSchema, name, func() bool {
			return comparableSchema(trusted.OutputSchema) != comparableSchema(observed.OutputSchema)
		})
		compare(store.DriftAnnotations, name, func() bool {
			return comparableAnnotations(trusted.Annotations) != comparableAnnotations(observed.Annotations)
		})
		compare(store.DriftIcons, name, func() bool {
			// Order-sensitive on purpose. A consumer that takes the first usable
			// icon sees a different one when two are swapped, and src is the field
			// the spec warns can point off-domain or at a data: URI.
			return canonicalJSON(trusted.Icons) != canonicalJSON(observed.Icons)
		})
	}
	for name := range afterTools {
		if _, ok := beforeTools[name]; !ok {
			drift.Add(store.DriftToolAdded, name)
		}
	}
	for _, names := range drift.Changes {
		slices.Sort(names)
	}
	return drift
}

// driftLabel is the singular per-tool phrasing for a diff line. ToolDriftKind's
// own Label reads after a count ("2 tools removed"), which is wrong before a
// single tool name.
func driftLabel(kind store.ToolDriftKind) string {
	switch kind {
	case store.DriftToolAdded:
		return "added"
	case store.DriftToolRemoved:
		return "removed"
	case store.DriftDescription:
		return "description changed"
	case store.DriftInputSchema:
		return "input schema changed"
	case store.DriftTitle:
		return "title changed"
	case store.DriftOutputSchema:
		return "output schema changed"
	case store.DriftAnnotations:
		return "annotations changed"
	case store.DriftIcons:
		return "icons changed"
	}
	return string(kind)
}

func toolDefinitionsByName(definitions []ToolDefinition) map[string]ToolDefinition {
	tools := make(map[string]ToolDefinition, len(definitions))
	for _, definition := range definitions {
		if definition.Name == "" {
			continue
		}
		if _, exists := tools[definition.Name]; exists {
			continue
		}
		tools[definition.Name] = definition
	}
	return tools
}

func listedTools(session exporter.SessionExport) []ToolDefinition {
	tools := make(map[string]ToolDefinition)
	for _, call := range session.Calls {
		if call.Method != "tools/list" {
			continue
		}
		// Each tool is decoded on its own, deliberately. Decoding the page into a
		// typed slice meant one tool with a non-string description discarded every
		// well-formed tool beside it, which is the same defect the store already
		// fixed for its own decode. A tool that is advertised should be compared
		// even when one of its fields is junk.
		var result struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(call.Result, &result) != nil {
			continue
		}
		if !hasCursor(call.Params) {
			clear(tools)
		}
		for _, rawTool := range result.Tools {
			var tool struct {
				Name string `json:"name"`
				// Raw with a tolerant second decode, so a wrong JSON type costs one
				// field rather than the whole tool.
				Description  json.RawMessage `json:"description"`
				Title        json.RawMessage `json:"title"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				OutputSchema json.RawMessage `json:"outputSchema"`
				Annotations  json.RawMessage `json:"annotations"`
				Icons        json.RawMessage `json:"icons"`
			}
			if json.Unmarshal(rawTool, &tool) != nil || tool.Name == "" {
				continue
			}
			if _, exists := tools[tool.Name]; exists {
				continue
			}
			var description, title string
			_ = json.Unmarshal(tool.Description, &description)
			_ = json.Unmarshal(tool.Title, &title)
			tools[tool.Name] = ToolDefinition{
				Name:         tool.Name,
				Description:  description,
				Title:        title,
				InputSchema:  append(json.RawMessage(nil), tool.InputSchema...),
				OutputSchema: append(json.RawMessage(nil), tool.OutputSchema...),
				Annotations:  append(json.RawMessage(nil), tool.Annotations...),
				Icons:        append(json.RawMessage(nil), tool.Icons...),
			}
		}
	}
	definitions := make([]ToolDefinition, 0, len(tools))
	for _, definition := range tools {
		definitions = append(definitions, definition)
	}
	slices.SortFunc(definitions, func(a, b ToolDefinition) int { return strings.Compare(a.Name, b.Name) })
	return definitions
}

func hasCursor(params json.RawMessage) bool {
	var request struct {
		Cursor string `json:"cursor"`
	}
	return json.Unmarshal(params, &request) == nil && request.Cursor != ""
}

type comparableCall struct {
	toolName  string
	arguments string
	status    string
	duration  *time.Duration
}

func callsBySignature(session exporter.SessionExport) map[string][]comparableCall {
	calls := make(map[string][]comparableCall)
	for _, call := range session.Calls {
		if !call.IsTool || call.ToolName == "" {
			continue
		}
		arguments := callArguments(call.Params)
		signature := call.ToolName + "\x00" + arguments
		comparable := comparableCall{
			toolName: call.ToolName, arguments: arguments, status: call.Status,
		}
		if call.DurationMS != nil {
			duration := time.Duration(*call.DurationMS * float64(time.Millisecond))
			comparable.duration = &duration
		}
		calls[signature] = append(calls[signature], comparable)
	}
	return calls
}

func callArguments(params json.RawMessage) string {
	var request struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &request) != nil || len(request.Arguments) == 0 {
		return "{}"
	}
	return canonicalJSON(request.Arguments)
}

func canonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return strings.TrimSpace(string(raw))
	}
	// jsonwire, because this is not only a comparison key. callArguments feeds the
	// same string into CallChange.Arguments, which WriteText prints to a terminal,
	// and a person reading a diff should see the arguments the tool was called
	// with rather than their escaped spelling. Key sorting, which is what makes
	// this canonical, is unaffected.
	canonical, err := jsonwire.Marshal(value)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(canonical)
}

func notableDurationChange(before, after time.Duration, opts Options) bool {
	difference := after - before
	if difference < 0 {
		difference = -difference
	}
	if difference == 0 {
		return false
	}
	if difference < opts.DurationThreshold {
		return false
	}
	shorter, longer := before, after
	if shorter > longer {
		shorter, longer = longer, shorter
	}
	if shorter <= 0 {
		return longer > 0
	}
	return float64(longer)/float64(shorter) >= opts.DurationRatio
}
