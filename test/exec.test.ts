/**
 * Unit tests for the shell-out helper. Uses tiny, universally-present commands
 * (sh/printf/false) so it stays deterministic and offline — no kubectl/gh/git.
 */

import { test, expect, describe } from "bun:test"
import { run, runJson, ExecError, JsonParseError, isExecError } from "../src/exec.ts"

describe("run", () => {
  test("captures stdout and exit code 0", async () => {
    const r = await run("printf", ["hello"])
    expect(r.code).toBe(0)
    expect(r.stdout).toBe("hello")
  })

  test("non-zero exit throws an ExecError with the code", async () => {
    let caught: unknown
    try {
      await run("sh", ["-c", "echo boom 1>&2; exit 3"])
    } catch (e) {
      caught = e
    }
    expect(isExecError(caught)).toBe(true)
    const err = caught as ExecError
    expect(err.code).toBe(3)
    expect(err.stderr).toContain("boom")
    expect(err.timedOut).toBe(false)
  })

  test("throwOnError:false returns the non-zero code instead of throwing", async () => {
    const r = await run("sh", ["-c", "exit 7"], { throwOnError: false })
    expect(r.code).toBe(7)
  })

  test("timeout kills the process and flags timedOut", async () => {
    let caught: unknown
    try {
      await run("sh", ["-c", "sleep 5"], { timeoutMs: 50 })
    } catch (e) {
      caught = e
    }
    expect(isExecError(caught)).toBe(true)
    expect((caught as ExecError).timedOut).toBe(true)
  })

  test("timeout still fires for a SIGTERM-ignoring child (SIGKILL escalation)", async () => {
    // Trap and ignore SIGTERM, then loop. A SIGTERM-only kill would hang the
    // await forever; SIGKILL escalation must still end it. Allow generous time
    // for the 2s grace period (default timeout is 120s, so we won't hit it).
    let caught: unknown
    const started = Date.now()
    try {
      await run("sh", ["-c", "trap '' TERM; while true; do sleep 1; done"], {
        timeoutMs: 50,
      })
    } catch (e) {
      caught = e
    }
    expect(isExecError(caught)).toBe(true)
    expect((caught as ExecError).timedOut).toBe(true)
    // Should resolve within the grace window, not hang indefinitely.
    expect(Date.now() - started).toBeLessThan(10_000)
  }, 15_000)

  test("missing binary → ExecError (no throw on spawn)", async () => {
    let caught: unknown
    try {
      await run("kc-definitely-not-a-real-binary-xyz", [])
    } catch (e) {
      caught = e
    }
    expect(isExecError(caught)).toBe(true)
    expect((caught as ExecError).code).toBeNull()
  })

  test("env override is passed through", async () => {
    const r = await run("sh", ["-c", "printf %s \"$KC_TEST_VAR\""], {
      env: { KC_TEST_VAR: "xyzzy" },
    })
    expect(r.stdout).toBe("xyzzy")
  })
})

describe("runJson", () => {
  test("parses JSON stdout", async () => {
    const out = await runJson<{ a: number; b: string[] }>("printf", ['{"a":1,"b":["x"]}'])
    expect(out).toEqual({ a: 1, b: ["x"] })
  })

  test("invalid JSON → JsonParseError", async () => {
    let caught: unknown
    try {
      await runJson("printf", ["not json"])
    } catch (e) {
      caught = e
    }
    expect(caught).toBeInstanceOf(JsonParseError)
  })
})
