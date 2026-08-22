package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/replay"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// replayDoneMsg carries the outcome of an async replay back into Update.
type replayDoneMsg struct {
	// token names the run this result belongs to, so a result the user walked
	// away from cannot be rendered against the run that replaced it.
	token uint64
	res   replay.Result
	err   error
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

func replayCmd(token uint64, command []string, cwd, method string, params json.RawMessage) tea.Cmd {
	return func() tea.Msg {
		res, err := replay.Replay(context.Background(), command, cwd, method, params, replayTimeout)
		return replayDoneMsg{token: token, res: res, err: err}
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
// callback separately, which keeps the editor boundary directly testable.
//
// The whole value is tried as one executable first, so a path containing a space
// works on its own. Anything else is split on whitespace, which covers the
// common command-plus-flags values such as "code --wait" without running a
// shell. What that cannot do is both at once, since a quoted path carrying flags
// needs a shell to unquote it, so that case is refused by name rather than left
// to fail as a mangled argv[0].
func prepareReplayEditor(call store.CallView, captured json.RawMessage) (*exec.Cmd, tea.ExecCallback, error) {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return nil, nil, fmt.Errorf("set $VISUAL or $EDITOR to edit replay params")
	}

	if _, err := decodeReplayParams(call.Params, true); err != nil {
		return nil, nil, fmt.Errorf("captured params: %w", err)
	}
	pretty, err := indentReplayParams(call.Params)
	if err != nil {
		return nil, nil, fmt.Errorf("format captured params: %w", err)
	}

	// A directory rather than a bare file, removed whole. Editors write companions
	// beside the file they were given, a vim swap or undo file, an emacs autosave,
	// a rename-style backup, and those hold the same captured bytes under whatever
	// name that editor chose. Unlinking the one path mcpsnoop knows about leaves
	// them behind.
	dir, err := os.MkdirTemp("", "mcpsnoop-replay-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create editor buffer: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, "params.json")
	if err := os.WriteFile(path, append(pretty, '\n'), 0o600); err != nil {
		return nil, nil, fmt.Errorf("write editor buffer: %w", err)
	}

	parts := []string{editor}
	if _, err := exec.LookPath(editor); err != nil {
		parts = strings.Fields(editor)
	}
	if len(parts) == 0 || parts[0] == "" {
		return nil, nil, errors.New("$VISUAL or $EDITOR is empty")
	}
	if _, err := exec.LookPath(parts[0]); err != nil && len(parts) > 1 {
		return nil, nil, fmt.Errorf("neither %q nor %q is an executable; mcpsnoop splits $VISUAL and $EDITOR on whitespace and runs no shell, so a path containing a space cannot also carry flags",
			editor, parts[0])
	}
	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	call.Params = append(json.RawMessage(nil), call.Params...)
	captured = append(json.RawMessage(nil), captured...)
	seeded := append([]byte(nil), append(pretty, '\n')...)
	done := func(runErr error) tea.Msg {
		defer os.RemoveAll(dir)
		if runErr != nil {
			return replayEditDoneMsg{err: fmt.Errorf("editor failed: %w", runErr)}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return replayEditDoneMsg{err: fmt.Errorf("read editor buffer: %w", err)}
		}
		// An unchanged buffer is not an edit, and R must not send on one. An editor
		// that hands the file to an already-running instance and exits, which is
		// what EDITOR=code without --wait and emacsclient -n do, returns here
		// before the user has typed anything, and sending then would fire a live
		// request at the server while they are still looking at the window.
		if bytes.Equal(raw, seeded) {
			return replayEditDoneMsg{err: errors.New("the editor saved no change, so nothing was sent; press r to replay the captured params as they are")}
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return replayEditDoneMsg{err: errors.New("edited params are empty")}
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

// indentReplayParams renders the captured params for the editor from the bytes
// the server actually sent, rather than from a decoded copy of them.
//
// encoding/json escapes &, < and > when it marshals, and it sorts the keys of a
// map, so a decode-and-re-encode showed a URL argument as
// "https://host?a=1&b=2" and reordered every object the user was about to
// edit. internal/jsonwire exists for the first half of that, and json.Indent for
// both: it rewrites nothing but the whitespace between tokens.
func indentReplayParams(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

// replayHTTPCmd sends a captured request to a live endpoint over HTTP, off the
// UI goroutine, and reports the outcome under the token of the run that asked
// for it so a result the user walked away from cannot be rendered against the
// run that replaced it.
func replayHTTPCmd(token uint64, target replay.HTTPTarget, method string, params json.RawMessage, routing replay.Routing) tea.Cmd {
	return func() tea.Msg {
		res, err := replay.ReplayHTTP(context.Background(), target, method, params, routing, replayTimeout)
		return replayDoneMsg{token: token, res: res, err: err}
	}
}
