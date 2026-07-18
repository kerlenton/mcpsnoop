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
	BeforeSession       string
	AfterSession        string
	AddedTools          []string
	RemovedTools        []string
	ChangedDescriptions []string
	ChangedSchemas      []string
	CallChanges         []CallChange
	DurationChanges     []DurationChange
}

// ToolDefinition is the behavior-affecting contract advertised for one MCP tool.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolChanges contains definition differences between two complete tool lists.
type ToolChanges struct {
	AddedTools          []string
	RemovedTools        []string
	ChangedDescriptions []string
	ChangedSchemas      []string
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
	return len(r.AddedTools) == 0 &&
		len(r.RemovedTools) == 0 &&
		len(r.ChangedDescriptions) == 0 &&
		len(r.ChangedSchemas) == 0 &&
		len(r.CallChanges) == 0 &&
		len(r.DurationChanges) == 0
}

// HasRegression reports whether the after session is worse than the before one:
// a removed tool or a changed input schema (a potentially breaking contract
// change), a call whose status got worse, or a call that got notably slower.
// Improvements, added tools, fixed calls, and speedups do not count.
func (r Report) HasRegression() bool {
	if len(r.RemovedTools) > 0 || len(r.ChangedDescriptions) > 0 || len(r.ChangedSchemas) > 0 {
		return true
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
	case "pending":
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
	toolChanges := CompareToolDefinitions(listedTools(before), listedTools(after))
	report.AddedTools = toolChanges.AddedTools
	report.RemovedTools = toolChanges.RemovedTools
	report.ChangedDescriptions = toolChanges.ChangedDescriptions
	report.ChangedSchemas = toolChanges.ChangedSchemas

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
	if len(report.AddedTools)+len(report.RemovedTools)+len(report.ChangedDescriptions)+len(report.ChangedSchemas) > 0 {
		if _, err := fmt.Fprintln(w, "tools:"); err != nil {
			return err
		}
		for _, name := range report.AddedTools {
			if _, err := fmt.Fprintf(w, "  added: %s\n", name); err != nil {
				return err
			}
		}
		for _, name := range report.RemovedTools {
			if _, err := fmt.Fprintf(w, "  removed: %s\n", name); err != nil {
				return err
			}
		}
		for _, name := range report.ChangedDescriptions {
			if _, err := fmt.Fprintf(w, "  description changed: %s\n", name); err != nil {
				return err
			}
		}
		for _, name := range report.ChangedSchemas {
			if _, err := fmt.Fprintf(w, "  schema changed: %s\n", name); err != nil {
				return err
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
func CompareToolDefinitions(before, after []ToolDefinition) ToolChanges {
	beforeTools := toolDefinitionsByName(before)
	afterTools := toolDefinitionsByName(after)
	var changes ToolChanges
	for name, trusted := range beforeTools {
		observed, ok := afterTools[name]
		switch {
		case !ok:
			changes.RemovedTools = append(changes.RemovedTools, name)
		default:
			if trusted.Description != observed.Description {
				changes.ChangedDescriptions = append(changes.ChangedDescriptions, name)
			}
			if canonicalJSON(trusted.InputSchema) != canonicalJSON(observed.InputSchema) {
				changes.ChangedSchemas = append(changes.ChangedSchemas, name)
			}
		}
	}
	for name := range afterTools {
		if _, ok := beforeTools[name]; !ok {
			changes.AddedTools = append(changes.AddedTools, name)
		}
	}
	slices.Sort(changes.AddedTools)
	slices.Sort(changes.RemovedTools)
	slices.Sort(changes.ChangedDescriptions)
	slices.Sort(changes.ChangedSchemas)
	return changes
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
		var result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		if json.Unmarshal(call.Result, &result) != nil {
			continue
		}
		if !hasCursor(call.Params) {
			clear(tools)
		}
		for _, tool := range result.Tools {
			if tool.Name == "" {
				continue
			}
			if _, exists := tools[tool.Name]; exists {
				continue
			}
			tools[tool.Name] = ToolDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
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
	canonical, err := json.Marshal(value)
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
