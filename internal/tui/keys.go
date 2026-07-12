package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap defines table navigation, move with j/k, page with ctrl-f/ctrl-b, drill
// in with enter, back out with esc, jump views with ":", filter with "/",
// help with "?".
type keyMap struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding
	Bottom   key.Binding

	Enter   key.Binding
	Back    key.Binding // esc / q, pop the view stack, at the root it does nothing
	Filter  key.Binding
	Command key.Binding
	Help    key.Binding

	Replay  key.Binding
	Caps    key.Binding
	Summary key.Binding
	Pause   key.Binding
	Follow  key.Binding
	Copy    key.Binding
	Export  key.Binding
	Delete  key.Binding

	Quit key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("j/k", "up/down")),
		Down:     key.NewBinding(key.WithKeys("down", "j")),
		PageUp:   key.NewBinding(key.WithKeys("ctrl+b", "pgup"), key.WithHelp("ctrl-b", "page up")),
		PageDown: key.NewBinding(key.WithKeys("ctrl+f", "pgdown"), key.WithHelp("ctrl-f", "page down")),
		Top:      key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g/G", "top/bottom")),
		Bottom:   key.NewBinding(key.WithKeys("G", "end")),

		Enter:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),
		Back:    key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("esc", "back")),
		Filter:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Command: key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "command")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),

		Replay:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "replay")),
		Caps:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "capabilities")),
		Summary: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "tool summary")),
		Pause:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
		Follow:  key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow")),
		Copy:    key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy JSON")),
		Export:  key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "export HTML")),
		Delete:  key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl-d", "delete session")),

		Quit: key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp(":q", "quit")),
	}
}
