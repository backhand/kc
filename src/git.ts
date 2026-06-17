/**
 * git.ts — repo context from the cwd.
 *
 * Is the cwd in a git work tree? Parse the `origin` remote → {owner, repo}
 * (both ssh and https forms) → derive the GHCR image `ghcr.io/<owner>/<repo>`.
 * The entry-point view and repo→namespace resolution build on this.
 */

import { run, isExecError } from "./exec.ts"
import type { RepoRef, RepoContext } from "./types.ts"

/**
 * Parse a git remote URL into {owner, repo}.
 *
 * Handles the forms the team actually uses:
 *   - ssh scp-like : git@github.com:thinkpilot/mailon.git
 *   - ssh url      : ssh://git@github.com/thinkpilot/mailon.git
 *   - https        : https://github.com/thinkpilot/mailon.git
 *   - https w/ user: https://user@github.com/thinkpilot/mailon
 *   - no .git suffix, optional trailing slash
 *
 * Returns null for anything it can't confidently parse. Not GitHub-only by
 * construction, but the {owner, repo} pair is all GHCR needs.
 */
export function parseRemote(url: string): RepoRef | null {
  const trimmed = url.trim()
  if (!trimmed) return null

  let path: string | null = null

  // scp-like syntax: [user@]host:owner/repo(.git)
  // Distinguished from a URL by having ":" but no "://".
  const scp = /^[^/]+@[^/:]+:(.+)$/.exec(trimmed)
  if (scp && !trimmed.includes("://")) {
    path = scp[1] ?? null
  } else {
    // Any URL scheme: ssh://, https://, git://, http://.
    try {
      const u = new URL(trimmed)
      path = u.pathname
    } catch {
      return null
    }
  }

  if (path === null) return null

  // Normalise: strip leading/trailing slashes and a trailing ".git".
  const cleaned = path.replace(/^\/+/, "").replace(/\.git$/i, "").replace(/\/+$/, "")
  const segments = cleaned.split("/").filter(Boolean)
  if (segments.length < 2) return null

  // owner is the first segment; repo is the last (handles nested paths
  // defensively, though GitHub uses exactly owner/repo).
  const owner = segments[0]
  const repo = segments[segments.length - 1]
  if (!owner || !repo) return null

  return { owner, repo }
}

/** Derive the GHCR image path for a repo: `ghcr.io/<owner>/<repo>` (lowercased). */
export function ghcrImageFor(ref: RepoRef): string {
  // GHCR (and OCI) image names are lowercase; GitHub orgs/repos may carry caps.
  return `ghcr.io/${ref.owner.toLowerCase()}/${ref.repo.toLowerCase()}`
}

/**
 * Resolve full repo context for a directory (default: process cwd).
 *
 * Degrades gracefully: outside a repo → `{ inRepo:false, … null }`; in a repo
 * with no `origin` → `{ inRepo:true, remote:null, ghcrImage:null }`.
 */
export async function getRepoContext(cwd?: string): Promise<RepoContext> {
  const opts = cwd ? { cwd } : {}

  // 1) Are we inside a work tree, and where is its root?
  let root: string | null = null
  try {
    const { stdout } = await run("git", ["rev-parse", "--show-toplevel"], opts)
    root = stdout.trim() || null
  } catch (err) {
    if (isExecError(err)) {
      // Not a repo (git exits 128) — a normal, expected outcome.
      return { inRepo: false, root: null, remote: null, ghcrImage: null }
    }
    throw err
  }

  if (root === null) {
    return { inRepo: false, root: null, remote: null, ghcrImage: null }
  }

  // 2) origin URL → {owner, repo}. Missing origin is fine.
  let remote: RepoRef | null = null
  try {
    const { stdout } = await run("git", ["remote", "get-url", "origin"], opts)
    remote = parseRemote(stdout)
  } catch {
    remote = null
  }

  const ghcrImage = remote ? ghcrImageFor(remote) : null
  return { inRepo: true, root, remote, ghcrImage }
}
