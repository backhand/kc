package tui

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/backhand/kc/internal/cache"
	"github.com/backhand/kc/internal/exec"
	"github.com/backhand/kc/internal/git"
	"github.com/backhand/kc/internal/github"
	"github.com/backhand/kc/internal/k8s"
	"github.com/backhand/kc/internal/store"
)

// Headless deploy-flow tests (teatest). They drive the modal end-to-end with:
//   - a fake Releases fetcher (annotated fixtures — no `gh`),
//   - a MOCKED exec runner that records the kubectl argv and returns canned
//     rollout output — NOTHING here runs a real `set image`/`rollout` against a
//     cluster (SPEC safety),
//   - a temp-dir learning store so RecordDeploy / DeployPresets are exercised
//     without touching real ~/.kc.

// ── Fixtures specific to the deploy flow ──────────────────────────────────────

// mailonDeployments are two single-container deployments on the mailon GHCR
// image (so DeriveRepo → thinkpilot/mailon and matchContainer → wildcard).
func mailonDeployments() []k8s.Deployment {
	return []k8s.Deployment{
		{Namespace: "mailon", Name: "responder",
			Image:         k8s.ImageRef{Name: "responder", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v0.6.9", Raw: "ghcr.io/thinkpilot/mailon:v0.6.9"},
			Images:        []k8s.ImageRef{{Name: "responder", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v0.6.9", Raw: "ghcr.io/thinkpilot/mailon:v0.6.9"}},
			ReadyReplicas: 2, DesiredReplicas: 2},
		{Namespace: "mailon", Name: "sender",
			Image:         k8s.ImageRef{Name: "sender", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v0.6.9", Raw: "ghcr.io/thinkpilot/mailon:v0.6.9"},
			Images:        []k8s.ImageRef{{Name: "sender", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v0.6.9", Raw: "ghcr.io/thinkpilot/mailon:v0.6.9"}},
			ReadyReplicas: 1, DesiredReplicas: 1},
	}
}

func mailonDeployNamespaceView() k8s.NamespaceView {
	return k8s.NamespaceView{Namespace: "mailon", Kind: k8s.KindUser, Deployments: mailonDeployments()}
}

// fakeReleases is the annotated version list the modal renders (newest first).
func fakeReleases() []github.ReleaseAnnotation {
	return []github.ReleaseAnnotation{
		{Tag: "v0.6.10", Name: "v0.6.10", Latest: true, Build: github.BuildReady, ImageAvailable: github.AvailPresent},
		{Tag: "v0.6.9", Name: "v0.6.9", Build: github.BuildReady, ImageAvailable: github.AvailPresent},
		{Tag: "v0.7.0-rc.1", Name: "v0.7.0-rc.1", Prerelease: true, Build: github.BuildBuilding, ImageAvailable: github.AvailUnknown},
		{Tag: "v0.6.8", Name: "v0.6.8", Build: github.BuildReady, ImageAvailable: github.AvailPresent},
		{Tag: "v0.6.7", Name: "v0.6.7", Build: github.BuildFailed, ImageAvailable: github.AvailAbsent},
		// 6th+ exist so paging back (`o`) has something to show.
		{Tag: "v0.6.6", Name: "v0.6.6", Build: github.BuildReady, ImageAvailable: github.AvailPresent},
		{Tag: "v0.6.5", Name: "v0.6.5", Build: github.BuildReady, ImageAvailable: github.AvailPresent},
	}
}

// recordingRunner captures every kubectl argv (thread-safe — teatest runs Cmds
// on goroutines) and returns canned `rollout status` output.
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) run(_ context.Context, _ string, args []string, _ exec.RunOptions) (exec.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	out := ""
	if contains2(args, "rollout") {
		out = "deployment \"x\" successfully rolled out\n"
	}
	return exec.RunResult{Stdout: out}, nil
}

func (r *recordingRunner) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func contains2(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// deployHarness builds Deps for the deploy flow: a mailon namespace view, the
// fake releases, the recording runner, and a temp-dir store.
func deployHarness(t *testing.T) (Deps, *recordingRunner, *store.ActionHistory) {
	t.Helper()
	base := t.TempDir()
	runner := &recordingRunner{}
	hist := store.New(store.Options{BaseDir: base})

	fetch := defaultFetchers()
	fetch.Namespace = func(_ context.Context, _ string) (k8s.NamespaceView, error) { return mailonDeployNamespaceView(), nil }
	fetch.AllDeployments = func(context.Context) ([]k8s.Deployment, error) { return mailonDeployments(), nil }
	fetch.Releases = func(_ context.Context, _ git.RepoRef, _ string, limit int) []github.ReleaseAnnotation {
		all := fakeReleases()
		if limit > 0 && limit < len(all) {
			return all[:limit]
		}
		return all
	}

	ovc := cache.New[k8s.ClusterOverview](cache.Options{BaseDir: base, Namespace: "overview"})
	deps := Deps{
		Cluster:        testCluster,
		App:            "mailon",
		OverviewCache:  ovc,
		NamespaceCache: cache.New[k8s.NamespaceView](cache.Options{BaseDir: base, Namespace: "namespace"}),
		PodsCache:      cache.New[[]k8s.Pod](cache.Options{BaseDir: base, Namespace: "pods"}),
		AllDeployCache: cache.New[[]k8s.Deployment](cache.Options{BaseDir: base, Namespace: "alldeploy"}),
		Fetch:          fetch,
		Runner:         runner.run,
		History:        hist,
		Entry:          Entry{Resolution: resolution("mailon")}, // land directly on the mailon namespace
	}
	return deps, runner, hist
}

func spaceMsg() tea.Msg     { return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}} }
func escMsg() tea.Msg       { return tea.KeyMsg{Type: tea.KeyEsc} }
func enterMsg() tea.Msg     { return tea.KeyMsg{Type: tea.KeyEnter} }
func backspaceMsg() tea.Msg { return tea.KeyMsg{Type: tea.KeyBackspace} }

// ctrlCMsg hard-quits even while the modal is open (the modal swallows `q`), so
// state-inspection tests can reach FinalModel without first closing the modal.
func ctrlCMsg() tea.Msg { return tea.KeyMsg{Type: tea.KeyCtrlC} }

// openModalOnMailon drives a fresh program to the mailon namespace and opens the
// deploy modal (d), returning the running test model.
func openModalOnMailon(t *testing.T, deps Deps) *teatest.TestModel {
	t.Helper()
	tm := teatest.NewTestModel(t, New(deps), teatest.WithInitialTermSize(120, 40))
	waitFor(t, tm, "responder", "mailon · [user]") // namespace view loaded (top-bar context)
	tm.Send(runeMsg('d'))
	waitFor(t, tm, "deploy — mailon", "select deployments")
	return tm
}

// ── Tests ──────────────────────────────────────────────────────────────────

// TestDeploy_OpensModal asserts `d` opens the modal in a namespace view and esc
// closes it back to the namespace view.
func TestDeploy_OpensModal(t *testing.T) {
	deps, _, _ := deployHarness(t)
	// openModalOnMailon already asserts the modal opened with "select
	// deployments"; both deployment rows are present in that frame.
	tm := openModalOnMailon(t, deps)

	// Esc closes the modal — back to the namespace view (DEPLOYMENT column).
	tm.Send(escMsg())
	waitFor(t, tm, "DEPLOYMENT", "mailon · [user]")

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestDeploy_PresetContainingCurrentPreChecked seeds a learned preset that
// CONTAINS the focused deployment and asserts it is pre-checked when the modal
// opens (SPEC Feature 1: "pre-check the first preset containing the deployment
// you're on"). The cursor lands on responder (row 0) in the mailon namespace, so
// the [responder sender] preset is the one to preselect.
func TestDeploy_PresetContainingCurrentPreChecked(t *testing.T) {
	deps, _, hist := deployHarness(t)
	scope := store.Scope{Cluster: testCluster, App: "mailon"}
	// A [sender]-only preset (does NOT contain responder) recorded first, then the
	// [responder sender] preset. Both rank as distinct presets; only the latter
	// contains the focused deployment (responder), so it is the one preselected —
	// even though it isn't necessarily presets[0].
	if err := hist.RecordDeploy(scope, []string{"sender"}); err != nil {
		t.Fatalf("seed preset: %v", err)
	}
	if err := hist.RecordDeploy(scope, []string{"responder", "sender"}); err != nil {
		t.Fatalf("seed preset: %v", err)
	}

	tm := openModalOnMailon(t, deps)
	tm.Send(ctrlCMsg())
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	m := fm.(Model)
	if m.deployModal == nil {
		t.Fatal("modal closed unexpectedly")
	}
	// The [responder sender] set (the first preset CONTAINING responder) is
	// pre-checked — both rows on.
	if !m.deployModal.sel.checked["responder"] || !m.deployModal.sel.checked["sender"] {
		t.Errorf("checked = %v, want responder+sender pre-checked (the preset containing the focused deployment)", m.deployModal.sel.checked)
	}
	// The cursor starts on the focused deployment (responder, row 0).
	if m.deployModal.sel.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (the focused responder row)", m.deployModal.sel.cursor)
	}
	m.quitting = false // View() blanks while quitting; render the real modal frame
	if !strings.Contains(m.View(), "responder+sender") {
		t.Errorf("modal view missing the preset chip; view=\n%s", m.View())
	}
}

// TestDeploy_NoPresetContainsCurrentFallsBackToCurrent asserts that when NO
// learned preset contains the focused deployment, deploy pre-checks just that
// deployment (SPEC Feature 1 fallback: "{current}"). The cursor lands on
// responder; the only preset is [sender], which doesn't contain it.
func TestDeploy_NoPresetContainsCurrentFallsBackToCurrent(t *testing.T) {
	deps, _, hist := deployHarness(t)
	scope := store.Scope{Cluster: testCluster, App: "mailon"}
	if err := hist.RecordDeploy(scope, []string{"sender"}); err != nil {
		t.Fatalf("seed preset: %v", err)
	}

	tm := openModalOnMailon(t, deps)
	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.deployModal == nil {
		t.Fatal("modal closed unexpectedly")
	}
	// Just the focused deployment (responder) is pre-checked; sender is not.
	if !m.deployModal.sel.checked["responder"] || m.deployModal.sel.checked["sender"] {
		t.Errorf("checked = %v, want only responder (fallback to {current} when no preset contains it)", m.deployModal.sel.checked)
	}
	// The [sender] chip is still offered (it's a learned preset, just not preselected).
	if len(m.deployModal.sel.presets) == 0 || !reflect.DeepEqual(m.deployModal.sel.presets[0], []string{"sender"}) {
		t.Errorf("presets[0] = %v, want [sender]", m.deployModal.sel.presets)
	}
}

// TestDeploy_ToggleAndPresetChip asserts space toggles the focused checkbox and
// a number key toggles a whole preset.
func TestDeploy_ToggleAndPresetChip(t *testing.T) {
	deps, _, hist := deployHarness(t)
	// Two presets: [responder sender] (top) and [sender].
	scope := store.Scope{Cluster: testCluster, App: "mailon"}
	_ = hist.RecordDeploy(scope, []string{"sender"})
	_ = hist.RecordDeploy(scope, []string{"responder", "sender"})

	tm := openModalOnMailon(t, deps)
	// Top preset [responder sender] is pre-checked (both on); chips render
	// "1:responder+sender" and "2:sender" — assert that on the opened frame's
	// final model below.

	// Space toggles the focused row (responder, cursor 0) OFF.
	tm.Send(spaceMsg())
	// Press "2" → toggle the [sender] preset; sender is still checked so it
	// flips OFF, leaving nothing checked.
	tm.Send(runeMsg('2'))

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.deployModal == nil {
		t.Fatal("modal closed unexpectedly")
	}
	// Both chips were available (two distinct presets).
	if len(m.deployModal.sel.presets) != 2 {
		t.Fatalf("presets = %v, want 2 (responder+sender, sender)", m.deployModal.sel.presets)
	}
	if m.deployModal.sel.checked["responder"] {
		t.Error("responder should be unchecked after space toggle")
	}
	if m.deployModal.sel.checked["sender"] {
		t.Error("sender should be unchecked after toggling its preset chip off")
	}
}

// TestDeploy_VersionListAnnotated asserts the version phase renders the annotated
// releases from the fixtures (build status + flags).
func TestDeploy_VersionListAnnotated(t *testing.T) {
	deps, _, _ := deployHarness(t)
	tm := openModalOnMailon(t, deps)

	// No history → the focused deployment (responder) is pre-checked (SPEC
	// Feature 1 fallback to {current}), so enter advances straight to versions.
	tm.Send(enterMsg()) // → version phase

	// The annotated list shows the tags + build/flags from fakeReleases.
	waitFor(t, tm, "v0.6.10", "latest", "v0.7.0-rc.1", "pre-release", "ready", "building")

	tm.Send(ctrlCMsg())
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestDeploy_VersionListPagesOlder asserts `o` pages the version list back to
// older releases (the 6th/7th fixtures appear, with the "older" page hint).
func TestDeploy_VersionListPagesOlder(t *testing.T) {
	deps, _, _ := deployHarness(t)
	tm := openModalOnMailon(t, deps)

	// responder is pre-checked (no-history fallback to {current}).
	tm.Send(enterMsg()) // → versions (page 0: v0.6.10 … v0.6.7)
	waitFor(t, tm, "v0.6.10")
	tm.Send(runeMsg('o')) // page back → older window (v0.6.6, v0.6.5)
	waitFor(t, tm, "v0.6.6", "v0.6.5", "older")

	tm.Send(ctrlCMsg())
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestDeploy_ConfirmShowsFromTo asserts the confirm screen shows the selected
// deployments and the correct from→to tags.
func TestDeploy_ConfirmShowsFromTo(t *testing.T) {
	deps, _, _ := deployHarness(t)
	tm := openModalOnMailon(t, deps)

	// responder is pre-checked (no-history fallback to {current}).
	tm.Send(enterMsg()) // → versions
	waitFor(t, tm, "v0.6.10")
	tm.Send(enterMsg()) // pick v0.6.10 (cursor 0) → confirm

	// Confirm screen: deployment + "v0.6.9 → v0.6.10".
	waitFor(t, tm, "confirm", "responder", "v0.6.9", "v0.6.10", "kubectl set image")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.deployModal == nil || len(m.deployModal.changes) != 1 {
		t.Fatalf("want 1 planned change, modal=%+v", m.deployModal)
	}
	c := m.deployModal.changes[0]
	if c.Deployment != "responder" || c.FromTag != "v0.6.9" || c.ToTag != "v0.6.10" {
		t.Errorf("change = %+v, want responder v0.6.9→v0.6.10", c)
	}
}

// TestDeploy_ConfirmInvokesSetImageWithCorrectArgv is the safety-critical test:
// confirming runs the mutation, and we assert the EXACT kubectl argv via the
// mocked runner — NOT a real cluster — plus that RecordDeploy persisted the set
// and the rollout view renders.
func TestDeploy_ConfirmInvokesSetImageWithCorrectArgv(t *testing.T) {
	deps, runner, hist := deployHarness(t)
	tm := openModalOnMailon(t, deps)

	// responder is pre-checked (no-history fallback to {current}; single-container
	// → "*=").
	tm.Send(enterMsg()) // → versions
	waitFor(t, tm, "v0.6.10")
	tm.Send(enterMsg()) // pick v0.6.10 → confirm
	waitFor(t, tm, "confirm", "v0.6.10")
	tm.Send(enterMsg()) // APPLY (confirm-gated mutation)

	// Rollout view renders and settles.
	waitFor(t, tm, "rollout", "responder", "done")

	tm.Send(escMsg()) // close
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// 1) The mutation was invoked with the correct kubectl argv (mocked exec).
	calls := runner.snapshot()
	var setImage, rollout []string
	for _, c := range calls {
		if contains2(c, "set") && contains2(c, "image") {
			setImage = c
		}
		if contains2(c, "rollout") && contains2(c, "status") {
			rollout = c
		}
	}
	if setImage == nil {
		t.Fatalf("no `set image` call captured; calls=%v", calls)
	}
	wantSet := []string{"-n", "mailon", "set", "image", "deployment/responder", "*=ghcr.io/thinkpilot/mailon:v0.6.10"}
	if !reflect.DeepEqual(setImage, wantSet) {
		t.Errorf("set image argv =\n  %v\nwant\n  %v", setImage, wantSet)
	}
	// Crucially: NOT a dry-run (the shipped apply is real). The reviewer
	// dry-run-checks separately; here the mocked runner guarantees no cluster hit.
	if contains2(setImage, "--dry-run=server") {
		t.Error("the confirmed apply must NOT be a dry-run")
	}
	if rollout == nil {
		t.Fatalf("no `rollout status` call captured; calls=%v", calls)
	}
	wantRollout := []string{"-n", "mailon", "rollout", "status", "deployment/responder", "--timeout=5m0s"}
	if !reflect.DeepEqual(rollout, wantRollout) {
		t.Errorf("rollout argv =\n  %v\nwant\n  %v", rollout, wantRollout)
	}

	// 2) RecordDeploy persisted the deployed set as a preset.
	presets := hist.DeployPresets(store.Scope{Cluster: testCluster, App: "mailon"})
	if len(presets) != 1 || !reflect.DeepEqual(presets[0], []string{"responder"}) {
		t.Errorf("DeployPresets = %v, want [[responder]] after RecordDeploy", presets)
	}
}

// TestDeploy_MultiSelectArgvBoth asserts that selecting two deployments fires a
// set-image for each, with the wildcard form (single-container fixtures).
func TestDeploy_MultiSelectArgvBoth(t *testing.T) {
	deps, runner, _ := deployHarness(t)
	tm := openModalOnMailon(t, deps)

	// responder is pre-checked (cursor 0, no-history fallback to {current}); add
	// sender so BOTH are selected.
	tm.Send(runeMsg('j')) // → sender
	tm.Send(spaceMsg())   // check sender
	tm.Send(enterMsg())   // → versions
	waitFor(t, tm, "v0.6.10")
	tm.Send(enterMsg()) // → confirm
	waitFor(t, tm, "responder", "sender", "v0.6.10")
	tm.Send(enterMsg()) // APPLY → rollout phase fires one apply+watch per deployment

	// Poll the (thread-safe) runner until both deployments have been set-imaged.
	got := waitForSetImageTargets(t, runner, "deployment/responder", "deployment/sender")
	if !got["deployment/responder"] || !got["deployment/sender"] {
		t.Errorf("set-image targets = %v, want both deployment/responder and deployment/sender", got)
	}

	tm.Send(ctrlCMsg())
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// waitForSetImageTargets polls the recording runner until it has seen a
// `set image` for each wanted deployment target (or fails after a short wait).
func waitForSetImageTargets(t *testing.T, runner *recordingRunner, want ...string) map[string]bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := map[string]bool{}
		for _, c := range runner.snapshot() {
			if contains2(c, "set") && contains2(c, "image") {
				for _, a := range c {
					if strings.HasPrefix(a, "deployment/") {
						got[a] = true
					}
				}
			}
		}
		all := true
		for _, w := range want {
			if !got[w] {
				all = false
			}
		}
		if all || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDeploy_EscFromVersionsGoesBack asserts esc steps back through phases
// rather than closing outright (versions → select), so the flow is reversible.
func TestDeploy_EscFromVersionsGoesBack(t *testing.T) {
	deps, _, _ := deployHarness(t)
	tm := openModalOnMailon(t, deps)

	// responder is pre-checked (no-history fallback to {current}).
	tm.Send(enterMsg()) // → versions
	waitFor(t, tm, "pick a version")
	tm.Send(escMsg()) // back to select
	waitFor(t, tm, "select deployments")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.deployModal == nil || m.deployModal.phase != phaseSelect {
		t.Fatalf("expected to be back on the select phase, modal=%+v", m.deployModal)
	}
}

// TestDeploy_NoMutationBeforeConfirm asserts that NOTHING is sent to kubectl
// until the final confirm — opening the modal and browsing versions must not
// invoke the runner (defence-in-depth for the "predict, then confirm" gate).
func TestDeploy_NoMutationBeforeConfirm(t *testing.T) {
	deps, runner, _ := deployHarness(t)
	tm := openModalOnMailon(t, deps)
	// responder is pre-checked (no-history fallback to {current}).
	tm.Send(enterMsg()) // → versions
	waitFor(t, tm, "v0.6.10")
	tm.Send(enterMsg()) // → confirm (still NO apply yet)
	waitFor(t, tm, "confirm")

	// Browsing this far must not have run kubectl at all.
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("kubectl invoked before confirm: %v", calls)
	}

	tm.Send(ctrlCMsg())
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}
