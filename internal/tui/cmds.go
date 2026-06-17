package tui

import (
	"context"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
)

// Fetchers are the data-layer calls the TUI depends on, abstracted so tests can
// inject deterministic fakes (offline, no kubectl). main uses realFetchers.
type Fetchers struct {
	Overview       func(ctx context.Context) (k8s.ClusterOverview, error)
	Namespace      func(ctx context.Context, ns string) (k8s.NamespaceView, error)
	DeploymentPods func(ctx context.Context, ns, deployment string) ([]k8s.Pod, error)
	AllDeployments func(ctx context.Context) ([]k8s.Deployment, error)
}

// realFetchers binds the data-layer functions to the given kube options.
func realFetchers(opts k8s.Options) Fetchers {
	return Fetchers{
		Overview: func(ctx context.Context) (k8s.ClusterOverview, error) {
			return k8s.GetClusterOverview(ctx, opts)
		},
		Namespace: func(ctx context.Context, ns string) (k8s.NamespaceView, error) {
			return k8s.GetNamespace(ctx, ns, opts)
		},
		DeploymentPods: func(ctx context.Context, ns, deployment string) ([]k8s.Pod, error) {
			return k8s.GetDeploymentPods(ctx, ns, deployment, opts)
		},
		AllDeployments: func(ctx context.Context) ([]k8s.Deployment, error) {
			return k8s.GetAllDeployments(ctx, opts)
		},
	}
}

// ── Messages ────────────────────────────────────────────────────────────────
//
// Each fetch lands as a typed `…LoadedMsg`. Update folds it into the matching
// level (by scope), swaps the data, persists the fresh snapshot, and clears the
// in-flight flag. Errors arrive on the same message (Err set) so the data layer
// failing keeps the stale data on screen with an error line.

// overviewLoadedMsg carries a fresh cluster overview.
type overviewLoadedMsg struct {
	overview k8s.ClusterOverview
	at       time.Time
	err      error
}

// namespaceLoadedMsg carries a fresh namespace view, tagged with its namespace
// so it lands on the right level even if the user has zoomed elsewhere.
type namespaceLoadedMsg struct {
	namespace string
	view      k8s.NamespaceView
	at        time.Time
	err       error
}

// podsLoadedMsg carries fresh pods for a (namespace, deployment).
type podsLoadedMsg struct {
	namespace  string
	deployment string
	pods       []k8s.Pod
	at         time.Time
	err        error
}

// allDeploymentsLoadedMsg carries every deployment cluster-wide; used only to
// derive the overview's per-namespace version hints.
type allDeploymentsLoadedMsg struct {
	deployments []k8s.Deployment
	at          time.Time
	err         error
}

// tickMsg drives the periodic background refresh.
type tickMsg time.Time

// fetchTimeout bounds a single background fetch so a wedged kubectl can't hold a
// tea.Cmd goroutine forever (the data layer also enforces its own per-command
// timeout; this is the outer bound for a multi-call aggregate).
const fetchTimeout = 25 * time.Second

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ── Fetch Cmds (bridge data layer → Msgs) ───────────────────────────────────

func (m Model) fetchOverview() tea.Cmd {
	f := m.deps.Fetch.Overview
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		ov, err := f(ctx)
		return overviewLoadedMsg{overview: ov, at: time.Now(), err: err}
	}
}

func (m Model) fetchNamespace(ns string) tea.Cmd {
	f := m.deps.Fetch.Namespace
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		v, err := f(ctx, ns)
		return namespaceLoadedMsg{namespace: ns, view: v, at: time.Now(), err: err}
	}
}

func (m Model) fetchPods(ns, deployment string) tea.Cmd {
	f := m.deps.Fetch.DeploymentPods
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		pods, err := f(ctx, ns, deployment)
		return podsLoadedMsg{namespace: ns, deployment: deployment, pods: pods, at: time.Now(), err: err}
	}
}

func (m Model) fetchAllDeployments() tea.Cmd {
	f := m.deps.Fetch.AllDeployments
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		deps, err := f(ctx)
		return allDeploymentsLoadedMsg{deployments: deps, at: time.Now(), err: err}
	}
}

// fetchFor returns the fetch Cmd appropriate to a level's kind (nil for a group
// level, which has no data of its own — its rows come from the overview).
func (m Model) fetchFor(l level) tea.Cmd {
	switch l.kind {
	case levelOverview:
		return m.fetchOverview()
	case levelNamespace:
		return m.fetchNamespace(l.namespace)
	case levelDeployment:
		return m.fetchPods(l.namespace, l.deployment)
	default:
		return nil
	}
}

// versionHintsFromDeployments derives a per-namespace version string from every
// deployment's primary image tag: the distinct tags in that namespace, joined.
// A namespace with a single tag shows that tag; multiple tags show them
// comma-separated (kept short — the renderer truncates).
func versionHintsFromDeployments(deployments []k8s.Deployment) map[string]string {
	byNs := make(map[string]map[string]struct{})
	for _, d := range deployments {
		tag := d.Image.Tag
		if tag == "" {
			continue
		}
		set := byNs[d.Namespace]
		if set == nil {
			set = make(map[string]struct{})
			byNs[d.Namespace] = set
		}
		set[tag] = struct{}{}
	}
	out := make(map[string]string, len(byNs))
	for ns, set := range byNs {
		tags := make([]string, 0, len(set))
		for t := range set {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		out[ns] = strings.Join(tags, ",")
	}
	return out
}
