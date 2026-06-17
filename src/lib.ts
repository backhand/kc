/**
 * kc data layer — public API barrel.
 *
 * The typed, TUI-agnostic, read-only foundation the views (step 3) and deploy
 * flow (step 4) consume. Everything reaches the world by shelling out to
 * kubectl / gh / git (see SPEC.md). Import from here:
 *
 *   import { getClusterOverview, getReleases, resolveNamespaces, ActionHistory } from "./lib.ts"
 *
 * NOTE: this is intentionally separate from index.tsx (the OpenTUI spike
 * entrypoint), which is left untouched until the views are wired (step 3).
 */

// Shared types.
export type * from "./types.ts"

// Shell-out primitives + typed errors.
export { run, runJson, isExecError, ExecError, JsonParseError } from "./exec.ts"
export type { RunOptions, RunResult } from "./exec.ts"

// Kubernetes (read-only).
export {
  getNodes,
  getNamespaces,
  getDeployments,
  getDeploymentPods,
  getAllDeployments,
  getClusterOverview,
  getNamespace,
  SYSTEM_NAMESPACES,
  classifyNamespace,
} from "./k8s/index.ts"
export type { K8sOptions } from "./k8s/index.ts"
// Pure parsers (exposed for reuse / testing).
export {
  parseImage,
  parseCpuToMillicores,
  parseMemoryToBytes,
  parseNodes,
  parseNamespaces,
  parseDeployments,
  parsePods,
  buildPodToDeployment,
} from "./k8s/parse.ts"

// Git repo context.
export { getRepoContext, parseRemote, ghcrImageFor } from "./git.ts"

// GitHub releases + annotations.
export {
  getReleases,
  annotateReleases,
  annotateBuild,
  ghcrManifestProbe,
} from "./github.ts"
export type { GitHubOptions, ImageProbe, RawRelease, RawRun } from "./github.ts"

// Repo → namespace resolution.
export { resolveNamespaces, resolveFromDeployments } from "./resolve.ts"

// Learning store.
export {
  ActionHistory,
  rankParams,
  canonicalKey,
  normalizeSet,
} from "./store/index.ts"
export type { ActionHistoryOptions, RankOptions } from "./store/index.ts"
