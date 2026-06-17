package tui

import (
	"context"
	"os"
	"os/exec"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/deploy"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
)

// The contextual operations on the selected workload (SPEC "Operations"):
//
//	[r] restart — `kubectl rollout restart deployment/<d>`, confirm-gated (a
//	    mutation, like deploy), then watched via the existing rollout view.
//	[L] logs    — `kubectl logs <target> --all-containers --tail=200 -f`,
//	    read-only, streamed by handing the terminal to kubectl (tea.ExecProcess).
//	[s] shell   — `kubectl exec -it <target> -- sh`, interactive, also via
//	    tea.ExecProcess (suspend the TUI → interact → resume on exit).
//
// All three resolve a single contextual target from the current view: the
// selected deployment in a namespace view, or the selected pod in a pods view
// (restart in the pods view targets the pod's PARENT deployment). The kubectl
// argv is built by the pure k8s.LogsArgs / k8s.ExecArgs / deploy.RolloutRestart*
// helpers so it is unit-testable without spawning anything.

// opTarget is the resolved subject of an operation in the current view.
type opTarget struct {
	namespace string
	// deployment is the workload to restart (`r`). In a pods view it is the
	// selected pod's PARENT deployment.
	deployment string
	// pod is the selected pod's name in a pods view, else empty.
	pod string
	// logsExecRef is the kubectl resource reference for logs/exec: "pod/<name>"
	// in a pods view (the exact selected pod), else "deployment/<name>" in a
	// namespace view (kubectl then picks a pod of the deployment — "a pod of the
	// deployment", per SPEC).
	logsExecRef string
	// label is a short human description for the confirm prompt / hints.
	label string
}

// opTarget resolves the operation subject for the visible view. ok is false in
// views without a workload (overview / group), or when the selected row is out
// of range (e.g. an empty list) — the op keys are then no-ops.
func (m Model) opTarget() (opTarget, bool) {
	top := m.top()
	switch top.kind {
	case levelNamespace:
		dep, ok := m.selectedDeployment(*top)
		if !ok {
			return opTarget{}, false
		}
		return opTarget{
			namespace:   top.namespace,
			deployment:  dep,
			logsExecRef: "deployment/" + dep,
			label:       "deployment/" + dep,
		}, true
	case levelDeployment:
		// Pods view: act on the selected pod; restart targets its parent
		// deployment (the level's deployment scope).
		if top.cursor < 0 || top.cursor >= len(top.pods) {
			return opTarget{}, false
		}
		pod := top.pods[top.cursor]
		return opTarget{
			namespace:   top.namespace,
			deployment:  top.deployment,
			pod:         pod.Name,
			logsExecRef: "pod/" + pod.Name,
			label:       "pod/" + pod.Name,
		}, true
	default:
		return opTarget{}, false
	}
}

// opContextAvailable reports whether r/L/s would act from the current view (a
// namespace or pods view with a selectable row). Used to highlight the footer.
func (m Model) opContextAvailable() bool {
	_, ok := m.opTarget()
	return ok
}

// ── Restart (confirm-gated mutation) ──────────────────────────────────────────

// restartState drives the `r` op: a confirm screen (what will restart), then the
// per-deployment rollout view (reusing rolloutLine / RolloutStatus). Held by
// Model.restartModal; nil when closed.
type restartState struct {
	namespace  string
	deployment string
	// origin describes what the user selected (e.g. "pod/x → deployment/y") for
	// the confirm prompt, so a pods-view restart makes the parent-deployment
	// targeting explicit.
	origin string

	confirmed bool        // gate: true once the user pressed enter to APPLY
	rollout   rolloutLine // single-deployment rollout state (restart is one deployment)
}

// openRestart opens the restart confirm modal for the current view's target. A
// no-op outside a workload view. Restart is a MUTATION, so it is confirm-gated:
// opening only shows what WILL restart — nothing runs until the confirm.
func (m Model) openRestart() (tea.Model, tea.Cmd) {
	t, ok := m.opTarget()
	if !ok {
		return m, nil
	}
	origin := "deployment/" + t.deployment
	if t.pod != "" {
		// Pods view: make the pod → parent-deployment restart explicit.
		origin = "pod/" + t.pod + "  →  deployment/" + t.deployment
	}
	m.restartModal = &restartState{
		namespace:  t.namespace,
		deployment: t.deployment,
		origin:     origin,
		rollout:    rolloutLine{deployment: t.deployment, state: rolloutPending},
	}
	return m, nil
}

// handleRestartKey routes a key to the restart modal: enter confirms (firing the
// mutation), esc backs out (before confirm) / closes (after). The user can never
// get stuck — esc always closes.
func (m Model) handleRestartKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rs := m.restartModal
	switch {
	case key.Matches(msg, keys.Cancel):
		// Esc closes the modal (a restart already in flight continues server-side,
		// mirroring the deploy rollout phase).
		m.restartModal = nil
		return m, nil
	case key.Matches(msg, keys.Confirm):
		if rs.confirmed {
			// Post-rollout: enter closes once settled; otherwise ignored so the
			// rollout view stays up while it runs.
			if rs.rollout.state == rolloutDone || rs.rollout.state == rolloutFailed {
				m.restartModal = nil
			}
			return m, nil
		}
		// Confirm-gated APPLY: record (best-effort) + fire the restart+watch.
		rs.confirmed = true
		rs.rollout.state = rolloutRunning
		m.recordRestart(rs)
		return m, m.runRestart(rs.namespace, rs.deployment)
	}
	return m, nil
}

// onRestartStep folds the restart's apply+rollout result into the modal.
func (m Model) onRestartStep(msg restartStepMsg) Model {
	rs := m.restartModal
	if rs == nil || rs.deployment != msg.deployment {
		return m
	}
	if msg.err != nil {
		rs.rollout.state = rolloutFailed
		rs.rollout.detail = msg.err.Error()
	} else {
		rs.rollout.state = rolloutDone
		rs.rollout.detail = msg.detail
	}
	return m
}

// recordRestart records the restart into the learning store (generic store, so
// restart can grow predictive defaults later — SPEC). Best-effort; nil store /
// not-in-a-repo simply skips. Scoped cluster × app exactly like deploy.
func (m Model) recordRestart(rs *restartState) {
	if m.deps.History == nil {
		return
	}
	_ = m.deps.History.Record("restart", m.deployScope(rs.namespace), nil)
}

// ── Logs / shell (interactive, via tea.ExecProcess) ───────────────────────────

// logsTail is how many trailing log lines the logs op (`L`) shows before
// following the live stream (SPEC: --tail=200).
const logsTail = 200

// runLogs streams the selected target's logs by handing the terminal to
// `kubectl logs … -f` via tea.ExecProcess (suspend the TUI → live stream →
// Ctrl-C returns to kc, which restores). Read-only — no confirm. A no-op outside
// a workload view.
func (m Model) runLogs() (tea.Model, tea.Cmd) {
	t, ok := m.opTarget()
	if !ok {
		return m, nil
	}
	args := k8s.LogsArgs(m.deps.KubeOpts, t.namespace, t.logsExecRef, logsTail, true)
	return m, m.execKubectl(args)
}

// runShell opens an interactive shell into the target via `kubectl exec -it …
// -- sh`, handed to the terminal with tea.ExecProcess (suspend → interact →
// resume on exit). In a pods view it execs into the exact selected pod; in a
// namespace view kubectl picks a pod of the deployment. A no-op outside a
// workload view.
func (m Model) runShell() (tea.Model, tea.Cmd) {
	t, ok := m.opTarget()
	if !ok {
		return m, nil
	}
	args := k8s.ExecArgs(m.deps.KubeOpts, t.namespace, t.logsExecRef)
	return m, m.execKubectl(args)
}

// kubectlExecCmd builds the *exec.Cmd for an interactive/streaming kubectl op
// (logs/shell). Pure (no spawn): the path+args are asserted directly in tests.
// The KUBECONFIG env from KubeOpts is threaded onto the command exactly as the
// read-only wrappers do, so the op targets the same cluster the views read.
func (m Model) kubectlExecCmd(args []string) *exec.Cmd {
	cmd := exec.Command("kubectl", args...)
	if env := m.deps.KubeOpts.ExecOptions().Env; len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}

// execKubectl turns a built kubectl command into the Cmd that runs it. In
// production that is tea.ExecProcess (suspend the program → hand the terminal to
// kubectl → resume kc on exit), landing an execFinishedMsg.
//
// Tests inject Deps.ExecHook: the hook captures the *exec.Cmd so its path+args
// can be asserted, and we return nil so the headless program NEVER spawns kubectl
// (SPEC safety: never open a real shell / stream `-f` in a test).
func (m Model) execKubectl(args []string) tea.Cmd {
	cmd := m.kubectlExecCmd(args)
	if m.deps.ExecHook != nil {
		m.deps.ExecHook(cmd)
		return nil
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return execFinishedMsg{err: err}
	})
}

// ── Cmds + messages for restart / exec ────────────────────────────────────────

// restartStepMsg carries the result of the restart's apply (`kubectl rollout
// restart`) + rollout-status watch.
type restartStepMsg struct {
	deployment string
	detail     string // a short success/status line for the rollout view
	err        error
}

// execFinishedMsg signals a suspended logs/shell session returned (the program
// is already restored by tea.ExecProcess). err is the spawn/exit error, if any.
type execFinishedMsg struct{ err error }

// runRestart performs the confirmed restart: `kubectl rollout restart` (THE
// mutation — confirm-gated in the UI) then watches the rollout with `kubectl
// rollout status`. The injected Runner (tests' capture func, else exec.Run)
// performs both, so headless tests assert argv without a cluster.
func (m Model) runRestart(ns, deployment string) tea.Cmd {
	kopts := m.deps.KubeOpts
	runner := m.deps.Runner
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), rolloutTimeout+fetchTimeout)
		defer cancel()

		// 1) Trigger the restart (the real, confirmed mutation).
		if _, err := deploy.RolloutRestart(ctx, kopts, ns, deployment,
			deploy.RestartOpts{Runner: runner}); err != nil {
			return restartStepMsg{deployment: deployment, err: err}
		}
		// 2) Watch the rollout to completion (reuses the deploy rollout watcher).
		res, err := deploy.RolloutStatus(ctx, kopts, ns, deployment,
			deploy.RolloutOpts{Timeout: rolloutTimeout, Runner: runner})
		if err != nil {
			return restartStepMsg{deployment: deployment, err: err}
		}
		return restartStepMsg{deployment: deployment, detail: lastLine(res.Stdout)}
	}
}
