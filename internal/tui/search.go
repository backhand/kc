package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/backhand/kc/internal/k8s"
	"github.com/backhand/kc/internal/store"
)

// The search-everywhere modal (the `/` op): jump to any resource cluster-wide.
//
// It opens over any normal view (not while a deploy/restart/scale modal is up —
// those own keys) and owns key handling while open. A text input sits at the
// top; below it:
//
//   - empty input  → up to recentLimit most-recent past searches (recency, not
//     frequency), navigable with ↑/↓. Enter on a focused recent re-runs it
//     (populates the field with that text and switches to showing results).
//   - any input    → live, filtered results across namespaces, deployments and
//     pods, ranked exact → prefix → substring. ↑/↓ move; Enter jumps to the
//     focused result and rebuilds the zoom stack so the user can still zoom
//     back out; esc closes.
//
// The index = namespaces ∪ deployments ∪ pods, cluster-wide. Namespaces and
// deployments are already loaded (the overview + the all-deployments feed), so
// they index instantly on open; pods are fetched when the modal opens and merge
// in when they land (a small "indexing…/updated" hint conveys this). Selecting a
// result records its query string (so recents = searches that led somewhere) and
// is the only thing that touches the learning store — search performs NO cluster
// mutation.

// recentLimit is how many recent searches the empty-input state offers.
const recentLimit = 5

// searchKind tags a result row's resource kind (for the small kind label + the
// jump behaviour).
type searchKind int

const (
	searchNamespace searchKind = iota
	searchDeployment
	searchPod
)

// searchItem is one entry in the search index: a resource addressable by a
// (namespace, deployment, pod) path. Only the fields relevant to its kind are
// set (a namespace item has just namespace; a deployment item adds deployment; a
// pod item adds pod).
type searchItem struct {
	kind       searchKind
	namespace  string
	deployment string
	pod        string
	// name is the field matched against the query (the namespace/deployment/pod
	// name, by kind) — lowercased once in matchName.
	name string
}

// searchState drives the `/` modal. Held by Model.searchModal; nil when closed.
type searchState struct {
	input textinput.Model

	// nsItems / depItems are the always-available index halves (namespaces +
	// deployments), snapshotted from the loaded overview + all-deployments at
	// open. podItems join when the cluster-wide pod fetch lands.
	nsItems  []searchItem
	depItems []searchItem
	podItems []searchItem
	// podsLoaded flips once the pod fetch returns (so the hint reads "updated"
	// rather than "indexing pods…").
	podsLoaded bool

	// recents are the most-recent distinct past queries (newest first), shown
	// while the input is empty.
	recents []string

	// results is the current filtered+ranked index for the typed query (empty
	// while the input is empty — recents show instead).
	results []searchItem

	// cursor is the focused row in whichever list is showing (recents or
	// results).
	cursor int
}

// searchScope scopes search history globally per cluster (App empty) — searches
// are a cross-app navigation aid, not tied to the repo the tool launched in.
func (m Model) searchScope() store.Scope {
	return store.Scope{Cluster: m.deps.Cluster, App: ""}
}

// openSearch opens the search modal from any normal view. It indexes the
// already-loaded namespaces + deployments immediately and fires the cluster-wide
// pod fetch (pods merge in when they land). Never opens while a mutation modal
// is up — handleKey checks those first.
func (m Model) openSearch() (tea.Model, tea.Cmd) {
	ti := textinput.New()
	ti.Placeholder = "search namespaces, deployments, pods…"
	ti.Prompt = "/ "
	ti.Focus()

	ss := &searchState{
		input:    ti,
		nsItems:  m.indexNamespaces(),
		depItems: m.indexDeployments(),
		recents:  m.recentSearches(),
	}
	m.searchModal = ss
	return m, m.fetchAllPods()
}

// recentSearches returns the recent distinct queries for the empty-input state
// (newest first). Empty when no store is wired or nothing was searched yet.
func (m Model) recentSearches() []string {
	if m.deps.History == nil {
		return nil
	}
	return m.deps.History.RecentSearches(m.searchScope(), recentLimit)
}

// indexNamespaces builds namespace index items from the loaded overview (the
// authoritative cluster-wide namespace list). Falls back to the namespaces
// implied by the loaded deployments if the overview has not landed yet.
func (m Model) indexNamespaces() []searchItem {
	seen := map[string]bool{}
	out := []searchItem{}
	add := func(ns string) {
		if ns == "" || seen[ns] {
			return
		}
		seen[ns] = true
		out = append(out, searchItem{kind: searchNamespace, namespace: ns, name: ns})
	}
	for i := range m.stack {
		if m.stack[i].kind == levelOverview {
			for _, ns := range m.stack[i].overview.Namespaces {
				add(ns.Name)
			}
		}
	}
	// Fallback: namespaces of any cached deployments, so the index isn't empty on
	// a cold overview.
	for _, d := range m.allDeploymentsForIndex() {
		add(d.Namespace)
	}
	return out
}

// indexDeployments builds deployment index items from the cluster-wide
// all-deployments snapshot (cache or the overview feed).
func (m Model) indexDeployments() []searchItem {
	deps := m.allDeploymentsForIndex()
	out := make([]searchItem, 0, len(deps))
	for _, d := range deps {
		out = append(out, searchItem{
			kind: searchDeployment, namespace: d.Namespace, deployment: d.Name, name: d.Name,
		})
	}
	return out
}

// allDeploymentsForIndex returns the cluster-wide deployments to index from,
// preferring the all-deployments cache (what the overview's version hints are
// built from). Empty when nothing is cached yet — deployments then simply don't
// appear until the feed lands and the user reopens search.
func (m Model) allDeploymentsForIndex() []k8s.Deployment {
	if m.deps.AllDeployCache == nil {
		return nil
	}
	deps, _, found := m.deps.AllDeployCache.Get(m.deps.Cluster)
	if !found {
		return nil
	}
	return deps
}

// onAllPodsLoaded merges the cluster-wide pods into the search index (a no-op
// when the modal has since closed). Pods that map to no deployment are skipped —
// they can't be addressed by the (namespace, deployment) jump. After merging it
// re-filters so a query typed before pods arrived now includes pod results.
func (m Model) onAllPodsLoaded(msg allPodsLoadedMsg) Model {
	ss := m.searchModal
	if ss == nil {
		return m // modal closed before pods landed — drop them
	}
	ss.podsLoaded = true
	if msg.err != nil {
		return m // keep namespaces+deployments; pods just won't be in the index
	}
	items := make([]searchItem, 0, len(msg.pods))
	for _, p := range msg.pods {
		if p.Deployment == "" {
			continue // not addressable by the deployment-scoped jump
		}
		items = append(items, searchItem{
			kind: searchPod, namespace: p.Namespace, deployment: p.Deployment, pod: p.Name, name: p.Name,
		})
	}
	ss.podItems = items
	m.refilterSearch(ss)
	return m
}

// handleSearchKey routes a key while the search modal is open. esc closes;
// ↑/↓ (and ctrl+p/ctrl+n) move within the showing list (recents or results);
// Enter selects the focused entry; everything else feeds the text input
// (typing/backspace/cursor), re-filtering after each edit.
//
// List navigation here is deliberately ARROW-only (not the views' j/k aliases):
// a free-text field needs every letter, so "jaeger" / "knative" must type
// rather than navigate. ctrl+p/ctrl+n are kept as a no-letter ergonomic
// alternative.
func (m Model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ss := m.searchModal
	switch {
	case key.Matches(msg, keys.Cancel):
		m.searchModal = nil // close (read-only — nothing to undo)
		return m, nil
	case msg.Type == tea.KeyUp, msg.Type == tea.KeyCtrlP:
		m.moveSearchCursor(-1)
		return m, nil
	case msg.Type == tea.KeyDown, msg.Type == tea.KeyCtrlN:
		m.moveSearchCursor(1)
		return m, nil
	case key.Matches(msg, keys.Confirm):
		return m.searchSelect()
	}

	// Everything else edits the text field. We forward to textinput AFTER the
	// keys above so the arrow/ctrl nav stays list-navigation (textinput binds the
	// arrows to suggestion cycling) and Enter stays "select".
	before := ss.input.Value()
	ss.input, _ = ss.input.Update(msg)
	if ss.input.Value() != before {
		// The query changed → re-filter and reset the cursor to the top result.
		m.refilterSearch(ss)
		ss.cursor = 0
	}
	return m, nil
}

// moveSearchCursor moves the focused row within the showing list (recents when
// the input is empty, else results), clamped.
func (m *Model) moveSearchCursor(delta int) {
	ss := m.searchModal
	n := m.searchRowCount(ss)
	if n == 0 {
		ss.cursor = 0
		return
	}
	ss.cursor += delta
	if ss.cursor < 0 {
		ss.cursor = 0
	}
	if ss.cursor >= n {
		ss.cursor = n - 1
	}
}

// searchRowCount is the number of navigable rows currently shown: recents while
// the input is empty, else results.
func (m Model) searchRowCount(ss *searchState) int {
	if m.searchShowingRecents(ss) {
		return len(ss.recents)
	}
	return len(ss.results)
}

// searchShowingRecents reports whether the empty-input recents list is showing
// (vs. live results). Recents show only when the field is empty.
func (m Model) searchShowingRecents(ss *searchState) bool {
	return strings.TrimSpace(ss.input.Value()) == ""
}

// searchSelect acts on Enter: on a focused recent (empty input) it re-runs that
// query (populates the field + switches to results); on a focused result it
// records the active query and jumps there, rebuilding the zoom stack.
func (m Model) searchSelect() (tea.Model, tea.Cmd) {
	ss := m.searchModal
	if m.searchShowingRecents(ss) {
		if ss.cursor < 0 || ss.cursor >= len(ss.recents) {
			return m, nil
		}
		q := ss.recents[ss.cursor]
		ss.input.SetValue(q)
		ss.input.CursorEnd()
		m.refilterSearch(ss)
		ss.cursor = 0
		return m, nil
	}
	if ss.cursor < 0 || ss.cursor >= len(ss.results) {
		return m, nil
	}
	item := ss.results[ss.cursor]
	// Record the query that led here (recents = searches that went somewhere).
	m.recordSearch(ss.input.Value())
	return m.jumpTo(item)
}

// recordSearch records the active query string into the learning store (recency
// list). Best-effort; nil store / empty query simply skips (RecordSearch trims).
func (m Model) recordSearch(query string) {
	if m.deps.History == nil {
		return
	}
	_ = m.deps.History.RecordSearch(m.searchScope(), query)
}

// jumpTo closes the modal and rebuilds the zoom stack to land on the selected
// result, seeding each new level from cache for an instant paint and firing its
// fetch. The base is ALWAYS levelOverview so the user can zoom all the way back
// out:
//
//	namespace  → [overview, namespace]
//	deployment → [overview, namespace, deployment]            (its pods view)
//	pod        → [overview, namespace, deployment] + cursor on the pod
//
// For a pod we stash the target pod on the deployment level so onPodsLoaded lands
// the cursor on it once pods arrive (the pods may not be in the seeded cache).
func (m Model) jumpTo(item searchItem) (tea.Model, tea.Cmd) {
	m.searchModal = nil

	base := m.seedOverview()
	cmds := []tea.Cmd{m.fetchFor(base)}

	switch item.kind {
	case searchNamespace:
		nsLevel := m.seedNamespace(item.namespace)
		m.stack = []level{base, nsLevel}
		cmds = append(cmds, m.fetchFor(nsLevel))

	case searchDeployment:
		nsLevel := m.seedNamespace(item.namespace)
		depLevel := m.seedDeployment(item.namespace, item.deployment)
		depLevel.loading = true
		m.stack = []level{base, nsLevel, depLevel}
		cmds = append(cmds, m.fetchFor(nsLevel), m.fetchFor(depLevel))

	case searchPod:
		nsLevel := m.seedNamespace(item.namespace)
		depLevel := m.seedDeployment(item.namespace, item.deployment)
		depLevel.loading = true
		depLevel.targetPod = item.pod // onPodsLoaded lands the cursor on it
		// If the seeded cache already has the pod, select it now for an instant
		// paint; onPodsLoaded re-confirms (and clears the target) when fresh pods
		// land.
		if row, ok := podRowOf(depLevel.pods, item.pod); ok {
			depLevel.cursor = row
		}
		m.stack = []level{base, nsLevel, depLevel}
		cmds = append(cmds, m.fetchFor(nsLevel), m.fetchFor(depLevel))
	}

	return m, tea.Batch(cmds...)
}

// ── Index filtering + ranking ─────────────────────────────────────────────────

// refilterSearch recomputes ss.results from the current query against the full
// index (namespaces + deployments + pods). An empty query clears results (the
// recents list shows instead).
func (m Model) refilterSearch(ss *searchState) {
	q := strings.TrimSpace(ss.input.Value())
	if q == "" {
		ss.results = nil
		return
	}
	ss.results = rankSearch(q, ss.nsItems, ss.depItems, ss.podItems)
}

// searchRank scores how well an item's name matches the query: lower is better.
// 0 = exact, 1 = prefix, 2 = substring, 3 = subsequence; -1 = no match. The
// match is case-insensitive (both sides lowercased by the caller).
func searchRank(name, q string) int {
	switch {
	case name == q:
		return 0
	case strings.HasPrefix(name, q):
		return 1
	case strings.Contains(name, q):
		return 2
	case isSubsequence(q, name):
		return 3
	default:
		return -1
	}
}

// isSubsequence reports whether every rune of needle appears in haystack in
// order (a loose fuzzy match, e.g. "rsp" matches "responder"). Both args are
// already lowercased.
func isSubsequence(needle, haystack string) bool {
	if needle == "" {
		return true
	}
	nr := []rune(needle)
	i := 0
	for _, h := range haystack {
		if h == nr[i] {
			i++
			if i == len(nr) {
				return true
			}
		}
	}
	return false
}

// kindOrder ranks kinds when score+name tie, so a namespace sorts before its
// deployments before their pods (the natural zoom order).
func kindOrder(k searchKind) int {
	switch k {
	case searchNamespace:
		return 0
	case searchDeployment:
		return 1
	default:
		return 2
	}
}

// scoredItem pairs an index item with its match score for ranking.
type scoredItem struct {
	item  searchItem
	score int
}

// rankSearch filters the combined index to the items matching q and returns them
// ranked: by match quality (exact → prefix → substring → subsequence), then kind
// (namespace → deployment → pod), then name, then the full path — a stable,
// predictable order. Matching is case-insensitive.
func rankSearch(q string, halves ...[]searchItem) []searchItem {
	ql := strings.ToLower(strings.TrimSpace(q))
	if ql == "" {
		return nil
	}
	var scored []scoredItem
	for _, half := range halves {
		for _, it := range half {
			s := searchRank(strings.ToLower(it.name), ql)
			if s < 0 {
				continue
			}
			scored = append(scored, scoredItem{item: it, score: s})
		}
	}
	sort.SliceStable(scored, func(a, b int) bool {
		ia, ib := scored[a], scored[b]
		if ia.score != ib.score {
			return ia.score < ib.score
		}
		if ko := kindOrder(ia.item.kind) - kindOrder(ib.item.kind); ko != 0 {
			return ko < 0
		}
		if ia.item.name != ib.item.name {
			return ia.item.name < ib.item.name
		}
		return searchPath(ia.item) < searchPath(ib.item)
	})
	out := make([]searchItem, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.item)
	}
	return out
}

// searchPath renders an item's full path for the result row and for tie-break
// ordering: "ns" · "ns · deploy" · "ns · deploy · pod" by kind.
func searchPath(it searchItem) string {
	switch it.kind {
	case searchNamespace:
		return it.namespace
	case searchDeployment:
		return it.namespace + " · " + it.deployment
	default:
		return it.namespace + " · " + it.deployment + " · " + it.pod
	}
}

// searchKindTag is the small kind label shown on a result row.
func searchKindTag(k searchKind) string {
	switch k {
	case searchNamespace:
		return "ns"
	case searchDeployment:
		return "deploy"
	default:
		return "pod"
	}
}
