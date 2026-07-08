package tui

import (
	"context"
	"errors"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// Run starts the hub and the live TUI. It blocks until the user quits or ctx is
// cancelled. The hub feeds the store and nudges the program on every frame; a
// periodic tick in the model catches anything sent before the program loop is
// ready and keeps pending-call timers live.
func Run(ctx context.Context, socketPath, sessionsDir string, slow time.Duration) error {
	st := store.New(slow)
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx))

	h := hub.New(socketPath, sessionsDir, func(e proxy.Envelope) {
		st.Ingest(e)
		p.Send(frameMsg{})
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
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx))

	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// RunOpenWithInput starts the TUI using a preloaded store and a custom input reader (e.g., controlling TTY).
func RunOpenWithInput(ctx context.Context, st *store.Store, in io.Reader) error {
	p := tea.NewProgram(New(st), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithInput(in))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
