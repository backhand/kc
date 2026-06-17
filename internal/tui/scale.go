package tui

import (
	"context"
	"strconv"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/deploy"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/store"
)

// The scale modal (the `s` op — SPEC "Operations"): scale a SET of deployments
// to one replica count. It mirrors the restart flow but swaps restart's
// confirm-only step for a replica-count step:
//
//	scaleSelect   → deployment checkboxes + preset chips (the shared selection)
//	scaleReplicas → enter the target replica count; the screen shows the selected
//	                set + the target, so it doubles as the confirm
//	scaleRollout  → per-deployment `kubectl scale … --replicas=N` + status watch
//
// The ONLY mutation is the confirm-gated deploy.Scale fired when leaving
// scaleReplicas with a valid count (≥0) AND a non-empty selection. esc backs out
// a phase (select→close, replicas→back to select), so the user can never get
// stuck. Scaling to 0 (pause) then back up is an explicit goal — replicas=0 is a
// valid target, handled end to end.

// scalePhase is one screen of the scale flow.
type scalePhase int

const (
	scaleSelect   scalePhase = iota // deployment checkboxes + preset chips
	scaleReplicas                   // enter the target replica count (doubles as confirm)
	scaleRollout                    // per-deployment scale + rollout status
)

// scaleState drives the `s` op: select a SET (like restart), enter a replica
// count (the confirm step), then the per-deployment rollout view (reusing
// rolloutLine / RolloutStatus). Held by Model.scaleModal; nil when closed.
type scaleState struct {
	namespace string
	phase     scalePhase

	// sel is the shared deployment checkbox + preset model (selection.go) — the
	// same piece deploy/restart use, so scale's select screen behaves identically.
	sel selection
	// origin describes what the user came from when opening from a pods view
	// (e.g. "pod/x → deployment/y"), shown on the select screen so the
	// pod → parent-deployment targeting is explicit. Empty from a namespace view.
	origin string

	// replicas is the lightweight numeric input for the target replica count: a
	// decimal string the user edits with digit/backspace keys (digit keys append,
	// backspace deletes; non-digits rejected). Defaulted to the focused
	// deployment's current DesiredReplicas. Parsed to an int (≥0) on confirm.
	replicas string

	// rollouts tracks per-deployment rollout state in the final phase (one line
	// per scaled deployment, like restart/deploy's rollouts).
	rollouts []rolloutLine
	// applied guards against re-firing the mutation if confirm is pressed twice.
	applied bool
}

// openScale opens the scale modal for the current view's namespace + its
// deployments — the SAME context as deploy/restart (deployContext), including
// from a pods view (the parent namespace's deployment list). A no-op outside a
// workload view. Scale is a MUTATION, so it is confirm-gated: select → enter a
// count → only then does anything run.
//
// The set is preselected exactly like deploy/restart: the first learned preset
// containing the focused deployment, else just that deployment. Scale shares
// DEPLOY's presets — see opPresets. The replica field defaults to the focused
// deployment's current DesiredReplicas (a sensible starting point to adjust).
func (m Model) openScale() (tea.Model, tea.Cmd) {
	ns, deployments, ok := m.deployContext()
	if !ok || len(deployments) == 0 {
		return m, nil
	}
	t, _ := m.opTarget() // for the pods-view origin line (parent-deployment targeting)
	origin := ""
	if t.pod != "" {
		origin = "pod/" + t.pod + "  →  deployment/" + t.deployment
	}
	current := m.currentDeployment()
	m.scaleModal = &scaleState{
		namespace: ns,
		phase:     scaleSelect,
		sel:       newSelection(deployments, m.opPresets(ns), current),
		origin:    origin,
		replicas:  defaultReplicas(deployments, current),
	}
	return m, nil
}

// defaultReplicas is the starting value for the replica input: the focused
// deployment's current DesiredReplicas (so scale opens pre-filled at the current
// size — "predict, then confirm"). Falls back to "1" when the focused deployment
// can't be located among the namespace's deployments.
func defaultReplicas(deployments []k8s.Deployment, current string) string {
	for _, d := range deployments {
		if d.Name == current {
			return strconv.Itoa(d.DesiredReplicas)
		}
	}
	return "1"
}

// handleScaleKey routes a key to the active scale phase. esc backs out a phase
// (and closes from the first), so the user can never get stuck.
func (m Model) handleScaleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ss := m.scaleModal
	switch ss.phase {
	case scaleSelect:
		return m.scaleSelectKey(msg)
	case scaleReplicas:
		return m.scaleReplicasKey(msg)
	case scaleRollout:
		return m.scaleRolloutKey(msg)
	}
	return m, nil
}

func (m Model) scaleSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ss := m.scaleModal
	switch {
	case key.Matches(msg, keys.Cancel):
		m.scaleModal = nil // close the modal (nothing has run — pre-mutation)
		return m, nil
	case key.Matches(msg, keys.Confirm):
		if !ss.sel.anyChecked() {
			return m, nil
		}
		ss.phase = scaleReplicas
		return m, nil
	}
	// Shared select keys: ↑/↓ move, space toggles the row, 1-9 toggle a preset.
	// NOTE: digits toggle PRESETS in this phase (handleSelectKey); they type the
	// replica NUMBER only in the scaleReplicas phase — distinct phases, no clash.
	ss.sel.handleSelectKey(msg)
	return m, nil
}

// scaleReplicasKey handles the replica-count step: a lightweight numeric input
// (digit keys append, backspace deletes, non-digits rejected) that doubles as
// the confirm. enter APPLIES (confirm-gated) when the count parses to ≥0 AND the
// selection is non-empty; esc steps back to select (nothing has run).
func (m Model) scaleReplicasKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ss := m.scaleModal
	switch {
	case key.Matches(msg, keys.Cancel):
		ss.phase = scaleSelect // back to the checkboxes (still nothing has run)
		return m, nil
	case key.Matches(msg, keys.Confirm):
		return m.scaleApply()
	}
	// Numeric editing. Digits append; backspace deletes the last digit. Everything
	// else (including the preset number-keys from the select phase) is ignored here
	// so the field only ever holds a decimal replica count.
	if n, ok := digitKey(msg); ok {
		// Reject a leading-zero pile-up ("007"): keep a single "0" until a non-zero
		// digit is typed, so the rendered count stays a clean decimal.
		if ss.replicas == "0" {
			ss.replicas = ""
		}
		ss.replicas += strconv.Itoa(n)
		return m, nil
	}
	if msg.Type == tea.KeyBackspace && len(ss.replicas) > 0 {
		ss.replicas = ss.replicas[:len(ss.replicas)-1]
		return m, nil
	}
	return m, nil
}

// scaleApply is the confirm-gated APPLY (leaving scaleReplicas via enter): with a
// valid count (≥0) AND a non-empty selection it records the scaled set (learning)
// and fires one scale+watch per checked deployment. A no-op (stays on the
// replica screen) when the count is empty/invalid or nothing is checked, and
// guarded against double-apply by `applied`.
func (m Model) scaleApply() (tea.Model, tea.Cmd) {
	ss := m.scaleModal
	if ss.applied {
		return m, nil // guard double-confirm
	}
	replicas, ok := parseReplicas(ss.replicas)
	if !ok {
		return m, nil // empty/invalid count → stay on the replica screen
	}
	names := ss.sel.checkedNames()
	if len(names) == 0 {
		return m, nil // belt-and-suspenders: never apply an empty set
	}
	ss.applied = true
	m.recordScale(ss, names)
	ss.phase = scaleRollout
	ss.rollouts = make([]rolloutLine, len(names))
	for i, name := range names {
		ss.rollouts[i] = rolloutLine{deployment: name, state: rolloutRunning}
	}
	cmds := make([]tea.Cmd, 0, len(names))
	for _, name := range names {
		cmds = append(cmds, m.runScale(ss.namespace, name, replicas))
	}
	return m, tea.Batch(cmds...)
}

func (m Model) scaleRolloutKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ss := m.scaleModal
	// esc/enter closes once everything settled; esc always closes (an in-flight
	// scale continues server-side, mirroring the deploy/restart rollout phase).
	switch {
	case key.Matches(msg, keys.Cancel), key.Matches(msg, keys.Confirm):
		if rolloutSettled(ss.rollouts) || key.Matches(msg, keys.Cancel) {
			m.scaleModal = nil
			return m, nil
		}
	}
	return m, nil
}

// onScaleStep folds one deployment's scale+rollout result into the modal.
func (m Model) onScaleStep(msg scaleStepMsg) Model {
	ss := m.scaleModal
	if ss == nil {
		return m
	}
	for i := range ss.rollouts {
		if ss.rollouts[i].deployment != msg.deployment {
			continue
		}
		if msg.err != nil {
			ss.rollouts[i].state = rolloutFailed
			ss.rollouts[i].detail = msg.err.Error()
		} else {
			ss.rollouts[i].state = rolloutDone
			ss.rollouts[i].detail = msg.detail
		}
	}
	return m
}

// recordScale records the scaled SET into the learning store under the same shape
// deploy/restart use ({deployments: [...]}), so a "scale" history builds its own
// presets over time (SPEC). Best-effort; nil store / not-in-a-repo simply skips.
// Scoped cluster × app exactly like deploy/restart.
func (m Model) recordScale(ss *scaleState, names []string) {
	if m.deps.History == nil {
		return
	}
	arr := make([]any, len(names))
	for i, s := range names {
		arr[i] = s
	}
	_ = m.deps.History.Record("scale", m.deployScope(ss.namespace), store.Params{"deployments": arr})
}

// parseReplicas parses the replica input to a non-negative int. ok is false for
// an empty or non-numeric/negative value (so the apply is gated on a valid ≥0
// count). The input only ever holds digits (scaleReplicasKey rejects the rest),
// so this is a clean Atoi; the guard is defence-in-depth.
func parseReplicas(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ── Cmds + messages for scale ──────────────────────────────────────────────────

// scaleStepMsg carries the result of the scale's apply (`kubectl scale`) +
// rollout-status watch (mirrors restartStepMsg).
type scaleStepMsg struct {
	deployment string
	detail     string // a short success/status line for the rollout view
	err        error
}

// runScale performs the confirmed scale: `kubectl scale … --replicas=N` (THE
// mutation — confirm-gated in the UI) then watches the rollout with `kubectl
// rollout status` (which returns promptly for replicas=0). The injected Runner
// (tests' capture func, else exec.Run) performs both, so headless tests assert
// argv without a cluster. Mirrors runRestart.
func (m Model) runScale(ns, deployment string, replicas int) tea.Cmd {
	kopts := m.deps.KubeOpts
	runner := m.deps.Runner
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), rolloutTimeout+fetchTimeout)
		defer cancel()

		// 1) Scale the deployment (the real, confirmed mutation).
		if _, err := deploy.Scale(ctx, kopts, ns, deployment, replicas,
			deploy.ScaleOpts{Runner: runner}); err != nil {
			return scaleStepMsg{deployment: deployment, err: err}
		}
		// 2) Watch the rollout to completion (reuses the deploy rollout watcher;
		//    returns promptly when the target is 0 replicas).
		res, err := deploy.RolloutStatus(ctx, kopts, ns, deployment,
			deploy.RolloutOpts{Timeout: rolloutTimeout, Runner: runner})
		if err != nil {
			return scaleStepMsg{deployment: deployment, err: err}
		}
		return scaleStepMsg{deployment: deployment, detail: lastLine(res.Stdout)}
	}
}
