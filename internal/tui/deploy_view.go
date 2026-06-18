package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/backhand/kc/internal/deploy"
	"github.com/backhand/kc/internal/github"
)

// Rendering for the deploy modal (SPEC "Deploy flow (v1)"). Plain, scannable
// frames — checkboxes, an annotated version list, the from→to confirm summary,
// and the per-deployment rollout view. Styling reuses view.go's palette.

var (
	chipStyle   = lipgloss.NewStyle().Foreground(accent)
	chipOnStyle = lipgloss.NewStyle().Foreground(good).Bold(true)
	// chipActiveStyle marks the preset chip ←/→ last activated — bold + underlined
	// accent so it stands out from both an inactive chip and a fully-checked one.
	chipActiveStyle = lipgloss.NewStyle().Foreground(accent).Bold(true).Underline(true)
	preStyle        = lipgloss.NewStyle().Foreground(warn) // pre-release flag
	checkOnStyle    = lipgloss.NewStyle().Foreground(good).Bold(true)
)

// renderDeployModal dispatches to the active phase's renderer.
func (m Model) renderDeployModal() string {
	ds := m.deployModal
	var b strings.Builder
	title := fmt.Sprintf("deploy — %s", ds.namespace)
	if ds.repoOK {
		title += headerStyle.Render(fmt.Sprintf("   (%s/%s)", ds.repo.Owner, ds.repo.Repo))
	}
	b.WriteString(titleStyle.Render("kc") + "  " + titleStyle.Render(title) + "\n\n")

	switch ds.phase {
	case phaseSelect:
		b.WriteString(m.renderDeploySelect(ds))
	case phaseVersions:
		b.WriteString(m.renderDeployVersions(ds))
	case phaseConfirm:
		b.WriteString(m.renderDeployConfirm(ds))
	case phaseAwaitBuild:
		b.WriteString(m.renderDeployAwait(ds))
	case phaseRollout:
		b.WriteString(m.renderDeployRollout(ds))
	}
	return b.String()
}

// renderDeployAwait renders the watch-the-build phase: kc is polling the selected
// version's Actions run and deploys automatically when it goes green. A
// failed/timed-out build aborts here — nothing was deployed.
func (m Model) renderDeployAwait(ds *deployState) string {
	var b strings.Builder
	tag := truncate(ds.selected.Tag, 40)
	run := hintStyle.Render(fmt.Sprintf("run #%d", ds.buildRunID))

	if ds.buildStatus == github.BuildFailed {
		b.WriteString(lipgloss.NewStyle().Foreground(bad).Bold(true).Render("✗ build did not succeed") + "\n\n")
		b.WriteString(fmt.Sprintf("  %s   %s\n", tag, run))
		if ds.buildErr != "" {
			b.WriteString("  " + errStyle.Render(truncate(ds.buildErr, 70)) + "\n")
		}
		b.WriteString("\n" + footerStyle.Render("enter/esc to close"))
		return b.String()
	}

	b.WriteString(lipgloss.NewStyle().Foreground(warn).Render("⏳ waiting for the build…") + "\n\n")
	b.WriteString(fmt.Sprintf("  %s   %s\n", tag, run))
	b.WriteString(hintStyle.Render("  watching the Actions run — kc deploys automatically when it goes green") + "\n")
	if ds.buildErr != "" { // a transient gh hiccup mid-poll
		b.WriteString(hintStyle.Render("  (retrying: "+truncate(ds.buildErr, 56)+")") + "\n")
	}
	b.WriteString("\n" + footerStyle.Render("esc to cancel"))
	return b.String()
}

// renderDeploySelect renders the deployment checkboxes + preset chips (shared
// with restart's select phase via renderSelectList).
func (m Model) renderDeploySelect(ds *deployState) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("select deployments to deploy") + "\n\n")
	b.WriteString(renderSelectList(&ds.sel))

	if !ds.repoOK {
		b.WriteString("\n" + errStyle.Render("no ghcr.io image on these deployments — cannot list releases") + "\n")
	}

	hint := "space toggle · 1-9 preset · ←/→ preset · enter versions › · esc cancel"
	b.WriteString("\n" + footerStyle.Render(hint))
	return b.String()
}

// renderDeployVersions renders the annotated release list.
func (m Model) renderDeployVersions(ds *deployState) string {
	var b strings.Builder
	sel := strings.Join(ds.sel.checkedNames(), ", ")
	b.WriteString(headerStyle.Render("pick a version for: "+sel) + "\n\n")

	if ds.releasesLoading && len(ds.releases) == 0 {
		b.WriteString(hintStyle.Render("  loading releases…") + "\n")
	} else if ds.releasesErr != "" {
		b.WriteString(errStyle.Render("  error: "+truncate(ds.releasesErr, 70)) + "\n")
	} else if len(ds.releases) == 0 {
		b.WriteString(hintStyle.Render("  (no releases)") + "\n")
	} else {
		b.WriteString(colHeadStyle.Render(fmt.Sprintf("  %-16s %-10s %-10s %s", "VERSION", "BUILD", "IMAGE", "FLAGS")) + "\n")
		for i, r := range ds.releases {
			marker := "  "
			line := releaseRow(r)
			if i == ds.relCursor {
				marker = "› "
				line = selectedStyle.Render(line)
			}
			b.WriteString(marker + line + "\n")
		}
	}
	if ds.relPage > 0 {
		b.WriteString(hintStyle.Render(fmt.Sprintf("  …page %d (older)", ds.relPage+1)) + "\n")
	}

	hint := "↑/↓ move · enter confirm › · o …older · esc back"
	b.WriteString("\n" + footerStyle.Render(hint))
	return b.String()
}

// releaseRow renders one annotated release line: tag, build status, image
// availability, and prerelease/latest flags.
func releaseRow(r github.ReleaseAnnotation) string {
	flags := []string{}
	if r.Prerelease {
		flags = append(flags, preStyle.Render("pre-release"))
	}
	if r.Latest {
		flags = append(flags, chipStyle.Render("latest"))
	}
	return fmt.Sprintf("%-16s %-10s %-10s %s",
		truncate(r.Tag, 16), buildLabel(r.Build), availLabel(r.ImageAvailable), strings.Join(flags, " "))
}

// buildLabel renders a colour-coded build status.
func buildLabel(b github.BuildStatus) string {
	switch b {
	case github.BuildReady:
		return lipgloss.NewStyle().Foreground(good).Render("ready")
	case github.BuildBuilding:
		return lipgloss.NewStyle().Foreground(warn).Render("building")
	case github.BuildFailed:
		return lipgloss.NewStyle().Foreground(bad).Render("failed")
	default:
		return hintStyle.Render("none")
	}
}

// availLabel renders the tri-state image availability.
func availLabel(a github.Availability) string {
	switch a {
	case github.AvailPresent:
		return lipgloss.NewStyle().Foreground(good).Render("present")
	case github.AvailAbsent:
		return lipgloss.NewStyle().Foreground(bad).Render("absent")
	default:
		return hintStyle.Render("—")
	}
}

// renderDeployConfirm renders the exactly-what-changes summary (from→to).
func (m Model) renderDeployConfirm(ds *deployState) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("confirm — the following will change:") + "\n\n")
	b.WriteString(colHeadStyle.Render(fmt.Sprintf("  %-22s %-14s %s", "DEPLOYMENT", "CONTAINER", "CHANGE")) + "\n")
	for _, c := range ds.changes {
		container := c.Container
		if container == "" {
			container = deploy.AllContainers // "*" — every container
		}
		from := c.FromTag
		if from == "" {
			from = "—"
		}
		change := fmt.Sprintf("%s → %s", from, lipgloss.NewStyle().Foreground(accent).Render(c.ToTag))
		if c.NoOp() {
			change += hintStyle.Render("  (no version change)")
		}
		b.WriteString(fmt.Sprintf("  %-22s %-14s %s\n", truncate(c.Deployment, 22), truncate(container, 14), change))
	}
	b.WriteString("\n" + headerStyle.Render(fmt.Sprintf("via: kubectl set image  (%d deployment(s))", len(ds.changes))) + "\n")

	// A still-building version is deployed via the watch-then-deploy flow.
	apply := "enter to APPLY"
	if ds.selected.Build == github.BuildBuilding {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(warn).Render(
			"⏳ "+ds.selected.Tag+" is still building — kc will watch its run and deploy when it goes green.") + "\n")
		apply = "enter to watch the build, then deploy"
	}

	hint := lipgloss.NewStyle().Foreground(warn).Render(apply) + footerStyle.Render(" · esc back")
	b.WriteString("\n" + hint)
	return b.String()
}

// renderRolloutLines renders ONE line per deployment — name, status, and (once
// settled) the status message inline after it. Keeping each deployment to a
// single line means the list never grows or shifts as rollouts finish: the
// detail used to wrap onto an indented line below, which pushed everything down.
// running is the in-progress label, which differs per op ("↻ rolling out…" /
// "↻ restarting…" / "↻ scaling…"). Shared by the deploy / restart / scale views.
func renderRolloutLines(rollouts []rolloutLine, running string) string {
	var b strings.Builder
	for _, l := range rollouts {
		var status string
		switch l.state {
		case rolloutDone:
			status = lipgloss.NewStyle().Foreground(good).Render("✓ done")
		case rolloutFailed:
			status = lipgloss.NewStyle().Foreground(bad).Render("✗ failed")
		default: // running / pending
			status = lipgloss.NewStyle().Foreground(warn).Render(running)
		}
		line := fmt.Sprintf("  %-22s %s", truncate(l.deployment, 22), status)
		if l.detail != "" {
			line += "  " + hintStyle.Render(truncate(l.detail, 50))
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderDeployRollout renders per-deployment rollout status.
func (m Model) renderDeployRollout(ds *deployState) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("rollout") + "\n\n")
	b.WriteString(renderRolloutLines(ds.rollouts, "↻ rolling out…"))

	var hint string
	if rolloutSettled(ds.rollouts) {
		hint = footerStyle.Render("enter/esc to close")
	} else {
		hint = footerStyle.Render("esc to close (rollout continues)")
	}
	b.WriteString("\n" + hint)
	return b.String()
}
