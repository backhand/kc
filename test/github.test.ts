/**
 * Unit tests for release annotation: build-status cross-ref + image availability,
 * against captured live `gh` fixtures (mailon).
 */

import { test, expect, describe } from "bun:test"
import { join } from "node:path"
import { annotateBuild, annotateReleases } from "../src/github.ts"
import type { RawRelease, RawRun } from "../src/github.ts"

const FIX = join(import.meta.dir, "fixtures")
async function fixture<T>(name: string): Promise<T> {
  return (await Bun.file(join(FIX, name)).json()) as T
}

describe("annotateBuild", () => {
  const runs: RawRun[] = [
    {
      databaseId: 1,
      event: "release",
      headBranch: "v1.0.0",
      status: "completed",
      conclusion: "success",
      workflowName: "Build & push container image",
    },
    {
      databaseId: 2,
      event: "release",
      headBranch: "v1.1.0",
      status: "completed",
      conclusion: "failure",
      workflowName: "Build & push container image",
    },
    {
      databaseId: 3,
      event: "release",
      headBranch: "v1.2.0",
      status: "in_progress",
      conclusion: null,
      workflowName: "Build & push container image",
    },
    // A non-release push run that happens to mention a tag in its title — must
    // NOT be matched as the release build.
    {
      databaseId: 4,
      event: "push",
      headBranch: "master",
      displayTitle: "chore(release): 2.0.0",
      status: "completed",
      conclusion: "success",
      workflowName: "CI",
    },
  ]

  test("success → ready", () => {
    expect(annotateBuild("v1.0.0", runs)).toEqual({ build: "ready", runId: 1 })
  })
  test("failure → failed", () => {
    expect(annotateBuild("v1.1.0", runs)).toEqual({ build: "failed", runId: 2 })
  })
  test("in_progress → building", () => {
    expect(annotateBuild("v1.2.0", runs)).toEqual({ build: "building", runId: 3 })
  })
  test("no matching release run → none", () => {
    expect(annotateBuild("v9.9.9", runs)).toEqual({ build: "none", runId: null })
  })
  test("does not match non-release events", () => {
    // "2.0.0" only appears in a push run's title → none.
    expect(annotateBuild("2.0.0", runs)).toEqual({ build: "none", runId: null })
  })
  test("matches via displayTitle when headBranch differs", () => {
    const r: RawRun[] = [
      {
        databaseId: 9,
        event: "release",
        headBranch: "refs/tags/v3.0.0",
        displayTitle: "v3.0.0",
        status: "completed",
        conclusion: "success",
      },
    ]
    expect(annotateBuild("v3.0.0", r)).toEqual({ build: "ready", runId: 9 })
  })

  test("picks the MOST RECENT matching run (highest id), regardless of array order", () => {
    // A failed build (older id) re-run to success (newer id) for the same tag.
    // `gh run list` order is not guaranteed, so feed them oldest-last too.
    const failedThenSucceeded: RawRun[] = [
      { databaseId: 50, event: "release", headBranch: "v4.0.0", status: "completed", conclusion: "success" },
      { databaseId: 40, event: "release", headBranch: "v4.0.0", status: "completed", conclusion: "failure" },
    ]
    expect(annotateBuild("v4.0.0", failedThenSucceeded)).toEqual({ build: "ready", runId: 50 })

    // Same data, reversed array order → still the newest (id 50) wins.
    expect(annotateBuild("v4.0.0", [...failedThenSucceeded].reverse())).toEqual({
      build: "ready",
      runId: 50,
    })
  })
})

describe("annotateReleases", () => {
  test("flags prerelease, latest; carries availability when provided", () => {
    const releases: RawRelease[] = [
      { tagName: "v2.0.0", isLatest: true, isPrerelease: false },
      { tagName: "v2.1.0-rc.1", isPrerelease: true },
      { tagName: "draft-x", isDraft: true }, // dropped
    ]
    const runs: RawRun[] = [
      {
        databaseId: 1,
        event: "release",
        headBranch: "v2.0.0",
        status: "completed",
        conclusion: "success",
      },
    ]
    const availability = new Map<string, boolean | null>([
      ["v2.0.0", true],
      ["v2.1.0-rc.1", null],
    ])
    const out = annotateReleases(releases, runs, availability)

    expect(out.length).toBe(2) // draft filtered out
    const stable = out.find((r) => r.tag === "v2.0.0")!
    expect(stable.latest).toBe(true)
    expect(stable.prerelease).toBe(false)
    expect(stable.build).toBe("ready")
    expect(stable.imageAvailable).toBe(true)

    const rc = out.find((r) => r.tag === "v2.1.0-rc.1")!
    expect(rc.prerelease).toBe(true)
    expect(rc.build).toBe("none")
    expect(rc.imageAvailable).toBeNull() // unknown
  })

  test("defaults imageAvailable to null when no availability map", () => {
    const out = annotateReleases([{ tagName: "v1.0.0" }], [])
    expect(out[0]!.imageAvailable).toBeNull()
    expect(out[0]!.name).toBe("v1.0.0") // falls back to tag
  })
})

describe("annotateReleases against live mailon fixtures", () => {
  test("matches real release builds (v0.6.9 failed, v0.6.5 ready, v0.6.10 none)", async () => {
    const releases = await fixture<RawRelease[]>("mailon-releases.json")
    const runs = await fixture<RawRun[]>("mailon-runs.json")
    const out = annotateReleases(releases, runs)
    const byTag = new Map(out.map((r) => [r.tag, r]))

    // From the captured run fixture:
    //   v0.6.9 release build → failure
    //   v0.6.5 release build → success
    //   v0.6.10 has no release-event run yet → none
    expect(byTag.get("v0.6.9")!.build).toBe("failed")
    expect(byTag.get("v0.6.5")!.build).toBe("ready")
    expect(byTag.get("v0.6.10")!.build).toBe("none")
    // v0.6.10 is the latest published release.
    expect(byTag.get("v0.6.10")!.latest).toBe(true)
    // No availability supplied → all unknown.
    expect(out.every((r) => r.imageAvailable === null)).toBe(true)
  })
})
