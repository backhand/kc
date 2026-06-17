package tui

import (
	"context"
	"os"
	"os/exec"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/deploy"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/store"
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

// ── Restart (confirm-gated mutation, set-based) ───────────────────────────────

// restartPhase is one screen of the restart flow. Restart mirrors deploy's
// multi-select flow MINUS the version step:
//
//	restartSelect  → deployment checkboxes + preset chips (the shared selection)
//	restartConfirm → the selected set that will restart, confirm-gated
//	restartRollout → per-deployment `kubectl rollout restart` + status watch
type restartPhase int

const (
	restartSelect  restartPhase = iota // deployment checkboxes + preset chips
	restartConfirm                     // selected-set summary, confirm-gated
	restartRollout                     // per-deployment rollout status
)

// restartState drives the `r` op: select a SET (like deploy), confirm what will
// restart, then the per-deployment rollout view (reusing rolloutLine /
// RolloutStatus). Held by Model.restartModal; nil when closed.
type restartState struct {
	namespace string
	phase     restartPhase

	// sel is the shared deployment checkbox + preset model (selection.go) — the
	// same piece deploy uses, so restart's select screen behaves identically.
	sel selection
	// origin describes what the user came from when opening from a pods view
	// (e.g. "pod/x → deployment/y"), shown on the select screen so the
	// pod → parent-deployment targeting is explicit. Empty from a namespace view.
	origin string

	// rollouts tracks per-deployment rollout state in the final phase (one line
	// per restarted deployment, like deploy's rollouts).
	rollouts []rolloutLine
	// applied guards against re-firing the mutation if confirm is pressed twice.
	applied bool
}

// openRestart opens the restart modal for the current view's namespace + its
// deployments — the SAME context as deploy (deployContext), including from a
// pods view (the parent namespace's deployment list). A no-op outside a workload
// view. Restart is a MUTATION, so it is confirm-gated: select → confirm → only
// then does anything run.
//
// The set is preselected exactly like deploy (Feature 1): the first learned
// preset containing the focused deployment, else just that deployment. Restart
// shares DEPLOY's presets — see restartPresets.
func (m Model) openRestart() (tea.Model, tea.Cmd) {
	ns, deployments, ok := m.deployContext()
	if !ok || len(deployments) == 0 {
		return m, nil
	}
	t, _ := m.opTarget() // for the pods-view origin line (parent-deployment targeting)
	origin := ""
	if t.pod != "" {
		origin = "pod/" + t.pod + "  →  deployment/" + t.deployment
	}
	m.restartModal = &restartState{
		namespace: ns,
		phase:     restartSelect,
		sel:       newSelection(deployments, m.restartPresets(ns), m.currentDeployment()),
		origin:    origin,
	}
	return m, nil
}

// restartPresets are the learned deployment-sets restart preselects from. We use
// DEPLOY's presets (DeployPresets): they represent "sets deployed together",
// which is exactly the set a user typically wants to restart together — and on a
// fresh install restart has no history of its own yet. Restart STILL records its
// own sets (recordRestart), so a "restart" history accrues for future use; we
// just don't read it back yet to avoid a cold-start with no chips.
func (m Model) restartPresets(ns string) [][]string {
	if m.deps.History == nil {
		return nil
	}
	return m.deps.History.DeployPresets(m.deployScope(ns))
}

// handleRestartKey routes a key to the active restart phase. esc backs out a
// phase (and closes from the first), so the user can never get stuck.
func (m Model) handleRestartKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rs := m.restartModal
	switch rs.phase {
	case restartSelect:
		return m.restartSelectKey(msg)
	case restartConfirm:
		return m.restartConfirmKey(msg)
	case restartRollout:
		return m.restartRolloutKey(msg)
	}
	return m, nil
}

func (m Model) restartSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rs := m.restartModal
	switch {
	case key.Matches(msg, keys.Cancel):
		m.restartModal = nil // close the modal (nothing has run — pre-mutation)
		return m, nil
	case key.Matches(msg, keys.Confirm):
		if !rs.sel.anyChecked() {
			return m, nil
		}
		rs.phase = restartConfirm
		return m, nil
	}
	// Shared select keys: ↑/↓ move, space toggles the row, 1-9 toggle a preset.
	rs.sel.handleSelectKey(msg)
	return m, nil
}

func (m Model) restartConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rs := m.restartModal
	switch {
	case key.Matches(msg, keys.Cancel):
		rs.phase = restartSelect // back to the checkboxes (still nothing has run)
		return m, nil
	case key.Matches(msg, keys.Confirm):
		if rs.applied {
			return m, nil // guard double-confirm
		}
		rs.applied = true
		// Confirm-gated APPLY: record the restarted SET (learning) + fire one
		// restart+watch per selected deployment.
		names := rs.sel.checkedNames()
		m.recordRestart(rs, names)
		rs.phase = restartRollout
		rs.rollouts = make([]rolloutLine, len(names))
		for i, name := range names {
			rs.rollouts[i] = rolloutLine{deployment: name, state: rolloutRunning}
		}
		cmds := make([]tea.Cmd, 0, len(names))
		for _, name := range names {
			cmds = append(cmds, m.runRestart(rs.namespace, name))
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) restartRolloutKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rs := m.restartModal
	// esc/enter closes once everything settled; esc always closes (an in-flight
	// restart continues server-side, mirroring the deploy rollout phase).
	switch {
	case key.Matches(msg, keys.Cancel), key.Matches(msg, keys.Confirm):
		if rolloutSettled(rs.rollouts) || key.Matches(msg, keys.Cancel) {
			m.restartModal = nil
			return m, nil
		}
	}
	return m, nil
}

// onRestartStep folds one deployment's restart+rollout result into the modal.
func (m Model) onRestartStep(msg restartStepMsg) Model {
	rs := m.restartModal
	if rs == nil {
		return m
	}
	for i := range rs.rollouts {
		if rs.rollouts[i].deployment != msg.deployment {
			continue
		}
		if msg.err != nil {
			rs.rollouts[i].state = rolloutFailed
			rs.rollouts[i].detail = msg.err.Error()
		} else {
			rs.rollouts[i].state = rolloutDone
			rs.rollouts[i].detail = msg.detail
		}
	}
	return m
}

// recordRestart records the restarted SET into the learning store under the same
// shape deploy uses ({deployments: [...]}), so a "restart" history builds its own
// presets over time (SPEC). Best-effort; nil store / not-in-a-repo simply skips.
// Scoped cluster × app exactly like deploy.
func (m Model) recordRestart(rs *restartState, names []string) {
	if m.deps.History == nil {
		return
	}
	arr := make([]any, len(names))
	for i, s := range names {
		arr[i] = s
	}
	_ = m.deps.History.Record("restart", m.deployScope(rs.namespace), store.Params{"deployments": arr})
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
