// Command kc is a keyboard-driven, Midnight-Commander-style CLI for daily
// Kubernetes operations. See SPEC.md for the design.
//
// This file is the de-risking spike skeleton: it proves the Go + Bubble Tea
// stack renders a bordered, navigable TUI and compiles to a small, static,
// portable binary that cross-compiles to Linux in one command. It deliberately
// contains NO kc features (no kubectl/gh/git, no views, no cache, no deploy);
// those land on top of this base per SPEC.md's build order.
package main

import (
	"fmt"
	"os"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// keyMap holds the navigation/quit bindings so help text stays in sync with
// the actual handlers.
type keyMap struct {
	Up   key.Binding
	Down key.Binding
	Quit key.Binding
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
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			Padding(0, 1)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#7D56F4")).
			Padding(1, 2)

	itemStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	selectedItemStyle = lipgloss.NewStyle().
				PaddingLeft(0).
				Foreground(lipgloss.Color("#7D56F4")).
				Bold(true)

	footerStyle = lipgloss.NewStyle().
			Faint(true).
			MarginTop(1)
)

// model is the Bubble Tea state: a single bordered list with a highlighted row.
type model struct {
	title  string
	items  []string
	cursor int
}

func initialModel() model {
	return model{
		title: "kc — Kubernetes operations",
		items: []string{
			"all-namespaces",
			"app group (mailon-*)",
			"namespace",
			"deployment",
			"pods",
		},
	}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Up):
			if m.cursor > 0 {
				m.cursor--
			}
		case key.Matches(msg, keys.Down):
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		}
	}
	return m, nil
}

func (m model) View() string {
	var body string
	body += titleStyle.Render(m.title) + "\n\n"

	for i, item := range m.items {
		if i == m.cursor {
			body += selectedItemStyle.Render("› "+item) + "\n"
		} else {
			body += itemStyle.Render(item) + "\n"
		}
	}

	box := boxStyle.Render(body)
	footer := footerStyle.Render("↑/↓ or j/k to move · q to quit")
	return box + "\n" + footer + "\n"
}

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "kc: %v\n", err)
		os.Exit(1)
	}
}
