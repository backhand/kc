package tui

import (
	"context"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/backhand/kc/internal/deploy"
	"github.com/backhand/kc/internal/git"
	"github.com/backhand/kc/internal/github"
	"github.com/backhand/kc/internal/k8s"
)

// releaseLimit is how many latest releases the deploy modal shows per page
// (SPEC: "5 latest GitHub releases").
const releaseLimit = 5

// Fetchers are the data-layer calls the TUI depends on, abstracted so tests can
// inject deterministic fakes (offline, no kubectl/gh). main uses realFetchers.
type Fetchers struct {
	Overview       func(ctx context.Context) (k8s.ClusterOverview, error)
	Namespace      func(ctx context.Context, ns string) (k8s.NamespaceView, error)
	DeploymentPods func(ctx context.Context, ns, deployment string) ([]k8s.Pod, error)
	AllDeployments func(ctx context.Context) ([]k8s.Deployment, error)
	// AllPods fetches every pod cluster-wide (each with its owning Deployment
	// resolved). Fired when the search modal opens so the `/` index can offer
	// pods alongside the always-loaded namespaces + deployments.
	AllPods func(ctx context.Context) ([]k8s.Pod, error)
	// Releases fetches the latest annotated releases for a repo (deploy modal's
	// version list). limit is how many to return; image is the GHCR image to
	// probe availability against.
	Releases func(ctx context.Context, repo git.RepoRef, image string, limit int) []github.ReleaseAnnotation
	// BuildStatus polls one Actions run's build status by id — the deploy flow's
	// "wait for the build" step (deploy a still-building version once it's green).
	// Injected so tests drive it without `gh`.
	BuildStatus func(ctx context.Context, repo git.RepoRef, runID int64) (github.BuildStatus, error)
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
		AllPods: func(ctx context.Context) ([]k8s.Pod, error) {
			return k8s.GetAllPods(ctx, opts)
		},
		Releases: func(ctx context.Context, repo git.RepoRef, image string, limit int) []github.ReleaseAnnotation {
			return github.GetReleases(ctx, repo, github.Options{Limit: limit, GHCRImage: image})
		},
		BuildStatus: func(ctx context.Context, repo git.RepoRef, runID int64) (github.BuildStatus, error) {
			return github.RunStatus(ctx, repo, runID, 0)
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

// allPodsLoadedMsg carries every pod cluster-wide; the search modal merges these
// into its index when they land (namespaces + deployments are indexed
// immediately on open; pods join when this arrives).
type allPodsLoadedMsg struct {
	pods []k8s.Pod
	at   time.Time
	err  error
}

// releasesLoadedMsg carries the deploy modal's annotated release list for a
// page. page is the 0-based page it was fetched for, so a stale response for a
// page the user has paged away from is ignored.
type releasesLoadedMsg struct {
	page     int
	releases []github.ReleaseAnnotation
	err      error
}

// deployStepMsg carries the result of one deployment's apply (`kubectl set
// image`) + rollout-status watch. One per deployed deployment.
type deployStepMsg struct {
	deployment string
	detail     string // a short success/status line for the rollout view
	err        error
}

// buildPolledMsg carries one poll of the watched build's Actions run (the deploy
// flow's "wait for the build" step). runID tags it so a stale poll from a
// cancelled wait — or a different watch — is ignored.
type buildPolledMsg struct {
	runID  int64
	status github.BuildStatus
	err    error
}

// tickMsg drives the periodic background refresh.
type tickMsg time.Time

// buildPollInterval is how often the deploy flow re-checks a still-building
// release's Actions run. A var so tests can shrink it.
var buildPollInterval = 8 * time.Second

// maxBuildPolls bounds the total wait (~32 min at 8s) so a wedged or stuck build
// can't poll forever; exceeding it aborts the deploy (the user can also esc).
const maxBuildPolls = 240

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

// fetchAllPods fetches every pod cluster-wide for the search index. Fired when
// the search modal opens (not on launch — the index needs them only while the
// modal is up). nil when no AllPods fetcher is wired (a partial Fetchers in a
// test that does not exercise pod results), so the modal still indexes
// namespaces + deployments.
func (m Model) fetchAllPods() tea.Cmd {
	f := m.deps.Fetch.AllPods
	if f == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		pods, err := f(ctx)
		return allPodsLoadedMsg{pods: pods, at: time.Now(), err: err}
	}
}

// rolloutTimeout bounds a single `kubectl rollout status` watch. A rollout that
// stalls past this surfaces as a failure in the rollout view rather than hanging
// the tea.Cmd goroutine. Generous — image pulls can be slow.
const rolloutTimeout = 5 * time.Minute

// fetchReleases fetches one page of annotated releases for the deploy modal. The
// data layer (internal/github) fetches the LATEST `limit` releases; we page back
// by fetching `limit*(page+1)` and slicing the trailing window, so `o`lder shows
// successively older releases without a cursor API in `gh`.
func (m Model) fetchReleases(repo git.RepoRef, image string, limit, page int) tea.Cmd {
	f := m.deps.Fetch.Releases
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		// Fetch enough to cover all pages up to and including this one, then take
		// this page's window (newest page = 0).
		all := f(ctx, repo, image, limit*(page+1))
		start := page * limit
		if start > len(all) {
			start = len(all)
		}
		end := start + limit
		if end > len(all) {
			end = len(all)
		}
		return releasesLoadedMsg{page: page, releases: all[start:end]}
	}
}

// pollBuild checks the watched build run once (after an optional delay) and lands
// a buildPolledMsg. The deploy flow re-issues it until the run goes ready (deploy)
// or failed (abort). Sleeping in the Cmd goroutine is fine — tea runs Cmds
// concurrently, so it never blocks the UI; the user can esc out of the wait.
func (m Model) pollBuild(repo git.RepoRef, runID int64, delay time.Duration) tea.Cmd {
	f := m.deps.Fetch.BuildStatus
	return func() tea.Msg {
		if delay > 0 {
			time.Sleep(delay)
		}
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		st, err := f(ctx, repo, runID)
		return buildPolledMsg{runID: runID, status: st, err: err}
	}
}

// runDeployStep applies one change with `kubectl set image` (THE mutation —
// confirm-gated in the UI flow) and then watches its rollout with `kubectl
// rollout status`. The injected Runner (tests' capture func, else exec.Run)
// performs both, so headless tests assert argv without a cluster.
func (m Model) runDeployStep(c deploy.Change) tea.Cmd {
	kopts := m.deps.KubeOpts
	runner := m.deps.Runner
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), rolloutTimeout+fetchTimeout)
		defer cancel()

		// 1) Apply the new image (the real, confirmed mutation).
		if _, err := deploy.SetImage(ctx, kopts, c.Namespace, c.Deployment, c.Container, c.Image,
			deploy.SetImageOpts{Runner: runner}); err != nil {
			return deployStepMsg{deployment: c.Deployment, err: err}
		}
		// 2) Watch the rollout to completion.
		res, err := deploy.RolloutStatus(ctx, kopts, c.Namespace, c.Deployment,
			deploy.RolloutOpts{Timeout: rolloutTimeout, Runner: runner})
		if err != nil {
			return deployStepMsg{deployment: c.Deployment, err: err}
		}
		return deployStepMsg{deployment: c.Deployment, detail: lastLine(res.Stdout)}
	}
}

// lastLine returns the last non-empty line of s (the meaningful tail of a
// `rollout status` stream), or "" when there is none.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
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
