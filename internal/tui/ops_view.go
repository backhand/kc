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

// renderOpHints renders the "[d]eploy [r]estart [L]ogs [s]hell" footer strip.
// Deploy brightens when a deploy is available (a namespace/deployment view with
// deployments); restart/logs/shell brighten when an op target is selectable (a
// namespace or pods view) — otherwise faint.
func (m Model) renderOpHints() string {
	deployHint := footerStyle.Render("[d]eploy")
	if m.deployContextAvailable() {
		deployHint = hintStyle.Render("[d]eploy")
	}
	rls := footerStyle.Render("[r]estart [L]ogs [s]hell")
	if m.opContextAvailable() {
		rls = hintStyle.Render("[r]estart [L]ogs [s]hell")
	}
	return deployHint + " " + rls
}

// renderRestartModal renders the restart confirm screen (what will restart) or,
// once confirmed, the per-deployment rollout view (the same look as deploy's).
func (m Model) renderRestartModal() string {
	rs := m.restartModal
	var b strings.Builder
	title := fmt.Sprintf("restart — %s", rs.namespace)
	b.WriteString(titleStyle.Render("kc") + "  " + titleStyle.Render(title) + "\n\n")

	if !rs.confirmed {
		b.WriteString(headerStyle.Render("confirm — the following will restart:") + "\n\n")
		b.WriteString("  " + lipgloss.NewStyle().Foreground(accent).Render(rs.origin) + "\n")
		b.WriteString("\n" + headerStyle.Render("via: kubectl rollout restart  (1 deployment)") + "\n")
		hint := lipgloss.NewStyle().Foreground(warn).Render("enter to RESTART") + footerStyle.Render(" · esc cancel")
		b.WriteString("\n" + hint)
		return b.String()
	}

	// Confirmed → rollout view (identical look to the deploy rollout phase).
	b.WriteString(headerStyle.Render("rollout") + "\n\n")
	l := rs.rollout
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

	var hint string
	if l.state == rolloutDone || l.state == rolloutFailed {
		hint = footerStyle.Render("enter/esc to close")
	} else {
		hint = footerStyle.Render("esc to close (restart continues)")
	}
	b.WriteString("\n" + hint)
	return b.String()
}
