package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/replay"
)

// replayDoneMsg carries the outcome of an async replay back into Update.
type replayDoneMsg struct {
	res replay.Result
	err error
}

const replayTimeout = 15 * time.Second

func replayCmd(command []string, cwd, method string, params json.RawMessage) tea.Cmd {
	return func() tea.Msg {
		res, err := replay.Replay(context.Background(), command, cwd, method, params, replayTimeout)
		return replayDoneMsg{res: res, err: err}
	}
}

// replayContent renders the replay outcome for the overlay.
func (m Model) replayContent(msg replayDoneMsg) string {
	var b strings.Builder
	b.WriteString(m.styles.panelTitle.Render("REPLAY — "+msg.res.Method) + "\n\n")

	if msg.err != nil {
		b.WriteString(m.styles.respErr.Render("failed: "+msg.err.Error()) + "\n")
		return b.String()
	}

	dur := msg.res.Duration.Round(time.Millisecond)
	switch {
	case msg.res.Err != nil:
		b.WriteString(m.styles.respErr.Render(fmt.Sprintf("ERROR %d: %s  (%s)", msg.res.Err.Code, msg.res.Err.Message, dur)) + "\n\n")
	default:
		b.WriteString(m.styles.resp.Render(fmt.Sprintf("ok  (%s)", dur)) + "\n\n")
	}

	if len(msg.res.Params) > 0 {
		b.WriteString(m.styles.dim.Render("request params") + "\n")
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
