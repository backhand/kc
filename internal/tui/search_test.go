package tui

import (
	"context"
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/cache"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/store"
)

// Headless tests for the search-everywhere modal (the `/` op). They inject fake
// fetchers + a temp-dir store — NEVER a real cluster. The index is seeded from
// the loaded overview (namespaces) + a pre-seeded all-deployments cache
// (deployments) so it is non-empty the moment `/` opens; the AllPods fetcher
// supplies cluster-wide pods that merge in.

// searchPods are cluster-wide pods spanning two namespaces, each resolving to a
// deployment so they are addressable by the (namespace, deployment) jump.
func searchPods() []k8s.Pod {
	return []k8s.Pod{
		{Namespace: "mailon", Name: "responder-aaa", Deployment: "responder", Phase: "Running", Ready: true, Node: "agent-0"},
		{Namespace: "mailon", Name: "responder-bbb", Deployment: "responder", Phase: "Running", Ready: true, Node: "agent-0"},
		{Namespace: "mailon", Name: "sender-ccc", Deployment: "sender", Phase: "Running", Ready: true, Node: "agent-0"},
		{Namespace: "mailon-staging", Name: "responder-zzz", Deployment: "responder", Phase: "Running", Ready: true, Node: "agent-0"},
		// A bare pod that maps to no deployment — must be skipped from the index.
		{Namespace: "kube-system", Name: "orphan-pod", Deployment: "", Phase: "Running", Ready: true, Node: "cp-0"},
	}
}

// searchDeploymentsFixture are the cluster-wide deployments the index draws from
// (pre-seeded into AllDeployCache). Two namespaces, distinct names.
func searchDeploymentsFixture() []k8s.Deployment {
	return []k8s.Deployment{
		{Namespace: "mailon", Name: "responder", Image: k8s.ImageRef{Tag: "v0.6.9"}, ReadyReplicas: 2, DesiredReplicas: 2},
		{Namespace: "mailon", Name: "sender", Image: k8s.ImageRef{Tag: "v0.6.9"}, ReadyReplicas: 1, DesiredReplicas: 1},
		{Namespace: "mailon-staging", Name: "responder", Image: k8s.ImageRef{Tag: "v0.7.0"}, ReadyReplicas: 1, DesiredReplicas: 1},
	}
}

// searchHarness builds Deps for the search flow: fresh overview + namespace +
// pods fetchers, an AllPods fetcher (gated by `release` so a test can assert the
// pre-pod index state first), a PRE-SEEDED all-deployments cache so deployments
// index instantly, and a temp-dir store.
type searchHarness struct {
	deps    Deps
	hist    *store.ActionHistory
	release chan struct{} // close to let the AllPods fetch return
}

func newSearchHarness(t *testing.T) searchHarness {
	t.Helper()
	base := t.TempDir()
	hist := store.New(store.Options{BaseDir: base})
	release := make(chan struct{})

	fetch := defaultFetchers()
	fetch.Namespace = func(_ context.Context, _ string) (k8s.NamespaceView, error) {
		return mailonDeployNamespaceView(), nil
	}
	fetch.DeploymentPods = func(_ context.Context, _, _ string) ([]k8s.Pod, error) { return responderPods(), nil }
	fetch.AllDeployments = func(context.Context) ([]k8s.Deployment, error) { return searchDeploymentsFixture(), nil }
	fetch.AllPods = func(context.Context) ([]k8s.Pod, error) {
		<-release
		return searchPods(), nil
	}

	adc := cache.New[[]k8s.Deployment](cache.Options{BaseDir: base, Namespace: "alldeploy"})
	// Seed the all-deployments cache so the index has deployments at open
	// (Init's background feed would also fill it, but seeding makes it
	// deterministic — no race with the modal opening).
	if err := adc.Put(testCluster, searchDeploymentsFixture()); err != nil {
		t.Fatalf("seed alldeploy cache: %v", err)
	}

	deps := Deps{
		Cluster:        testCluster,
		App:            "mailon",
		OverviewCache:  cache.New[k8s.ClusterOverview](cache.Options{BaseDir: base, Namespace: "overview"}),
		NamespaceCache: cache.New[k8s.NamespaceView](cache.Options{BaseDir: base, Namespace: "namespace"}),
		PodsCache:      cache.New[[]k8s.Pod](cache.Options{BaseDir: base, Namespace: "pods"}),
		AllDeployCache: adc,
		Fetch:          fetch,
		History:        hist,
	}
	return searchHarness{deps: deps, hist: hist, release: release}
}

func slashMsg() tea.Msg { return runeMsg('/') }

// onOverviewSearchable drives a fresh program to the loaded overview (so the
// index can read its namespaces) and opens the search modal with `/`.
func onOverviewSearchable(t *testing.T, h searchHarness) *teatest.TestModel {
	t.Helper()
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 40))
	waitFor(t, tm, "mailon", "all-namespaces", "updated") // overview loaded → namespaces indexable
	tm.Send(slashMsg())
	waitFor(t, tm, "search", "indexing pods…") // modal open; pods not yet released
	return tm
}

// ── Open / type / results / rank ──────────────────────────────────────────────

// TestSearch_TypeShowsRankedResults opens `/`, releases pods, types "responder"
// and asserts namespace/deployment/pod results all appear, ranked sensibly
// (deployment "responder" — an EXACT match — ranks above the substring-matching
// "responder-aaa" pod).
func TestSearch_TypeShowsRankedResults(t *testing.T) {
	h := newSearchHarness(t)
	tm := onOverviewSearchable(t, h)

	close(h.release) // let pods land → index updates
	waitFor(t, tm, "index updated", "pods")

	for _, r := range "responder" {
		tm.Send(runeMsg(r))
	}
	// Deployment + pod + (no exact-name namespace, but the path shows the ns).
	waitFor(t, tm, "[deploy", "responder", "[pod", "responder-aaa")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.searchModal == nil {
		t.Fatal("search modal closed unexpectedly")
	}
	res := m.searchModal.results
	if len(res) == 0 {
		t.Fatal("no results for 'responder'")
	}
	// Rank: the FIRST result is an exact-name match — the "responder" deployment
	// (a deployment, by kindOrder, before any pod that ties on score).
	if res[0].kind != searchDeployment || res[0].name != "responder" {
		t.Fatalf("top result = %+v, want the exact-match responder deployment first", res[0])
	}
	// A pod result is present and addressable (resolved to its deployment).
	var hasPod bool
	for _, it := range res {
		if it.kind == searchPod && it.pod == "responder-aaa" && it.deployment == "responder" && it.namespace == "mailon" {
			hasPod = true
		}
	}
	if !hasPod {
		t.Fatalf("expected the responder-aaa pod result; results=%+v", res)
	}
	// The orphan pod (no deployment) was excluded from the index.
	for _, it := range res {
		if it.pod == "orphan-pod" {
			t.Fatal("orphan pod (no owning deployment) must not be indexed")
		}
	}
}

// TestSearch_TypesLettersThatAliasNavKeys is a regression guard: the field must
// accept every letter, including "j"/"k" (the views' down/up aliases). If search
// nav reused those aliases, a query like "jaeger"/"knative" would be
// un-typeable. We type "kj" and assert it lands in the field verbatim (and
// matches nothing — proving it typed rather than navigated).
func TestSearch_TypesLettersThatAliasNavKeys(t *testing.T) {
	h := newSearchHarness(t)
	tm := onOverviewSearchable(t, h)

	for _, r := range "kj" { // both are down/up aliases in the list views
		tm.Send(runeMsg(r))
	}
	// A new frame is emitted because the field changed; "kj" matches nothing.
	waitFor(t, tm, "no matches")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if got := m.searchModal.input.Value(); got != "kj" {
		t.Fatalf("input = %q, want \"kj\" (j/k must type into the field, not navigate)", got)
	}
}

// TestSearch_JumpToNamespaceRebuildsStack types a namespace name and Enter jumps
// there, rebuilding the stack to [overview, namespace].
func TestSearch_JumpToNamespaceRebuildsStack(t *testing.T) {
	h := newSearchHarness(t)
	tm := onOverviewSearchable(t, h)

	for _, r := range "mailon-staging" {
		tm.Send(runeMsg(r))
	}
	waitFor(t, tm, "[ns", "mailon-staging")
	tm.Send(enterMsg()) // jump

	// Lands on the namespace view (top-bar context).
	waitFor(t, tm, "mailon-staging · [user]")

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	assertStack(t, m, []levelKind{levelOverview, levelNamespace})
	if got := m.top().namespace; got != "mailon-staging" {
		t.Fatalf("jumped to namespace %q, want mailon-staging", got)
	}
}

// TestSearch_JumpToDeploymentRebuildsStack types a deployment name, Enter jumps
// to its pods view: stack [overview, namespace, deployment].
func TestSearch_JumpToDeploymentRebuildsStack(t *testing.T) {
	h := newSearchHarness(t)
	tm := onOverviewSearchable(t, h)

	for _, r := range "sender" {
		tm.Send(runeMsg(r))
	}
	waitFor(t, tm, "[deploy", "sender")
	tm.Send(enterMsg()) // jump to the sender deployment's pods view

	// Pods view top-bar context is "<ns> · <deploy>".
	waitFor(t, tm, "mailon · sender", "POD")

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	assertStack(t, m, []levelKind{levelOverview, levelNamespace, levelDeployment})
	if top := m.top(); top.namespace != "mailon" || top.deployment != "sender" {
		t.Fatalf("jumped to %s/%s, want mailon/sender", top.namespace, top.deployment)
	}
}

// TestSearch_JumpToPodPreselects types a pod name, Enter jumps to its parent
// deployment's pods view with the pod pre-selected (cursor on it once pods load).
// The pods fetcher returns responderPods() (responder-aaa, responder-bbb), so a
// jump to responder-bbb must land the cursor on row 1.
func TestSearch_JumpToPodPreselects(t *testing.T) {
	h := newSearchHarness(t)
	tm := onOverviewSearchable(t, h)

	close(h.release)
	waitFor(t, tm, "index updated")

	for _, r := range "responder-bbb" {
		tm.Send(runeMsg(r))
	}
	waitFor(t, tm, "[pod", "responder-bbb")
	tm.Send(enterMsg()) // jump to responder pods, pre-select responder-bbb

	waitFor(t, tm, "mailon · responder", "POD", "responder-bbb")

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	assertStack(t, m, []levelKind{levelOverview, levelNamespace, levelDeployment})
	top := m.top()
	if top.deployment != "responder" {
		t.Fatalf("jumped to deployment %q, want responder (the pod's parent)", top.deployment)
	}
	// responderPods(): [responder-aaa, responder-bbb] → cursor must be on row 1.
	if top.cursor != 1 {
		t.Fatalf("pod-jump cursor = %d, want 1 (responder-bbb pre-selected)", top.cursor)
	}
	if top.targetPod != "" {
		t.Fatalf("targetPod should be cleared after pods load, got %q", top.targetPod)
	}
}

// TestSearch_RecordsQueryOnSelect asserts selecting a result records the active
// query string (recents = searches that led somewhere) under the global
// (cluster, "") scope.
func TestSearch_RecordsQueryOnSelect(t *testing.T) {
	h := newSearchHarness(t)
	tm := onOverviewSearchable(t, h)

	for _, r := range "sender" {
		tm.Send(runeMsg(r))
	}
	waitFor(t, tm, "[deploy", "sender")
	tm.Send(enterMsg()) // jump (records "sender")
	waitFor(t, tm, "mailon · sender")

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	got := h.hist.RecentSearches(store.Scope{Cluster: testCluster, App: ""}, 5)
	if !reflect.DeepEqual(got, []string{"sender"}) {
		t.Fatalf("recorded recents = %v, want [sender]", got)
	}
}

// ── Recents (empty input) ─────────────────────────────────────────────────────

// TestSearch_RecentsShowNewestFirstAndRerun seeds search history, opens `/`, and
// asserts the 5 newest distinct queries show (newest first) while the input is
// empty; ↓ then Enter on a focused recent re-runs it (populates the field and
// switches to results).
func TestSearch_RecentsShowNewestFirstAndRerun(t *testing.T) {
	h := newSearchHarness(t)
	scope := store.Scope{Cluster: testCluster, App: ""}
	// Seed 6 distinct (oldest→newest); only the 5 newest show, newest first.
	for _, q := range []string{"oldest", "web", "knowledge", "ingester", "sender", "responder"} {
		if err := h.hist.RecordSearch(scope, q); err != nil {
			t.Fatalf("seed search: %v", err)
		}
	}

	tm := onOverviewSearchable(t, h)
	// The modal opens on the recents list (input empty). onOverviewSearchable
	// already confirmed it is up; the recents-order assertion is on the recents
	// slice itself (final model below) — teatest's frame stream doesn't re-emit a
	// static frame, so we drive input rather than re-waiting on the same frame.

	// ↓ moves onto the 2nd recent ("sender"); Enter re-runs it (a new frame —
	// the results list — which teatest does emit).
	tm.Send(keyMsg(tea.KeyDown))
	tm.Send(enterMsg())
	// Now showing results for "sender" (the deployment), not the recents list.
	waitFor(t, tm, "[deploy", "sender")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.searchModal == nil {
		t.Fatal("modal closed unexpectedly")
	}
	// Recents are newest-first, capped at 5, with "oldest" excluded.
	wantRecents := []string{"responder", "sender", "ingester", "knowledge", "web"}
	if !reflect.DeepEqual(m.searchModal.recents, wantRecents) {
		t.Fatalf("recents = %v, want %v", m.searchModal.recents, wantRecents)
	}
	// Enter on the focused recent populated the field + switched to results.
	if got := m.searchModal.input.Value(); got != "sender" {
		t.Fatalf("after re-run, input = %q, want sender", got)
	}
	if len(m.searchModal.results) == 0 {
		t.Fatal("re-run produced no results")
	}
}

// ── Guards: modal precedence + esc ────────────────────────────────────────────

// TestSearch_DoesNotOpenWhileDeployModalOpen asserts `/` is swallowed by an open
// deploy modal (which owns keys) — the search modal must NOT open underneath it.
func TestSearch_DoesNotOpenWhileDeployModalOpen(t *testing.T) {
	deps, _, _ := deployHarness(t)
	tm := openModalOnMailon(t, deps) // deploy modal open

	tm.Send(slashMsg()) // must be swallowed by the deploy modal

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.searchModal != nil {
		t.Fatal("search must not open while the deploy modal owns keys")
	}
	if m.deployModal == nil {
		t.Fatal("deploy modal should still be open (slash was swallowed, not a cancel)")
	}
}

// TestSearch_DoesNotOpenWhileScaleModalOpen asserts `/` is swallowed by an open
// scale modal too (the same modal-precedence guard).
func TestSearch_DoesNotOpenWhileScaleModalOpen(t *testing.T) {
	deps, _, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)
	tm.Send(runeMsg('s')) // open scale modal
	waitFor(t, tm, "scale — mailon", "select deployments to scale")

	tm.Send(slashMsg()) // swallowed by the scale modal

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.searchModal != nil {
		t.Fatal("search must not open while the scale modal owns keys")
	}
	if m.scaleModal == nil {
		t.Fatal("scale modal should still be open")
	}
}

// TestSearch_EscCloses asserts esc closes the search modal back to the
// underlying view.
func TestSearch_EscCloses(t *testing.T) {
	h := newSearchHarness(t)
	tm := onOverviewSearchable(t, h)

	tm.Send(escMsg())
	waitFor(t, tm, "all-namespaces", "NAMESPACE") // back to the overview

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.searchModal != nil {
		t.Fatal("esc should close the search modal")
	}
}

// TestSearch_FooterHasHint asserts the `/` search hint is surfaced in the footer
// key hints (kept honest with the new binding).
func TestSearch_FooterHasHint(t *testing.T) {
	h := newSearchHarness(t)
	tm := teatest.NewTestModel(t, New(h.deps), teatest.WithInitialTermSize(120, 40))
	waitFor(t, tm, "all-namespaces", "search") // the "/" "search" hint in the footer
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// ── helpers ────────────────────────────────────────────────────────────────────

// assertStack asserts the model's stack kinds match want, top last.
func assertStack(t *testing.T, m Model, want []levelKind) {
	t.Helper()
	if len(m.stack) != len(want) {
		t.Fatalf("stack depth = %d, want %d (%v)", len(m.stack), len(want), stackKinds(m))
	}
	for i, k := range want {
		if m.stack[i].kind != k {
			t.Fatalf("stack[%d].kind = %v, want %v (full=%v)", i, m.stack[i].kind, k, stackKinds(m))
		}
	}
}

func stackKinds(m Model) []levelKind {
	out := make([]levelKind, len(m.stack))
	for i := range m.stack {
		out[i] = m.stack[i].kind
	}
	return out
}
