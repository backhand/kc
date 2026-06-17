package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
)

// ── Styles ──────────────────────────────────────────────────────────────────

var (
	accent = lipgloss.Color("#7D56F4")
	dim    = lipgloss.Color("#6C6C6C")
	good   = lipgloss.Color("#43BF6D")
	warn   = lipgloss.Color("#E0AF68")
	bad    = lipgloss.Color("#E06C75")

	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(accent)
	headerStyle   = lipgloss.NewStyle().Foreground(dim)
	colHeadStyle  = lipgloss.NewStyle().Faint(true)
	selectedStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	systemStyle   = lipgloss.NewStyle().Faint(true)
	footerStyle   = lipgloss.NewStyle().Faint(true)
	errStyle      = lipgloss.NewStyle().Foreground(bad)
	hintStyle     = lipgloss.NewStyle().Foreground(dim)
)

// minListRows is the floor for the list area: even on a tiny terminal we keep
// at least one row visible (the per-View geometry subtracts the actual chrome —
// title, header block, column header, footer — from the height).
const minListRows = 1

// visibleRows is how many list rows fit given the terminal height and the
// header height of the visible level. Used by the cursor/offset clamping in
// update.go.
func (m Model) visibleRows() int {
	header := m.headerHeight(*m.top())
	// title(1) + blank(1) + header + blank(1) + colhead(1) + footer(2)
	chrome := 1 + 1 + header + 1 + 1 + 2
	rows := m.height - chrome
	if rows < minListRows {
		return minListRows
	}
	return rows
}

// headerHeight returns how many lines the per-level header block occupies.
func (m Model) headerHeight(l level) int {
	switch l.kind {
	case levelOverview:
		// One line per node, plus a totals line.
		n := len(l.overview.Nodes)
		if n == 0 {
			return 1
		}
		return n + 1
	default:
		return 1 // a single context line
	}
}

// View renders the visible level: title, header, column header, the scrolled
// row list, then the footer (breadcrumb + freshness + key hints). When the
// deploy modal is open it takes over the whole frame.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	if m.deployModal != nil {
		return m.renderDeployModal()
	}
	if m.restartModal != nil {
		return m.renderRestartModal()
	}
	top := *m.top()

	var b strings.Builder
	b.WriteString(titleStyle.Render("kc") + "  " + headerStyle.Render(m.breadcrumb()) + "\n\n")
	b.WriteString(m.renderHeader(top) + "\n")
	b.WriteString(m.renderList(top))
	b.WriteString("\n" + m.renderFooter(top))
	return b.String()
}

// ── Header (per level) ───────────────────────────────────────────────────────

func (m Model) renderHeader(l level) string {
	switch l.kind {
	case levelOverview:
		return m.renderNodeHeader(l)
	case levelGroup:
		return headerStyle.Render(fmt.Sprintf("app group %q — %d namespaces", l.app, len(l.groupNs)))
	case levelNamespace:
		return headerStyle.Render(fmt.Sprintf("namespace %s [%s] — %d deployments",
			l.namespace, l.nsView.Kind, len(l.nsView.Deployments)))
	case levelDeployment:
		return headerStyle.Render(fmt.Sprintf("%s/%s — %d pods", l.namespace, l.deployment, len(l.pods)))
	}
	return ""
}

// renderNodeHeader renders one line per node (role, readiness, cpu+mem gauges)
// and a cluster totals line — the all-namespaces resource header.
func (m Model) renderNodeHeader(l level) string {
	if len(l.overview.Nodes) == 0 {
		return headerStyle.Render("no node data yet")
	}
	var b strings.Builder
	for _, n := range l.overview.Nodes {
		role := "worker"
		if n.ControlPlane {
			role = "control-plane"
		}
		ready := lipgloss.NewStyle().Foreground(good).Render("Ready")
		if !n.Ready {
			ready = lipgloss.NewStyle().Foreground(bad).Render("NotReady")
		}
		cpu := m.gaugeCell(usagePtrCPU(n.Usage), n.Capacity.CPUMillicores, "cpu", formatCPU)
		mem := m.gaugeCell(usagePtrMem(n.Usage), n.Capacity.MemoryBytes, "mem", formatMem)
		b.WriteString(headerStyle.Render(fmt.Sprintf("  %-26s %-13s %-9s  %s  %s",
			truncate(n.Name, 26), role, ready, cpu, mem)) + "\n")
	}
	// Totals line.
	tot := l.overview.Totals
	cpuTot := m.gaugeCell(totalCPU(tot), tot.Capacity.CPUMillicores, "cpu", formatCPU)
	memTot := m.gaugeCell(totalMem(tot), tot.Capacity.MemoryBytes, "mem", formatMem)
	b.WriteString(titleStyle.Render(fmt.Sprintf("  %-26s %-13s %-9s  %s  %s",
		"cluster", "", "", cpuTot, memTot)))
	return b.String()
}

// gaugeCell renders "label used/cap ▕███░░▏" with a small bar. used == -1 means
// "no metrics" → a dash. cap == 0 → just the bar omitted.
func (m Model) gaugeCell(used, cap int64, label string, fmtFn func(int64) string) string {
	if cap <= 0 {
		return fmt.Sprintf("%s —", label)
	}
	if used < 0 {
		return fmt.Sprintf("%s —/%s", label, fmtFn(cap))
	}
	frac := float64(used) / float64(cap)
	style := lipgloss.NewStyle().Foreground(good)
	switch {
	case frac >= 0.9:
		style = lipgloss.NewStyle().Foreground(bad)
	case frac >= 0.7:
		style = lipgloss.NewStyle().Foreground(warn)
	}
	return fmt.Sprintf("%s %s/%s %s", label, fmtFn(used), fmtFn(cap), style.Render(bar(frac, 8)))
}

// usagePtrCPU / usagePtrMem return -1 for nil usage ("no metrics"), else the
// component value.
func usagePtrCPU(u *k8s.Usage) int64 {
	if u == nil {
		return -1
	}
	return u.CPUMillicores
}
func usagePtrMem(u *k8s.Usage) int64 {
	if u == nil {
		return -1
	}
	return u.MemoryBytes
}
func totalCPU(t k8s.Totals) int64 {
	if t.Usage == nil {
		return -1
	}
	return t.Usage.CPUMillicores
}
func totalMem(t k8s.Totals) int64 {
	if t.Usage == nil {
		return -1
	}
	return t.Usage.MemoryBytes
}

// ── List (per level), scrolled + selection ──────────────────────────────────

func (m Model) renderList(l level) string {
	colHead, rows := m.rows(l)

	var b strings.Builder
	b.WriteString(colHeadStyle.Render(colHead) + "\n")

	if len(rows) == 0 {
		if l.loading && !l.loaded {
			b.WriteString(hintStyle.Render("  loading…"))
		} else {
			b.WriteString(hintStyle.Render("  (none)"))
		}
		return b.String()
	}

	vis := m.visibleRows()
	start := l.offset
	end := start + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := start; i < end; i++ {
		marker := "  "
		line := rows[i].text
		if i == l.cursor {
			marker = "› "
			line = selectedStyle.Render(line)
		} else if rows[i].faint {
			line = systemStyle.Render(line)
		}
		b.WriteString(marker + line + "\n")
	}
	// Scroll hint when there is more below/above.
	if len(rows) > vis {
		b.WriteString(hintStyle.Render(fmt.Sprintf("  … %d–%d of %d", start+1, end, len(rows))))
	}
	return strings.TrimRight(b.String(), "\n")
}

// row is one rendered list line plus presentation flags.
type row struct {
	text  string
	faint bool // system namespace, dimmed
}

// rows returns the column header and the rendered rows for a level.
func (m Model) rows(l level) (colHead string, rows []row) {
	switch l.kind {
	case levelOverview:
		return m.overviewRows(l)
	case levelGroup:
		return m.groupRows(l)
	case levelNamespace:
		return m.namespaceRows(l)
	case levelDeployment:
		return m.podRows(l)
	}
	return "", nil
}

func (m Model) overviewRows(l level) (string, []row) {
	head := fmt.Sprintf("  %-24s %-8s %s", "NAMESPACE", "KIND", "VERSION")
	out := make([]row, 0, len(l.overview.Namespaces))
	for _, ns := range l.overview.Namespaces {
		ver := ""
		if l.versionHints != nil {
			ver = l.versionHints[ns.Name]
		}
		text := fmt.Sprintf("%-24s %-8s %s", truncate(ns.Name, 24), ns.Kind, truncate(ver, 24))
		out = append(out, row{text: text, faint: ns.Kind == k8s.KindSystem})
	}
	return head, out
}

func (m Model) groupRows(l level) (string, []row) {
	head := fmt.Sprintf("  %-24s", "NAMESPACE")
	out := make([]row, 0, len(l.groupNs))
	for _, ns := range l.groupNs {
		out = append(out, row{text: truncate(ns, 24)})
	}
	return head, out
}

func (m Model) namespaceRows(l level) (string, []row) {
	head := fmt.Sprintf("  %-22s %-18s %-7s %s", "DEPLOYMENT", "VERSION", "READY", "USAGE")
	out := make([]row, 0, len(l.nsView.Deployments))
	for _, d := range l.nsView.Deployments {
		ver := d.Image.Tag
		if ver == "" {
			ver = "—"
		}
		ready := fmt.Sprintf("%d/%d", d.ReadyReplicas, d.DesiredReplicas)
		text := fmt.Sprintf("%-22s %-18s %-7s %s",
			truncate(d.Name, 22), truncate(ver, 18), ready, usageCell(d.Usage))
		out = append(out, row{text: text})
	}
	return head, out
}

func (m Model) podRows(l level) (string, []row) {
	head := fmt.Sprintf("  %-34s %-10s %-22s %-4s %s", "POD", "STATUS", "NODE", "RST", "USAGE")
	out := make([]row, 0, len(l.pods))
	for _, p := range l.pods {
		status := p.Phase
		if p.Phase == "Running" && !p.Ready {
			status = "NotReady"
		}
		node := p.Node
		if node == "" {
			node = "—"
		}
		text := fmt.Sprintf("%-34s %-10s %-22s %-4d %s",
			truncate(p.Name, 34), truncate(status, 10), truncate(node, 22), p.Restarts, usageCell(p.Usage))
		out = append(out, row{text: text})
	}
	return head, out
}

// ── Footer (breadcrumb + freshness + key hints) ──────────────────────────────

// breadcrumb renders the zoom path, e.g. "all-namespaces › mailon › responder".
func (m Model) breadcrumb() string {
	parts := make([]string, 0, len(m.stack))
	for _, l := range m.stack {
		switch l.kind {
		case levelOverview:
			parts = append(parts, "all-namespaces")
		case levelGroup:
			parts = append(parts, l.app+"-*")
		case levelNamespace:
			parts = append(parts, l.namespace)
		case levelDeployment:
			parts = append(parts, l.deployment)
		}
	}
	return strings.Join(parts, " › ")
}

// freshness renders the SPEC freshness indicator: "↻ refreshing…" while a fetch
// is in flight, else "updated Ns ago" (or "stale · Ns ago" past staleAfter).
func (m Model) freshness(l level) string {
	if l.loading {
		return "↻ refreshing…"
	}
	if !l.loaded {
		return "no data"
	}
	age := time.Since(l.stamped)
	if age > staleAfter {
		return "stale · " + formatAge(age) + " ago"
	}
	return "updated " + formatAge(age) + " ago"
}

func (m Model) renderFooter(l level) string {
	m.help.ShowAll = m.showHelp
	hints := m.help.View(keys)

	fresh := m.freshness(l)
	left := hintStyle.Render(fresh)
	if l.err != "" {
		left = errStyle.Render("error: " + truncate(l.err, 60))
	}

	// Op-key hints. All four contextual ops act on the selected workload: deploy
	// from a namespace/deployment view; restart/logs/shell from a namespace or
	// pods view. Available ops render bright, unavailable ones faint.
	ops := m.renderOpHints()

	return left + "    " + ops + "\n" + footerStyle.Render(hints)
}
