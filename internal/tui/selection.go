package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
)

// selection is the shared deployment checkbox + preset model used by BOTH the
// deploy modal's `select` phase and the restart modal's `select` phase. It
// owns the list of deployments, the current checked set, the learned presets
// (one-key chips, most-likely first) and the focused row — plus the keys that
// drive them (space toggles a row, 1-9 toggle a preset, ↑/↓ move the cursor).
//
// Factoring this out keeps deploy and restart's select screens behaving
// identically (same UI, same keys, same preselect rule) with a single source of
// truth. Deploy adds a version step after select; restart goes straight to
// confirm — but the selection itself is the same piece.
type selection struct {
	// deployments in the namespace (with container names), in display order.
	deployments []k8s.Deployment
	// checked is the current selection, keyed by deployment name.
	checked map[string]bool
	// presets are the learned deployment-sets (most-likely first). Chips
	// 1..len(presets) toggle a whole preset.
	presets [][]string
	// cursor is the focused row.
	cursor int
}

// newSelection builds a selection, preselecting the set that contains the
// deployment the user is focused on (`current`) — SPEC "predict, then confirm":
//
//   - Pre-check the FIRST learned preset (in ranked order) that contains
//     `current`, intersected with the deployments that still exist.
//   - If NO preset contains `current`, pre-check just {current}.
//   - If `current` is empty (couldn't be determined — shouldn't happen from the
//     namespace/deployment views these modals open from), fall back to the old
//     behavior: pre-check presets[0].
//
// The cursor starts on `current`'s row (0 when it can't be located).
func newSelection(deployments []k8s.Deployment, presets [][]string, current string) selection {
	s := selection{
		deployments: deployments,
		checked:     map[string]bool{},
		presets:     presets,
	}
	existing := s.existing()

	switch {
	case current == "":
		// current unknown (shouldn't happen from the namespace/deployment views the
		// modals open from) → previous behavior: pre-check the top preset (kept to
		// names that still exist), and fall back to the sole deployment when there
		// is exactly one and nothing learned, so the modal never opens empty.
		if len(presets) > 0 {
			s.checkAll(presets[0], existing)
		}
		if !s.anyChecked() && len(deployments) == 1 {
			s.checked[deployments[0].Name] = true
		}
	default:
		// Pre-check the first preset that contains current, else just {current}.
		if preset, ok := firstPresetContaining(presets, current, existing); ok {
			s.checkAll(preset, existing)
		} else if existing[current] {
			s.checked[current] = true
		}
		s.cursor = s.rowOf(current)
	}
	return s
}

// firstPresetContaining returns the first preset (ranked order) that contains
// `current` as an existing deployment. ok is false when none do.
func firstPresetContaining(presets [][]string, current string, existing map[string]bool) ([]string, bool) {
	if !existing[current] {
		return nil, false
	}
	for _, p := range presets {
		for _, name := range p {
			if name == current && existing[name] {
				return p, true
			}
		}
	}
	return nil, false
}

// existing is the set of deployment names that currently exist.
func (s *selection) existing() map[string]bool {
	out := make(map[string]bool, len(s.deployments))
	for _, d := range s.deployments {
		out[d.Name] = true
	}
	return out
}

// checkAll checks every name in `names` that still exists.
func (s *selection) checkAll(names []string, existing map[string]bool) {
	for _, name := range names {
		if existing[name] {
			s.checked[name] = true
		}
	}
}

// rowOf returns the row index of a deployment by name, or 0 when not found.
func (s *selection) rowOf(name string) int {
	for i, d := range s.deployments {
		if d.Name == name {
			return i
		}
	}
	return 0
}

// moveUp / moveDown move the focused row, clamped to the list.
func (s *selection) moveUp() {
	if s.cursor > 0 {
		s.cursor--
	}
}

func (s *selection) moveDown() {
	if s.cursor < len(s.deployments)-1 {
		s.cursor++
	}
}

// toggle flips the focused row's checkbox.
func (s *selection) toggle() {
	if s.cursor >= 0 && s.cursor < len(s.deployments) {
		name := s.deployments[s.cursor].Name
		s.checked[name] = !s.checked[name]
	}
}

// togglePreset toggles a whole preset: if every (existing) name in the preset is
// already checked, uncheck them all; otherwise check them all (so a chip is a
// single-key "select this set"). Names no longer present are skipped.
func (s *selection) togglePreset(preset []string) {
	existing := s.existing()
	allOn := true
	for _, name := range preset {
		if existing[name] && !s.checked[name] {
			allOn = false
			break
		}
	}
	for _, name := range preset {
		if existing[name] {
			s.checked[name] = !allOn
		}
	}
}

// anyChecked reports whether at least one deployment is checked.
func (s *selection) anyChecked() bool {
	for _, v := range s.checked {
		if v {
			return true
		}
	}
	return false
}

// checkedNames returns the checked deployment names, sorted (stable plan/record).
func (s *selection) checkedNames() []string {
	out := []string{}
	for name, on := range s.checked {
		if on {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// presetFullyChecked reports whether every (existing) name in a preset is
// currently checked — so a chip can render "active".
func (s *selection) presetFullyChecked(preset []string) bool {
	existing := s.existing()
	any := false
	for _, name := range preset {
		if !existing[name] {
			continue
		}
		any = true
		if !s.checked[name] {
			return false
		}
	}
	return any
}

// handleSelectKey applies the shared select-phase navigation/toggle keys
// (↑/↓ move, space toggles the row, 1-9 toggle a preset) to the selection,
// returning true when the key was consumed. The phase-specific keys (Confirm to
// advance, Cancel to back out) stay with each modal's own handler.
func (s *selection) handleSelectKey(msg tea.KeyMsg) bool {
	switch {
	case key.Matches(msg, keys.Up):
		s.moveUp()
		return true
	case key.Matches(msg, keys.Down):
		s.moveDown()
		return true
	case key.Matches(msg, keys.Toggle):
		s.toggle()
		return true
	}
	// Number keys 1..9 toggle a whole preset.
	if n, ok := digitKey(msg); ok && n >= 1 && n <= len(s.presets) {
		s.togglePreset(s.presets[n-1])
		return true
	}
	return false
}

// renderSelectList renders the shared checkbox list + preset chips for a
// selection. Both the deploy and restart `select` phases use it, so their lists
// are visually identical. The caller adds the phase-specific title, any extra
// notes (e.g. deploy's "no ghcr image"), and the footer hint.
func renderSelectList(s *selection) string {
	var b strings.Builder

	for i, d := range s.deployments {
		box := "[ ]"
		if s.checked[d.Name] {
			box = checkOnStyle.Render("[x]")
		}
		marker := "  "
		name := d.Name
		ver := d.Image.Tag
		if ver == "" {
			ver = "—"
		}
		line := fmt.Sprintf("%s %-22s %s", box, truncate(name, 22), headerStyle.Render(ver))
		if i == s.cursor {
			marker = "› "
			line = selectedStyle.Render(fmt.Sprintf("%s %-22s ", box, truncate(name, 22))) + headerStyle.Render(ver)
		}
		b.WriteString(marker + line + "\n")
	}

	// Preset chips (one-key toggles for learned deployment-sets).
	if len(s.presets) > 0 {
		b.WriteString("\n" + headerStyle.Render("presets:") + " ")
		chips := make([]string, 0, len(s.presets))
		for i, p := range s.presets {
			label := fmt.Sprintf("%d:%s", i+1, strings.Join(p, "+"))
			style := chipStyle
			if s.presetFullyChecked(p) {
				style = chipOnStyle
			}
			chips = append(chips, style.Render(label))
		}
		b.WriteString(strings.Join(chips, "  ") + "\n")
	}
	return b.String()
}
