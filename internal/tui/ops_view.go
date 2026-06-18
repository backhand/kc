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

// renderOpHints renders the "[d]eploy [r]estart [l]ogs [s]cale [e]xec" footer
// strip. Deploy brightens when a deploy is available (a namespace/deployment view
// with deployments); restart/logs/scale/exec brighten when an op target is
// selectable (a namespace or pods view) — otherwise faint. Scale is a deployment
// op, so it brightens in the same group as restart/logs/exec.
func (m Model) renderOpHints() string {
	deployHint := footerStyle.Render("[d]eploy")
	if m.deployContextAvailable() {
		deployHint = hintStyle.Render("[d]eploy")
	}
	rls := footerStyle.Render("[r]estart [l]ogs [s]cale [e]xec")
	if m.opContextAvailable() {
		rls = hintStyle.Render("[r]estart [l]ogs [s]cale [e]xec")
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
	hint := "space toggle · 1-9 preset · ←/→ preset · enter confirm › · esc cancel"
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
	b.WriteString(renderRolloutLines(rs.rollouts, "↻ restarting…"))

	var hint string
	if rolloutSettled(rs.rollouts) {
		hint = footerStyle.Render("enter/esc to close")
	} else {
		hint = footerStyle.Render("esc to close (restart continues)")
	}
	b.WriteString("\n" + hint)
	return b.String()
}
