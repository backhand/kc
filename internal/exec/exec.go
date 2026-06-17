// Package exec is the one place kc reaches the outside world.
//
// Everything (kubectl / gh / git) goes through here: spawn a process, capture
// stdout/stderr, enforce a timeout, and surface typed errors. kc is a portable
// orchestrator (see SPEC.md) — no native clients, just well-behaved shell-outs.
// argv is passed as a slice and handed straight to os/exec (never a shell
// string), so there is no shell-injection surface.
//
// Ported from tools/kc-bun/src/exec.ts.
package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// DefaultTimeout is the deadline applied when RunOptions.Timeout is zero.
const DefaultTimeout = 15 * time.Second

// killGrace is how long a process gets after SIGTERM before we escalate to
// SIGKILL on timeout (matches the TS reference's 2s grace period).
const killGrace = 2 * time.Second

// ExecError is returned when a spawned command fails: non-zero exit, timeout,
// or a launch failure (e.g. binary not on PATH).
type ExecError struct {
	Command string
	Args    []string
	// Code is the process exit code, or -1 if it never produced one
	// (killed / launch failure). TimedOut disambiguates the kill case.
	Code     int
	Stdout   string
	Stderr   string
	TimedOut bool
	// Msg overrides the default message when set (e.g. launch failure).
	Msg string
}

func (e *ExecError) Error() string {
	base := e.Msg
	if base == "" {
		if e.TimedOut {
			base = fmt.Sprintf("`%s` timed out", e.Command)
		} else {
			base = fmt.Sprintf("`%s` exited with code %d", e.Command, e.Code)
		}
	}
	// Append a trimmed stderr tail so the error is self-describing in logs.
	if tail := stderrTail(e.Stderr); tail != "" {
		return base + ": " + tail
	}
	return base
}

// stderrTail returns the last up-to-3 non-empty-trimmed lines of stderr.
func stderrTail(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, "\n")
}

// JsonParseError is returned when a command's stdout was expected to be JSON
// but did not parse.
type JsonParseError struct {
	Command string
	Stdout  string
	Cause   error
}

func (e *JsonParseError) Error() string {
	return fmt.Sprintf("failed to parse JSON from `%s`: %v", e.Command, e.Cause)
}

func (e *JsonParseError) Unwrap() error { return e.Cause }

// RunOptions configures a single Run / RunJSON invocation.
type RunOptions struct {
	// Timeout before the process is killed. Zero means DefaultTimeout.
	Timeout time.Duration
	// Env are extra variables layered on top of the current process env
	// (KEY=VALUE form). A later entry overrides an earlier one.
	Env []string
	// Dir is the working directory for the spawned process (empty = inherit).
	Dir string
	// AllowNonZero, when true, means a non-zero exit does NOT return an
	// ExecError — the caller inspects RunResult.Code. Useful for probes
	// (e.g. "does this resource exist?").
	AllowNonZero bool
}

// RunResult is the captured outcome of a successful (or AllowNonZero) Run.
type RunResult struct {
	Code   int
	Stdout string
	Stderr string
}

// Run spawns `command args…`, captures output, and enforces a timeout.
//
// On timeout we send SIGTERM, then SIGKILL after a short grace period: a child
// that ignores SIGTERM (e.g. a wedged credential helper) would otherwise keep
// its piped stdout/stderr open and hang the wait well past the deadline.
// SIGKILL is uncatchable, so it guarantees the streams close.
//
// By default a non-zero exit (or a timeout, or a launch failure) returns an
// *ExecError; set RunOptions.AllowNonZero to handle the exit code yourself.
func Run(ctx context.Context, command string, args []string, opts RunOptions) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// Own deadline context so we can distinguish "we timed out" from a parent
	// cancellation, and so SIGKILL escalation is under our control (Cancel +
	// WaitDelay below) rather than os/exec's default immediate kill.
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, command, args...)
	cmd.Dir = opts.Dir
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	cmd.Stdin = nil

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	// On context expiry, CommandContext invokes Cancel (we send SIGTERM);
	// WaitDelay then forces a SIGKILL after the grace period if the child
	// lingers with its pipes still open. SIGKILL is uncatchable, so the
	// deadline is honoured even for a SIGTERM-ignoring child.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace

	if err := cmd.Start(); err != nil {
		// Spawn itself failed (e.g. binary not on PATH).
		return RunResult{Code: -1}, &ExecError{
			Command:  command,
			Args:     args,
			Code:     -1,
			Stderr:   err.Error(),
			TimedOut: false,
			Msg:      fmt.Sprintf("failed to launch `%s` (is it on PATH?)", command),
		}
	}

	waitErr := cmd.Wait()
	stdout := outBuf.String()
	stderr := errBuf.String()
	code := cmd.ProcessState.ExitCode()

	timedOut := cctx.Err() == context.DeadlineExceeded
	if timedOut {
		return RunResult{Code: code, Stdout: stdout, Stderr: stderr}, &ExecError{
			Command:  command,
			Args:     args,
			Code:     code,
			Stdout:   stdout,
			Stderr:   stderr,
			TimedOut: true,
		}
	}

	if waitErr != nil && code != 0 && !opts.AllowNonZero {
		return RunResult{Code: code, Stdout: stdout, Stderr: stderr}, &ExecError{
			Command:  command,
			Args:     args,
			Code:     code,
			Stdout:   stdout,
			Stderr:   stderr,
			TimedOut: false,
		}
	}
	if waitErr != nil && code == 0 {
		// A non-ExitError failure that wasn't a timeout (rare: I/O on the
		// pipes, WaitDelay kill without a deadline, etc). Surface it.
		return RunResult{Code: code, Stdout: stdout, Stderr: stderr}, &ExecError{
			Command: command,
			Args:    args,
			Code:    code,
			Stdout:  stdout,
			Stderr:  stderr,
			Msg:     fmt.Sprintf("`%s` failed: %v", command, waitErr),
		}
	}

	return RunResult{Code: code, Stdout: stdout, Stderr: stderr}, nil
}

// RunJSON is like Run, but decodes stdout as JSON into *out.
//
// Returns an *ExecError on a failed command and a *JsonParseError when stdout
// is not valid JSON. (No schema validation — callers narrow the shape; the
// typed wrappers in k8s/ and github/ own that.)
func RunJSON(ctx context.Context, command string, args []string, opts RunOptions, out any) error {
	res, err := Run(ctx, command, args, opts)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(res.Stdout), out); err != nil {
		return &JsonParseError{
			Command: command + " " + strings.Join(args, " "),
			Stdout:  res.Stdout,
			Cause:   err,
		}
	}
	return nil
}

// AsExecError reports whether err is (or wraps) an *ExecError, returning it.
func AsExecError(err error) (*ExecError, bool) {
	var e *ExecError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsExecError reports whether err is (or wraps) an *ExecError.
func IsExecError(err error) bool {
	_, ok := AsExecError(err)
	return ok
}
