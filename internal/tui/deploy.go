package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/deploy"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/git"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/github"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/store"
)

// The deploy modal (SPEC "Deploy flow (v1)"). A four-phase flow overlaid on the
// normal views:
//
//	select   → deployment checkboxes (top learned preset pre-checked) + chips
//	versions → 5 latest releases, annotated (build + image), `o`lder pages back
//	confirm  → exactly-what-changes (deployments + from→to tags), confirm-gated
//	rollout  → kubectl rollout status per deployed deployment
//
// Everything Kubernetes/GitHub-shaped comes from internal/{k8s,github,deploy};
// this file is the state machine + key handling. The ONLY mutation is the
// confirm-gated deploy.SetImage fired when leaving `confirm`.

// deployPhase is one screen of the deploy flow.
type deployPhase int

const (
	phaseSelect   deployPhase = iota // deployment checkboxes + preset chips
	phaseVersions                    // annotated release list
	phaseConfirm                     // change summary, confirm-gated
	phaseRollout                     // per-deployment rollout status
)

// rolloutLine tracks one deployment's rollout state in the final phase.
type rolloutLine struct {
	deployment string
	state      rolloutState
	detail     string // last status line / error tail
}

type rolloutState int

const (
	rolloutPending rolloutState = iota // queued, not started
	rolloutRunning                     // set image issued / waiting on status
	rolloutDone                        // rollout completed
	rolloutFailed                      // set image or rollout status failed
)

// deployState is the deploy modal's full state. Held by Model.deployModal; nil
// when the modal is closed.
type deployState struct {
	namespace string
	phase     deployPhase

	// sel is the shared deployment checkbox + preset model (the `select` phase);
	// see selection.go. Deploy and restart share it.
	sel selection

	// repo / repoImage derived from the deployments' GHCR image (so deploy works
	// even when kc wasn't launched from the repo). repoOK is false when no
	// ghcr.io image was found.
	repo      git.RepoRef
	repoImage string
	repoOK    bool

	// Version list (annotated releases), the focused row, and paging state.
	releases        []github.ReleaseAnnotation
	relCursor       int
	relPage         int // 0-based page; `o` pages back to older releases
	releasesLoading bool
	releasesErr     string

	// changes is the planned per-deployment image change, computed entering
	// confirm and applied (confirm-gated) leaving it.
	changes []deploy.Change

	// rollouts tracks per-deployment rollout state in the final phase.
	rollouts []rolloutLine
	// applied guards against re-firing the mutation if confirm is pressed twice.
	applied bool
}

// openDeploy opens the deploy modal for the namespace the user is currently in
// (a namespace view, or a deployment view inside one). A no-op when there is no
// namespace context, or no deployments to deploy.
//
// It seeds the selection with the learned preset CONTAINING the deployment the
// user is focused on (else just that deployment — see newSelection), derives the
// release repo from the deployments' GHCR image, and kicks off the release fetch.
func (m Model) openDeploy() (tea.Model, tea.Cmd) {
	ns, deployments, ok := m.deployContext()
	if !ok || len(deployments) == 0 {
		return m, nil
	}

	ds := &deployState{
		namespace: ns,
		phase:     phaseSelect,
		// Preselect the set containing the focused deployment (SPEC Feature 1).
		sel: newSelection(deployments, m.deployPresets(ns, deployments), m.currentDeployment()),
	}

	// Derive the release repo from the running images (SPEC).
	if repo, repoOK := deploy.DeriveRepo(deployments); repoOK {
		ds.repo = repo
		ds.repoImage = git.GHCRImageFor(repo)
		ds.repoOK = true
	}

	m.deployModal = ds
	if ds.repoOK {
		ds.releasesLoading = true
		return m, m.fetchReleases(ds.repo, ds.repoImage, releaseLimit, 0)
	}
	return m, nil
}

// currentDeployment is the deployment the user is focused on, for preselecting
// the deploy/restart set (SPEC Feature 1):
//
//   - namespace view (levelNamespace): the cursor-selected deployment.
//   - pods view (levelDeployment): the deployment whose pods are shown.
//
// Empty elsewhere (the deploy/restart modals don't open from those views, so the
// selection falls back to its top-preset behavior — a belt-and-suspenders guard).
func (m Model) currentDeployment() string {
	top := m.top()
	switch top.kind {
	case levelNamespace:
		if dep, ok := m.selectedDeployment(*top); ok {
			return dep
		}
	case levelDeployment:
		return top.deployment
	}
	return ""
}

// deployContext returns the namespace + its deployments for the current view.
// Available from a namespace level (use its loaded deployments) or a deployment
// level (use its parent namespace level's deployments). ok is false elsewhere.
func (m Model) deployContext() (ns string, deployments []k8s.Deployment, ok bool) {
	top := m.top()
	switch top.kind {
	case levelNamespace:
		return top.namespace, top.nsView.Deployments, true
	case levelDeployment:
		// Find the parent namespace level on the stack for its deployment list.
		for i := len(m.stack) - 1; i >= 0; i-- {
			if m.stack[i].kind == levelNamespace && m.stack[i].namespace == top.namespace {
				return top.namespace, m.stack[i].nsView.Deployments, true
			}
		}
		return "", nil, false
	default:
		return "", nil, false
	}
}

// deployContextAvailable reports whether `d` would open the deploy modal from
// the current view (a namespace or deployment view with deployments). Used to
// highlight the footer hint.
func (m Model) deployContextAvailable() bool {
	_, deployments, ok := m.deployContext()
	return ok && len(deployments) > 0
}

// deployPresets returns the learned deployment-sets for the namespace, most
// likely first. Empty when no history is wired or nothing was recorded.
func (m Model) deployPresets(ns string, _ []k8s.Deployment) [][]string {
	if m.deps.History == nil {
		return nil
	}
	return m.deps.History.DeployPresets(m.deployScope(ns))
}

// deployScope is the learning scope for deploys in a namespace: cluster × app.
// The app key is the repo/app name when launched in a repo, else the namespace
// (so presets are still remembered per namespace without a repo context).
func (m Model) deployScope(ns string) store.Scope {
	app := m.deps.App
	if app == "" {
		app = ns
	}
	return store.Scope{Cluster: m.deps.Cluster, App: app}
}

// ── Key handling (per phase) ─────────────────────────────────────────────────

// handleDeployKey routes a key to the active modal phase. Returns the updated
// model + any Cmd. Esc/cancel backs out a phase (and closes the modal from the
// first), so the user can never get stuck.
func (m Model) handleDeployKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ds := m.deployModal
	switch ds.phase {
	case phaseSelect:
		return m.deploySelectKey(msg)
	case phaseVersions:
		return m.deployVersionsKey(msg)
	case phaseConfirm:
		return m.deployConfirmKey(msg)
	case phaseRollout:
		return m.deployRolloutKey(msg)
	}
	return m, nil
}

func (m Model) deploySelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ds := m.deployModal
	switch {
	case key.Matches(msg, keys.Cancel):
		m.deployModal = nil // close the modal
		return m, nil
	case key.Matches(msg, keys.Confirm):
		// Advance to the version list — only with a selection and a derived repo.
		if !ds.sel.anyChecked() || !ds.repoOK {
			return m, nil
		}
		ds.phase = phaseVersions
		return m, nil
	}
	// Shared select keys: ↑/↓ move, space toggles the row, 1-9 toggle a preset.
	ds.sel.handleSelectKey(msg)
	return m, nil
}

func (m Model) deployVersionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ds := m.deployModal
	switch {
	case key.Matches(msg, keys.Cancel):
		ds.phase = phaseSelect // back to checkboxes
		return m, nil
	case key.Matches(msg, keys.Up):
		if ds.relCursor > 0 {
			ds.relCursor--
		}
		return m, nil
	case key.Matches(msg, keys.Down):
		if ds.relCursor < len(ds.releases)-1 {
			ds.relCursor++
		}
		return m, nil
	case key.Matches(msg, keys.Older):
		// Page back to older releases (fetch the next page-worth).
		if ds.releasesLoading {
			return m, nil
		}
		ds.relPage++
		ds.releasesLoading = true
		ds.relCursor = 0
		return m, m.fetchReleases(ds.repo, ds.repoImage, releaseLimit, ds.relPage)
	case key.Matches(msg, keys.Confirm):
		if ds.relCursor < 0 || ds.relCursor >= len(ds.releases) {
			return m, nil
		}
		tag := ds.releases[ds.relCursor].Tag
		ds.changes = deploy.PlanChanges(ds.sel.deployments, ds.sel.checkedNames(), ds.repoImage, tag)
		if len(ds.changes) == 0 {
			return m, nil
		}
		ds.phase = phaseConfirm
		return m, nil
	}
	return m, nil
}

func (m Model) deployConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ds := m.deployModal
	switch {
	case key.Matches(msg, keys.Cancel):
		ds.phase = phaseVersions // back to the version list
		return m, nil
	case key.Matches(msg, keys.Confirm):
		if ds.applied {
			return m, nil // guard double-confirm
		}
		ds.applied = true
		// Record the deploy preset (learning) and start the rollout.
		m.recordDeploy(ds)
		ds.phase = phaseRollout
		ds.rollouts = make([]rolloutLine, len(ds.changes))
		for i, c := range ds.changes {
			ds.rollouts[i] = rolloutLine{deployment: c.Deployment, state: rolloutRunning}
		}
		// Fire one apply+watch Cmd per change. Each lands as a deployStepMsg.
		cmds := make([]tea.Cmd, 0, len(ds.changes))
		for _, c := range ds.changes {
			cmds = append(cmds, m.runDeployStep(c))
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) deployRolloutKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ds := m.deployModal
	// In the rollout phase, esc/enter closes the modal once everything settled;
	// while in-flight, esc just closes (the rollout continues server-side).
	switch {
	case key.Matches(msg, keys.Cancel), key.Matches(msg, keys.Confirm):
		if rolloutSettled(ds.rollouts) || key.Matches(msg, keys.Cancel) {
			m.deployModal = nil
			return m, nil
		}
	}
	return m, nil
}

// recordDeploy records the deployed deployment-set into the learning store so
// the next deploy pre-checks it (SPEC: store.RecordDeploy). Best-effort; a
// nil store / write failure is ignored.
func (m Model) recordDeploy(ds *deployState) {
	if m.deps.History == nil {
		return
	}
	names := make([]string, 0, len(ds.changes))
	for _, c := range ds.changes {
		names = append(names, c.Deployment)
	}
	_ = m.deps.History.RecordDeploy(m.deployScope(ds.namespace), names)
}

// ── Loaded-message folding ───────────────────────────────────────────────────

// onReleasesLoaded folds a release fetch into the modal (if still on the version
// phase / open). Page-aware: a stale page response for a page the user has since
// left is ignored.
func (m Model) onReleasesLoaded(msg releasesLoadedMsg) Model {
	ds := m.deployModal
	if ds == nil || msg.page != ds.relPage {
		return m
	}
	ds.releasesLoading = false
	if msg.err != nil {
		ds.releasesErr = msg.err.Error()
		return m
	}
	ds.releasesErr = ""
	ds.releases = msg.releases
	if ds.relCursor >= len(ds.releases) {
		ds.relCursor = 0
	}
	return m
}

// onDeployStep folds one deployment's apply+rollout result into the modal.
func (m Model) onDeployStep(msg deployStepMsg) Model {
	ds := m.deployModal
	if ds == nil {
		return m
	}
	for i := range ds.rollouts {
		if ds.rollouts[i].deployment != msg.deployment {
			continue
		}
		if msg.err != nil {
			ds.rollouts[i].state = rolloutFailed
			ds.rollouts[i].detail = msg.err.Error()
		} else {
			ds.rollouts[i].state = rolloutDone
			ds.rollouts[i].detail = msg.detail
		}
	}
	return m
}

// ── small helpers ────────────────────────────────────────────────────────────

// digitKey returns the digit (1..9, and 0) a key message represents, if any.
func digitKey(msg tea.KeyMsg) (int, bool) {
	if msg.Type != tea.KeyRunes || len(msg.Runes) != 1 {
		return 0, false
	}
	r := msg.Runes[0]
	if r < '0' || r > '9' {
		return 0, false
	}
	return int(r - '0'), true
}

// rolloutSettled reports whether every tracked rollout has finished (done or
// failed) — i.e. nothing is still running/pending.
func rolloutSettled(lines []rolloutLine) bool {
	for _, l := range lines {
		if l.state == rolloutRunning || l.state == rolloutPending {
			return false
		}
	}
	return true
}
