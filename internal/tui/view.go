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

// Node-header column widths. The cluster-total row and the per-node rows share
// these so the name / role / cpu-gauge / mem-gauge columns start at the same
// offset on every line. The cells are joined with fixed-width lipgloss styles
// (ANSI-aware) so a colored cell like the readiness badge doesn't shift the
// columns after it — fmt's %-Ns counts the escape bytes, lipgloss.Width doesn't.
const (
	nodeNameW  = 26
	nodeRoleW  = 13
	nodeReadyW = 9
)

var (
	nodeNameCol  = lipgloss.NewStyle().Width(nodeNameW)
	nodeRoleCol  = lipgloss.NewStyle().Width(nodeRoleW)
	nodeReadyCol = lipgloss.NewStyle().Width(nodeReadyW)
)

// minListRows is the floor for the list area: even on a tiny terminal we keep
// at least one row visible (the per-View geometry subtracts the actual chrome —
// top bar, resource header, column header, footer — from the height).
const minListRows = 1

// visibleRows is how many list rows fit given the terminal height and the
// header height of the visible level. Used by the cursor/offset clamping in
// update.go.
func (m Model) visibleRows() int {
	// topbar(1) + blank(1) + header-block + colhead(1) + footer(3). The header
	// block is the overview node lines plus their trailing blank separator; the
	// non-overview views render no resource header (headerHeight == 0). The footer
	// term is 3 = its blank separator line + the op-key line + the help line.
	chrome := 1 + 1 + m.headerHeight(*m.top()) + 1 + 3
	rows := m.height - chrome
	if rows < minListRows {
		return minListRows
	}
	return rows
}

// headerHeight returns how many lines the per-level resource header occupies,
// including the blank line that separates it from the list. Only the overview
// has one (the cluster-total + per-node block); every other view's context now
// lives in the top bar, so they have no resource header.
func (m Model) headerHeight(l level) int {
	if l.kind != levelOverview {
		return 0
	}
	// "cluster" totals line + one line per node, then a blank separator.
	n := len(l.overview.Nodes)
	if n == 0 {
		return 1 + 1 // "no node data yet" + blank
	}
	return (n + 1) + 1
}

// View renders the visible level: the top bar (context breadcrumb + freshness),
// the resource header, the column header, the scrolled row list, then the footer
// (key hints). When the deploy modal is open it takes over the whole frame.
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
	b.WriteString(m.renderTopBar(top) + "\n\n")
	// Only the overview carries a resource header (the node block). Its context
	// — and every other view's — now lives in the top bar, so the non-overview
	// views go straight from the top bar to the column header. A blank line
	// separates the node block from the namespace list.
	if top.kind == levelOverview {
		b.WriteString(m.renderHeader(top) + "\n\n")
	}
	b.WriteString(m.renderList(top))
	// A blank line separates the data (the list) from the footer's action keys,
	// so "[d]eploy [r]estart …" reads as a distinct action strip rather than
	// another data row. visibleRows() accounts for this extra chrome line.
	b.WriteString("\n\n" + m.renderFooter(top))
	return b.String()
}

// renderTopBar is the one-line context bar: "kc · <context>" on the left, the
// freshness indicator right-aligned to the terminal width. The context adapts
// per view (see topBarContext) so the bar carries the current view's scope.
func (m Model) renderTopBar(l level) string {
	left := titleStyle.Render("kc") + headerStyle.Render(" · "+m.topBarContext(l))
	right := hintStyle.Render(m.freshness(l))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		gap = 2 // always keep a visible separation, even on a narrow terminal
	}
	return left + strings.Repeat(" ", gap) + right
}

// topBarContext renders the current view's scope for the top bar, joined by "·":
//
//	overview:    all-namespaces
//	group:       <app>-*
//	namespace:   <namespace> · [user] · N deployments
//	deployment:  <namespace> · <deployment>
func (m Model) topBarContext(l level) string {
	switch l.kind {
	case levelOverview:
		return "all-namespaces"
	case levelGroup:
		return l.app + "-*"
	case levelNamespace:
		return fmt.Sprintf("%s · [%s] · %d deployments",
			l.namespace, l.nsView.Kind, len(l.nsView.Deployments))
	case levelDeployment:
		return l.namespace + " · " + l.deployment
	}
	return ""
}

// ── Header (per level) ───────────────────────────────────────────────────────

func (m Model) renderHeader(l level) string {
	// Only the overview has a resource header; every other view's context lives
	// in the top bar (renderTopBar), and View() skips calling this for them.
	if l.kind == levelOverview {
		return m.renderNodeHeader(l)
	}
	return ""
}

// renderNodeHeader renders the all-namespaces resource header: the cluster-total
// row first, then one line per node (role, readiness, cpu+mem gauges). All rows
// share the same fixed column starts (nodeNameW/nodeRoleW/nodeReadyW), so the
// cpu/mem gauge LABELS line up across the cluster row and every node row.
//
// The bars line up too, via a two-pass render: the "used/cap" value text varies
// in display width per row (e.g. the cluster's "4.7Gi/11.3Gi" vs a node's
// "2.8Gi/7.6Gi"), so a single-pass render shifts the bar right on wider rows.
// We first gather every cell's value text and bar fraction, take the max value-
// text width per column (cpu, mem), then left-pad each value to that width so
// the bars start at a fixed column on every row.
func (m Model) renderNodeHeader(l level) string {
	if len(l.overview.Nodes) == 0 {
		return headerStyle.Render("no node data yet")
	}

	// One gauge spec per (row × column): the label, the formatted value text and
	// the bar fraction (or hasBar=false when cap<=0). Pass 1 collects them so we
	// can size the value column to the widest value text across all rows.
	type rowSpec struct {
		style            lipgloss.Style
		name, role, redy string
		cpu, mem         gauge
	}
	specs := make([]rowSpec, 0, len(l.overview.Nodes)+1)

	// Cluster totals row on top.
	tot := l.overview.Totals
	specs = append(specs, rowSpec{
		style: titleStyle, name: "cluster",
		cpu: gaugeFor(totalCPU(tot), tot.Capacity.CPUMillicores, "cpu", formatCPU),
		mem: gaugeFor(totalMem(tot), tot.Capacity.MemoryBytes, "mem", formatMem),
	})
	// Each node underneath.
	for _, n := range l.overview.Nodes {
		role := "worker"
		if n.ControlPlane {
			role = "control-plane"
		}
		ready := lipgloss.NewStyle().Foreground(good).Render("Ready")
		if !n.Ready {
			ready = lipgloss.NewStyle().Foreground(bad).Render("NotReady")
		}
		specs = append(specs, rowSpec{
			style: headerStyle, name: truncate(n.Name, nodeNameW), role: role, redy: ready,
			cpu: gaugeFor(usagePtrCPU(n.Usage), n.Capacity.CPUMillicores, "cpu", formatCPU),
			mem: gaugeFor(usagePtrMem(n.Usage), n.Capacity.MemoryBytes, "mem", formatMem),
		})
	}

	// Pass 1: widest value text per column.
	var cpuValW, memValW int
	for _, s := range specs {
		if w := lipgloss.Width(s.cpu.value); w > cpuValW {
			cpuValW = w
		}
		if w := lipgloss.Width(s.mem.value); w > memValW {
			memValW = w
		}
	}

	// Pass 2: render every cell with its value left-padded to the column width so
	// the bars align.
	rows := make([]string, 0, len(specs))
	for _, s := range specs {
		rows = append(rows, m.nodeHeaderRow(s.style, s.name, s.role, s.redy,
			s.cpu.render(cpuValW), s.mem.render(memValW)))
	}
	return strings.Join(rows, "\n")
}

// nodeHeaderRow lays out one resource-header line with fixed column starts. The
// name/role/ready cells are width-padded with lipgloss (ANSI-aware) so a colored
// readiness badge doesn't shift the cpu/mem gauges that follow it — fmt's %-Ns
// would otherwise count the badge's escape bytes and skew every later column.
// rowStyle colors the whole line (titleStyle for the cluster row, headerStyle
// for nodes); the readiness badge and gauge bars keep their own colors via
// lipgloss style nesting.
func (m Model) nodeHeaderRow(rowStyle lipgloss.Style, name, role, ready, cpu, mem string) string {
	line := "  " +
		nodeNameCol.Render(name) + " " +
		nodeRoleCol.Render(role) + " " +
		nodeReadyCol.Render(ready) + "  " +
		cpu + "  " + mem
	return rowStyle.Render(line)
}

// gaugeBarW is the bar's inner rune count. It's reserved even when there is no
// bar (cap<=0) so the cpu cell keeps a fixed width and the mem label that
// follows it stays column-aligned across every row.
const gaugeBarW = 8

// gauge is the decomposed parts of one gauge cell: the label, the "used/cap"
// value text (or "—" / "—/cap"), and the bar fraction. Splitting render lets
// renderNodeHeader size the value column to the widest value text across rows
// (so the bars line up) before drawing.
type gauge struct {
	label  string
	value  string  // "used/cap", or "—" (no cap), or "—/cap" (no usage)
	frac   float64 // bar fill; only meaningful when hasBar
	hasBar bool    // false when cap<=0 (no bar; its width is still reserved)
}

// gaugeFor builds a gauge for used/cap. used == -1 means "no metrics" → the
// value reads "—/cap" with no bar (an empty bar would misread as 0% rather than
// "unknown"); cap <= 0 → the value is just "—" and there's no bar. In both
// bar-less cases render still reserves the bar's width so columns stay aligned.
func gaugeFor(used, cap int64, label string, fmtFn func(int64) string) gauge {
	switch {
	case cap <= 0:
		return gauge{label: label, value: "—"}
	case used < 0:
		return gauge{label: label, value: "—/" + fmtFn(cap)}
	default:
		return gauge{
			label:  label,
			value:  fmtFn(used) + "/" + fmtFn(cap),
			frac:   float64(used) / float64(cap),
			hasBar: true,
		}
	}
}

// render draws the gauge as "<label> <value> <bar>" with the value left-padded
// to valW (the column's widest value text) so the bar starts at a fixed column.
// The bar's width is always reserved — when there's no bar (cap<=0) the trailing
// spaces keep the cell, and the following column, aligned.
func (g gauge) render(valW int) string {
	value := leftPad(g.value, valW)
	bar := strings.Repeat(" ", gaugeBarW)
	if g.hasBar {
		style := lipgloss.NewStyle().Foreground(good)
		switch {
		case g.frac >= 0.9:
			style = lipgloss.NewStyle().Foreground(bad)
		case g.frac >= 0.7:
			style = lipgloss.NewStyle().Foreground(warn)
		}
		bar = style.Render(barGlyphs(g.frac, gaugeBarW))
	}
	return g.label + " " + value + " " + bar
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

// ── Freshness + footer (key hints) ───────────────────────────────────────────

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

// renderFooter renders the op-key hints + the help line. The freshness indicator
// has moved to the top bar (renderTopBar); the footer keeps only the key hints,
// surfacing a fetch error in their place when one is set.
func (m Model) renderFooter(l level) string {
	m.help.ShowAll = m.showHelp
	hints := m.help.View(keys)

	// Op-key hints come from renderOpHints: deploy brightens in a namespace/
	// deployment view; restart/logs/shell brighten from a namespace or pods view;
	// unavailable ops stay faint. Freshness now lives in the top bar
	// (renderTopBar), so the footer carries only the hints — or a fetch error in
	// their place.
	ops := m.renderOpHints()
	if l.err != "" {
		ops = errStyle.Render("error: " + truncate(l.err, 60))
	}

	return ops + "\n" + footerStyle.Render(hints)
}
