package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/cache"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/resolve"
)

// resolution builds a resolve.Resolution over the given namespaces, grouped by
// <app>-* prefix (mirroring resolve.FromDeployments' grouping).
func resolution(namespaces ...string) resolve.Resolution {
	res := resolve.Resolution{Image: "ghcr.io/thinkpilot/mailon"}
	byApp := map[string][]string{}
	for _, ns := range namespaces {
		res.Namespaces = append(res.Namespaces, resolve.ResolvedNamespace{
			Namespace: ns, Deployments: []string{"responder"},
		})
		app := ns
		if i := strings.Index(app, "-"); i != -1 {
			app = app[:i]
		}
		byApp[app] = append(byApp[app], ns)
	}
	for app, ns := range byApp {
		res.Groups = append(res.Groups, resolve.Group{App: app, Namespaces: ns})
	}
	return res
}

// ── Fixtures ────────────────────────────────────────────────────────────────

const testCluster = "test-cluster"

func freshOverview() k8s.ClusterOverview {
	return k8s.ClusterOverview{
		Nodes: []k8s.Node{
			{Name: "cp-0", ControlPlane: true, Ready: true, KubeletVersion: "v1.30.0",
				Capacity: k8s.Usage{CPUMillicores: 4000, MemoryBytes: 8 << 30},
				Usage:    &k8s.Usage{CPUMillicores: 800, MemoryBytes: 2 << 30}},
			{Name: "agent-0", Ready: true, KubeletVersion: "v1.30.0",
				Capacity: k8s.Usage{CPUMillicores: 4000, MemoryBytes: 8 << 30},
				Usage:    &k8s.Usage{CPUMillicores: 1200, MemoryBytes: 3 << 30}},
		},
		Namespaces: []k8s.Namespace{
			{Name: "mailon", Kind: k8s.KindUser, Phase: "Active"},
			{Name: "mailon-staging", Kind: k8s.KindUser, Phase: "Active"},
			{Name: "kube-system", Kind: k8s.KindSystem, Phase: "Active"},
		},
		Totals: k8s.Totals{
			Usage:    &k8s.Usage{CPUMillicores: 2000, MemoryBytes: 5 << 30},
			Capacity: k8s.Usage{CPUMillicores: 8000, MemoryBytes: 16 << 30},
		},
	}
}

// staleOverview is what we seed into the cache: a different namespace set so we
// can distinguish "rendered from cache" from "rendered from the fresh fetch".
func staleOverview() k8s.ClusterOverview {
	return k8s.ClusterOverview{
		Nodes: []k8s.Node{
			{Name: "cp-0", ControlPlane: true, Ready: true, KubeletVersion: "v1.29.0",
				Capacity: k8s.Usage{CPUMillicores: 4000, MemoryBytes: 8 << 30}},
		},
		Namespaces: []k8s.Namespace{
			{Name: "cached-only-ns", Kind: k8s.KindUser, Phase: "Active"},
		},
		Totals: k8s.Totals{Capacity: k8s.Usage{CPUMillicores: 4000, MemoryBytes: 8 << 30}},
	}
}

func mailonNamespaceView() k8s.NamespaceView {
	return k8s.NamespaceView{
		Namespace: "mailon",
		Kind:      k8s.KindUser,
		Deployments: []k8s.Deployment{
			{Namespace: "mailon", Name: "responder", Image: k8s.ImageRef{Tag: "v0.6.9"},
				ReadyReplicas: 2, DesiredReplicas: 2,
				Usage: &k8s.Usage{CPUMillicores: 150, MemoryBytes: 256 << 20}},
			{Namespace: "mailon", Name: "sender", Image: k8s.ImageRef{Tag: "v0.6.9"},
				ReadyReplicas: 1, DesiredReplicas: 1},
		},
	}
}

func responderPods() []k8s.Pod {
	return []k8s.Pod{
		{Namespace: "mailon", Name: "responder-aaa", Deployment: "responder", Phase: "Running",
			Ready: true, Node: "agent-0", Restarts: 0,
			Usage: &k8s.Usage{CPUMillicores: 75, MemoryBytes: 128 << 20}},
		{Namespace: "mailon", Name: "responder-bbb", Deployment: "responder", Phase: "Running",
			Ready: true, Node: "agent-0", Restarts: 3},
	}
}

// ── Test harness ─────────────────────────────────────────────────────────────

// harness builds Deps wired to temp-dir caches and the given fetchers. The
// overview cache is pre-seeded with staleOverview so optimistic-render tests can
// assert the cached frame paints before any fetch resolves.
type harness struct {
	deps    Deps
	baseDir string
}

func newHarness(t *testing.T, fetch Fetchers) harness {
	t.Helper()
	base := t.TempDir()
	now := time.Now()
	clock := func() time.Time { return now }

	ovc := cache.New[k8s.ClusterOverview](cache.Options{BaseDir: base, Namespace: "overview", Now: clock})
	// Seed the cache so the first paint is the cached (stale) snapshot.
	if err := ovc.Put(testCluster, staleOverview()); err != nil {
		t.Fatalf("seed overview cache: %v", err)
	}

	deps := Deps{
		Cluster:        testCluster,
		OverviewCache:  ovc,
		NamespaceCache: cache.New[k8s.NamespaceView](cache.Options{BaseDir: base, Namespace: "namespace"}),
		PodsCache:      cache.New[[]k8s.Pod](cache.Options{BaseDir: base, Namespace: "pods"}),
		AllDeployCache: cache.New[[]k8s.Deployment](cache.Options{BaseDir: base, Namespace: "alldeploy"}),
		Fetch:          fetch,
	}
	return harness{deps: deps, baseDir: base}
}

// defaultFetchers returns immediately with the fresh fixtures.
func defaultFetchers() Fetchers {
	return Fetchers{
		Overview: func(context.Context) (k8s.ClusterOverview, error) { return freshOverview(), nil },
		Namespace: func(_ context.Context, ns string) (k8s.NamespaceView, error) {
			return mailonNamespaceView(), nil
		},
		DeploymentPods: func(_ context.Context, ns, dep string) ([]k8s.Pod, error) {
			return responderPods(), nil
		},
		AllDeployments: func(context.Context) ([]k8s.Deployment, error) {
			return mailonNamespaceView().Deployments, nil
		},
	}
}

func runeMsg(r rune) tea.Msg       { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func keyMsg(t tea.KeyType) tea.Msg { return tea.KeyMsg{Type: t} }

// ── Tests ────────────────────────────────────────────────────────────────────

// TestOptimisticRender is the centerpiece: the cached (stale) snapshot must
// paint immediately — before any fetch resolves — and then the fresh fetch must
// swap the data in AND rewrite the cache. We gate the overview fetch on a
// channel so we can assert ordering: the cached namespace appears first, the
// fresh namespace only after we release the fetch.
func TestOptimisticRender(t *testing.T) {
	release := make(chan struct{})
	fetch := defaultFetchers()
	fetch.Overview = func(context.Context) (k8s.ClusterOverview, error) {
		<-release // block until the test allows the fetch to complete
		return freshOverview(), nil
	}
	// Keep the version-hint feed from racing the assertions.
	fetch.AllDeployments = func(context.Context) ([]k8s.Deployment, error) {
		<-release
		return nil, nil
	}
	h := newHarness(t, fetch)

	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 30))

	// 1) First paint is the CACHED snapshot — "cached-only-ns" present, the fresh
	//    "mailon" namespace absent — and the freshness reads "refreshing…".
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("cached-only-ns")) &&
			bytes.Contains(b, []byte("refreshing")) &&
			!bytes.Contains(b, []byte("mailon"))
	}, teatest.WithDuration(3*time.Second))

	// 2) Release the fetch; the fresh snapshot must replace the cached one.
	close(release)
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("mailon")) &&
			bytes.Contains(b, []byte("kube-system")) &&
			!bytes.Contains(b, []byte("cached-only-ns")) &&
			bytes.Contains(b, []byte("updated"))
	}, teatest.WithDuration(3*time.Second))

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// 3) The fresh snapshot was persisted to the cache (overwriting the seed):
	//    a fresh reader sees the fresh namespaces, not the stale one.
	got, _, found := h.deps.OverviewCache.Get(testCluster)
	if !found {
		t.Fatal("overview cache missing after fetch")
	}
	names := nsNames(got)
	if !contains(names, "mailon") || contains(names, "cached-only-ns") {
		t.Fatalf("cache not rewritten with fresh snapshot; namespaces=%v", names)
	}
}

// TestNavigationZoomStack drives the full drill-in path
// (all-namespaces → namespace → deployment → pods) and the Backspace zoom-out,
// asserting each level's distinctive content renders.
func TestNavigationZoomStack(t *testing.T) {
	h := newHarness(t, defaultFetchers())
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 30))

	// Land on the fresh overview.
	waitFor(t, tm, "mailon", "all-namespaces")

	// Enter → namespace view (cursor starts on the first row = "mailon").
	tm.Send(keyMsg(tea.KeyEnter))
	waitFor(t, tm, "responder", "DEPLOYMENT")

	// Enter → deployment's pods.
	tm.Send(keyMsg(tea.KeyEnter))
	waitFor(t, tm, "responder-aaa", "POD")

	// Backspace → back to the namespace view.
	tm.Send(keyMsg(tea.KeyBackspace))
	waitFor(t, tm, "responder", "DEPLOYMENT")

	// Backspace → back to the overview.
	tm.Send(keyMsg(tea.KeyBackspace))
	waitFor(t, tm, "kube-system", "all-namespaces")

	// Backspace at the root is a no-op — it renders no new frame (so we can't
	// WaitFor here; teatest's reader drains consumed bytes). The final-model
	// stack assertion below proves the root was never popped.
	tm.Send(keyMsg(tea.KeyBackspace))

	tm.Send(runeMsg('q'))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	final, ok := fm.(Model)
	if !ok {
		t.Fatalf("final model type %T", fm)
	}
	if len(final.stack) != 1 || final.top().kind != levelOverview {
		t.Fatalf("expected to end on the overview root; stack depth=%d", len(final.stack))
	}
}

// TestCursorMovement asserts ↑/↓ (and j/k) move the selection, clamped, and
// that the selection drives which child Enter drills into.
func TestCursorMovement(t *testing.T) {
	h := newHarness(t, defaultFetchers())
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 30))
	waitFor(t, tm, "mailon-staging", "all-namespaces")

	// Move down to the 2nd namespace ("mailon-staging"), then Enter; the
	// namespace fetcher returns the mailon view regardless, but the top-bar
	// context must read "mailon-staging · [user]" (proving cursor→selection
	// wiring drives which child is pushed).
	tm.Send(runeMsg('j')) // -> mailon-staging
	tm.Send(keyMsg(tea.KeyEnter))
	waitFor(t, tm, "mailon-staging · [user]")

	tm.Send(runeMsg('q'))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	final := fm.(Model)
	if got := final.top().namespace; got != "mailon-staging" {
		t.Fatalf("drilled into namespace %q, want mailon-staging (cursor not honored)", got)
	}
}

// TestQuitClean asserts `q` tears the program down cleanly from the entry view.
func TestQuitClean(t *testing.T) {
	h := newHarness(t, defaultFetchers())
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(100, 24))
	waitFor(t, tm, "all-namespaces", "")
	tm.Send(runeMsg('q'))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	if final, ok := fm.(Model); !ok || !final.quitting {
		t.Fatalf("expected a clean quit; model=%#v ok=%v", fm, ok)
	}
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestEntryAtNamespace asserts the contextual entry point: with a single
// resolved namespace, New lands directly on that namespace view (not the
// overview), and Backspace reconstructs the stack upward to all-namespaces.
func TestEntryAtNamespace(t *testing.T) {
	h := newHarness(t, defaultFetchers())
	h.deps.Entry = Entry{Resolution: resolution("mailon")}
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 30))

	// Entry view is the mailon namespace (deployments visible immediately); the
	// top-bar context reads "mailon · [user]".
	waitFor(t, tm, "responder", "mailon · [user]")

	// Backspace zooms out to the overview.
	tm.Send(keyMsg(tea.KeyBackspace))
	waitFor(t, tm, "kube-system", "all-namespaces")

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestFetchErrorKeepsStaleData asserts a fetch error surfaces an error line but
// does NOT clear the cached data (SPEC: "keep stale data").
func TestFetchErrorKeepsStaleData(t *testing.T) {
	fetch := defaultFetchers()
	fetch.Overview = func(context.Context) (k8s.ClusterOverview, error) {
		return k8s.ClusterOverview{}, context.DeadlineExceeded
	}
	h := newHarness(t, fetch)
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 30))

	// The cached namespace stays on screen and an error line appears.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("cached-only-ns")) && bytes.Contains(b, []byte("error:"))
	}, teatest.WithDuration(3*time.Second))

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestOverviewFrameLayout captures the rendered overview frame and asserts the
// header layout: the cluster-total row comes first, the per-node rows underneath
// share the cluster row's column starts (the "cpu" gauge token at an identical
// offset on every resource row), a blank line separates the node block from the
// NAMESPACE list, and the freshness indicator lives in the top bar (line 1,
// after the "kc · …" breadcrumb) rather than in the footer.
func TestOverviewFrameLayout(t *testing.T) {
	h := newHarness(t, defaultFetchers())
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 30))
	// Wait for the fresh overview (mailon + the node rows + a freshness stamp).
	waitFor(t, tm, "mailon", "kube-system", "updated")
	tm.Send(runeMsg('q'))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	m, ok := fm.(Model)
	if !ok {
		t.Fatalf("final model type %T", fm)
	}
	m.quitting = false // View() blanks while quitting; render the last real frame
	frame := stripANSI(m.View())
	lines := strings.Split(frame, "\n")

	// 1) Freshness in the TOP BAR: line 0 carries "kc · all-namespaces" AND the
	//    freshness stamp; the footer (key hints) must NOT carry it.
	if !strings.Contains(lines[0], "kc · all-namespaces") || !strings.Contains(lines[0], "updated") {
		t.Fatalf("top bar missing breadcrumb+freshness; got %q", lines[0])
	}
	for _, ln := range lines {
		if strings.Contains(ln, "[d]eploy") && strings.Contains(ln, "updated") {
			t.Fatalf("freshness leaked into the footer line: %q", ln)
		}
	}

	// Locate the resource rows and the NAMESPACE column header by content.
	idxCluster, idxFirstNode, idxNamespace := -1, -1, -1
	for i, ln := range lines {
		s := strings.TrimSpace(ln)
		switch {
		case idxCluster < 0 && strings.HasPrefix(s, "cluster"):
			idxCluster = i
		case idxCluster >= 0 && idxFirstNode < 0 && strings.HasPrefix(s, "cp-0"):
			idxFirstNode = i
		case strings.HasPrefix(s, "NAMESPACE"):
			idxNamespace = i
		}
	}
	if idxCluster < 0 || idxFirstNode < 0 || idxNamespace < 0 {
		t.Fatalf("could not locate header rows in frame:\n%s", frame)
	}

	// 2) Cluster total on TOP, the first node below it.
	if !(idxCluster < idxFirstNode) {
		t.Fatalf("cluster row (%d) must precede the node rows (%d)", idxCluster, idxFirstNode)
	}

	// 1) Columns aligned: the "cpu " token starts at the same offset on the
	//    cluster row and every node row (cp-0, agent-0).
	clusterCPU := strings.Index(lines[idxCluster], "cpu ")
	if clusterCPU < 0 {
		t.Fatalf("no cpu gauge on the cluster row: %q", lines[idxCluster])
	}
	for i := idxFirstNode; i < idxNamespace; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if got := strings.Index(lines[i], "cpu "); got != clusterCPU {
			t.Fatalf("cpu column misaligned: cluster@%d vs node row %q cpu@%d",
				clusterCPU, lines[i], got)
		}
	}

	// 3) Blank line between the node block and the NAMESPACE list. The line
	//    directly above the NAMESPACE header must be empty.
	if got := strings.TrimSpace(lines[idxNamespace-1]); got != "" {
		t.Fatalf("expected a blank line before the NAMESPACE list, got %q", lines[idxNamespace-1])
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// stripANSI removes ANSI escape sequences so frame assertions compare visible
// text and column offsets (lipgloss color codes would otherwise skew indices).
func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// waitFor waits until the output contains all of the given substrings.
func waitFor(t *testing.T, tm *teatest.TestModel, subs ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, s := range subs {
			if s == "" {
				continue
			}
			if !bytes.Contains(b, []byte(s)) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(3*time.Second))
}

func nsNames(ov k8s.ClusterOverview) []string {
	out := make([]string, 0, len(ov.Namespaces))
	for _, n := range ov.Namespaces {
		out = append(out, n.Name)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// confirm the seed file actually exists on disk (sanity for the cache wiring).
func TestCacheSeedOnDisk(t *testing.T) {
	h := newHarness(t, defaultFetchers())
	entries, err := os.ReadDir(filepath.Join(h.baseDir, "cache", "overview"))
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	var jsons int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsons++
		}
	}
	if jsons == 0 {
		t.Fatal("expected a seeded overview snapshot file on disk")
	}
}
