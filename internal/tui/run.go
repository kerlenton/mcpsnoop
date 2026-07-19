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

	hubCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Baseline observation reads and sometimes writes files, so keep it off the
	// frame-delivery goroutine. A single worker observes the sessions handed to it
	// and feeds drift back through the same frame nudge the callback uses. One worker
	// also serializes observations within the process, so the trust-on-first-use
	// handling in the manager is never raced from here.
	observe := make(chan string, 256)
	go func() {
		for {
			select {
			case sessionID := <-observe:
				observeAndNudge(baselines, st, sessionID, func() { p.Send(frameMsg{}) })
			case <-hubCtx.Done():
				return
			}
		}
	}()

	h := hub.NewWithOptions(socketPath, sessionsDir, func(e proxy.Envelope) {
		event := st.Ingest(e)
		if event.Kind == store.EventResponse && event.Call != nil && event.Call.Method == "tools/list" {
			if _, complete := st.ToolDefinitions(e.SessionID); complete {
				// Non-blocking hand-off; the delivery path must never wait on baseline IO.
				// The buffer is far larger than any realistic burst of tools/list results.
				select {
				case observe <- e.SessionID:
				default:
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

	go func() { _ = h.Run(hubCtx) }()

	_, err := p.Run()
	cancel() // stop the hub and the observation worker once the UI exits
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// observeAndNudge observes one session's tool baseline, records a per-session
// BaselineError on failure, and nudges the UI so the result renders. It runs off
// the frame-delivery path (a worker or a background goroutine).
func observeAndNudge(m *toolbaseline.Manager, st *store.Store, sessionID string, nudge func()) {
	if _, _, err := toolbaseline.ObserveSession(m, st, sessionID); err != nil {
		st.SetToolDrift(sessionID, store.ToolDrift{BaselineError: err.Error()})
	}
	nudge()
}

// RunOpen starts the TUI using a preloaded store without starting the live hub.
func RunOpen(ctx context.Context, st *store.Store) error {
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx))
	go observeAllInBackground(p, st)
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// RunOpenWithInput starts the TUI using a preloaded store and a custom input reader (e.g., controlling TTY).
func RunOpenWithInput(ctx context.Context, st *store.Store, in io.Reader) error {
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithInput(in))
	go observeAllInBackground(p, st)
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// observeAllInBackground observes every session's baseline off the startup path,
// nudging the UI after each so a large capture renders immediately and drift
// markers fill in incrementally instead of blocking the first frame.
func observeAllInBackground(p *tea.Program, st *store.Store) {
	toolbaseline.ObserveAll(toolbaseline.New(paths.ToolBaselinesDir()), st, func() {
		p.Send(frameMsg{})
	})
}
