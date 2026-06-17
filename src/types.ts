/**
 * Shared types for the kc data layer.
 *
 * Everything here is plain data — TUI-agnostic. The views (step 3) and the
 * deploy flow (step 4) consume these; nothing here knows about rendering.
 */

// ─────────────────────────────────────────────────────────────────────────
// Resource usage
// ─────────────────────────────────────────────────────────────────────────

/**
 * A point-in-time resource sample, already normalised to base units:
 *   - `cpuMillicores` — CPU in millicores (1000m = 1 core).
 *   - `memoryBytes`   — memory in bytes.
 *
 * `null` means "no metrics available" (e.g. metrics-server absent / `top` empty),
 * which we treat distinctly from a genuine zero.
 */
export interface Usage {
  cpuMillicores: number
  memoryBytes: number
}

// ─────────────────────────────────────────────────────────────────────────
// Nodes
// ─────────────────────────────────────────────────────────────────────────

export interface Node {
  name: string
  /** Roles parsed from `node-role.kubernetes.io/*` labels (e.g. ["control-plane","etcd"]). */
  roles: string[]
  /** True when the node is a control-plane / master (sorted/styled distinctly upstream). */
  controlPlane: boolean
  ready: boolean
  kubeletVersion: string
  /** Schedulable capacity, normalised. */
  capacity: Usage
  /** Live usage from metrics, or null when metrics are unavailable. */
  usage: Usage | null
}

// ─────────────────────────────────────────────────────────────────────────
// Namespaces
// ─────────────────────────────────────────────────────────────────────────

/**
 * "user" = an app namespace you operate on; "system" = cluster plumbing
 * (kube-*, cert-manager, dex, buildkit, actions-runner, …) sorted to the bottom.
 */
export type NamespaceKind = "user" | "system"

export interface Namespace {
  name: string
  kind: NamespaceKind
  /** Phase from `.status.phase` (usually "Active"). */
  phase: string
}

// ─────────────────────────────────────────────────────────────────────────
// Deployments & pods
// ─────────────────────────────────────────────────────────────────────────

/** A container image split into its repository and tag halves. */
export interface ImageRef {
  /** Full image string as deployed, e.g. "ghcr.io/thinkpilot/mailon:v0.6.9". */
  raw: string
  /** Everything left of the tag, e.g. "ghcr.io/thinkpilot/mailon". */
  repository: string
  /** The tag, e.g. "v0.6.9". `null` if the image carried no tag (implicitly :latest). */
  tag: string | null
  /** A digest if pinned by @sha256:…, else null. */
  digest: string | null
}

export interface Deployment {
  namespace: string
  name: string
  /**
   * The deployment's container image. Multi-container deployments expose the
   * first/primary container here; `images` holds every container.
   */
  image: ImageRef
  /** Every container's image (primary first), for multi-container deployments. */
  images: ImageRef[]
  readyReplicas: number
  desiredReplicas: number
  /** Sum of this deployment's pods' usage, or null if no metrics for any pod. */
  usage: Usage | null
}

export type PodPhase = "Pending" | "Running" | "Succeeded" | "Failed" | "Unknown" | string

export interface Pod {
  namespace: string
  name: string
  /** Owning deployment name, resolved via the ReplicaSet ownerRef chain. */
  deployment: string | null
  phase: PodPhase
  /** True when every container reports ready. */
  ready: boolean
  /** Node the pod is scheduled on (may be empty for unscheduled pods). */
  node: string
  /** Total restarts across all containers. */
  restarts: number
  /** Pod usage from metrics, or null when unavailable. */
  usage: Usage | null
}

// ─────────────────────────────────────────────────────────────────────────
// Aggregates returned by the public API
// ─────────────────────────────────────────────────────────────────────────

/** The all-namespaces landing view: node header + classified namespace rows. */
export interface ClusterOverview {
  nodes: Node[]
  namespaces: Namespace[]
  /** Summed node usage / capacity, or null usage when metrics are unavailable. */
  totals: { usage: Usage | null; capacity: Usage }
}

/** A single namespace view: its deployments with versions, readiness and usage. */
export interface NamespaceView {
  namespace: string
  kind: NamespaceKind
  deployments: Deployment[]
}

// ─────────────────────────────────────────────────────────────────────────
// Git / GitHub
// ─────────────────────────────────────────────────────────────────────────

/** Parsed git origin → GitHub coordinates. */
export interface RepoRef {
  owner: string
  repo: string
}

/** Full repo context derived from the cwd. */
export interface RepoContext {
  /** True when cwd resolves into a git work tree. */
  inRepo: boolean
  /** Absolute path to the repo root, or null when not in a repo. */
  root: string | null
  /** Parsed owner/repo from the `origin` remote, or null. */
  remote: RepoRef | null
  /** Derived GHCR image path `ghcr.io/<owner>/<repo>`, or null. */
  ghcrImage: string | null
}

/** Build status of a release's image, cross-referenced from Actions. */
export type BuildStatus =
  | "ready" // a publishing run for this tag completed successfully
  | "building" // a publishing run for this tag is queued / in progress
  | "failed" // the publishing run for this tag failed / was cancelled
  | "none" // no publishing run found for this tag

export interface ReleaseAnnotation {
  tag: string
  /** Release display name (falls back to tag). */
  name: string
  prerelease: boolean
  /** Whether GitHub flags this as the latest non-prerelease. */
  latest: boolean
  publishedAt: string | null
  build: BuildStatus
  /** The Actions run id behind `build`, when one was matched. */
  buildRunId: number | null
  /**
   * Whether the tagged image exists in GHCR:
   *   - true  — confirmed present in the registry,
   *   - false — confirmed absent,
   *   - null  — could not be determined (no registry credentials / probe disabled).
   * When null, callers may fall back to `build === "ready"` as a proxy.
   */
  imageAvailable: boolean | null
}

/** A namespace resolved from a repo's GHCR image, grouped under its app. */
export interface ResolvedNamespace {
  namespace: string
  /** Deployment names in this namespace using the repo's image. */
  deployments: string[]
}

export interface RepoResolution {
  /** The GHCR image we resolved against (e.g. "ghcr.io/thinkpilot/mailon"). */
  image: string | null
  /** Namespaces running that image. Empty when nothing matches. */
  namespaces: ResolvedNamespace[]
  /**
   * App groups: namespaces collapsed by their `<app>-*` prefix
   * (e.g. mailon + mailon-staging → group "mailon").
   */
  groups: { app: string; namespaces: string[] }[]
}

// ─────────────────────────────────────────────────────────────────────────
// Learning store
// ─────────────────────────────────────────────────────────────────────────

/** One recorded action occurrence. */
export interface ActionRecord {
  /** Arbitrary action name: "deploy" | "logs" | "restart" | "shell" | … */
  action: string
  /** Free-form params for the action (e.g. { deployments: ["web"] }). */
  params: Record<string, unknown>
  /** Epoch milliseconds. */
  ts: number
}

/** Scope key: an action's params are ranked within a (cluster × app) scope. */
export interface Scope {
  /** Cluster identifier (e.g. kube-context name). */
  cluster: string
  /** App identifier (e.g. repo name or namespace). */
  app: string
}
