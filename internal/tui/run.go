package tui

import (
	"context"
	"errors"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/toolbaseline"
)

// Run starts the hub and the live TUI. It blocks until the user quits or ctx is
// cancelled. The hub feeds the store and nudges the program on every frame, and
// a periodic tick in the model catches anything sent before the program loop is
// ready and keeps pending-call timers live.
func Run(ctx context.Context, socketPath, sessionsDir string) error {
	return RunWithHistoryLimit(ctx, socketPath, sessionsDir, hub.DefaultBackfillLimit)
}

// RunWithHistoryLimit starts the live TUI with a bounded history replay.
// A historyLimit of 0 loads every session log.
func RunWithHistoryLimit(ctx context.Context, socketPath, sessionsDir string, historyLimit int) error {
	st := store.New()
	baselines := toolbaseline.New(paths.ToolBaselinesDir())
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx))

	h := hub.NewWithOptions(socketPath, sessionsDir, func(e proxy.Envelope) {
		event := st.Ingest(e)
		if event.Kind == store.EventResponse && event.Call != nil && event.Call.Method == "tools/list" {
			if _, complete := st.ToolDefinitions(e.SessionID); complete {
				if _, _, err := toolbaseline.ObserveSession(baselines, st, e.SessionID); err != nil {
					st.SetToolDrift(e.SessionID, store.ToolDrift{BaselineError: err.Error()})
				}
			}
		}
		p.Send(frameMsg{})
	}, hub.Options{
		BackfillLimit: historyLimit,
		OnBackfill: func(report hub.BackfillReport) {
			if report.Loaded < report.Total {
				p.Send(historyTruncatedMsg{loaded: report.Loaded, total: report.Total})
			}
		},
	})

	hubCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = h.Run(hubCtx) }()

	_, err := p.Run()
	cancel() // stop the hub once the UI exits
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// RunOpen starts the TUI using a preloaded store without starting the live hub.
func RunOpen(ctx context.Context, st *store.Store) error {
	toolbaseline.ObserveAll(toolbaseline.New(paths.ToolBaselinesDir()), st)
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx))

	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// RunOpenWithInput starts the TUI using a preloaded store and a custom input reader (e.g., controlling TTY).
func RunOpenWithInput(ctx context.Context, st *store.Store, in io.Reader) error {
	toolbaseline.ObserveAll(toolbaseline.New(paths.ToolBaselinesDir()), st)
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithInput(in))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
