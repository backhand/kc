package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Rendering for the scale modal (the `s` op). Mirrors the restart modal's
// frames, swapping restart's confirm step for a replica-count step that doubles
// as the confirm (it shows the selected set + the target). Styling reuses
// view.go's palette and the deploy/restart rollout look (rolloutLine).

// renderScaleModal dispatches to the active scale phase. Scale is restart's flow
// with a replica-count step instead of confirm-only: select a SET → enter a
// count → per-deployment rollout (the rollout view is visually identical).
func (m Model) renderScaleModal() string {
	ss := m.scaleModal
	var b strings.Builder
	title := fmt.Sprintf("scale — %s", ss.namespace)
	b.WriteString(titleStyle.Render("kc") + "  " + titleStyle.Render(title) + "\n\n")

	switch ss.phase {
	case scaleSelect:
		b.WriteString(m.renderScaleSelect(ss))
	case scaleReplicas:
		b.WriteString(m.renderScaleReplicas(ss))
	case scaleRollout:
		b.WriteString(m.renderScaleRollout(ss))
	}
	return b.String()
}

// renderScaleSelect renders the deployment checkboxes + preset chips (shared with
// deploy/restart's select phase via renderSelectList).
func (m Model) renderScaleSelect(ss *scaleState) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("select deployments to scale") + "\n\n")
	b.WriteString(renderSelectList(&ss.sel))
	// When opened from a pods view, make the pod → parent-deployment focus
	// explicit (the cursor preselected that deployment's set).
	if ss.origin != "" {
		b.WriteString("\n" + headerStyle.Render("from: ") + lipgloss.NewStyle().Foreground(accent).Render(ss.origin) + "\n")
	}
	hint := "space toggle · 1-9 preset · enter replicas › · esc cancel"
	b.WriteString("\n" + footerStyle.Render(hint))
	return b.String()
}

// renderScaleReplicas renders the replica-count step: the selected set + the
// target replica count (so it doubles as the confirm). The numeric input shows a
// caret so it reads as an editable field.
func (m Model) renderScaleReplicas(ss *scaleState) string {
	var b strings.Builder
	names := ss.sel.checkedNames()
	b.WriteString(headerStyle.Render("scale — set the target replica count:") + "\n\n")
	for _, name := range names {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(accent).Render("deployment/"+name) + "\n")
	}

	// The editable replica field (caret shows where the next digit lands). A "0"
	// target is an explicit pause; flag it so the operator sees it as intentional.
	field := selectedStyle.Render(ss.replicas + "▏")
	b.WriteString("\n" + headerStyle.Render("replicas: ") + field)
	if ss.replicas == "0" {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(warn).Render("(scale to zero — pause)"))
	}
	b.WriteString("\n")
	b.WriteString("\n" + headerStyle.Render(fmt.Sprintf("via: kubectl scale --replicas=%s  (%d deployment(s))", ss.replicas, len(names))) + "\n")

	hint := lipgloss.NewStyle().Foreground(warn).Render("enter to SCALE") + footerStyle.Render(" · digits edit · backspace · esc back")
	b.WriteString("\n" + hint)
	return b.String()
}

// renderScaleRollout renders per-deployment rollout status (identical look to the
// deploy/restart rollout phase).
func (m Model) renderScaleRollout(ss *scaleState) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("rollout") + "\n\n")
	for _, l := range ss.rollouts {
		var status string
		switch l.state {
		case rolloutRunning, rolloutPending:
			status = lipgloss.NewStyle().Foreground(warn).Render("↻ scaling…")
		case rolloutDone:
			status = lipgloss.NewStyle().Foreground(good).Render("✓ done")
		case rolloutFailed:
			status = lipgloss.NewStyle().Foreground(bad).Render("✗ failed")
		}
		b.WriteString(fmt.Sprintf("  %-22s %s\n", truncate(l.deployment, 22), status))
		if l.detail != "" {
			b.WriteString(hintStyle.Render("      "+truncate(l.detail, 64)) + "\n")
		}
	}

	var hint string
	if rolloutSettled(ss.rollouts) {
		hint = footerStyle.Render("enter/esc to close")
	} else {
		hint = footerStyle.Render("esc to close (scale continues)")
	}
	b.WriteString("\n" + hint)
	return b.String()
}
