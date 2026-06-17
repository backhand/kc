package deploy

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/backhand/kc/internal/exec"
	"github.com/backhand/kc/internal/k8s"
)

// Unit tests for the ONLY mutating helper in kc. These assert the constructed
// kubectl argv via a MOCKED runner — NOTHING here spawns kubectl or touches a
// cluster (SPEC safety: never run a real `set image` while verifying).

// captureRunner records the (command, args, opts) it is called with and returns
// a canned result, so tests assert argv without executing anything.
type captureRunner struct {
	calls []capturedCall
	res   exec.RunResult
	err   error
}

type capturedCall struct {
	command string
	args    []string
	opts    exec.RunOptions
}

func (c *captureRunner) run(_ context.Context, command string, args []string, opts exec.RunOptions) (exec.RunResult, error) {
	c.calls = append(c.calls, capturedCall{command: command, args: args, opts: opts})
	return c.res, c.err
}

// contains reports whether ss includes want (used to assert a flag is/ isn't in
// the constructed argv).
func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ── Pure argv builders ────────────────────────────────────────────────────────

func TestContainerArg(t *testing.T) {
	if got := ContainerArg(""); got != "*" {
		t.Errorf("empty container = %q, want * (every container)", got)
	}
	if got := ContainerArg("  "); got != "*" {
		t.Errorf("blank container = %q, want *", got)
	}
	if got := ContainerArg("web"); got != "web" {
		t.Errorf("named container = %q, want web", got)
	}
	if got := ContainerArg("  web  "); got != "web" {
		t.Errorf("padded container = %q, want web (trimmed)", got)
	}
}

func TestSetImageArgs(t *testing.T) {
	// Named container, no context, no dry-run.
	got := SetImageArgs(k8s.Options{}, "mailon", "web", "web", "ghcr.io/thinkpilot/mailon:v0.6.10", false)
	want := []string{"-n", "mailon", "set", "image", "deployment/web", "web=ghcr.io/thinkpilot/mailon:v0.6.10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetImageArgs =\n  %v\nwant\n  %v", got, want)
	}

	// Empty container → "*" wildcard.
	got = SetImageArgs(k8s.Options{}, "mailon", "sender", "", "ghcr.io/thinkpilot/mailon:v0.6.10", false)
	want = []string{"-n", "mailon", "set", "image", "deployment/sender", "*=ghcr.io/thinkpilot/mailon:v0.6.10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wildcard SetImageArgs =\n  %v\nwant\n  %v", got, want)
	}

	// Dry-run appends --dry-run=server; context prepends --context.
	got = SetImageArgs(k8s.Options{Context: "k3s"}, "mailon", "web", "web", "img:tag", true)
	want = []string{"--context", "k3s", "-n", "mailon", "set", "image", "deployment/web", "web=img:tag", "--dry-run=server"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dry-run+context SetImageArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestScaleArgs(t *testing.T) {
	// No context: `-n <ns> scale deployment/<d> --replicas=<N>`.
	got := ScaleArgs(k8s.Options{}, "mailon", "responder", 3)
	want := []string{"-n", "mailon", "scale", "deployment/responder", "--replicas=3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ScaleArgs =\n  %v\nwant\n  %v", got, want)
	}

	// replicas=0 is an explicit pause (scale-to-zero): the argv is --replicas=0.
	got = ScaleArgs(k8s.Options{}, "mailon", "responder", 0)
	want = []string{"-n", "mailon", "scale", "deployment/responder", "--replicas=0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scale-to-zero ScaleArgs =\n  %v\nwant\n  %v", got, want)
	}

	// Context prepends --context (mirrors the other builders' shape).
	got = ScaleArgs(k8s.Options{Context: "k3s"}, "mailon", "sender", 5)
	want = []string{"--context", "k3s", "-n", "mailon", "scale", "deployment/sender", "--replicas=5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("context ScaleArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestRolloutStatusArgs(t *testing.T) {
	got := RolloutStatusArgs(k8s.Options{}, "mailon", "web", 0)
	want := []string{"-n", "mailon", "rollout", "status", "deployment/web"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RolloutStatusArgs =\n  %v\nwant\n  %v", got, want)
	}

	got = RolloutStatusArgs(k8s.Options{Context: "k3s"}, "mailon", "web", 90*time.Second)
	want = []string{"--context", "k3s", "-n", "mailon", "rollout", "status", "deployment/web", "--timeout=1m30s"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RolloutStatusArgs w/ timeout+context =\n  %v\nwant\n  %v", got, want)
	}
}

func TestRolloutRestartArgs(t *testing.T) {
	// No context, no dry-run.
	got := RolloutRestartArgs(k8s.Options{}, "mailon", "responder", false)
	want := []string{"-n", "mailon", "rollout", "restart", "deployment/responder"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RolloutRestartArgs =\n  %v\nwant\n  %v", got, want)
	}

	// Dry-run appends --dry-run=server; context prepends --context (mirrors
	// SetImageArgs' shape — the safe live touch the reviewer uses).
	got = RolloutRestartArgs(k8s.Options{Context: "k3s"}, "mailon", "responder", true)
	want = []string{"--context", "k3s", "-n", "mailon", "rollout", "restart", "deployment/responder", "--dry-run=server"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dry-run+context RolloutRestartArgs =\n  %v\nwant\n  %v", got, want)
	}
}

// ── SetImage / RolloutRestart / RolloutStatus via mocked runner (NO cluster) ───

func TestSetImage_InvokesKubectlWithCorrectArgv(t *testing.T) {
	cap := &captureRunner{res: exec.RunResult{Stdout: "deployment.apps/web image updated"}}
	_, err := SetImage(context.Background(), k8s.Options{Kubeconfig: "/tmp/kc"}, "mailon", "web", "web",
		"ghcr.io/thinkpilot/mailon:v0.6.10", SetImageOpts{Runner: cap.run})
	if err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 kubectl call, got %d", len(cap.calls))
	}
	c := cap.calls[0]
	if c.command != "kubectl" {
		t.Errorf("command = %q, want kubectl", c.command)
	}
	want := []string{"-n", "mailon", "set", "image", "deployment/web", "web=ghcr.io/thinkpilot/mailon:v0.6.10"}
	if !reflect.DeepEqual(c.args, want) {
		t.Errorf("argv =\n  %v\nwant\n  %v", c.args, want)
	}
	// KUBECONFIG threaded into the exec env.
	if len(c.opts.Env) != 1 || c.opts.Env[0] != "KUBECONFIG=/tmp/kc" {
		t.Errorf("env = %v, want [KUBECONFIG=/tmp/kc]", c.opts.Env)
	}
}

func TestSetImage_DryRunArgv(t *testing.T) {
	cap := &captureRunner{}
	_, err := SetImage(context.Background(), k8s.Options{}, "mailon", "web", "", "img:tag",
		SetImageOpts{DryRun: true, Runner: cap.run})
	if err != nil {
		t.Fatalf("SetImage dry-run: %v", err)
	}
	got := cap.calls[0].args
	want := []string{"-n", "mailon", "set", "image", "deployment/web", "*=img:tag", "--dry-run=server"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dry-run argv =\n  %v\nwant\n  %v", got, want)
	}
}

func TestRolloutRestart_InvokesKubectlWithCorrectArgv(t *testing.T) {
	cap := &captureRunner{res: exec.RunResult{Stdout: "deployment.apps/responder restarted"}}
	_, err := RolloutRestart(context.Background(), k8s.Options{Kubeconfig: "/tmp/kc"}, "mailon", "responder",
		RestartOpts{Runner: cap.run})
	if err != nil {
		t.Fatalf("RolloutRestart: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 kubectl call, got %d", len(cap.calls))
	}
	c := cap.calls[0]
	if c.command != "kubectl" {
		t.Errorf("command = %q, want kubectl", c.command)
	}
	want := []string{"-n", "mailon", "rollout", "restart", "deployment/responder"}
	if !reflect.DeepEqual(c.args, want) {
		t.Errorf("argv =\n  %v\nwant\n  %v", c.args, want)
	}
	// Not a dry-run by default — the real restart.
	if contains(c.args, "--dry-run=server") {
		t.Error("default RolloutRestart must NOT be a dry-run")
	}
	// KUBECONFIG threaded into the exec env (same as SetImage).
	if len(c.opts.Env) != 1 || c.opts.Env[0] != "KUBECONFIG=/tmp/kc" {
		t.Errorf("env = %v, want [KUBECONFIG=/tmp/kc]", c.opts.Env)
	}
}

func TestRolloutRestart_DryRunArgv(t *testing.T) {
	cap := &captureRunner{}
	_, err := RolloutRestart(context.Background(), k8s.Options{}, "mailon", "responder",
		RestartOpts{DryRun: true, Runner: cap.run})
	if err != nil {
		t.Fatalf("RolloutRestart dry-run: %v", err)
	}
	got := cap.calls[0].args
	want := []string{"-n", "mailon", "rollout", "restart", "deployment/responder", "--dry-run=server"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dry-run argv =\n  %v\nwant\n  %v", got, want)
	}
}

func TestScale_InvokesKubectlWithCorrectArgv(t *testing.T) {
	cap := &captureRunner{res: exec.RunResult{Stdout: "deployment.apps/responder scaled"}}
	_, err := Scale(context.Background(), k8s.Options{Kubeconfig: "/tmp/kc"}, "mailon", "responder", 3,
		ScaleOpts{Runner: cap.run})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 kubectl call, got %d", len(cap.calls))
	}
	c := cap.calls[0]
	if c.command != "kubectl" {
		t.Errorf("command = %q, want kubectl", c.command)
	}
	want := []string{"-n", "mailon", "scale", "deployment/responder", "--replicas=3"}
	if !reflect.DeepEqual(c.args, want) {
		t.Errorf("argv =\n  %v\nwant\n  %v", c.args, want)
	}
	// KUBECONFIG threaded into the exec env (same as SetImage / RolloutRestart).
	if len(c.opts.Env) != 1 || c.opts.Env[0] != "KUBECONFIG=/tmp/kc" {
		t.Errorf("env = %v, want [KUBECONFIG=/tmp/kc]", c.opts.Env)
	}
}

func TestScale_ToZeroArgv(t *testing.T) {
	cap := &captureRunner{}
	_, err := Scale(context.Background(), k8s.Options{}, "mailon", "responder", 0, ScaleOpts{Runner: cap.run})
	if err != nil {
		t.Fatalf("Scale to zero: %v", err)
	}
	got := cap.calls[0].args
	want := []string{"-n", "mailon", "scale", "deployment/responder", "--replicas=0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scale-to-zero argv =\n  %v\nwant\n  %v", got, want)
	}
}

func TestRolloutStatus_InvokesKubectl(t *testing.T) {
	cap := &captureRunner{res: exec.RunResult{Stdout: "deployment \"web\" successfully rolled out\n"}}
	res, err := RolloutStatus(context.Background(), k8s.Options{}, "mailon", "web",
		RolloutOpts{Timeout: time.Minute, Runner: cap.run})
	if err != nil {
		t.Fatalf("RolloutStatus: %v", err)
	}
	if res.Stdout == "" {
		t.Error("expected the canned stdout to pass through")
	}
	want := []string{"-n", "mailon", "rollout", "status", "deployment/web", "--timeout=1m0s"}
	if !reflect.DeepEqual(cap.calls[0].args, want) {
		t.Errorf("argv =\n  %v\nwant\n  %v", cap.calls[0].args, want)
	}
}

// ── DeriveRepo ─────────────────────────────────────────────────────────────────

func TestDeriveRepo(t *testing.T) {
	deps := []k8s.Deployment{
		{Name: "web", Images: []k8s.ImageRef{{Repository: "ghcr.io/thinkpilot/mailon", Tag: "v0.6.9"}}},
	}
	ref, ok := DeriveRepo(deps)
	if !ok {
		t.Fatal("expected a repo to be derived from the ghcr.io image")
	}
	if ref.Owner != "thinkpilot" || ref.Repo != "mailon" {
		t.Errorf("derived %+v, want {thinkpilot mailon}", ref)
	}
}

func TestDeriveRepo_SkipsNonGHCRUntilMatch(t *testing.T) {
	deps := []k8s.Deployment{
		{Name: "redis", Images: []k8s.ImageRef{{Repository: "docker.io/library/redis", Tag: "7"}}},
		{Name: "web", Images: []k8s.ImageRef{{Repository: "ghcr.io/thinkpilot/consistant", Tag: "v1.2.3"}}},
	}
	ref, ok := DeriveRepo(deps)
	if !ok || ref.Owner != "thinkpilot" || ref.Repo != "consistant" {
		t.Errorf("DeriveRepo = (%+v, %v), want {thinkpilot consistant}, true", ref, ok)
	}
}

func TestDeriveRepo_NoGHCRImage(t *testing.T) {
	deps := []k8s.Deployment{
		{Name: "redis", Images: []k8s.ImageRef{{Repository: "docker.io/library/redis", Tag: "7"}}},
	}
	if _, ok := DeriveRepo(deps); ok {
		t.Error("expected ok=false when no ghcr.io image is present")
	}
}

// ── PlanChanges ────────────────────────────────────────────────────────────────

func TestPlanChanges_SingleContainerUsesWildcard(t *testing.T) {
	deps := []k8s.Deployment{
		{Namespace: "mailon", Name: "sender", Images: []k8s.ImageRef{
			{Name: "sender", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v0.6.9"},
		}},
	}
	changes := PlanChanges(deps, []string{"sender"}, "ghcr.io/thinkpilot/mailon", "v0.6.10")
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	c := changes[0]
	// Single container → empty Container (the "*" wildcard at apply time).
	if c.Container != "" {
		t.Errorf("container = %q, want empty (wildcard) for a single-container deployment", c.Container)
	}
	if c.FromTag != "v0.6.9" || c.ToTag != "v0.6.10" {
		t.Errorf("from→to = %s→%s, want v0.6.9→v0.6.10", c.FromTag, c.ToTag)
	}
	if c.Image != "ghcr.io/thinkpilot/mailon:v0.6.10" {
		t.Errorf("image = %q, want ghcr.io/thinkpilot/mailon:v0.6.10", c.Image)
	}
}

func TestPlanChanges_MultiContainerTargetsMatchingContainer(t *testing.T) {
	// A deployment with an app container + a sidecar: only the matching app
	// container (by repo) must be targeted by NAME, so the sidecar is untouched.
	deps := []k8s.Deployment{
		{Namespace: "mailon", Name: "web", Images: []k8s.ImageRef{
			{Name: "app", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v0.6.9"},
			{Name: "cloud-sql-proxy", Repository: "gcr.io/cloudsql-docker/gce-proxy", Tag: "1.33"},
		}},
	}
	changes := PlanChanges(deps, []string{"web"}, "ghcr.io/thinkpilot/mailon", "v0.6.10")
	c := changes[0]
	if c.Container != "app" {
		t.Errorf("container = %q, want app (the matching container, not the sidecar)", c.Container)
	}
	if c.Image != "ghcr.io/thinkpilot/mailon:v0.6.10" {
		t.Errorf("image = %q, want ghcr.io/thinkpilot/mailon:v0.6.10", c.Image)
	}
}

func TestPlanChanges_SortedAndSkipsUnknown(t *testing.T) {
	deps := []k8s.Deployment{
		{Namespace: "mailon", Name: "web", Images: []k8s.ImageRef{{Name: "web", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v1"}}},
		{Namespace: "mailon", Name: "sender", Images: []k8s.ImageRef{{Name: "sender", Repository: "ghcr.io/thinkpilot/mailon", Tag: "v1"}}},
	}
	changes := PlanChanges(deps, []string{"web", "sender", "ghost"}, "ghcr.io/thinkpilot/mailon", "v2")
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2 (unknown 'ghost' skipped)", len(changes))
	}
	if changes[0].Deployment != "sender" || changes[1].Deployment != "web" {
		t.Errorf("order = [%s %s], want [sender web] (sorted)", changes[0].Deployment, changes[1].Deployment)
	}
}

func TestChange_NoOp(t *testing.T) {
	if !(Change{FromTag: "v1", ToTag: "v1"}).NoOp() {
		t.Error("same from/to should be a no-op")
	}
	if (Change{FromTag: "v1", ToTag: "v2"}).NoOp() {
		t.Error("different from/to is not a no-op")
	}
}
