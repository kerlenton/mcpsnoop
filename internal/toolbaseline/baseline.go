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

	"github.com/kerlenton/mcpsnoop/internal/sessiondiff"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

const baselineVersion = 1

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
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
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
	return compare(baseline.Tools, normalize(current)), false, nil
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
func ObserveAll(m *Manager, st *store.Store) {
	for _, session := range st.Sessions() {
		if _, ok := st.ToolDefinitions(session.ID); !ok {
			continue
		}
		if _, _, err := ObserveSession(m, st, session.ID); err != nil {
			st.SetToolDrift(session.ID, store.ToolDrift{BaselineError: err.Error()})
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
	if baseline.Version != baselineVersion || baseline.Server != server {
		return snapshot{}, fmt.Errorf("tool baseline %q: unsupported or mismatched baseline", server)
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
	encoder := json.NewEncoder(tmp)
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
	encoder := json.NewEncoder(tmp)
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
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: json.RawMessage(canonicalJSON(definition.InputSchema)),
		})
	}
	slices.SortFunc(tools, func(a, b toolDefinition) int { return strings.Compare(a.Name, b.Name) })
	return tools
}

func normalizeStored(definitions []toolDefinition) []toolDefinition {
	for i := range definitions {
		definitions[i].InputSchema = json.RawMessage(canonicalJSON(definitions[i].InputSchema))
	}
	slices.SortFunc(definitions, func(a, b toolDefinition) int { return strings.Compare(a.Name, b.Name) })
	return definitions
}

func compare(before, after []toolDefinition) Report {
	changes := sessiondiff.CompareToolDefinitions(toSessionDiffTools(before), toSessionDiffTools(after))
	return Report{
		AddedTools:          changes.AddedTools,
		RemovedTools:        changes.RemovedTools,
		ChangedDescriptions: changes.ChangedDescriptions,
		ChangedSchemas:      changes.ChangedSchemas,
	}
}

func toSessionDiffTools(definitions []toolDefinition) []sessiondiff.ToolDefinition {
	tools := make([]sessiondiff.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, sessiondiff.ToolDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: append(json.RawMessage(nil), definition.InputSchema...),
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
	canonical, err := json.Marshal(value)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(canonical)
}
