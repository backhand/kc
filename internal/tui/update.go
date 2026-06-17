package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/store"
)

// Update folds messages into the model: key navigation (zoom in/out, move the
// cursor), the periodic refresh tick, window resizes, and the fetch
// `…LoadedMsg`s (swap data + persist cache + clear loading).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.help.Width = msg.Width
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		// Re-fetch the visible level for a live feel, then re-arm the tick.
		top := m.top()
		top.loading = true
		return m, tea.Batch(m.fetchFor(*top), tickCmd())

	case overviewLoadedMsg:
		return m.onOverviewLoaded(msg), nil

	case namespaceLoadedMsg:
		return m.onNamespaceLoaded(msg), nil

	case podsLoadedMsg:
		return m.onPodsLoaded(msg), nil

	case allDeploymentsLoadedMsg:
		return m.onAllDeploymentsLoaded(msg), nil

	case releasesLoadedMsg:
		return m.onReleasesLoaded(msg), nil

	case deployStepMsg:
		return m.onDeployStep(msg), nil
	}
	return m, nil
}

// ── Key handling ────────────────────────────────────────────────────────────

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl+C always hard-quits, even mid-modal (an escape hatch that never
	// triggers a mutation — it just tears the program down).
	if msg.Type == tea.KeyCtrlC {
		m.quitting = true
		return m, tea.Quit
	}

	// While the deploy modal is open it owns all other keys (its esc/cancel is
	// the way out), so navigation/quit can't fire underneath it.
	if m.deployModal != nil {
		return m.handleDeployKey(msg)
	}

	switch {
	case key.Matches(msg, keys.Quit):
		m.quitting = true
		return m, tea.Quit

	case key.Matches(msg, keys.Help):
		m.showHelp = !m.showHelp
		return m, nil

	case key.Matches(msg, keys.Deploy):
		return m.openDeploy()

	case key.Matches(msg, keys.Up):
		m.moveCursor(-1)
		return m, nil

	case key.Matches(msg, keys.Down):
		m.moveCursor(1)
		return m, nil

	case key.Matches(msg, keys.Enter):
		return m.drillIn()

	case key.Matches(msg, keys.Back):
		return m.zoomOut()
	}
	return m, nil
}

// moveCursor moves the selection within the visible level, clamped to its row
// count, and adjusts the scroll offset to keep the cursor visible.
func (m *Model) moveCursor(delta int) {
	top := m.top()
	n := m.rowCount(*top)
	if n == 0 {
		top.cursor = 0
		top.offset = 0
		return
	}
	top.cursor += delta
	if top.cursor < 0 {
		top.cursor = 0
	}
	if top.cursor >= n {
		top.cursor = n - 1
	}
	m.clampOffset(top)
}

// clampOffset keeps the cursor within the visible window for the current
// terminal height.
func (m *Model) clampOffset(l *level) {
	rows := m.visibleRows()
	if rows <= 0 {
		l.offset = 0
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+rows {
		l.offset = l.cursor - rows + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

// drillIn pushes the child of the currently-selected row, seeding it from cache
// and firing its fetch. A no-op when the selected row has no child (e.g. a pod,
// or an empty list).
func (m Model) drillIn() (tea.Model, tea.Cmd) {
	top := m.top()
	switch top.kind {
	case levelOverview:
		ns, ok := m.selectedNamespace(*top)
		if !ok {
			return m, nil
		}
		// A namespace that is part of a multi-namespace app group could zoom to
		// the group, but from the overview the natural drill is into the
		// namespace itself; the group level is only reconstructed on entry.
		child := m.seedNamespace(ns)
		m.stack = append(m.stack, child)
		m.recordNamespaceView(ns)
		return m, m.fetchFor(child)

	case levelGroup:
		top := m.top()
		if top.cursor < 0 || top.cursor >= len(top.groupNs) {
			return m, nil
		}
		ns := top.groupNs[top.cursor]
		child := m.seedNamespace(ns)
		m.stack = append(m.stack, child)
		m.recordNamespaceView(ns)
		return m, m.fetchFor(child)

	case levelNamespace:
		dep, ok := m.selectedDeployment(*top)
		if !ok {
			return m, nil
		}
		child := m.seedDeployment(top.namespace, dep)
		child.loading = true
		m.stack = append(m.stack, child)
		return m, m.fetchFor(child)

	default:
		// levelDeployment: pods are the leaves — nothing to drill into.
		return m, nil
	}
}

// zoomOut pops the stack (Backspace / Esc). A no-op at the base (overview), so
// the user can never pop the root out from under themselves.
func (m Model) zoomOut() (tea.Model, tea.Cmd) {
	if len(m.stack) <= 1 {
		return m, nil
	}
	m.stack = m.stack[:len(m.stack)-1]
	return m, nil
}

// ── Loaded-message folding (swap data + persist + clear loading) ────────────

func (m Model) onOverviewLoaded(msg overviewLoadedMsg) Model {
	for i := range m.stack {
		if m.stack[i].kind != levelOverview {
			continue
		}
		m.stack[i].loading = false
		if msg.err != nil {
			m.stack[i].err = msg.err.Error()
			continue
		}
		m.stack[i].err = ""
		m.stack[i].overview = msg.overview
		m.stack[i].loaded = true
		m.stack[i].stamped = msg.at
		m.clampCursor(&m.stack[i])
	}
	if msg.err == nil && m.deps.OverviewCache != nil {
		_ = m.deps.OverviewCache.Put(m.deps.Cluster, msg.overview)
	}
	return m
}

func (m Model) onNamespaceLoaded(msg namespaceLoadedMsg) Model {
	for i := range m.stack {
		if m.stack[i].kind != levelNamespace || m.stack[i].namespace != msg.namespace {
			continue
		}
		m.stack[i].loading = false
		if msg.err != nil {
			m.stack[i].err = msg.err.Error()
			continue
		}
		m.stack[i].err = ""
		m.stack[i].nsView = msg.view
		m.stack[i].loaded = true
		m.stack[i].stamped = msg.at
		m.clampCursor(&m.stack[i])
	}
	if msg.err == nil && m.deps.NamespaceCache != nil {
		_ = m.deps.NamespaceCache.Put(m.nsKey(msg.namespace), msg.view)
	}
	return m
}

func (m Model) onPodsLoaded(msg podsLoadedMsg) Model {
	for i := range m.stack {
		if m.stack[i].kind != levelDeployment ||
			m.stack[i].namespace != msg.namespace ||
			m.stack[i].deployment != msg.deployment {
			continue
		}
		m.stack[i].loading = false
		if msg.err != nil {
			m.stack[i].err = msg.err.Error()
			continue
		}
		m.stack[i].err = ""
		m.stack[i].pods = msg.pods
		m.stack[i].loaded = true
		m.stack[i].stamped = msg.at
		m.clampCursor(&m.stack[i])
	}
	if msg.err == nil && m.deps.PodsCache != nil {
		_ = m.deps.PodsCache.Put(m.podsKey(msg.namespace, msg.deployment), msg.pods)
	}
	return m
}

func (m Model) onAllDeploymentsLoaded(msg allDeploymentsLoadedMsg) Model {
	if msg.err != nil {
		return m // keep any cached hints; this feed is best-effort
	}
	hints := m.deps.VersionHintFunc(msg.deployments)
	for i := range m.stack {
		if m.stack[i].kind == levelOverview {
			m.stack[i].versionHints = hints
		}
	}
	if m.deps.AllDeployCache != nil {
		_ = m.deps.AllDeployCache.Put(m.deps.Cluster, msg.deployments)
	}
	return m
}

// clampCursor re-clamps the cursor/offset after data changes under it (e.g. a
// refresh returns fewer rows than before).
func (m *Model) clampCursor(l *level) {
	n := m.rowCount(*l)
	if n == 0 {
		l.cursor = 0
		l.offset = 0
		return
	}
	if l.cursor >= n {
		l.cursor = n - 1
	}
	m.clampOffset(l)
}

// recordNamespaceView remembers that the user viewed a namespace for the
// current app, so the next launch in this repo can prefer it (SPEC: "remember
// the last-viewed namespace per app"). This is a local learning-store write
// (not a cluster mutation); it is skipped when not launched in a repo or when no
// store is wired. Best-effort — a write failure is ignored.
func (m *Model) recordNamespaceView(ns string) {
	if m.deps.History == nil || m.deps.App == "" {
		return
	}
	scope := store.Scope{Cluster: m.deps.Cluster, App: m.deps.App}
	_ = m.deps.History.Record("view-namespace", scope, store.Params{"namespace": ns})
}

// ── Selection helpers ────────────────────────────────────────────────────────

// overviewNamespaces returns the overview's namespace names in row order. The
// data layer already orders user-first then system, so cursor index i maps
// directly to the i-th rendered row.
func overviewNamespaces(l level) []string {
	out := make([]string, 0, len(l.overview.Namespaces))
	for _, ns := range l.overview.Namespaces {
		out = append(out, ns.Name)
	}
	return out
}

func (m Model) selectedNamespace(l level) (string, bool) {
	names := overviewNamespaces(l)
	if l.cursor < 0 || l.cursor >= len(names) {
		return "", false
	}
	return names[l.cursor], true
}

func (m Model) selectedDeployment(l level) (string, bool) {
	if l.cursor < 0 || l.cursor >= len(l.nsView.Deployments) {
		return "", false
	}
	return l.nsView.Deployments[l.cursor].Name, true
}

// rowCount returns the number of selectable rows in a level.
func (m Model) rowCount(l level) int {
	switch l.kind {
	case levelOverview:
		return len(l.overview.Namespaces)
	case levelGroup:
		return len(l.groupNs)
	case levelNamespace:
		return len(l.nsView.Deployments)
	case levelDeployment:
		return len(l.pods)
	default:
		return 0
	}
}

// staleAfter is the age beyond which seeded cache data is no longer described as
// "updated Ns ago" but as "stale" — purely cosmetic.
const staleAfter = 60 * time.Second
