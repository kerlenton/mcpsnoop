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
	"time"

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
	if isIncompleteBaseline(err) {
		baseline, err = m.loadAfterConcurrentCreate(server)
	}
	if errors.Is(err, os.ErrNotExist) {
		candidate := snapshot{Version: baselineVersion, Server: server, Tools: normalize(current)}
		if err := m.writeNew(candidate); err == nil {
			return Report{}, true, nil
		} else if !errors.Is(err, os.ErrExist) {
			return Report{}, false, err
		}
		baseline, err = m.loadAfterConcurrentCreate(server)
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

func ObserveAll(m *Manager, st *store.Store) error {
	for _, session := range st.Sessions() {
		if _, ok := st.ToolDefinitions(session.ID); !ok {
			continue
		}
		if _, _, err := ObserveSession(m, st, session.ID); err != nil {
			return err
		}
	}
	return nil
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
			server := session.Label
			if server == "" {
				server = session.ID
			}
			return server, nil
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
		return snapshot{}, fmt.Errorf("tool baseline %q: %w", server, err)
	}
	if baseline.Version != baselineVersion || baseline.Server != server {
		return snapshot{}, fmt.Errorf("tool baseline %q: unsupported or mismatched baseline", server)
	}
	baseline.Tools = normalizeStored(baseline.Tools)
	return baseline, nil
}

func (m *Manager) loadAfterConcurrentCreate(server string) (snapshot, error) {
	for range 100 {
		baseline, err := m.load(server)
		if err == nil || !isIncompleteBaseline(err) {
			return baseline, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return m.load(server)
}

func isIncompleteBaseline(err error) bool {
	var syntaxError *json.SyntaxError
	return errors.As(err, &syntaxError)
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

func (m *Manager) writeNew(baseline snapshot) error {
	if strings.TrimSpace(baseline.Server) == "" {
		return errors.New("tool baseline: empty server label")
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	path := m.Path(baseline.Server)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(baseline); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
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
