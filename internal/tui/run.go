package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

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

// liveStore is the store the live TUI runs on. It is bounded, unlike the one
// check, export, diff and open build, because a hub is left running while a
// batch command reads a finite log once and has to see all of it.
//
// Split out from Run so the choice is a value a test can read rather than a
// literal inside a call no test can reach.
func liveStore() *store.Store {
	return store.NewBounded(store.DefaultLiveBodyLimit, store.DefaultLiveFrameLimit)
}

// RunWithHistoryLimit starts the live TUI with a bounded history replay.
// A historyLimit of 0 loads every session log.
func RunWithHistoryLimit(ctx context.Context, socketPath, sessionsDir string, historyLimit int) error {
	return runWithHistoryLimit(ctx, socketPath, sessionsDir, historyLimit)
}

func runWithHistoryLimit(ctx context.Context, socketPath, sessionsDir string, historyLimit int) error {
	st := liveStore()
	baselines := toolbaseline.New(paths.ToolBaselinesDir())
	hubCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(hubCtx))

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

	handler := func(e proxy.Envelope) {
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
	}
	options := hub.Options{
		BackfillLimit: historyLimit,
		OnBackfill: func(report hub.BackfillReport) {
			if report.Loaded < report.Total {
				p.Send(historyTruncatedMsg{loaded: report.Loaded, total: report.Total})
			}
		},
	}
	h := hub.NewWithOptions(socketPath, sessionsDir, handler, options)

	// A second hub on one MCPSNOOP_HOME cannot take the socket, and Run returns
	// before it backfills, so swallowing the error left the user staring at an
	// empty screen with the footer still claiming to listen. Say which socket is
	// taken and stop, rather than pretend to be the hub.
	hubErr := make(chan error, 1)
	go func() { hubErr <- h.Run(hubCtx) }()
	select {
	case err := <-hubErr:
		if errors.Is(err, hub.ErrHubRunning) {
			cancel()
			return fmt.Errorf("another mcpsnoop hub already owns %s; close it, or point this one at another MCPSNOOP_HOME", socketPath)
		}
		if err != nil {
			cancel()
			return err
		}
	case <-time.After(150 * time.Millisecond):
		// It took the socket, so it is the hub. Anything it reports later is a
		// shutdown, which the program exit already covers.
	}

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
func RunOpen(ctx context.Context, st *store.Store, opts ...Option) error {
	p := tea.NewProgram(New(st, opts...), tea.WithAltScreen(), tea.WithContext(ctx))
	go observeAllInBackground(p, st)
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// RunOpenWithInput starts the TUI using a preloaded store and a custom input reader (e.g., controlling TTY).
func RunOpenWithInput(ctx context.Context, st *store.Store, in io.Reader, opts ...Option) error {
	p := tea.NewProgram(New(st, opts...), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithInput(in))
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
