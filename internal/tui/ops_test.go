package tui

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/cache"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/store"
)

// Headless tests for the contextual operations (SPEC "Operations"):
//   - `r` restart: a confirm-gated MUTATION → asserts the EXACT `kubectl rollout
//     restart` argv via a MOCKED runner (no cluster), then the rollout view.
//   - `L` logs / `s` shell: read-only/interactive ops handed to the terminal via
//     tea.ExecProcess — asserts the constructed *exec.Cmd (path+args) via an
//     ExecHook WITHOUT spawning kubectl (never opens a real shell / streams -f).
//   - targeting: a pod in the pods view, the deployment in the namespace view.
//
// SAFETY: nothing here runs a real `rollout restart`, opens a shell, or streams
// logs — the runner is mocked and exec is hook-captured.

// capturedExec records the *exec.Cmd the ExecHook intercepts (thread-safe —
// teatest runs Cmds on goroutines).
type capturedExec struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
}

func (c *capturedExec) hook(cmd *exec.Cmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cmds = append(c.cmds, cmd)
}

func (c *capturedExec) last() *exec.Cmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cmds) == 0 {
		return nil
	}
	return c.cmds[len(c.cmds)-1]
}

// opsHarness builds Deps for the ops flow: the mailon namespace view (with
// responder pods for the pods view), the recording runner for the restart
// mutation, an ExecHook to capture logs/shell commands, and a temp-dir store.
func opsHarness(t *testing.T) (Deps, *recordingRunner, *capturedExec, *store.ActionHistory) {
	t.Helper()
	base := t.TempDir()
	runner := &recordingRunner{}
	captured := &capturedExec{}
	hist := store.New(store.Options{BaseDir: base})

	fetch := defaultFetchers()
	fetch.Namespace = func(_ context.Context, _ string) (k8s.NamespaceView, error) { return mailonDeployNamespaceView(), nil }
	fetch.AllDeployments = func(context.Context) ([]k8s.Deployment, error) { return mailonDeployments(), nil }
	fetch.DeploymentPods = func(_ context.Context, _, _ string) ([]k8s.Pod, error) { return responderPods(), nil }

	deps := Deps{
		Cluster:        testCluster,
		App:            "mailon",
		KubeOpts:       k8s.Options{Context: "k3s"}, // assert the context is threaded into argv
		OverviewCache:  cache.New[k8s.ClusterOverview](cache.Options{BaseDir: base, Namespace: "overview"}),
		NamespaceCache: cache.New[k8s.NamespaceView](cache.Options{BaseDir: base, Namespace: "namespace"}),
		PodsCache:      cache.New[[]k8s.Pod](cache.Options{BaseDir: base, Namespace: "pods"}),
		AllDeployCache: cache.New[[]k8s.Deployment](cache.Options{BaseDir: base, Namespace: "alldeploy"}),
		Fetch:          fetch,
		Runner:         runner.run,
		ExecHook:       captured.hook,
		History:        hist,
		Entry:          Entry{Resolution: resolution("mailon")}, // land on the mailon namespace
	}
	return deps, runner, captured, hist
}

// onMailonNamespace drives a fresh program to the mailon namespace view.
func onMailonNamespace(t *testing.T, deps Deps) *teatest.TestModel {
	t.Helper()
	tm := teatest.NewTestModel(t, New(deps), teatest.WithInitialTermSize(120, 40))
	waitFor(t, tm, "responder", "mailon · [user]", "DEPLOYMENT")
	return tm
}

// onResponderPods drives to the mailon namespace then drills into the responder
// deployment's pods view (responder is the first row, cursor 0).
func onResponderPods(t *testing.T, deps Deps) *teatest.TestModel {
	t.Helper()
	tm := onMailonNamespace(t, deps)
	tm.Send(enterMsg()) // drill into responder → pods view
	waitFor(t, tm, "responder-aaa", "POD")
	return tm
}

// ── Restart (confirm-gated mutation, set-based) ───────────────────────────────
//
// Restart now mirrors deploy's multi-select flow minus the version step:
// select a SET → confirm → per-deployment rollout. These tests drive that flow
// and assert `kubectl rollout restart` fires for EACH selected deployment via the
// MOCKED runner — NEVER a real cluster.

// TestRestart_NamespaceViewSelectSetConfirmAndArgv is the safety-critical restart
// test: `r` in the namespace view opens the SELECT phase with the focused
// deployment's set preselected; we add the second deployment, confirm, and assert
// the EXACT `kubectl rollout restart` (+ `rollout status` watch) argv fires for
// EACH selected deployment via the mocked runner — NOT a real cluster.
func TestRestart_NamespaceViewSelectSetConfirmAndArgv(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('r')) // open restart SELECT (responder preselected — focused, no history)
	waitFor(t, tm, "restart — mailon", "select deployments to restart", "responder", "sender")

	// Nothing has touched kubectl yet (the select phase is pre-mutation).
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("kubectl invoked in the restart select phase: %v", calls)
	}

	// Add sender to the set (responder is already preselected), then confirm.
	tm.Send(runeMsg('j')) // → sender row
	tm.Send(spaceMsg())   // check sender (now {responder, sender})
	tm.Send(enterMsg())   // → confirm
	waitFor(t, tm, "confirm", "deployment/responder", "deployment/sender", "kubectl rollout restart")

	// Still nothing run (confirm-gated).
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("kubectl invoked before restart confirm: %v", calls)
	}

	tm.Send(enterMsg()) // RESTART (confirm-gated mutation) → per-deployment rollout
	waitFor(t, tm, "rollout", "responder", "sender", "done")

	tm.Send(escMsg()) // close
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// `rollout restart` + `rollout status` fired for EACH selected deployment.
	calls := runner.snapshot()
	restart := map[string][]string{}
	status := map[string][]string{}
	for _, c := range calls {
		target := restartTarget(c)
		if target == "" {
			continue
		}
		if contains2(c, "rollout") && contains2(c, "restart") {
			restart[target] = c
		}
		if contains2(c, "rollout") && contains2(c, "status") {
			status[target] = c
		}
	}
	for _, dep := range []string{"responder", "sender"} {
		wantRestart := []string{"--context", "k3s", "-n", "mailon", "rollout", "restart", "deployment/" + dep}
		if !reflect.DeepEqual(restart["deployment/"+dep], wantRestart) {
			t.Errorf("rollout restart argv for %s =\n  %v\nwant\n  %v", dep, restart["deployment/"+dep], wantRestart)
		}
		// Crucially NOT a dry-run (the confirmed restart is real; the mocked runner
		// guarantees no cluster hit — the reviewer dry-run-checks separately).
		if contains2(restart["deployment/"+dep], "--dry-run=server") {
			t.Errorf("the confirmed restart for %s must NOT be a dry-run", dep)
		}
		wantStatus := []string{"--context", "k3s", "-n", "mailon", "rollout", "status", "deployment/" + dep, "--timeout=5m0s"}
		if !reflect.DeepEqual(status["deployment/"+dep], wantStatus) {
			t.Errorf("rollout status argv for %s =\n  %v\nwant\n  %v", dep, status["deployment/"+dep], wantStatus)
		}
	}
}

// TestRestart_PodsViewTargetsParentDeploymentSet asserts that `r` in the pods
// view targets the pod's PARENT deployment's set (not the pod), with the correct
// argv. The pods-view context uses the parent namespace's deployment list, and
// the focused deployment (responder, the pods' parent) is preselected.
func TestRestart_PodsViewTargetsParentDeploymentSet(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onResponderPods(t, deps) // pods view, cursor on responder-aaa (parent = responder)

	tm.Send(runeMsg('r')) // restart → SELECT with responder (the parent) preselected
	// The select screen makes the pod → parent-deployment focus explicit.
	waitFor(t, tm, "restart — mailon", "select deployments to restart", "responder-aaa", "deployment/responder")
	tm.Send(enterMsg()) // → confirm (just {responder})
	waitFor(t, tm, "confirm", "deployment/responder")
	tm.Send(enterMsg()) // RESTART
	waitFor(t, tm, "rollout", "done")

	tm.Send(escMsg())
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	var restart []string
	for _, c := range runner.snapshot() {
		if contains2(c, "rollout") && contains2(c, "restart") {
			restart = c
		}
	}
	want := []string{"--context", "k3s", "-n", "mailon", "rollout", "restart", "deployment/responder"}
	if !reflect.DeepEqual(restart, want) {
		t.Errorf("pods-view restart argv =\n  %v\nwant\n  %v (must target the PARENT deployment)", restart, want)
	}
}

// TestRestart_EscOnSelectCancelsBeforeMutation asserts esc on the SELECT phase
// closes the modal WITHOUT running anything (the confirm-gate guarantee).
func TestRestart_EscOnSelectCancelsBeforeMutation(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('r')) // open restart SELECT
	waitFor(t, tm, "select deployments to restart", "responder")
	tm.Send(escMsg()) // cancel from select → back to the namespace view
	waitFor(t, tm, "DEPLOYMENT", "mailon · [user]")

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.restartModal != nil {
		t.Error("restart modal should be closed after esc on select")
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("esc must not run kubectl; calls=%v", calls)
	}
}

// TestRestart_EscOnConfirmCancelsBeforeMutation asserts esc on the CONFIRM phase
// steps back to select (reversible) and that confirming was never reached, so
// NOTHING ran (defence-in-depth for the confirm gate).
func TestRestart_EscOnConfirmCancelsBeforeMutation(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('r')) // open restart SELECT (responder preselected)
	waitFor(t, tm, "select deployments to restart")
	tm.Send(enterMsg()) // → confirm
	waitFor(t, tm, "confirm", "deployment/responder")
	tm.Send(escMsg()) // back to select (still nothing run)
	waitFor(t, tm, "select deployments to restart")

	// kubectl must not have been touched on the way to confirm and back.
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("esc on confirm must not run kubectl; calls=%v", calls)
	}

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.restartModal == nil || m.restartModal.phase != restartSelect {
		t.Fatalf("expected to be back on the restart select phase, modal=%+v", m.restartModal)
	}
}

// restartTarget returns the "deployment/<name>" argument of a kubectl call, or ""
// when the call has none.
func restartTarget(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "deployment/") {
			return a
		}
	}
	return ""
}

// ── Scale (confirm-gated mutation, set-based) ──────────────────────────────────
//
// Scale mirrors restart's set-select flow but swaps the confirm-only step for a
// replica-count step (which doubles as the confirm). These tests drive that flow
// and assert `kubectl scale … --replicas=N` fires for EACH selected deployment via
// the MOCKED runner — NEVER a real cluster — including a replicas=0 case, and that
// NOTHING runs before the apply (the confirm-gate).

// TestScale_NamespaceViewSelectSetReplicasAndArgv is the safety-critical scale
// test: `s` in the namespace view opens the SELECT phase (focused set
// preselected); we add the second deployment, advance to the replicas screen,
// type a count, Enter, and assert the EXACT `kubectl scale … --replicas=N` (+
// `rollout status` watch) argv fires for EACH selected deployment via the mocked
// runner — and that ZERO kubectl ran before the apply.
func TestScale_NamespaceViewSelectSetReplicasAndArgv(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s')) // open scale SELECT (responder preselected — focused, no history)
	waitFor(t, tm, "scale — mailon", "select deployments to scale", "responder", "sender")

	// Nothing has touched kubectl yet (the select phase is pre-mutation).
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("kubectl invoked in the scale select phase: %v", calls)
	}

	// Add sender to the set (responder is already preselected), then advance.
	tm.Send(runeMsg('j')) // → sender row
	tm.Send(spaceMsg())   // check sender (now {responder, sender})
	tm.Send(enterMsg())   // → replicas screen (the confirm step)
	waitFor(t, tm, "set the target replica count", "deployment/responder", "deployment/sender", "kubectl scale")

	// Still nothing run (confirm-gated): browsing to the replica screen is pre-apply.
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("kubectl invoked before the scale apply: %v", calls)
	}

	// Type the target count: clear the default (digit handling resets a leading 0;
	// here the default is "2", so just append → "23"? No — backspace then type 3).
	tm.Send(backspaceMsg()) // delete the default digit
	tm.Send(runeMsg('3'))   // → "3"
	waitFor(t, tm, "replicas: 3")

	tm.Send(enterMsg()) // SCALE (confirm-gated mutation) → per-deployment rollout
	waitFor(t, tm, "rollout", "responder", "sender", "done")

	tm.Send(escMsg()) // close
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// `scale --replicas=3` + `rollout status` fired for EACH selected deployment.
	calls := runner.snapshot()
	scale := map[string][]string{}
	status := map[string][]string{}
	for _, c := range calls {
		target := restartTarget(c)
		if target == "" {
			continue
		}
		if contains2(c, "scale") {
			scale[target] = c
		}
		if contains2(c, "rollout") && contains2(c, "status") {
			status[target] = c
		}
	}
	for _, dep := range []string{"responder", "sender"} {
		wantScale := []string{"--context", "k3s", "-n", "mailon", "scale", "deployment/" + dep, "--replicas=3"}
		if !reflect.DeepEqual(scale["deployment/"+dep], wantScale) {
			t.Errorf("scale argv for %s =\n  %v\nwant\n  %v", dep, scale["deployment/"+dep], wantScale)
		}
		wantStatus := []string{"--context", "k3s", "-n", "mailon", "rollout", "status", "deployment/" + dep, "--timeout=5m0s"}
		if !reflect.DeepEqual(status["deployment/"+dep], wantStatus) {
			t.Errorf("rollout status argv for %s =\n  %v\nwant\n  %v", dep, status["deployment/"+dep], wantStatus)
		}
	}
}

// TestScale_ArrowSwitchesPreset asserts ←/→ in the scale select phase cycle the
// active preset end-to-end (key → shared handleSelectKey → render): → sets the
// selection to exactly the next preset, the active chip renders with the "›"
// marker, and the wiring is shared so deploy/restart get it too (selection-level
// unit tests cover the cycling/clamp matrix; this proves the modal wiring).
func TestScale_ArrowSwitchesPreset(t *testing.T) {
	deps, _, _, hist := opsHarness(t)
	// Two presets: [responder sender] (top, contains the focused responder → it's
	// the one pre-checked & active) and [sender].
	scope := store.Scope{Cluster: testCluster, App: "mailon"}
	_ = hist.RecordDeploy(scope, []string{"sender"})
	_ = hist.RecordDeploy(scope, []string{"responder", "sender"})

	tm := onMailonNamespace(t, deps)
	tm.Send(runeMsg('s')) // open scale SELECT
	waitFor(t, tm, "select deployments to scale", "responder+sender", "←/→ preset")

	// → cycles the active preset to the NEXT one ([sender]); the active chip shows
	// the "›2:sender" marker once it lands there.
	tm.Send(keyMsg(tea.KeyRight))
	waitFor(t, tm, "›2:sender")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.scaleModal == nil {
		t.Fatal("scale modal closed unexpectedly")
	}
	// The selection is now EXACTLY the [sender] preset (index 1) — responder off.
	if m.scaleModal.sel.activePreset != 1 {
		t.Errorf("activePreset = %d, want 1 (the [sender] preset after →)", m.scaleModal.sel.activePreset)
	}
	if m.scaleModal.sel.checked["responder"] || !m.scaleModal.sel.checked["sender"] {
		t.Errorf("checked = %v, want exactly sender (→ set the selection to the [sender] preset)", m.scaleModal.sel.checked)
	}
}

// TestScale_ToZero asserts the explicit pause path: scale a single deployment to
// 0 replicas. The replica field accepts "0" and the apply fires
// `kubectl scale … --replicas=0` (the SPEC's pause-then-resume goal, zero end).
func TestScale_ToZero(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s')) // open scale SELECT (responder preselected)
	waitFor(t, tm, "select deployments to scale", "responder")
	tm.Send(enterMsg()) // → replicas screen (just {responder})
	waitFor(t, tm, "set the target replica count", "deployment/responder")

	// Default is responder's DesiredReplicas (2); backspace then type 0 → "0".
	tm.Send(backspaceMsg())
	tm.Send(runeMsg('0'))
	waitFor(t, tm, "replicas: 0", "scale to zero")

	tm.Send(enterMsg()) // SCALE TO ZERO
	waitFor(t, tm, "rollout", "responder", "done")

	tm.Send(escMsg())
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	var scale []string
	for _, c := range runner.snapshot() {
		if contains2(c, "scale") {
			scale = c
		}
	}
	want := []string{"--context", "k3s", "-n", "mailon", "scale", "deployment/responder", "--replicas=0"}
	if !reflect.DeepEqual(scale, want) {
		t.Errorf("scale-to-zero argv =\n  %v\nwant\n  %v", scale, want)
	}
}

// TestScale_DefaultsToCurrentDesiredReplicas asserts the replica field opens
// pre-filled at the focused deployment's current DesiredReplicas (responder = 2),
// a sensible starting point the user adjusts ("predict, then confirm").
func TestScale_DefaultsToCurrentDesiredReplicas(t *testing.T) {
	deps, _, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s')) // open scale SELECT
	waitFor(t, tm, "select deployments to scale")
	tm.Send(enterMsg()) // → replicas screen
	// responder's DesiredReplicas is 2 (mailonDeployments fixture).
	waitFor(t, tm, "replicas: 2")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.scaleModal == nil || m.scaleModal.replicas != "2" {
		t.Fatalf("replica field = %q, want \"2\" (responder's current DesiredReplicas)", m.scaleModal.replicas)
	}
}

// TestScale_EscOnReplicasBacksToSelect asserts esc on the replicas screen steps
// back to select (reversible) and that NOTHING ran (defence-in-depth for the
// confirm gate: the mutation only fires on Enter at the replicas screen).
func TestScale_EscOnReplicasBacksToSelect(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s')) // open scale SELECT
	waitFor(t, tm, "select deployments to scale")
	tm.Send(enterMsg()) // → replicas
	waitFor(t, tm, "set the target replica count")
	tm.Send(escMsg()) // back to select (still nothing run)
	waitFor(t, tm, "select deployments to scale")

	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("esc on the replicas screen must not run kubectl; calls=%v", calls)
	}

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.scaleModal == nil || m.scaleModal.phase != scaleSelect {
		t.Fatalf("expected to be back on the scale select phase, modal=%+v", m.scaleModal)
	}
}

// TestScale_EscOnSelectCancelsBeforeMutation asserts esc on the SELECT phase
// closes the modal WITHOUT running anything (the confirm-gate guarantee).
func TestScale_EscOnSelectCancelsBeforeMutation(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s')) // open scale SELECT
	waitFor(t, tm, "select deployments to scale", "responder")
	tm.Send(escMsg()) // cancel from select → back to the namespace view
	waitFor(t, tm, "DEPLOYMENT", "mailon · [user]")

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.scaleModal != nil {
		t.Error("scale modal should be closed after esc on select")
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("esc must not run kubectl; calls=%v", calls)
	}
}

// TestScale_RecordsSetIntoLearningStore asserts the scaled set is recorded under
// the "scale" action ({deployments: [...]}) so a scale history accrues (SPEC).
func TestScale_RecordsSetIntoLearningStore(t *testing.T) {
	deps, _, _, hist := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s')) // open scale SELECT (responder preselected)
	waitFor(t, tm, "select deployments to scale")
	tm.Send(enterMsg()) // → replicas
	waitFor(t, tm, "set the target replica count")
	tm.Send(enterMsg()) // SCALE (default replicas) → fires the mutation + records
	waitFor(t, tm, "rollout", "done")

	tm.Send(escMsg())
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// The scale action recorded the set under the "scale" key ({deployments:[...]}),
	// in the same shape deploy/restart use — so a scale history accrues (SPEC).
	scope := store.Scope{Cluster: testCluster, App: "mailon"}
	ranked := hist.Ranked("scale", scope)
	if len(ranked) == 0 {
		t.Fatalf("expected the scaled set to be recorded under the scale action; got none")
	}
	raw, ok := ranked[0]["deployments"].([]any)
	if !ok || len(raw) != 1 || raw[0] != "responder" {
		t.Errorf("recorded scale set = %v, want [responder]", ranked[0]["deployments"])
	}
}

// TestScale_SOpensScaleNotShell is the keymap regression test: `s` opens the
// scale modal (NOT a shell). The shell op moved to `e`; `s` must NEVER build an
// exec command.
func TestScale_SOpensScaleNotShell(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s')) // s → scale modal (previously shell)
	waitFor(t, tm, "scale — mailon", "select deployments to scale")

	tm.Send(ctrlCMsg())
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.scaleModal == nil {
		t.Error("s should open the scale modal")
	}
	if c := captured.last(); c != nil {
		t.Errorf("s must NOT build a shell/exec command; got %v", c.Args)
	}
}

// ── Logs / shell (interactive, ExecProcess) ───────────────────────────────────

// TestLogs_NamespaceViewBuildsDeploymentCmd asserts lowercase `l` in the
// namespace view builds the correct `kubectl logs deployment/<d> …` *exec.Cmd,
// captured via the ExecHook (NOT spawned — no live stream).
func TestLogs_NamespaceViewBuildsDeploymentCmd(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('l')) // logs for the selected deployment (responder)
	cmd := waitForExec(t, captured)

	assertKubectl(t, cmd, []string{
		"--context", "k3s", "-n", "mailon", "logs", "deployment/responder",
		"--all-containers", "--tail=200", "-f",
	})

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestLogs_PodsViewBuildsPodCmd asserts the capital-`L` alias still works (in the
// pods view) and targets the EXACT selected pod (`pod/<name>`).
func TestLogs_PodsViewBuildsPodCmd(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onResponderPods(t, deps) // cursor on responder-aaa

	tm.Send(runeMsg('L'))
	cmd := waitForExec(t, captured)

	assertKubectl(t, cmd, []string{
		"--context", "k3s", "-n", "mailon", "logs", "pod/responder-aaa",
		"--all-containers", "--tail=200", "-f",
	})

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestLogs_LowercaseLOpensLogsNotDrillIn is the regression test for the keymap
// fix: lowercase `l` opens logs for the selected deployment — it must NOT drill
// into the pods view. `l` used to be bound to drill-in, which silently swallowed
// the logs op (the reported "[L]ogs doesn't work": pressing l navigated to the
// pods view instead of streaming logs).
func TestLogs_LowercaseLOpensLogsNotDrillIn(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('l')) // lowercase l → logs (previously drill-in)
	cmd := waitForExec(t, captured)
	assertKubectl(t, cmd, []string{
		"--context", "k3s", "-n", "mailon", "logs", "deployment/responder",
		"--all-containers", "--tail=200", "-f",
	})

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if got := m.top().kind; got != levelNamespace {
		t.Errorf("lowercase l navigated (level=%v); it must open logs, not drill in", got)
	}
}

// TestShell_NamespaceViewBuildsDeploymentExec asserts `e` (exec — formerly `s`)
// in the namespace view builds `kubectl exec -it deployment/<d> -- sh` (kubectl
// picks a pod of the deployment), captured WITHOUT opening a real shell.
func TestShell_NamespaceViewBuildsDeploymentExec(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('e')) // exec (the key formerly bound to `s`)
	cmd := waitForExec(t, captured)

	assertKubectl(t, cmd, []string{
		"--context", "k3s", "-n", "mailon", "exec", "-it", "deployment/responder", "--", "sh",
	})

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestShell_PodsViewExecsSelectedPod asserts `e` (exec) in the pods view execs
// into the EXACT selected pod (move to the 2nd pod first to prove cursor→target
// wiring).
func TestShell_PodsViewExecsSelectedPod(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onResponderPods(t, deps)

	tm.Send(runeMsg('j')) // move to the 2nd pod (responder-bbb)
	waitFor(t, tm, "responder-bbb")
	tm.Send(runeMsg('e')) // exec
	cmd := waitForExec(t, captured)

	assertKubectl(t, cmd, []string{
		"--context", "k3s", "-n", "mailon", "exec", "-it", "pod/responder-bbb", "--", "sh",
	})

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestOps_NoTargetInOverviewIsNoOp asserts the deployment/pod ops (restart, logs,
// scale, exec) do nothing in the overview (no workload selected) — no modal, no
// captured exec, no kubectl.
func TestOps_NoTargetInOverviewIsNoOp(t *testing.T) {
	deps, runner, captured, _ := opsHarness(t)
	deps.Entry = Entry{} // plain all-namespaces entry (no resolution)
	tm := teatest.NewTestModel(t, New(deps), teatest.WithInitialTermSize(120, 40))
	waitFor(t, tm, "all-namespaces", "NAMESPACE")

	tm.Send(runeMsg('r')) // restart
	tm.Send(runeMsg('L')) // logs
	tm.Send(runeMsg('s')) // scale
	tm.Send(runeMsg('e')) // exec

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.restartModal != nil {
		t.Error("restart modal should not open in the overview (no workload selected)")
	}
	if m.scaleModal != nil {
		t.Error("scale modal should not open in the overview (no workload selected)")
	}
	if c := captured.last(); c != nil {
		t.Errorf("no exec command should be built in the overview; got %v", c.Args)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Errorf("no kubectl should run in the overview; calls=%v", calls)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// waitForExec polls until the ExecHook has captured a command (the op handler
// ran), or fails after a short wait.
func waitForExec(t *testing.T, captured *capturedExec) *exec.Cmd {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c := captured.last(); c != nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the op to build an exec command")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertKubectl asserts the captured command runs `kubectl` with exactly the
// given args (Args[0] is the resolved program path, Args[1:] are the kubectl
// args). It NEVER runs the command.
func assertKubectl(t *testing.T, cmd *exec.Cmd, wantArgs []string) {
	t.Helper()
	// cmd.Path is the resolved kubectl path; the invoked program name is Args[0].
	if !strings.HasSuffix(cmd.Args[0], "kubectl") {
		t.Errorf("program = %q, want kubectl", cmd.Args[0])
	}
	got := cmd.Args[1:]
	if !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("kubectl argv =\n  %v\nwant\n  %v", got, wantArgs)
	}
}
