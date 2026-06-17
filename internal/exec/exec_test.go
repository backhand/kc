package exec

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These tests use tiny, universally-present commands (sh/printf/false) so they
// stay deterministic and offline — no kubectl/gh/git.
// Ported from tools/kc-bun/test/exec.test.ts.

func TestRun_CapturesStdoutAndExitZero(t *testing.T) {
	r, err := Run(context.Background(), "printf", []string{"hello"}, RunOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Code != 0 {
		t.Errorf("code = %d, want 0", r.Code)
	}
	if r.Stdout != "hello" {
		t.Errorf("stdout = %q, want %q", r.Stdout, "hello")
	}
}

func TestRun_NonZeroExitReturnsExecError(t *testing.T) {
	_, err := Run(context.Background(), "sh", []string{"-c", "echo boom 1>&2; exit 3"}, RunOptions{})
	ee, ok := AsExecError(err)
	if !ok {
		t.Fatalf("expected ExecError, got %T: %v", err, err)
	}
	if ee.Code != 3 {
		t.Errorf("code = %d, want 3", ee.Code)
	}
	if !contains(ee.Stderr, "boom") {
		t.Errorf("stderr %q does not contain boom", ee.Stderr)
	}
	if ee.TimedOut {
		t.Error("timedOut = true, want false")
	}
	// The stderr tail should be folded into the message.
	if !contains(ee.Error(), "boom") {
		t.Errorf("error message %q omits stderr tail", ee.Error())
	}
}

func TestRun_AllowNonZeroReturnsCode(t *testing.T) {
	r, err := Run(context.Background(), "sh", []string{"-c", "exit 7"}, RunOptions{AllowNonZero: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Code != 7 {
		t.Errorf("code = %d, want 7", r.Code)
	}
}

func TestRun_TimeoutKillsAndFlagsTimedOut(t *testing.T) {
	_, err := Run(context.Background(), "sh", []string{"-c", "sleep 5"}, RunOptions{Timeout: 50 * time.Millisecond})
	ee, ok := AsExecError(err)
	if !ok {
		t.Fatalf("expected ExecError, got %T: %v", err, err)
	}
	if !ee.TimedOut {
		t.Error("timedOut = false, want true")
	}
}

func TestRun_TimeoutEscalatesToSIGKILL(t *testing.T) {
	// Trap and ignore SIGTERM, then loop. A SIGTERM-only kill would hang the
	// wait forever; the WaitDelay SIGKILL escalation must still end it.
	start := time.Now()
	_, err := Run(context.Background(), "sh",
		[]string{"-c", "trap '' TERM; while true; do sleep 1; done"},
		RunOptions{Timeout: 50 * time.Millisecond})
	ee, ok := AsExecError(err)
	if !ok {
		t.Fatalf("expected ExecError, got %T: %v", err, err)
	}
	if !ee.TimedOut {
		t.Error("timedOut = false, want true")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v, expected resolution within the grace window", elapsed)
	}
}

func TestRun_MissingBinaryReturnsExecError(t *testing.T) {
	_, err := Run(context.Background(), "kc-definitely-not-a-real-binary-xyz", nil, RunOptions{})
	ee, ok := AsExecError(err)
	if !ok {
		t.Fatalf("expected ExecError, got %T: %v", err, err)
	}
	if ee.Code != -1 {
		t.Errorf("code = %d, want -1 for a launch failure", ee.Code)
	}
}

func TestRun_EnvOverridePassedThrough(t *testing.T) {
	r, err := Run(context.Background(), "sh", []string{"-c", `printf %s "$KC_TEST_VAR"`},
		RunOptions{Env: []string{"KC_TEST_VAR=xyzzy"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Stdout != "xyzzy" {
		t.Errorf("stdout = %q, want xyzzy", r.Stdout)
	}
}

func TestRun_ParentContextCancellationNotFlaggedAsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := Run(ctx, "sh", []string{"-c", "sleep 5"}, RunOptions{Timeout: 5 * time.Second})
	ee, ok := AsExecError(err)
	if !ok {
		t.Fatalf("expected ExecError, got %T: %v", err, err)
	}
	// Parent cancellation is not our deadline → not flagged as timedOut.
	if ee.TimedOut {
		t.Error("parent cancellation should not be reported as a timeout")
	}
}

func TestRunJSON_ParsesStdout(t *testing.T) {
	var out struct {
		A int      `json:"a"`
		B []string `json:"b"`
	}
	if err := RunJSON(context.Background(), "printf", []string{`{"a":1,"b":["x"]}`}, RunOptions{}, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.A != 1 || len(out.B) != 1 || out.B[0] != "x" {
		t.Errorf("decoded = %+v, want {A:1 B:[x]}", out)
	}
}

func TestRunJSON_InvalidJSONReturnsJsonParseError(t *testing.T) {
	var out map[string]any
	err := RunJSON(context.Background(), "printf", []string{"not json"}, RunOptions{}, &out)
	var jpe *JsonParseError
	if !errors.As(err, &jpe) {
		t.Fatalf("expected *JsonParseError, got %T: %v", err, err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
