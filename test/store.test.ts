/**
 * Unit tests for the learning store: canonicalisation, recency-weighted ranking,
 * deploy presets, and a round-trip to a TEMP dir.
 *
 * Every test injects an explicit temp baseDir — real `~/.kc` is never written.
 */

import { test, expect, describe, beforeEach, afterEach } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir, homedir } from "node:os"
import { join } from "node:path"
import {
  ActionHistory,
  rankParams,
  canonicalKey,
  normalizeSet,
} from "../src/store/index.ts"
import type { ActionRecord, Scope } from "../src/types.ts"

const SCOPE: Scope = { cluster: "thinkpilot-k3s", app: "mailon" }

// ── Pure helpers ────────────────────────────────────────────────────────────

describe("canonicalKey", () => {
  test("key order independent", () => {
    expect(canonicalKey({ a: 1, b: 2 })).toBe(canonicalKey({ b: 2, a: 1 }))
  })
  test("nested objects/arrays normalised", () => {
    expect(canonicalKey({ x: { p: 1, q: 2 }, list: [1, 2] })).toBe(
      canonicalKey({ list: [1, 2], x: { q: 2, p: 1 } }),
    )
  })
  test("distinct values differ", () => {
    expect(canonicalKey({ deployments: ["web"] })).not.toBe(
      canonicalKey({ deployments: ["web", "sender"] }),
    )
  })
})

describe("normalizeSet", () => {
  test("dedupes, trims, sorts", () => {
    expect(normalizeSet([" web ", "sender", "web", "", "sender"])).toEqual(["sender", "web"])
  })
})

describe("rankParams (recency-weighted frequency)", () => {
  const now = 1_000_000_000_000
  const day = 24 * 60 * 60 * 1000

  test("more frequent params rank higher", () => {
    const records: ActionRecord[] = [
      { action: "deploy", params: { d: ["web"] }, ts: now - day },
      { action: "deploy", params: { d: ["web"] }, ts: now - 2 * day },
      { action: "deploy", params: { d: ["sender"] }, ts: now - day },
    ]
    const ranked = rankParams<{ d: string[] }>(records, { now })
    expect(ranked[0]!.d).toEqual(["web"])
    expect(ranked.length).toBe(2)
  })

  test("recency outweighs a single old occurrence with a short half-life", () => {
    const records: ActionRecord[] = [
      // 'old' deployed once long ago.
      { action: "deploy", params: { d: ["old"] }, ts: now - 60 * day },
      // 'fresh' deployed once just now.
      { action: "deploy", params: { d: ["fresh"] }, ts: now },
    ]
    const ranked = rankParams<{ d: string[] }>(records, { now, halfLifeMs: day })
    expect(ranked[0]!.d).toEqual(["fresh"])
  })

  test("ties broken by most-recent occurrence", () => {
    const records: ActionRecord[] = [
      { action: "deploy", params: { d: ["a"] }, ts: now - 10 * day },
      { action: "deploy", params: { d: ["b"] }, ts: now - 1 * day },
    ]
    // With a very long half-life both score ~1; the more recent ('b') wins.
    const ranked = rankParams<{ d: string[] }>(records, {
      now,
      halfLifeMs: 10_000 * day,
    })
    expect(ranked[0]!.d).toEqual(["b"])
  })
})

// ── Persistence round-trip (TEMP dir) ───────────────────────────────────────

describe("ActionHistory persistence", () => {
  let dir: string

  beforeEach(async () => {
    dir = await mkdtemp(join(tmpdir(), "kc-store-test-"))
  })
  afterEach(async () => {
    await rm(dir, { recursive: true, force: true })
  })

  test("never resolves to the real ~/.kc when baseDir is injected", () => {
    const store = new ActionHistory({ baseDir: dir })
    expect(store.path.startsWith(dir)).toBe(true)
    expect(store.path).not.toContain(join(homedir(), ".kc"))
  })

  test("round-trips records to disk and reloads them in a fresh instance", async () => {
    const t0 = 1_700_000_000_000
    const write = new ActionHistory({ baseDir: dir, now: () => t0 })
    await write.record("deploy", SCOPE, { deployments: ["web"] })

    // The state file exists under the temp dir.
    expect(await Bun.file(write.path).exists()).toBe(true)

    // A brand-new instance reads the same persisted state.
    const read = new ActionHistory({ baseDir: dir })
    const ranked = await read.ranked("deploy", SCOPE)
    expect(ranked).toEqual([{ deployments: ["web"] }])
  })

  test("scopes are isolated by cluster × app", async () => {
    const store = new ActionHistory({ baseDir: dir })
    await store.recordDeploy({ cluster: "c1", app: "mailon" }, ["web"])
    await store.recordDeploy({ cluster: "c1", app: "other" }, ["api"])
    await store.recordDeploy({ cluster: "c2", app: "mailon" }, ["sender"])

    expect(await store.deployPresets({ cluster: "c1", app: "mailon" })).toEqual([["web"]])
    expect(await store.deployPresets({ cluster: "c1", app: "other" })).toEqual([["api"]])
    expect(await store.deployPresets({ cluster: "c2", app: "mailon" })).toEqual([["sender"]])
  })

  test("missing state file → empty rankings (no crash)", async () => {
    const store = new ActionHistory({ baseDir: join(dir, "does-not-exist-yet") })
    expect(await store.ranked("deploy", SCOPE)).toEqual([])
    expect(await store.deployPresets(SCOPE)).toEqual([])
  })

  test("corrupt state file → starts clean", async () => {
    await Bun.write(join(dir, "state.json"), "{ not valid json ")
    const store = new ActionHistory({ baseDir: dir })
    expect(await store.ranked("deploy", SCOPE)).toEqual([])
  })
})

// ── Deploy presets — the spec's headline scenario ───────────────────────────

describe("deploy presets (SPEC scenario)", () => {
  let dir: string
  beforeEach(async () => {
    dir = await mkdtemp(join(tmpdir(), "kc-presets-"))
  })
  afterEach(async () => {
    await rm(dir, { recursive: true, force: true })
  })

  test("record [web] then [responder,sender] → two presets, most-recent first", async () => {
    let clock = 1_700_000_000_000
    const store = new ActionHistory({ baseDir: dir, now: () => clock })

    clock += 1000
    await store.recordDeploy(SCOPE, ["web"])
    clock += 1000
    await store.recordDeploy(SCOPE, ["responder", "sender"])

    const presets = await store.deployPresets(SCOPE)
    expect(presets.length).toBe(2)
    // Most-recent first (recency-weighted; equal frequency → newer wins).
    expect(presets[0]).toEqual(["responder", "sender"]) // normalized (sorted) set
    expect(presets[1]).toEqual(["web"])
  })

  test("most-recent-first holds even when both records share a timestamp", async () => {
    // Real-world case: two deploys within the same millisecond → identical ts.
    // Insertion order must still decide "most recent" (the lastIndex tie-break).
    const frozen = 1_700_000_000_000
    const store = new ActionHistory({ baseDir: dir, now: () => frozen })
    await store.recordDeploy(SCOPE, ["web"])
    await store.recordDeploy(SCOPE, ["responder", "sender"])

    const presets = await store.deployPresets(SCOPE)
    expect(presets[0]).toEqual(["responder", "sender"])
    expect(presets[1]).toEqual(["web"])
  })

  test("re-deploying the same set increases its rank, not its count of presets", async () => {
    let clock = 1_700_000_000_000
    const store = new ActionHistory({ baseDir: dir, now: () => clock })

    // [web] deployed twice, [sender] once — but [sender] more recently.
    clock += 1000
    await store.recordDeploy(SCOPE, ["web"])
    clock += 1000
    await store.recordDeploy(SCOPE, ["web"])
    clock += 1000
    await store.recordDeploy(SCOPE, ["sender"])

    const presets = await store.deployPresets(SCOPE)
    // Still two distinct permutations.
    expect(presets.length).toBe(2)
    // [web] has higher frequency → ranks first under the default half-life.
    expect(presets[0]).toEqual(["web"])

    // Order-insensitive: deploying [sender,responder] then [responder,sender]
    // is the same permutation.
    clock += 1000
    await store.recordDeploy(SCOPE, ["sender", "responder"])
    clock += 1000
    await store.recordDeploy(SCOPE, ["responder", "sender"])
    const presets2 = await store.deployPresets(SCOPE)
    const permutation = presets2.filter(
      (p) => p.length === 2 && p[0] === "responder" && p[1] === "sender",
    )
    expect(permutation.length).toBe(1)
  })
})
