/**
 * github.ts — `gh` wrappers for the deploy flow's version list.
 *
 * The N latest *releases* for a repo (pre-releases included and flagged), each
 * annotated with:
 *   - build status  — cross-referenced from the release's "build & push" run
 *                     (`gh run list`): ready / building / failed / none.
 *   - image availability — does the tag exist in GHCR?
 *
 * GHCR note: an authoritative registry check needs `read:packages`-scoped
 * credentials. When the registry can't be queried (no creds / private package
 * → 401/403), the probe returns `null` ("unknown") and callers fall back to
 * `build === "ready"` as a proxy. The probe is injectable so tests stay offline.
 */

import { run, runJson } from "./exec.ts"
import type { BuildStatus, ReleaseAnnotation, RepoRef } from "./types.ts"

// ── Raw `gh` JSON shapes ────────────────────────────────────────────────────

/** `gh release list --json tagName,name,isPrerelease,isDraft,isLatest,publishedAt`. */
export interface RawRelease {
  tagName: string
  name?: string
  isPrerelease?: boolean
  isDraft?: boolean
  isLatest?: boolean
  publishedAt?: string
}

/** `gh run list --json databaseId,headBranch,status,conclusion,workflowName,event,displayTitle`. */
export interface RawRun {
  databaseId: number
  headBranch?: string
  status?: string // queued | in_progress | completed | …
  conclusion?: string | null // success | failure | cancelled | … | null
  workflowName?: string
  event?: string // release | push | …
  displayTitle?: string
}

// ── Build-status cross-referencing (pure) ──────────────────────────────────

/**
 * Find the Actions run that builds/publishes the image for `tag`, and reduce it
 * to a {@link BuildStatus}.
 *
 * Matching heuristic (robust to workflow renames): prefer a run triggered by
 * the `release` event whose `headBranch` (the ref) or `displayTitle` equals the
 * tag. The team's release build runs on `event: release` with `headBranch` set
 * to the tag (e.g. v0.6.9) — see the mailon fixtures.
 *
 * status/conclusion → BuildStatus:
 *   completed + success            → ready
 *   completed + failure/cancelled… → failed
 *   queued / in_progress           → building
 *   no matching run                → none
 */
export function annotateBuild(
  tag: string,
  runs: RawRun[],
): { build: BuildStatus; runId: number | null } {
  // Pick the MOST RECENT matching run, not the first: `gh run list` ordering is
  // not contractually newest-first, and a failed build is often re-run to
  // success for the same tag. GitHub run ids increase monotonically with time,
  // so the highest databaseId is the newest run.
  let match: RawRun | undefined
  for (const r of runs) {
    if (r.event !== "release") continue
    if (r.headBranch !== tag && r.displayTitle !== tag) continue
    if (!match || r.databaseId > match.databaseId) match = r
  }
  if (!match) return { build: "none", runId: null }

  const id = match.databaseId ?? null
  if (match.status !== "completed") {
    // queued / in_progress / waiting / requested / pending → still building.
    return { build: "building", runId: id }
  }
  if (match.conclusion === "success") return { build: "ready", runId: id }
  // failure | cancelled | timed_out | action_required | startup_failure | null
  return { build: "failed", runId: id }
}

/**
 * Combine releases + runs into annotations (pure). Image availability is filled
 * in separately by {@link getReleases} via the async probe; here it defaults to
 * `null` ("unknown") unless an availability map is supplied.
 */
export function annotateReleases(
  releases: RawRelease[],
  runs: RawRun[],
  availability: Map<string, boolean | null> = new Map(),
): ReleaseAnnotation[] {
  return releases
    .filter((r) => !r.isDraft) // drafts aren't deployable versions
    .map((r) => {
      const { build, runId } = annotateBuild(r.tagName, runs)
      return {
        tag: r.tagName,
        name: r.name?.trim() || r.tagName,
        prerelease: r.isPrerelease ?? false,
        latest: r.isLatest ?? false,
        publishedAt: r.publishedAt ?? null,
        build,
        buildRunId: runId,
        // `null` for tags with no probe result (absent key → undefined → null).
        imageAvailable: availability.get(r.tagName) ?? null,
      }
    })
}

// ── GHCR image availability (injectable probe) ──────────────────────────────

/** Resolves whether `image:tag` exists in a registry. null = couldn't tell. */
export type ImageProbe = (image: string, tag: string) => Promise<boolean | null>

/**
 * Default GHCR probe: HEAD the OCI manifest for `<image>:<tag>`.
 *
 * Mints a pull token from the gh CLI's token and queries
 * `https://ghcr.io/v2/<path>/manifests/<tag>`:
 *   200       → true (present)
 *   404       → false (absent)
 *   401 / 403 → null (no package-read permission — unknown, fall back)
 *
 * Returns null on any error so availability never blocks the version list.
 */
export const ghcrManifestProbe: ImageProbe = async (image, tag) => {
  // image looks like "ghcr.io/<owner>/<repo>"; strip the registry host.
  const m = /^ghcr\.io\/(.+)$/.exec(image)
  if (!m) return null
  const path = m[1]

  let token: string
  try {
    const { stdout } = await run("gh", ["auth", "token"], { timeoutMs: 5_000 })
    token = stdout.trim()
  } catch {
    return null
  }
  if (!token) return null

  try {
    // GHCR mints a registry pull token when presented a GH token as the password.
    const tokRes = await fetch(
      `https://ghcr.io/token?service=ghcr.io&scope=repository:${path}:pull`,
      { headers: { Authorization: `Basic ${btoa(`x:${token}`)}` } },
    )
    const registryToken = (await tokRes.json().catch(() => ({})))?.token as string | undefined
    if (!registryToken) return null

    const res = await fetch(`https://ghcr.io/v2/${path}/manifests/${encodeURIComponent(tag)}`, {
      method: "HEAD",
      headers: {
        Authorization: `Bearer ${registryToken}`,
        Accept: [
          "application/vnd.oci.image.index.v1+json",
          "application/vnd.oci.image.manifest.v1+json",
          "application/vnd.docker.distribution.manifest.list.v2+json",
          "application/vnd.docker.distribution.manifest.v2+json",
        ].join(", "),
      },
    })
    if (res.status === 200) return true
    if (res.status === 404) return false
    return null // 401/403/5xx → unknown
  } catch {
    return null
  }
}

// ── Async API ──────────────────────────────────────────────────────────────

export interface GitHubOptions {
  /** How many latest releases to fetch. Default 5 (the deploy modal shows 5). */
  limit?: number
  /** How many recent runs to scan for build status. Default 50. */
  runLimit?: number
  /** Per-command timeout in ms. */
  timeoutMs?: number
  /**
   * GHCR image path to probe availability against (e.g. ghcr.io/thinkpilot/mailon).
   * When omitted, availability stays `null` (unknown) for every release.
   */
  ghcrImage?: string | null
  /** Override the image-availability probe (tests pass a stub; default is GHCR). */
  probe?: ImageProbe
}

function repoSlug(repo: RepoRef): string {
  return `${repo.owner}/${repo.repo}`
}

/**
 * Latest releases for a repo, annotated with build status + image availability.
 *
 * Degrades gracefully throughout: a repo with no releases → `[]`; a `gh` error
 * (auth, network, repo gone) → `[]` rather than a throw; build/availability
 * annotation never throws on its own. The deploy modal can render whatever
 * comes back without guarding against exceptions.
 */
export async function getReleases(
  repo: RepoRef,
  opts: GitHubOptions = {},
): Promise<ReleaseAnnotation[]> {
  const slug = repoSlug(repo)
  const limit = opts.limit ?? 5
  const runLimit = opts.runLimit ?? 50
  const timeoutMs = opts.timeoutMs

  const [releases, runs] = await Promise.all([
    fetchReleases(slug, limit, timeoutMs),
    fetchRuns(slug, runLimit, timeoutMs),
  ])

  // Probe image availability per tag (only when a GHCR image is supplied).
  const availability = new Map<string, boolean | null>()
  if (opts.ghcrImage) {
    const probe = opts.probe ?? ghcrManifestProbe
    await Promise.all(
      releases.map(async (r) => {
        availability.set(r.tagName, await probe(opts.ghcrImage!, r.tagName))
      }),
    )
  }

  return annotateReleases(releases, runs, availability)
}

/** `gh release list` → raw releases; [] on any error (e.g. no releases / gh hiccup). */
async function fetchReleases(slug: string, limit: number, timeoutMs?: number): Promise<RawRelease[]> {
  try {
    return await ghJson<RawRelease[]>(
      [
        "release",
        "list",
        "--repo",
        slug,
        "--limit",
        String(limit),
        "--json",
        "tagName,name,isPrerelease,isDraft,isLatest,publishedAt",
      ],
      timeoutMs,
    )
  } catch {
    return []
  }
}

/** `gh run list` → raw runs; [] on any error. */
async function fetchRuns(slug: string, runLimit: number, timeoutMs?: number): Promise<RawRun[]> {
  try {
    return await ghJson<RawRun[]>(
      [
        "run",
        "list",
        "--repo",
        slug,
        "--limit",
        String(runLimit),
        "--json",
        "databaseId,headBranch,status,conclusion,workflowName,event,displayTitle",
      ],
      timeoutMs,
    )
  } catch {
    return []
  }
}

async function ghJson<T>(args: string[], timeoutMs?: number): Promise<T> {
  return runJson<T>("gh", args, { timeoutMs })
}
