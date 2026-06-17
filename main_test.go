package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

// TestSmoke drives the app headlessly through teatest: render the bordered box,
// move the highlight down past the bottom edge, back up, then quit with `q`.
// It asserts the box renders, navigation moves the cursor, and the program
// exits cleanly (FinalModel returns => Run() finished without a wedge).
func TestSmoke(t *testing.T) {
	tm := teatest.NewTestModel(t, initialModel(), teatest.WithInitialTermSize(80, 24))

	// First paint must contain the title and the list items.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("kc — Kubernetes operations")) &&
			bytes.Contains(b, []byte("all-namespaces")) &&
			bytes.Contains(b, []byte("pods"))
	}, teatest.WithDuration(3*time.Second))

	// Navigate: 'j' down a few times (and one past the end to prove clamping),
	// then 'k' back up, then a final down. Cursor should land on index 1.
	for i := 0; i < 6; i++ {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	for i := 0; i < 5; i++ {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})

	// Quit with 'q'.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	// FinalModel returning means the event loop tore down cleanly.
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	final, ok := fm.(model)
	if !ok {
		t.Fatalf("final model has unexpected type %T", fm)
	}
	if final.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after 6×down(clamped) + 5×up + 1×down", final.cursor)
	}

	// The marker on the highlighted row should sit on the second item.
	view := final.View()
	if !strings.Contains(view, "› app group (mailon-*)") {
		t.Fatalf("highlight not on second item; view:\n%s", view)
	}

	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}
