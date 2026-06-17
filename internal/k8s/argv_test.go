package k8s

import (
	"reflect"
	"testing"
	"time"
)

// Unit tests for the pure interactive/streaming argv builders (logs, exec). The
// `l` logs and `s` shell ops hand the terminal to kubectl via tea.ExecProcess;
// the data layer only owns this argv assembly, so the EXACT command is asserted
// here without spawning anything (SPEC safety: never open a real shell / stream
// `-f` in a test).

func TestLogsArgs(t *testing.T) {
	// Pod target, follow stream, default tail (the `l` op in the pods view).
	got := LogsArgs(Options{}, "mailon", "pod/responder-aaa", 200, true)
	want := []string{"-n", "mailon", "logs", "pod/responder-aaa", "--all-containers", "--tail=200", "-f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LogsArgs (pod, follow) =\n  %v\nwant\n  %v", got, want)
	}

	// Deployment target, with a kube-context prepended (the `l` op in the
	// namespace view).
	got = LogsArgs(Options{Context: "k3s"}, "mailon", "deployment/responder", 200, true)
	want = []string{"--context", "k3s", "-n", "mailon", "logs", "deployment/responder", "--all-containers", "--tail=200", "-f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LogsArgs (deployment, context) =\n  %v\nwant\n  %v", got, want)
	}

	// No-follow probe (the safe read-only path-check the reviewer runs live):
	// `--tail=1`, NO `-f`.
	got = LogsArgs(Options{}, "mailon", "pod/responder-aaa", 1, false)
	want = []string{"-n", "mailon", "logs", "pod/responder-aaa", "--all-containers", "--tail=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LogsArgs (probe, no-follow) =\n  %v\nwant\n  %v", got, want)
	}
	if contains(got, "-f") {
		t.Error("the no-follow probe must NOT include -f")
	}

	// tail < 0 omits the flag entirely (kubectl default backlog).
	got = LogsArgs(Options{}, "mailon", "deployment/responder", -1, false)
	want = []string{"-n", "mailon", "logs", "deployment/responder", "--all-containers"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LogsArgs (no tail) =\n  %v\nwant\n  %v", got, want)
	}
}

func TestExecArgs(t *testing.T) {
	// Default command → `sh`; -it for an interactive TTY (the `s` op).
	got := ExecArgs(Options{}, "mailon", "responder-aaa")
	want := []string{"-n", "mailon", "exec", "-it", "responder-aaa", "--", "sh"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExecArgs (default sh) =\n  %v\nwant\n  %v", got, want)
	}

	// Context prepended; explicit multi-token command after the `--` separator.
	got = ExecArgs(Options{Context: "k3s"}, "mailon", "responder-aaa", "bash", "-l")
	want = []string{"--context", "k3s", "-n", "mailon", "exec", "-it", "responder-aaa", "--", "bash", "-l"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExecArgs (context, bash -l) =\n  %v\nwant\n  %v", got, want)
	}
}

func TestExecOptions_ThreadsKubeconfig(t *testing.T) {
	ro := Options{Kubeconfig: "/tmp/kc", Timeout: 7 * time.Second}.ExecOptions()
	if ro.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s", ro.Timeout)
	}
	if len(ro.Env) != 1 || ro.Env[0] != "KUBECONFIG=/tmp/kc" {
		t.Errorf("env = %v, want [KUBECONFIG=/tmp/kc]", ro.Env)
	}
}
