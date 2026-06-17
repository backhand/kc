// Package tui is the kc Bubble Tea application: the one-stack zoom navigation
// over the read-only data layer, with optimistic render-from-cache + background
// refresh.
//
// Shape (SPEC "Startup & data freshness — optimistic caching"):
//   - The model holds a stack of levels (all-namespaces → app-group → namespace
//     → deployment → pods). The top of the stack is the visible view.
//   - Each level caches its own snapshot. New constructs the initial stack from
//     whatever is on disk (sync cache reads — just file reads), so the first
//     paint is instant and never blocks on kubectl.
//   - Init fires the fetch Cmd for the entry level; Update folds in the
//     `…LoadedMsg`, swaps the data, persists the fresh snapshot via the cache,
//     and clears the in-flight flag. A periodic tick re-fetches the visible
//     level.
//   - View always renders the current data plus a freshness indicator.
//
// Everything Kubernetes-shaped comes from internal/* (k8s, git, resolve, cache,
// store) — this package only orchestrates and renders.
package tui

import (
	"os/exec"
	"time"

	"github.com/charmbracelet/bubbles/help"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/backhand/kc/internal/cache"
	"github.com/backhand/kc/internal/deploy"
	"github.com/backhand/kc/internal/k8s"
	"github.com/backhand/kc/internal/resolve"
	"github.com/backhand/kc/internal/store"
)

// refreshInterval is how often the visible level re-fetches for a live feel
// (SPEC: "A periodic tick re-fetches for a live feel").
const refreshInterval = 5 * time.Second

// levelKind is one rung of the zoom stack.
type levelKind int

const (
	// levelOverview is the all-namespaces landing: node header + namespace rows.
	levelOverview levelKind = iota
	// levelGroup is an <app>-* group spanning multiple namespaces.
	levelGroup
	// levelNamespace is a single namespace's deployments.
	levelNamespace
	// levelDeployment is one deployment's pods.
	levelDeployment
)

// level is one entry on the navigation stack: which kind of view, what it is
// scoped to, its cursor, its loaded data and per-level freshness/loading state.
//
// Only the fields relevant to the kind are populated. The data fields are the
// parsed domain snapshots from internal/k8s; nil/empty until a fetch lands (or
// until a cached snapshot is seeded at construction).
type level struct {
	kind levelKind

	// Scope identifiers (populated per kind).
	app        string   // levelGroup: the <app> prefix
	groupNs    []string // levelGroup: the namespaces in the group
	namespace  string   // levelNamespace / levelDeployment
	deployment string   // levelDeployment

	// targetPod is a pod name to select once this deployment level's pods load
	// (levelDeployment only). Set when the level is pushed by a search "jump to
	// pod" so the cursor lands on the searched-for pod the moment pods arrive;
	// onPodsLoaded consumes and clears it. Empty for the normal drill-in path.
	targetPod string

	cursor int
	offset int // viewport scroll offset (first visible row)

	// Data per kind.
	overview k8s.ClusterOverview // levelOverview
	nsView   k8s.NamespaceView   // levelNamespace
	pods     []k8s.Pod           // levelDeployment

	// versionHints maps namespace name → a short version string for the
	// overview rows (distinct image tags of that namespace's deployments).
	// Populated by the background all-deployments fetch.
	versionHints map[string]string

	// Freshness / loading state.
	loading bool      // a fetch is in flight for this level
	loaded  bool      // at least one successful fetch has landed
	stamped time.Time // when the current data was produced (cache StoredAt or fetch time)
	err     string    // last fetch error, shown but does not clear data
}

// Model is the kc application model.
type Model struct {
	stack []level // nav stack; top (last) is visible

	// Dependencies / config (injected so tests never touch the real ~/.kc or a
	// real cluster).
	deps Deps

	// Terminal size (from WindowSizeMsg).
	width  int
	height int

	help     help.Model
	showHelp bool

	// deployModal is the active deploy flow (SPEC "Deploy flow (v1)"), or nil
	// when the normal zoom views are showing. When set, it owns key handling and
	// rendering until dismissed.
	deployModal *deployState

	// restartModal is the active restart confirm/rollout flow (the `r` op), or
	// nil. Like deployModal it owns key handling + rendering while set. The
	// confirm-gated `kubectl rollout restart` is the only mutation it fires.
	restartModal *restartState

	// scaleModal is the active scale flow (the `s` op), or nil. Like the other
	// modals it owns key handling + rendering while set. The confirm-gated
	// `kubectl scale --replicas=N` (per checked deployment) is the only mutation
	// it fires.
	scaleModal *scaleState

	// searchModal is the active search-everywhere flow (the `/` op), or nil. Like
	// the other modals it owns key handling + rendering while set. It is
	// read-only: selecting a result rebuilds the zoom stack to jump there (no
	// mutation). It cannot open while a deploy/restart/scale modal is up.
	searchModal *searchState

	quitting bool
}

// Deps bundles everything the model needs from the outside world. main wires
// the real implementations; tests inject fakes + temp dirs.
type Deps struct {
	// KubeOpts threads kubeconfig/context/timeout into every kubectl shell-out.
	KubeOpts k8s.Options
	// Cluster is the cache key for cluster-scoped snapshots (the kube-context
	// name). Namespace/deployment caches key off cluster + scope.
	Cluster string

	// Cache stores keyed off Cluster (× namespace × deployment). Constructed
	// against ~/.kc in main; against a temp dir in tests.
	OverviewCache   *cache.Cache[k8s.ClusterOverview]
	NamespaceCache  *cache.Cache[k8s.NamespaceView]
	PodsCache       *cache.Cache[[]k8s.Pod]
	AllDeployCache  *cache.Cache[[]k8s.Deployment]
	VersionHintFunc func([]k8s.Deployment) map[string]string

	// History is the generic learning store: remembers the last-viewed namespace
	// per app on entry AND the deploy presets (RecordDeploy / DeployPresets) the
	// deploy modal pre-checks. Nil disables recording.
	History *store.ActionHistory
	// App is the repo/app name the learning records are scoped under (empty when
	// not launched in a repo — recording is then skipped).
	App string

	// Runner executes the mutating kubectl ops (deploy's `set image` / restart's
	// `rollout restart` / the shared `rollout status`). Nil = the real exec.Run
	// shell-out (the deploy package's default); tests inject a capture func to
	// assert the constructed kubectl argv WITHOUT touching a cluster.
	Runner deploy.Runner

	// ExecHook, when set, intercepts the interactive/streaming op commands
	// (logs/shell) instead of handing them to tea.ExecProcess. Tests inject it to
	// capture the constructed *exec.Cmd (path+args) WITHOUT spawning kubectl —
	// never opening a real shell or streaming `-f`. Nil in production (the ops
	// then suspend the TUI and run on the real terminal).
	ExecHook func(*exec.Cmd)

	// Fetchers are the data-layer calls, injectable for tests. When nil, New
	// falls back to the real internal/k8s + internal/resolve functions bound to
	// KubeOpts.
	Fetch Fetchers

	// Entry describes where to land on launch (resolved from the repo context
	// by main, or the zero value for the plain all-namespaces entry).
	Entry Entry
}

// Entry is the contextual entry point (SPEC "Entry point is contextual").
type Entry struct {
	// Resolution is the repo→namespace resolution (empty when not in a repo or
	// nothing matched).
	Resolution resolve.Resolution
	// PreferNamespace is the last-viewed namespace for this app (from History),
	// preferred when the app spans several namespaces. Empty = none remembered.
	PreferNamespace string
}

// New builds the initial model, seeding each entry level from the cache so the
// first paint is instant. It does NOT fetch — Init returns the fetch Cmds.
func New(deps Deps) Model {
	if deps.Fetch.Overview == nil {
		deps.Fetch = realFetchers(deps.KubeOpts)
	}
	if deps.Fetch.Releases == nil {
		// Fall back to the real release fetcher if the caller supplied other
		// fetchers but not Releases (so a partial Fetchers still deploys).
		deps.Fetch.Releases = realFetchers(deps.KubeOpts).Releases
	}
	if deps.VersionHintFunc == nil {
		deps.VersionHintFunc = versionHintsFromDeployments
	}

	m := Model{
		deps:   deps,
		help:   help.New(),
		width:  80,
		height: 24,
	}
	m.stack = m.initialStack()
	return m
}

// initialStack reconstructs the entry stack from the repo resolution, seeding
// each level from cache. The stack always has levelOverview at its base so
// Backspace can zoom all the way out.
func (m *Model) initialStack() []level {
	base := m.seedOverview()

	res := m.deps.Entry.Resolution
	// Not in a repo / nothing resolved → land on all-namespaces.
	if len(res.Namespaces) == 0 {
		return []level{base}
	}

	// Choose the target namespace: the remembered one if it is among the
	// resolved namespaces, else the first resolved namespace.
	target := res.Namespaces[0].Namespace
	if pref := m.deps.Entry.PreferNamespace; pref != "" {
		for _, rn := range res.Namespaces {
			if rn.Namespace == pref {
				target = pref
				break
			}
		}
	}

	// Find the group the target namespace belongs to.
	var grp *resolve.Group
	for i := range res.Groups {
		for _, ns := range res.Groups[i].Namespaces {
			if ns == target {
				grp = &res.Groups[i]
				break
			}
		}
		if grp != nil {
			break
		}
	}

	stack := []level{base}
	// Push the app-group level only when the app spans >1 namespace, so the
	// single-namespace case lands directly on the namespace (SPEC).
	if grp != nil && len(grp.Namespaces) > 1 {
		stack = append(stack, m.seedGroup(grp.App, grp.Namespaces))
	}
	stack = append(stack, m.seedNamespace(target))
	// Strengthen the last-viewed preference for next launch.
	m.recordNamespaceView(target)
	return stack
}

// ── Level seeding (sync cache reads — instant, never blocks) ────────────────

func (m *Model) seedOverview() level {
	// loading=true: Init always fires this level's fetch, so the freshness
	// indicator should read "↻ refreshing…" from the first paint.
	l := level{kind: levelOverview, loading: true}
	if m.deps.OverviewCache != nil {
		if ov, age, found := m.deps.OverviewCache.Get(m.deps.Cluster); found {
			l.overview = ov
			l.loaded = true
			l.stamped = time.Now().Add(-age)
		}
	}
	if m.deps.AllDeployCache != nil {
		if deps, _, found := m.deps.AllDeployCache.Get(m.deps.Cluster); found {
			l.versionHints = m.deps.VersionHintFunc(deps)
		}
	}
	return l
}

func (m *Model) seedGroup(app string, namespaces []string) level {
	return level{kind: levelGroup, app: app, groupNs: namespaces}
}

func (m *Model) seedNamespace(ns string) level {
	l := level{kind: levelNamespace, namespace: ns, loading: true}
	// Pre-classify so the header reads "[user]"/"[system]" immediately, before
	// the fetch lands (the fetch confirms the same Kind).
	l.nsView = k8s.NamespaceView{Namespace: ns, Kind: k8s.ClassifyNamespace(ns)}
	if m.deps.NamespaceCache != nil {
		if v, age, found := m.deps.NamespaceCache.Get(m.nsKey(ns)); found {
			l.nsView = v
			l.loaded = true
			l.stamped = time.Now().Add(-age)
		}
	}
	return l
}

func (m *Model) seedDeployment(ns, deployment string) level {
	l := level{kind: levelDeployment, namespace: ns, deployment: deployment}
	if m.deps.PodsCache != nil {
		if pods, age, found := m.deps.PodsCache.Get(m.podsKey(ns, deployment)); found {
			l.pods = pods
			l.loaded = true
			l.stamped = time.Now().Add(-age)
		}
	}
	return l
}

// ── Cache keys ──────────────────────────────────────────────────────────────

func (m *Model) nsKey(ns string) string        { return m.deps.Cluster + "/" + ns }
func (m *Model) podsKey(ns, dep string) string { return m.deps.Cluster + "/" + ns + "/" + dep }

// top returns a pointer to the visible level.
func (m *Model) top() *level { return &m.stack[len(m.stack)-1] }

// Init fires the fetch for the entry level(s) and starts the refresh tick.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tickCmd()}
	// Refresh every level on the entry stack so a deep entry (repo → namespace)
	// has fresh data at each rung the user can zoom back to, and so the overview
	// version hints populate.
	for i := range m.stack {
		if c := m.fetchFor(m.stack[i]); c != nil {
			cmds = append(cmds, c)
		}
	}
	cmds = append(cmds, m.fetchAllDeployments())
	return tea.Batch(cmds...)
}
