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
	// activePreset tracks which preset ←/→ last activated (an index into
	// presets), so the active chip can render distinctly and ←/→ cycle from it.
	// Initialised to the preset newSelection pre-checked (the first containing the
	// focused deployment), or -1 when the open fell back to {current}/{top}. ←/→
	// clamp it to [0, len(presets)-1]; from -1 both arrows land on 0.
	activePreset int
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
		deployments:  deployments,
		checked:      map[string]bool{},
		presets:      presets,
		activePreset: -1, // -1 until a preset is pre-checked (or ←/→ activates one)
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
			s.activePreset = 0
		}
		if !s.anyChecked() && len(deployments) == 1 {
			s.checked[deployments[0].Name] = true
		}
	default:
		// Pre-check the first preset that contains current, else just {current}.
		if idx, ok := firstPresetContaining(presets, current, existing); ok {
			s.checkAll(presets[idx], existing)
			s.activePreset = idx
		} else if existing[current] {
			s.checked[current] = true
		}
		s.cursor = s.rowOf(current)
	}
	return s
}

// firstPresetContaining returns the index of the first preset (ranked order)
// that contains `current` as an existing deployment. ok is false when none do.
func firstPresetContaining(presets [][]string, current string, existing map[string]bool) (int, bool) {
	if !existing[current] {
		return -1, false
	}
	for i, p := range presets {
		for _, name := range p {
			if name == current && existing[name] {
				return i, true
			}
		}
	}
	return -1, false
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

// setToPreset SETS the selection to exactly the preset at index i: its existing
// names checked, everything else unchecked, and i recorded as the active preset.
// Used by ←/→ — distinct from togglePreset (the number keys), which flips a
// preset on/off without disturbing the rest of the selection.
func (s *selection) setToPreset(i int) {
	if i < 0 || i >= len(s.presets) {
		return
	}
	existing := s.existing()
	s.checked = map[string]bool{}
	for _, name := range s.presets[i] {
		if existing[name] {
			s.checked[name] = true
		}
	}
	s.activePreset = i
}

// cyclePreset moves the active preset by delta (-1 for ←, +1 for →) through
// 1..n and SETS the selection to it. Clamped to [0, len(presets)-1]; from the
// initial -1 (no preset pre-checked) both arrows land on 0. A no-op when there
// are no presets.
func (s *selection) cyclePreset(delta int) {
	if len(s.presets) == 0 {
		return
	}
	next := s.activePreset + delta
	if s.activePreset < 0 {
		next = 0 // from -1, either arrow lands on the first preset
	}
	if next < 0 {
		next = 0
	}
	if next > len(s.presets)-1 {
		next = len(s.presets) - 1
	}
	s.setToPreset(next)
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
// (↑/↓ move, ←/→ cycle the active preset, space toggles the row, 1-9 toggle a
// preset) to the selection, returning true when the key was consumed. The
// phase-specific keys (Confirm to advance, Cancel to back out) stay with each
// modal's own handler.
//
// ←/→ match tea.KeyLeft/tea.KeyRight directly (not keys.Up/Down) — they each SET
// the selection to one preset and cycle the active index, which is distinct from
// the number keys' per-preset toggle. The select-phase handlers run Cancel/Confirm
// (esc/enter) first, and neither is bound to ←/→, so the arrows reach here.
func (s *selection) handleSelectKey(msg tea.KeyMsg) bool {
	switch {
	case key.Matches(msg, keys.Up):
		s.moveUp()
		return true
	case key.Matches(msg, keys.Down):
		s.moveDown()
		return true
	case msg.Type == tea.KeyLeft:
		s.cyclePreset(-1)
		return true
	case msg.Type == tea.KeyRight:
		s.cyclePreset(+1)
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

	// Preset chips (one-key toggles for learned deployment-sets). The chip ←/→
	// last activated is marked with a leading "›" and the active style, so it's
	// clear which preset the arrows are cycling through.
	if len(s.presets) > 0 {
		b.WriteString("\n" + headerStyle.Render("presets:") + " ")
		chips := make([]string, 0, len(s.presets))
		for i, p := range s.presets {
			label := fmt.Sprintf("%d:%s", i+1, strings.Join(p, "+"))
			style := chipStyle
			if s.presetFullyChecked(p) {
				style = chipOnStyle
			}
			if i == s.activePreset {
				label = "›" + label
				style = chipActiveStyle
			}
			chips = append(chips, style.Render(label))
		}
		b.WriteString(strings.Join(chips, "  ") + "\n")
	}
	return b.String()
}
