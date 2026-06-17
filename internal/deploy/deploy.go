// Package deploy holds the mutating code in kc: the imperative `kubectl set
// image` that the confirm-gated deploy flow runs, the `kubectl rollout restart`
// the restart op runs, the `kubectl scale` the scale op runs, plus the `kubectl
// rollout status` they all watch afterwards (SPEC "Deploy flow (v1)" /
// "Operations").
//
// Everything else in kc is read-only. This package is deliberately tiny and
// isolated so the mutation surface is auditable in one place.
//
//   - The argv is built by the pure SetImageArgs / RolloutRestartArgs /
//     ScaleArgs / RolloutStatusArgs helpers, so the exact command can be asserted
//     in a unit test (and dry-run-checked by a reviewer) without spawning anything.
//   - Execution goes through an injectable Runner (default: exec.Run) so headless
//     tests capture the constructed argv WITHOUT hitting a cluster.
//   - The mutations (SetImage / RolloutRestart) support a server-side dry run
//     (`--dry-run=server`): the modal can validate a change against the apiserver
//     as a no-op before the real apply/restart.
//
// Release vX.Y.Z → image ghcr.io/<owner>/<app>:vX.Y.Z. v1 is imperative
// `kubectl set image` (SPEC notes a later Kustomize-bump mode is pluggable).
package deploy

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/backhand/kc/internal/exec"
	"github.com/backhand/kc/internal/k8s"
)

// AllContainers is the kubectl wildcard that targets every container in the pod
// template (`kubectl set image deploy/x *=img`). Used when no specific container
// name is known — correct for single-container deployments.
const AllContainers = "*"

// Runner executes a command and returns its captured result. It matches
// exec.Run's signature so the real implementation is a thin pass-through; tests
// inject a capture func to assert argv offline (no cluster, no kubectl).
type Runner func(ctx context.Context, command string, args []string, opts exec.RunOptions) (exec.RunResult, error)

// defaultRunner is the production runner — a real shell-out via internal/exec.
func defaultRunner(ctx context.Context, command string, args []string, opts exec.RunOptions) (exec.RunResult, error) {
	return exec.Run(ctx, command, args, opts)
}

// SetImageOpts configures a single SetImage call.
type SetImageOpts struct {
	// DryRun, when true, appends `--dry-run=server`: kubectl validates the change
	// against the apiserver and reports the diff WITHOUT mutating anything. The
	// deploy modal uses this to preview/validate before the real apply.
	DryRun bool
	// Runner overrides the executor (tests inject a capture func). Nil = the real
	// exec.Run shell-out.
	Runner Runner
}

// RolloutOpts configures a RolloutStatus call.
type RolloutOpts struct {
	// Timeout caps `kubectl rollout status` server-side (`--timeout=<d>`); zero
	// omits the flag (kubectl then waits indefinitely). The per-process exec
	// timeout (k8s.Options.Timeout) still bounds the spawn either way.
	Timeout time.Duration
	// Runner overrides the executor (tests inject a capture func). Nil = exec.Run.
	Runner Runner
}

// RestartOpts configures a single RolloutRestart call. Mirrors SetImageOpts so
// the restart op has the same dry-run + injectable-runner surface as deploy.
type RestartOpts struct {
	// DryRun, when true, appends `--dry-run=server` (mirroring SetImageOpts).
	//
	// CAVEAT: unlike `kubectl set image`, the `kubectl rollout restart`
	// subcommand does NOT accept --dry-run in current kubectl (it errors
	// "unknown flag: --dry-run"). The flag is supported here for argv symmetry
	// with SetImage and forward-compatibility, but the confirm-gated UI flow runs
	// the REAL restart (DryRun=false) — the confirm screen is the safety gate, not
	// a server dry-run. Kept pure + unit-tested so the day kubectl supports it,
	// the wiring is already correct.
	DryRun bool
	// Runner overrides the executor (tests inject a capture func). Nil = the real
	// exec.Run shell-out.
	Runner Runner
}

// ScaleOpts configures a single Scale call. Mirrors RestartOpts so the scale op
// has the same injectable-runner surface as deploy/restart.
type ScaleOpts struct {
	// Runner overrides the executor (tests inject a capture func). Nil = the real
	// exec.Run shell-out.
	Runner Runner
}

// ContainerArg returns the kubectl container token for `set image`: the given
// container name, or AllContainers ("*") when the name is empty. Trimmed.
func ContainerArg(container string) string {
	c := strings.TrimSpace(container)
	if c == "" {
		return AllContainers
	}
	return c
}

// SetImageArgs builds the kubectl argv for setting a deployment's container
// image — pure, so the exact command is unit-testable and dry-run-checkable.
//
//	kubectl [--context <c>] -n <ns> set image deployment/<deployment> \
//	    <container>=<image> [--dry-run=server]
//
// container "" → the "*" wildcard (every container — correct for a
// single-container deployment). The kube context (from k8s.Options) is threaded
// so the mutation lands on the same cluster the views read.
func SetImageArgs(kopts k8s.Options, ns, deployment, container, image string, dryRun bool) []string {
	args := contextArgs(kopts)
	args = append(args, "-n", ns, "set", "image", "deployment/"+deployment,
		ContainerArg(container)+"="+image)
	if dryRun {
		args = append(args, "--dry-run=server")
	}
	return args
}

// RolloutRestartArgs builds the kubectl argv for restarting a deployment — pure,
// so the exact command is unit-testable. Mirrors SetImageArgs' shape (context
// threaded, dry-run appended).
//
//	kubectl [--context <c>] -n <ns> rollout restart deployment/<deployment> \
//	    [--dry-run=server]
//
// `rollout restart` bumps the pod-template's restartedAt annotation, which the
// Deployment controller rolls out as a normal rolling update (so the existing
// rollout-status view watches it exactly like a deploy).
//
// NOTE: the `--dry-run=server` branch exists for argv symmetry with SetImageArgs
// (and is unit-tested), but current kubectl's `rollout restart` rejects the flag
// — see RestartOpts.DryRun. The confirm-gated UI runs the real restart.
func RolloutRestartArgs(kopts k8s.Options, ns, deployment string, dryRun bool) []string {
	args := contextArgs(kopts)
	args = append(args, "-n", ns, "rollout", "restart", "deployment/"+deployment)
	if dryRun {
		args = append(args, "--dry-run=server")
	}
	return args
}

// ScaleArgs builds the kubectl argv for scaling a deployment to a fixed replica
// count — pure, so the exact command is unit-testable. Mirrors the other
// builders' shape (context threaded first).
//
//	kubectl [--context <c>] -n <ns> scale deployment/<deployment> \
//	    --replicas=<N>
//
// replicas=0 is a valid, explicit scale-to-zero (pause): the argv is
// `--replicas=0` and the Deployment controller spins the pods down. Scaling back
// up is the same call with N>0. There is no dry-run branch — `kubectl scale` is a
// direct replica write, gated by the modal's confirm screen (the replica-count
// step doubles as the confirm).
func ScaleArgs(kopts k8s.Options, ns, deployment string, replicas int) []string {
	args := contextArgs(kopts)
	args = append(args, "-n", ns, "scale", "deployment/"+deployment,
		"--replicas="+strconv.Itoa(replicas))
	return args
}

// RolloutStatusArgs builds the kubectl argv for watching a rollout — pure.
//
//	kubectl [--context <c>] -n <ns> rollout status deployment/<deployment> \
//	    [--timeout=<d>]
func RolloutStatusArgs(kopts k8s.Options, ns, deployment string, timeout time.Duration) []string {
	args := contextArgs(kopts)
	args = append(args, "-n", ns, "rollout", "status", "deployment/"+deployment)
	if timeout > 0 {
		args = append(args, "--timeout="+timeout.String())
	}
	return args
}

// contextArgs returns the leading `--context <c>` (or nothing). A fresh slice so
// callers can append freely. Mirrors k8s.Options.args, kept local so the
// mutating package owns its own argv assembly.
func contextArgs(kopts k8s.Options) []string {
	if kopts.Context != "" {
		return []string{"--context", kopts.Context}
	}
	return []string{}
}

// SetImage runs `kubectl set image` to roll a deployment's container onto a new
// image. This is the real mutation (a no-op only when o.DryRun is set).
//
// Returns the captured kubectl output; an *exec.ExecError on failure (the
// modal surfaces the trimmed stderr). The kubeconfig/context/timeout from kopts
// are threaded through exactly as the read-only wrappers do.
func SetImage(ctx context.Context, kopts k8s.Options, ns, deployment, container, image string, o SetImageOpts) (exec.RunResult, error) {
	run := o.Runner
	if run == nil {
		run = defaultRunner
	}
	args := SetImageArgs(kopts, ns, deployment, container, image, o.DryRun)
	return run(ctx, "kubectl", args, runOpts(kopts))
}

// RolloutRestart runs `kubectl rollout restart` to restart a deployment's pods
// (the `r` op — SPEC "Operations"). This is a real mutation (a no-op only when
// o.DryRun is set): it bumps the pod template's restartedAt annotation and the
// Deployment controller does a rolling restart, which the rollout view then
// watches via RolloutStatus exactly like a deploy.
//
// Returns the captured kubectl output; an *exec.ExecError on failure. The
// kubeconfig/context/timeout from kopts are threaded through exactly as SetImage
// and the read-only wrappers do.
func RolloutRestart(ctx context.Context, kopts k8s.Options, ns, deployment string, o RestartOpts) (exec.RunResult, error) {
	run := o.Runner
	if run == nil {
		run = defaultRunner
	}
	args := RolloutRestartArgs(kopts, ns, deployment, o.DryRun)
	return run(ctx, "kubectl", args, runOpts(kopts))
}

// Scale runs `kubectl scale … --replicas=<N>` to set a deployment's replica
// count (the `s` op — SPEC "Operations"). This is a real mutation: it writes the
// Deployment's spec.replicas and the controller reconciles to it. replicas=0 is
// an explicit, valid pause (scale-to-zero); scaling back up is the same call with
// N>0. After scaling, the scale op watches with RolloutStatus exactly like deploy
// and restart (RolloutStatus returns promptly for replicas=0).
//
// Returns the captured kubectl output; an *exec.ExecError on failure. The
// kubeconfig/context/timeout from kopts are threaded through exactly as SetImage
// / RolloutRestart and the read-only wrappers do.
func Scale(ctx context.Context, kopts k8s.Options, ns, deployment string, replicas int, o ScaleOpts) (exec.RunResult, error) {
	run := o.Runner
	if run == nil {
		run = defaultRunner
	}
	args := ScaleArgs(kopts, ns, deployment, replicas)
	return run(ctx, "kubectl", args, runOpts(kopts))
}

// RolloutStatus runs `kubectl rollout status` for a deployment, blocking until
// the rollout completes (or the timeout fires). The rollout view calls this once
// per deployed deployment.
func RolloutStatus(ctx context.Context, kopts k8s.Options, ns, deployment string, o RolloutOpts) (exec.RunResult, error) {
	run := o.Runner
	if run == nil {
		run = defaultRunner
	}
	args := RolloutStatusArgs(kopts, ns, deployment, o.Timeout)
	return run(ctx, "kubectl", args, runOpts(kopts))
}

// runOpts builds the exec options (KUBECONFIG env + per-command timeout) from
// k8s.Options, matching the read-only wrappers so a mutation honours the same
// kubeconfig/timeout the views use.
func runOpts(kopts k8s.Options) exec.RunOptions {
	ro := exec.RunOptions{Timeout: kopts.Timeout}
	if kopts.Kubeconfig != "" {
		ro.Env = []string{"KUBECONFIG=" + kopts.Kubeconfig}
	}
	return ro
}
