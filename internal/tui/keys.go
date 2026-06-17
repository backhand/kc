package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap holds every binding so the help text (bubbles/help) stays in lockstep
// with the actual handlers. The d/r/l/s operation keys are declared so they
// appear in the footer hint, but they are NOT wired to any handler — operations
// are step 4 (deploy/restart/logs/shell). They are intentionally inert here.
type keyMap struct {
	Up    key.Binding
	Down  key.Binding
	Enter key.Binding
	Back  key.Binding
	Help  key.Binding
	Quit  key.Binding

	// Reserved operation hints (step 4 — not handled yet).
	Deploy  key.Binding
	Restart key.Binding
	Logs    key.Binding
	Shell   key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter", "l", "right"),
		key.WithHelp("enter", "drill in"),
	),
	Back: key.NewBinding(
		key.WithKeys("backspace", "esc", "h", "left"),
		key.WithHelp("backspace", "zoom out"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
	Deploy: key.NewBinding(
		key.WithKeys("d"),
		key.WithHelp("d", "deploy"),
	),
	Restart: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "restart"),
	),
	Logs: key.NewBinding(
		key.WithKeys("L"),
		key.WithHelp("L", "logs"),
	),
	Shell: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "shell"),
	),
}

// Note: Enter binds "l"/right and Back binds "h"/left so vi-style horizontal
// motion zooms the stack, matching ↑/↓ + j/k for vertical motion. "L" (capital)
// is the logs hint to avoid colliding with the lowercase "l" drill-in.

// ShortHelp / FullHelp implement help.KeyMap so bubbles/help can render the
// footer. ShortHelp is the one-line strip; FullHelp is the `?` expansion.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.Back, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Back},
		{k.Deploy, k.Restart, k.Logs, k.Shell},
		{k.Help, k.Quit},
	}
}
