// Package toolbaseline persists and compares trust-on-first-use MCP tool definitions.
package toolbaseline

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/sessiondiff"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// baselineVersion is the shape of the file this build writes. Version 1 recorded
// only name, description and input_schema; version 2 adds title, output_schema,
// annotations and icons. Version 1 files stay readable and keep answering for
// the fields they do record, per versionCoverageGap.
const baselineVersion = 2

type Report = store.ToolDrift

type Manager struct {
	dir string
	mu  sync.Mutex
}

type snapshot struct {
	Version int              `json:"version"`
	Server  string           `json:"server"`
	Tools   []toolDefinition `json:"tools"`
}

type toolDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	Title        string          `json:"title,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Icons        json.RawMessage `json:"icons,omitempty"`
}

// versionCoverageGap names the drift kinds a snapshot of the given version has
// no record of, so they are skipped rather than compared against nothing.
//
// This is what makes the upgrade safe. A version 1 file has no annotations key
// by construction, since the store never captured one, so comparing it would
// report annotation drift on every tool that has any, all at once, across every
// installed baseline. That false rug-pull alarm is worse than the gap it would
// be reporting, because it is exactly what teaches people to stop reading the
// signal. The gap closes when the operator next records a baseline.
func versionCoverageGap(version int) []store.ToolDriftKind {
	if version >= 2 {
		return nil
	}
	return []store.ToolDriftKind{
		store.DriftTitle,
		store.DriftOutputSchema,
		store.DriftAnnotations,
		store.DriftIcons,
	}
}

func New(dir string) *Manager { return &Manager{dir: dir} }

func (m *Manager) Path(server string) string {
	hash := sha256.Sum256([]byte(server))
	return filepath.Join(m.dir, fmt.Sprintf("%x.json", hash[:16]))
}

// Observe creates a first-seen baseline or compares the current definition set
// with the existing baseline. created reports whether this observation trusted
// and persisted the first definition set.
func (m *Manager) Observe(server string, current []store.ToolDefinition) (Report, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	baseline, err := m.load(server)
	if errors.Is(err, os.ErrNotExist) {
		candidate := snapshot{Version: baselineVersion, Server: server, Tools: normalize(current)}
		if err := m.writeNew(candidate); err == nil {
			return Report{}, true, nil
		} else if !errors.Is(err, os.ErrExist) {
			return Report{}, false, err
		}
		// Another writer linked the baseline first. Because writeNew links a fully
		// written file atomically, the target is complete and a plain load succeeds.
		baseline, err = m.load(server)
	}
	if err != nil {
		return Report{}, false, err
	}
	return compare(baseline.Tools, normalize(current), baseline.Version), false, nil
}

func (m *Manager) Accept(server string, current []store.ToolDefinition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accept(server, current)
}

func (m *Manager) accept(server string, current []store.ToolDefinition) error {
	if strings.TrimSpace(server) == "" {
		return errors.New("tool baseline: empty server label")
	}
	return m.write(snapshot{Version: baselineVersion, Server: server, Tools: normalize(current)})
}

func (m *Manager) Reset(server string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := os.Remove(m.Path(server))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func ObserveSession(m *Manager, st *store.Store, sessionID string) (Report, bool, error) {
	server, definitions, err := sessionDefinitions(st, sessionID)
	if err != nil {
		return Report{}, false, err
	}
	report, created, err := m.Observe(server, definitions)
	if err == nil {
		st.SetToolDrift(sessionID, report)
	}
	return report, created, err
}

// ObserveAll observes every session that has a complete tool list, recording a
// per-session BaselineError rather than returning on the first failure, so one
// bad baseline file never blocks the TUI from opening or hides other sessions.
// onSession, when non-nil, fires after each session so a caller running this in
// the background can render drift markers incrementally instead of in one batch.
func ObserveAll(m *Manager, st *store.Store, onSession func()) {
	for _, session := range st.Sessions() {
		if _, ok := st.ToolDefinitions(session.ID); !ok {
			continue
		}
		if _, _, err := ObserveSession(m, st, session.ID); err != nil {
			st.SetToolDrift(session.ID, store.ToolDrift{BaselineError: err.Error()})
		}
		if onSession != nil {
			onSession()
		}
	}
}

func AcceptSession(m *Manager, st *store.Store, sessionID string) (string, error) {
	server, definitions, err := sessionDefinitions(st, sessionID)
	if err != nil {
		return "", err
	}
	if err := m.Accept(server, definitions); err != nil {
		return "", err
	}
	st.SetToolDrift(sessionID, Report{})
	return server, nil
}

func ResetSession(m *Manager, st *store.Store, sessionID string) (string, error) {
	server, err := sessionLabel(st, sessionID)
	if err != nil {
		return "", err
	}
	if err := m.Reset(server); err != nil {
		return "", err
	}
	st.SetToolDrift(sessionID, Report{})
	return server, nil
}

func sessionDefinitions(st *store.Store, sessionID string) (string, []store.ToolDefinition, error) {
	definitions, ok := st.ToolDefinitions(sessionID)
	if !ok {
		return "", nil, fmt.Errorf("session %q has no complete tools/list result", sessionID)
	}
	server, err := sessionLabel(st, sessionID)
	if err != nil {
		return "", nil, err
	}
	return server, definitions, nil
}

func sessionLabel(st *store.Store, sessionID string) (string, error) {
	for _, session := range st.Sessions() {
		if session.ID == sessionID {
			// A baseline is keyed on the server label. Falling back to the session id
			// would key it per run, so drift would never be detected and the directory
			// would fill with orphan files. Fail clearly instead.
			if session.Label == "" {
				return "", fmt.Errorf("session %q has no server label; a tool baseline needs a stable label, set one with --label", sessionID)
			}
			return session.Label, nil
		}
	}
	return "", fmt.Errorf("session %q not found", sessionID)
}

func (m *Manager) load(server string) (snapshot, error) {
	data, err := os.ReadFile(m.Path(server))
	if err != nil {
		return snapshot{}, err
	}
	var baseline snapshot
	if err := json.Unmarshal(data, &baseline); err != nil {
		return snapshot{}, fmt.Errorf("tool baseline %q is corrupt (%w); run mcpsnoop baseline --reset to trust the next complete tools/list", server, err)
	}
	if baseline.Server != server {
		// A different server's file under this name means a copied directory or a
		// hash collision, not an upgrade. Comparing against it would be nonsense.
		return snapshot{}, fmt.Errorf("tool baseline %q records server %q; run mcpsnoop baseline --reset to record this one", server, baseline.Server)
	}
	if baseline.Version < 1 || baseline.Version > baselineVersion {
		// Forward-only. A file from a newer mcpsnoop may record fields this build
		// cannot interpret, so refusing is honest; an older one is handled by
		// versionCoverageGap rather than rejected.
		return snapshot{}, fmt.Errorf("tool baseline %q is version %d, newer than this mcpsnoop understands (%d); upgrade mcpsnoop, or run mcpsnoop baseline --reset to record it afresh", server, baseline.Version, baselineVersion)
	}
	baseline.Tools = normalizeStored(baseline.Tools)
	return baseline, nil
}

func (m *Manager) write(baseline snapshot) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dir, ".tool-baseline-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	encoder := jsonwire.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(baseline); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, m.Path(baseline.Server))
}

// writeNew persists a first-seen baseline without ever exposing a partial file.
// It fully writes a temp file in the same directory, then hard-links it onto the
// target. Link is atomic and returns os.ErrExist when the target already exists,
// which the caller treats as a concurrent create, so the trust-on-first-use race
// is decided by the filesystem rather than by an O_EXCL open that a crash could
// leave truncated. Same directory means same filesystem, so the link is valid.
func (m *Manager) writeNew(baseline snapshot) error {
	if strings.TrimSpace(baseline.Server) == "" {
		return errors.New("tool baseline: empty server label")
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dir, ".tool-baseline-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	encoder := jsonwire.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(baseline); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(name, m.Path(baseline.Server)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return err // a concurrent writer won the race; caller reloads the winner
		}
		return fmt.Errorf("tool baseline %q: %w", baseline.Server, err)
	}
	return nil
}

func normalize(definitions []store.ToolDefinition) []toolDefinition {
	tools := make([]toolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if definition.Name == "" {
			continue
		}
		tools = append(tools, toolDefinition{
			Name:         definition.Name,
			Description:  definition.Description,
			InputSchema:  json.RawMessage(canonicalJSON(definition.InputSchema)),
			Title:        definition.Title,
			OutputSchema: rawOrNil(definition.OutputSchema),
			Annotations:  rawOrNil(definition.Annotations),
			Icons:        rawOrNil(definition.Icons),
		})
	}
	slices.SortFunc(tools, func(a, b toolDefinition) int { return strings.Compare(a.Name, b.Name) })
	return tools
}

// rawOrNil canonicalises a field that a server may not have sent at all, and
// keeps absent absent. Canonicalising an empty value would store the literal
// "null", which is a different thing: absent output_schema means the tool makes
// no promise about structuredContent, while an explicit null is a value the
// server chose to send.
func rawOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(canonicalJSON(raw))
}

func normalizeStored(definitions []toolDefinition) []toolDefinition {
	for i := range definitions {
		definitions[i].InputSchema = json.RawMessage(canonicalJSON(definitions[i].InputSchema))
		definitions[i].OutputSchema = rawOrNil(definitions[i].OutputSchema)
		definitions[i].Annotations = rawOrNil(definitions[i].Annotations)
		definitions[i].Icons = rawOrNil(definitions[i].Icons)
	}
	slices.SortFunc(definitions, func(a, b toolDefinition) int { return strings.Compare(a.Name, b.Name) })
	return definitions
}

func compare(before, after []toolDefinition, version int) Report {
	return sessiondiff.CompareToolDefinitions(
		toSessionDiffTools(before), toSessionDiffTools(after), versionCoverageGap(version)...)
}

func toSessionDiffTools(definitions []toolDefinition) []sessiondiff.ToolDefinition {
	tools := make([]sessiondiff.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, sessiondiff.ToolDefinition{
			Name:         definition.Name,
			Description:  definition.Description,
			InputSchema:  append(json.RawMessage(nil), definition.InputSchema...),
			Title:        definition.Title,
			OutputSchema: append(json.RawMessage(nil), definition.OutputSchema...),
			Annotations:  append(json.RawMessage(nil), definition.Annotations...),
			Icons:        append(json.RawMessage(nil), definition.Icons...),
		})
	}
	return tools
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
	// jsonwire here as well as on the two encoders. normalize bakes this result
	// into the stored RawMessage before the outer encoder ever runs, and turning
	// escaping off does not unescape what is already escaped, so converting only
	// the encoders would leave input_schema rewritten in the durable record of
	// what --accept trusted. Safe in both directions: load re-runs
	// normalizeStored, so a baseline written before this still compares equal.
	canonical, err := jsonwire.Marshal(value)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(canonical)
}
