package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/replay"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// replayDoneMsg carries the outcome of an async replay back into Update.
type replayDoneMsg struct {
	res replay.Result
	err error
}

// replayEditDoneMsg is emitted only after the editor has exited, its temporary
// file has been read and removed, and the saved value has passed JSON-object
// validation. Until then the model's current replay loop remains untouched.
type replayEditDoneMsg struct {
	call     store.CallView
	captured json.RawMessage
	err      error
}

const replayTimeout = 15 * time.Second

func replayCmd(command []string, cwd, method string, params json.RawMessage) tea.Cmd {
	return func() tea.Msg {
		res, err := replay.Replay(context.Background(), command, cwd, method, params, replayTimeout)
		return replayDoneMsg{res: res, err: err}
	}
}

func editReplayCmd(call store.CallView, captured json.RawMessage) (tea.Cmd, error) {
	cmd, done, err := prepareReplayEditor(call, captured)
	if err != nil {
		return nil, err
	}
	return tea.ExecProcess(cmd, done), nil
}

// prepareReplayEditor creates the private buffer and returns the process plus
// callback separately, which keeps the editor boundary directly testable. An
// exact executable value is tried first so paths containing spaces work; common
// command-plus-flags values such as "code --wait" use a deliberately shell-free
// whitespace split.
func prepareReplayEditor(call store.CallView, captured json.RawMessage) (*exec.Cmd, tea.ExecCallback, error) {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return nil, nil, fmt.Errorf("set $VISUAL or $EDITOR to edit replay params")
	}

	obj, err := decodeReplayParams(call.Params, true)
	if err != nil {
		return nil, nil, fmt.Errorf("captured params: %w", err)
	}
	pretty, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("format captured params: %w", err)
	}

	f, err := os.CreateTemp("", "mcpsnoop-replay-*.json")
	if err != nil {
		return nil, nil, fmt.Errorf("create editor buffer: %w", err)
	}
	path := f.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("secure editor buffer: %w", err)
	}
	if _, err := f.Write(append(pretty, '\n')); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("write editor buffer: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, nil, fmt.Errorf("close editor buffer: %w", err)
	}

	parts := []string{editor}
	if _, err := exec.LookPath(editor); err != nil {
		parts = strings.Fields(editor)
	}
	if len(parts) == 0 || parts[0] == "" {
		return nil, nil, fmt.Errorf("$VISUAL or $EDITOR is empty")
	}
	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	call.Params = append(json.RawMessage(nil), call.Params...)
	captured = append(json.RawMessage(nil), captured...)
	done := func(runErr error) tea.Msg {
		defer os.Remove(path)
		if runErr != nil {
			return replayEditDoneMsg{err: fmt.Errorf("editor failed: %w", runErr)}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return replayEditDoneMsg{err: fmt.Errorf("read editor buffer: %w", err)}
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return replayEditDoneMsg{err: fmt.Errorf("edited params are empty")}
		}
		if _, err := decodeReplayParams(raw, false); err != nil {
			return replayEditDoneMsg{err: err}
		}
		call.Params = append(json.RawMessage(nil), bytes.TrimSpace(raw)...)
		return replayEditDoneMsg{call: call, captured: captured}
	}
	keep = true
	return cmd, done, nil
}

// decodeReplayParams accepts exactly one JSON object. An absent captured params
// value is represented to the editor as {}, while a user-saved empty file is an
// explicit error handled before this function is called.
func decodeReplayParams(raw json.RawMessage, allowEmpty bool) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		if !allowEmpty {
			return nil, fmt.Errorf("edited params are empty")
		}
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("edited params are not valid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("edited params contain more than one JSON value")
		}
		return nil, fmt.Errorf("edited params are not valid JSON: %w", err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("edited params must be a JSON object")
	}
	return obj, nil
}

func replayParamsEquivalent(a, b json.RawMessage) bool {
	aObj, aErr := decodeReplayParams(a, true)
	bObj, bErr := decodeReplayParams(b, true)
	if aErr != nil || bErr != nil {
		return false
	}
	aCanonical, aErr := json.Marshal(aObj)
	bCanonical, bErr := json.Marshal(bObj)
	return aErr == nil && bErr == nil && bytes.Equal(aCanonical, bCanonical)
}

// replayContent renders the replay outcome for the overlay.
func (m Model) replayContent(msg replayDoneMsg) string {
	var b strings.Builder
	title := "REPLAY · " + msg.res.Method
	if m.replayEdited {
		title = "REPLAY · EDITED · " + msg.res.Method
	}
	b.WriteString(m.styles.panelTitle.Render(title) + "\n\n")

	if msg.err != nil {
		b.WriteString(m.styles.respErr.Render("failed: "+msg.err.Error()) + "\n")
		return b.String()
	}

	dur := msg.res.Duration.Round(time.Millisecond)
	switch {
	case msg.res.Err != nil:
		b.WriteString(m.styles.respErr.Render(fmt.Sprintf("ERROR %s  (%s)", rpcErrorText(*msg.res.Err), dur)) + "\n\n")
	case msg.res.ToolErr:
		b.WriteString(m.styles.respErr.Render(fmt.Sprintf("ERROR tool reported an error  (%s)", dur)) + "\n\n")
	default:
		b.WriteString(m.styles.resp.Render(fmt.Sprintf("ok  (%s)", dur)) + "\n\n")
	}

	if m.replayEdited {
		b.WriteString(m.styles.dim.Render("captured params") + "\n")
		b.WriteString(indentJSON(m.replayCaptured) + "\n\n")
	}
	if len(msg.res.Params) > 0 {
		label := "request params"
		if m.replayEdited {
			label = "params sent"
		}
		b.WriteString(m.styles.dim.Render(label) + "\n")
		b.WriteString(indentJSON(msg.res.Params) + "\n\n")
	}
	b.WriteString(m.styles.dim.Render("response") + "\n")
	b.WriteString(indentJSON(msg.res.Response))
	return b.String()
}

func indentJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(empty)"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
