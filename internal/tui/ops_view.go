package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Rendering for the contextual operations: the footer op-key hints and the
// restart confirm/rollout modal. Plain, scannable frames reusing view.go's
// palette and the deploy rollout look (rolloutLine) so restart's rollout view is
// visually identical to deploy's.

// renderOpHints renders the "[d]eploy [r]estart [l]ogs [s]hell" footer strip.
// Deploy brightens when a deploy is available (a namespace/deployment view with
// deployments); restart/logs/shell brighten when an op target is selectable (a
// namespace or pods view) — otherwise faint.
func (m Model) renderOpHints() string {
	deployHint := footerStyle.Render("[d]eploy")
	if m.deployContextAvailable() {
		deployHint = hintStyle.Render("[d]eploy")
	}
	rls := footerStyle.Render("[r]estart [l]ogs [s]hell")
	if m.opContextAvailable() {
		rls = hintStyle.Render("[r]estart [l]ogs [s]hell")
	}
	return deployHint + " " + rls
}

// renderRestartModal dispatches to the active restart phase. Restart is deploy's
// flow minus the version step: select a SET → confirm → per-deployment rollout
// (the rollout view is visually identical to deploy's).
func (m Model) renderRestartModal() string {
	rs := m.restartModal
	var b strings.Builder
	title := fmt.Sprintf("restart — %s", rs.namespace)
	b.WriteString(titleStyle.Render("kc") + "  " + titleStyle.Render(title) + "\n\n")

	switch rs.phase {
	case restartSelect:
		b.WriteString(m.renderRestartSelect(rs))
	case restartConfirm:
		b.WriteString(m.renderRestartConfirm(rs))
	case restartRollout:
		b.WriteString(m.renderRestartRollout(rs))
	}
	return b.String()
}

// renderRestartSelect renders the deployment checkboxes + preset chips (shared
// with deploy's select phase via renderSelectList).
func (m Model) renderRestartSelect(rs *restartState) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("select deployments to restart") + "\n\n")
	b.WriteString(renderSelectList(&rs.sel))
	// When opened from a pods view, make the pod → parent-deployment focus
	// explicit (the cursor preselected that deployment's set).
	if rs.origin != "" {
		b.WriteString("\n" + headerStyle.Render("from: ") + lipgloss.NewStyle().Foreground(accent).Render(rs.origin) + "\n")
	}
	hint := "space toggle · 1-9 preset · enter confirm › · esc cancel"
	b.WriteString("\n" + footerStyle.Render(hint))
	return b.String()
}

// renderRestartConfirm renders the selected-set summary (confirm-gated).
func (m Model) renderRestartConfirm(rs *restartState) string {
	var b strings.Builder
	names := rs.sel.checkedNames()
	b.WriteString(headerStyle.Render("confirm — the following will restart:") + "\n\n")
	for _, name := range names {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(accent).Render("deployment/"+name) + "\n")
	}
	b.WriteString("\n" + headerStyle.Render(fmt.Sprintf("via: kubectl rollout restart  (%d deployment(s))", len(names))) + "\n")
	hint := lipgloss.NewStyle().Foreground(warn).Render("enter to RESTART") + footerStyle.Render(" · esc back")
	b.WriteString("\n" + hint)
	return b.String()
}

// renderRestartRollout renders per-deployment rollout status (identical look to
// the deploy rollout phase).
func (m Model) renderRestartRollout(rs *restartState) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("rollout") + "\n\n")
	for _, l := range rs.rollouts {
		var status string
		switch l.state {
		case rolloutRunning, rolloutPending:
			status = lipgloss.NewStyle().Foreground(warn).Render("↻ restarting…")
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
	if rolloutSettled(rs.rollouts) {
		hint = footerStyle.Render("enter/esc to close")
	} else {
		hint = footerStyle.Render("esc to close (restart continues)")
	}
	b.WriteString("\n" + hint)
	return b.String()
}
