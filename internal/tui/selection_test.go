package tui

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/backhand/kc/internal/k8s"
)

// Unit tests for the shared selection model's ←/→ preset cycling (Feature 1).
// Driven through handleSelectKey (the SAME entry point deploy/restart/scale all
// route the select phase through), so wiring all three is covered by exercising
// the one shared piece.

func selDeployments(names ...string) []k8s.Deployment {
	out := make([]k8s.Deployment, 0, len(names))
	for _, n := range names {
		out = append(out, k8s.Deployment{Name: n})
	}
	return out
}

func leftKey() tea.KeyMsg  { return tea.KeyMsg{Type: tea.KeyLeft} }
func rightKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRight} }
func digit(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// checkedSet is the current selection as a sorted name list (checkedNames).
func checkedSet(s *selection) []string { return s.checkedNames() }

// TestSelection_ArrowsCyclePresets is the core Feature-1 test: in the select
// phase, → activates the NEXT preset (selection == that preset exactly), ←
// the previous; it clamps at both ends; and the number-key toggles still work.
func TestSelection_ArrowsCyclePresets(t *testing.T) {
	deps := selDeployments("responder", "sender", "web")
	// Three presets, ranked. newSelection pre-checks the first one CONTAINING the
	// focused deployment ("sender") → that's preset index 1 ([sender web]).
	presets := [][]string{
		{"responder"},
		{"sender", "web"},
		{"web"},
	}
	s := newSelection(deps, presets, "sender")

	// Pre-check + active index land on the preset containing the focused deployment.
	if s.activePreset != 1 {
		t.Fatalf("activePreset = %d, want 1 (the first preset containing 'sender')", s.activePreset)
	}
	if got := checkedSet(&s); !reflect.DeepEqual(got, []string{"sender", "web"}) {
		t.Fatalf("initial checked = %v, want [sender web]", got)
	}

	// → activates the NEXT preset (index 2 = [web]); selection == that preset exactly.
	if !s.handleSelectKey(rightKey()) {
		t.Fatal("→ not consumed by handleSelectKey")
	}
	if s.activePreset != 2 {
		t.Errorf("after →, activePreset = %d, want 2", s.activePreset)
	}
	if got := checkedSet(&s); !reflect.DeepEqual(got, []string{"web"}) {
		t.Errorf("after →, checked = %v, want exactly [web]", got)
	}

	// → at the last preset CLAMPS (stays on index 2 = [web]).
	s.handleSelectKey(rightKey())
	if s.activePreset != 2 || !reflect.DeepEqual(checkedSet(&s), []string{"web"}) {
		t.Errorf("→ at end: activePreset=%d checked=%v, want clamp at 2 [web]", s.activePreset, checkedSet(&s))
	}

	// ← steps back to the previous preset (index 1 = [sender web]) exactly.
	s.handleSelectKey(leftKey())
	if s.activePreset != 1 {
		t.Errorf("after ←, activePreset = %d, want 1", s.activePreset)
	}
	if got := checkedSet(&s); !reflect.DeepEqual(got, []string{"sender", "web"}) {
		t.Errorf("after ←, checked = %v, want exactly [sender web]", got)
	}

	// ← back to index 0 = [responder].
	s.handleSelectKey(leftKey())
	if s.activePreset != 0 || !reflect.DeepEqual(checkedSet(&s), []string{"responder"}) {
		t.Errorf("after ←: activePreset=%d checked=%v, want 0 [responder]", s.activePreset, checkedSet(&s))
	}

	// ← at the first preset CLAMPS (stays on index 0 = [responder]).
	s.handleSelectKey(leftKey())
	if s.activePreset != 0 || !reflect.DeepEqual(checkedSet(&s), []string{"responder"}) {
		t.Errorf("← at start: activePreset=%d checked=%v, want clamp at 0 [responder]", s.activePreset, checkedSet(&s))
	}

	// Number-key toggles STILL work: "2" toggles preset index 1 ([sender web]) ON
	// (it isn't fully checked right now — only responder is), without disturbing
	// the active-preset index that ←/→ track.
	if !s.handleSelectKey(digit('2')) {
		t.Fatal("digit '2' not consumed by handleSelectKey")
	}
	if !s.checked["sender"] || !s.checked["web"] {
		t.Errorf("after '2': checked = %v, want sender+web toggled on", checkedSet(&s))
	}
	if !s.checked["responder"] {
		t.Error("'2' (togglePreset) must not clear responder — it toggles only its own preset")
	}
}

// TestSelection_ArrowsFromUnmatchedStartLandOnZero asserts that when the open
// fell back to {current} (no preset matched → activePreset == -1), BOTH ← and →
// land on the first preset (index 0).
func TestSelection_ArrowsFromUnmatchedStartLandOnZero(t *testing.T) {
	deps := selDeployments("responder", "sender")
	presets := [][]string{{"sender"}} // does NOT contain the focused "responder"
	for _, arrow := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"→", rightKey()},
		{"←", leftKey()},
	} {
		s := newSelection(deps, presets, "responder")
		if s.activePreset != -1 {
			t.Fatalf("[%s] activePreset = %d, want -1 (fell back to {current})", arrow.name, s.activePreset)
		}
		if got := checkedSet(&s); !reflect.DeepEqual(got, []string{"responder"}) {
			t.Fatalf("[%s] initial checked = %v, want [responder] (fallback to {current})", arrow.name, got)
		}
		s.handleSelectKey(arrow.key)
		if s.activePreset != 0 {
			t.Errorf("[%s] from -1, activePreset = %d, want 0", arrow.name, s.activePreset)
		}
		if got := checkedSet(&s); !reflect.DeepEqual(got, []string{"sender"}) {
			t.Errorf("[%s] from -1, checked = %v, want exactly [sender] (preset 0)", arrow.name, got)
		}
	}
}

// TestSelection_ArrowsNoOpWithoutPresets asserts ←/→ are a no-op when there are
// no presets (nothing to cycle): the selection and active index are unchanged.
func TestSelection_ArrowsNoOpWithoutPresets(t *testing.T) {
	s := newSelection(selDeployments("responder", "sender"), nil, "responder")
	if s.activePreset != -1 {
		t.Fatalf("activePreset = %d, want -1 with no presets", s.activePreset)
	}
	before := checkedSet(&s)
	for _, k := range []tea.KeyMsg{leftKey(), rightKey()} {
		// The keys are still consumed (matched), but cyclePreset no-ops.
		s.handleSelectKey(k)
		if s.activePreset != -1 {
			t.Errorf("activePreset = %d after arrow with no presets, want -1", s.activePreset)
		}
		if got := checkedSet(&s); !reflect.DeepEqual(got, before) {
			t.Errorf("checked changed to %v after arrow with no presets, want %v unchanged", got, before)
		}
	}
}
