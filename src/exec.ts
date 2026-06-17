/**
 * exec.ts — the one place kc reaches the outside world.
 *
 * Everything (kubectl / gh / git) goes through here: spawn a process, capture
 * stdout/stderr, enforce a timeout, and surface typed errors. `kc` is a
 * portable orchestrator (see SPEC.md) — no native clients, just well-behaved
 * shell-outs. Uses Bun's `spawn`; argv is passed as an array (never a shell
 * string), so there is no shell-injection surface.
 */

/** Thrown when a spawned command fails: non-zero exit, timeout, or not found. */
export class ExecError extends Error {
  readonly command: string
  readonly args: readonly string[]
  /** Process exit code, or null if it never produced one (e.g. killed/ENOENT). */
  readonly code: number | null
  readonly stdout: string
  readonly stderr: string
  /** True when the failure was the timeout killing the process. */
  readonly timedOut: boolean

  constructor(opts: {
    command: string
    args: readonly string[]
    code: number | null
    stdout: string
    stderr: string
    timedOut: boolean
    message?: string
  }) {
    const base =
      opts.message ??
      (opts.timedOut
        ? `\`${opts.command}\` timed out`
        : `\`${opts.command}\` exited with code ${opts.code ?? "?"}`)
    // Append a trimmed stderr tail so the error is self-describing in logs.
    const tail = opts.stderr.trim().split("\n").slice(-3).join("\n")
    super(tail ? `${base}: ${tail}` : base)
    this.name = "ExecError"
    this.command = opts.command
    this.args = opts.args
    this.code = opts.code
    this.stdout = opts.stdout
    this.stderr = opts.stderr
    this.timedOut = opts.timedOut
  }
}

/** Thrown when a command's stdout was expected to be JSON but did not parse. */
export class JsonParseError extends Error {
  readonly command: string
  readonly stdout: string
  constructor(command: string, stdout: string, cause: unknown) {
    super(`failed to parse JSON from \`${command}\`: ${(cause as Error)?.message ?? cause}`)
    this.name = "JsonParseError"
    this.command = command
    this.stdout = stdout
  }
}

export interface RunOptions {
  /** Milliseconds before the process is killed. Default 15000. */
  timeoutMs?: number
  /** Extra env on top of process.env. */
  env?: Record<string, string | undefined>
  /** Working directory for the spawned process. */
  cwd?: string
  /**
   * When false, a non-zero exit does NOT throw — the caller inspects `.code`.
   * Useful for probes (e.g. "does this resource exist?"). Default true.
   */
  throwOnError?: boolean
}

export interface RunResult {
  code: number
  stdout: string
  stderr: string
}

const DEFAULT_TIMEOUT_MS = 15_000

/**
 * Spawn `command argv…`, capture output, enforce a timeout.
 *
 * Resolves with `{ code, stdout, stderr }`. By default a non-zero exit (or a
 * timeout, or ENOENT) rejects with {@link ExecError}; pass `throwOnError:false`
 * to handle the exit code yourself.
 */
export async function run(
  command: string,
  args: readonly string[],
  opts: RunOptions = {},
): Promise<RunResult> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS
  const throwOnError = opts.throwOnError ?? true

  let proc: ReturnType<typeof Bun.spawn>
  try {
    proc = Bun.spawn([command, ...args], {
      cwd: opts.cwd,
      env: opts.env ? { ...process.env, ...opts.env } : process.env,
      stdout: "pipe",
      stderr: "pipe",
      stdin: "ignore",
    })
  } catch (cause) {
    // Spawn itself failed (e.g. binary not on PATH).
    throw new ExecError({
      command,
      args,
      code: null,
      stdout: "",
      stderr: String((cause as Error)?.message ?? cause),
      timedOut: false,
      message: `failed to launch \`${command}\` (is it on PATH?)`,
    })
  }

  // On timeout: SIGTERM, then SIGKILL after a short grace period. A child that
  // ignores SIGTERM (e.g. a wedged credential helper) would otherwise keep its
  // piped stdout/stderr open and leave `proc.exited` unresolved, hanging the
  // await below well past timeoutMs. SIGKILL is uncatchable, so it guarantees
  // the streams close and the deadline is honoured.
  let timedOut = false
  let killTimer: ReturnType<typeof setTimeout> | undefined
  const timer = setTimeout(() => {
    timedOut = true
    proc.kill("SIGTERM")
    killTimer = setTimeout(() => proc.kill("SIGKILL"), 2_000)
  }, timeoutMs)

  try {
    // With stdout/stderr "pipe", these are ReadableStreams at runtime; Bun's
    // type widens them to `number | ReadableStream`, so narrow explicitly.
    const out = proc.stdout as ReadableStream<Uint8Array>
    const err = proc.stderr as ReadableStream<Uint8Array>
    const [stdout, stderr, code] = await Promise.all([
      new Response(out).text(),
      new Response(err).text(),
      proc.exited,
    ])

    if (timedOut) {
      throw new ExecError({ command, args, code, stdout, stderr, timedOut: true })
    }
    if (code !== 0 && throwOnError) {
      throw new ExecError({ command, args, code, stdout, stderr, timedOut: false })
    }
    return { code, stdout, stderr }
  } finally {
    clearTimeout(timer)
    if (killTimer) clearTimeout(killTimer)
  }
}

/**
 * Like {@link run}, but parses stdout as JSON into `T`.
 *
 * Throws {@link ExecError} on a failed command and {@link JsonParseError} when
 * stdout is not valid JSON. (No runtime schema validation — callers narrow the
 * shape; the typed wrappers in k8s/ and github/ own that.)
 */
export async function runJson<T>(
  command: string,
  args: readonly string[],
  opts: RunOptions = {},
): Promise<T> {
  const { stdout } = await run(command, args, opts)
  try {
    return JSON.parse(stdout) as T
  } catch (cause) {
    throw new JsonParseError(`${command} ${args.join(" ")}`, stdout, cause)
  }
}

/** True if `err` is an {@link ExecError} (narrowing helper for callers). */
export function isExecError(err: unknown): err is ExecError {
  return err instanceof ExecError
}
