package tui

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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

// ── Restart (confirm-gated mutation) ──────────────────────────────────────────

// TestRestart_NamespaceViewConfirmAndArgv is the safety-critical restart test:
// `r` in the namespace view opens the confirm, enter fires the mutation, and we
// assert the EXACT `kubectl rollout restart` argv via the mocked runner — NOT a
// real cluster.
func TestRestart_NamespaceViewConfirmAndArgv(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('r')) // open restart confirm for the selected deployment (responder)
	waitFor(t, tm, "restart — mailon", "will restart", "deployment/responder", "kubectl rollout restart")

	// Nothing has touched kubectl yet (confirm-gated).
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("kubectl invoked before restart confirm: %v", calls)
	}

	tm.Send(enterMsg()) // RESTART (confirm-gated mutation)
	waitFor(t, tm, "rollout", "responder", "done")

	tm.Send(escMsg()) // close
	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// The restart mutation ran with the correct argv, then the rollout watch.
	calls := runner.snapshot()
	var restart, status []string
	for _, c := range calls {
		if contains2(c, "rollout") && contains2(c, "restart") {
			restart = c
		}
		if contains2(c, "rollout") && contains2(c, "status") {
			status = c
		}
	}
	if restart == nil {
		t.Fatalf("no `rollout restart` call captured; calls=%v", calls)
	}
	wantRestart := []string{"--context", "k3s", "-n", "mailon", "rollout", "restart", "deployment/responder"}
	if !reflect.DeepEqual(restart, wantRestart) {
		t.Errorf("rollout restart argv =\n  %v\nwant\n  %v", restart, wantRestart)
	}
	// Crucially NOT a dry-run (the confirmed restart is real; the mocked runner
	// guarantees no cluster hit — the reviewer dry-run-checks separately).
	if contains2(restart, "--dry-run=server") {
		t.Error("the confirmed restart must NOT be a dry-run")
	}
	if status == nil {
		t.Fatalf("no `rollout status` watch captured; calls=%v", calls)
	}
	wantStatus := []string{"--context", "k3s", "-n", "mailon", "rollout", "status", "deployment/responder", "--timeout=5m0s"}
	if !reflect.DeepEqual(status, wantStatus) {
		t.Errorf("rollout status argv =\n  %v\nwant\n  %v", status, wantStatus)
	}
}

// TestRestart_PodsViewTargetsParentDeployment asserts that `r` in the pods view
// restarts the pod's PARENT deployment (not the pod), with the correct argv.
func TestRestart_PodsViewTargetsParentDeployment(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onResponderPods(t, deps) // pods view, cursor on responder-aaa

	tm.Send(runeMsg('r')) // restart → parent deployment "responder"
	// Confirm prompt makes the pod → parent-deployment targeting explicit.
	waitFor(t, tm, "restart — mailon", "responder-aaa", "deployment/responder")
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

// TestRestart_EscCancelsBeforeMutation asserts esc on the confirm closes the
// modal WITHOUT running anything (defence-in-depth for the confirm gate).
func TestRestart_EscCancelsBeforeMutation(t *testing.T) {
	deps, runner, _, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('r')) // open restart confirm
	waitFor(t, tm, "will restart", "deployment/responder")
	tm.Send(escMsg()) // cancel → back to the namespace view
	waitFor(t, tm, "DEPLOYMENT", "mailon · [user]")

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.restartModal != nil {
		t.Error("restart modal should be closed after esc")
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("esc must not run kubectl; calls=%v", calls)
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

// TestShell_NamespaceViewBuildsDeploymentExec asserts `s` in the namespace view
// builds `kubectl exec -it deployment/<d> -- sh` (kubectl picks a pod of the
// deployment), captured WITHOUT opening a real shell.
func TestShell_NamespaceViewBuildsDeploymentExec(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onMailonNamespace(t, deps)

	tm.Send(runeMsg('s'))
	cmd := waitForExec(t, captured)

	assertKubectl(t, cmd, []string{
		"--context", "k3s", "-n", "mailon", "exec", "-it", "deployment/responder", "--", "sh",
	})

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestShell_PodsViewExecsSelectedPod asserts `s` in the pods view execs into the
// EXACT selected pod (move to the 2nd pod first to prove cursor→target wiring).
func TestShell_PodsViewExecsSelectedPod(t *testing.T) {
	deps, _, captured, _ := opsHarness(t)
	tm := onResponderPods(t, deps)

	tm.Send(runeMsg('j')) // move to the 2nd pod (responder-bbb)
	waitFor(t, tm, "responder-bbb")
	tm.Send(runeMsg('s'))
	cmd := waitForExec(t, captured)

	assertKubectl(t, cmd, []string{
		"--context", "k3s", "-n", "mailon", "exec", "-it", "pod/responder-bbb", "--", "sh",
	})

	tm.Send(runeMsg('q'))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestOps_NoTargetInOverviewIsNoOp asserts r/L/s do nothing in the overview
// (no workload selected) — no modal, no captured exec, no kubectl.
func TestOps_NoTargetInOverviewIsNoOp(t *testing.T) {
	deps, runner, captured, _ := opsHarness(t)
	deps.Entry = Entry{} // plain all-namespaces entry (no resolution)
	tm := teatest.NewTestModel(t, New(deps), teatest.WithInitialTermSize(120, 40))
	waitFor(t, tm, "all-namespaces", "NAMESPACE")

	tm.Send(runeMsg('r'))
	tm.Send(runeMsg('L'))
	tm.Send(runeMsg('s'))

	tm.Send(runeMsg('q'))
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(Model)
	if m.restartModal != nil {
		t.Error("restart modal should not open in the overview (no workload selected)")
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
