/**
 * Pure parsers: raw kubectl JSON → kc domain types.
 *
 * No I/O — every function here is deterministic and offline-testable against
 * captured fixtures. The async wrappers in ./index.ts feed these.
 */

import type {
  Node,
  Namespace,
  NamespaceKind,
  Deployment,
  Pod,
  Usage,
  ImageRef,
} from "../types.ts"
import type {
  RawNode,
  RawNodeMetrics,
  RawPodMetrics,
  RawNamespace,
  RawDeployment,
  RawReplicaSet,
  RawPod,
  RawMetricUsage,
} from "./raw.ts"

// ─────────────────────────────────────────────────────────────────────────
// Quantity parsing (Kubernetes resource.Quantity → base units)
// ─────────────────────────────────────────────────────────────────────────

/** Round, but coerce a non-finite result (NaN from a bad parse) to 0. */
function safeRound(n: number): number {
  return Number.isFinite(n) ? Math.round(n) : 0
}

/**
 * Parse a Kubernetes CPU quantity into millicores.
 *   "250m" → 250 · "2" → 2000 · "1500000000n" → 1500 · "0.5" → 500
 * Suffixes: n (nano), u (micro), m (milli); bare = cores.
 */
export function parseCpuToMillicores(q: string | undefined | null): number {
  if (!q) return 0
  const s = q.trim()
  if (s.endsWith("n")) return safeRound(Number(s.slice(0, -1)) / 1e6)
  if (s.endsWith("u")) return safeRound(Number(s.slice(0, -1)) / 1e3)
  if (s.endsWith("m")) return safeRound(Number(s.slice(0, -1)))
  return safeRound(Number(s) * 1000)
}

const BINARY_SUFFIX: Record<string, number> = {
  Ki: 2 ** 10,
  Mi: 2 ** 20,
  Gi: 2 ** 30,
  Ti: 2 ** 40,
  Pi: 2 ** 50,
  Ei: 2 ** 60,
}
const DECIMAL_SUFFIX: Record<string, number> = {
  k: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
}

/**
 * Parse a Kubernetes memory quantity into bytes.
 *   "128Mi" → 134217728 · "2Gi" → 2147483648 · "500M" → 500000000 · "1024" → 1024
 *
 * Also accepts the milli suffix `m` (resource.Quantity is one type for CPU and
 * memory; some kubelets emit fractional-byte memory like "512000000m" = bytes ×
 * 1/1000). Any unrecognised/garbage value degrades to 0 (via {@link safeRound})
 * rather than NaN — a single bad sample must not poison usage sums.
 */
export function parseMemoryToBytes(q: string | undefined | null): number {
  if (!q) return 0
  const s = q.trim()
  // Binary (Ki/Mi/…) — check the 2-char suffix first.
  const bin = s.slice(-2)
  if (bin in BINARY_SUFFIX) {
    return safeRound(Number(s.slice(0, -2)) * BINARY_SUFFIX[bin]!)
  }
  // Milli suffix: value is in thousandths of a byte.
  if (s.endsWith("m")) {
    return safeRound(Number(s.slice(0, -1)) / 1000)
  }
  // Decimal (k/M/G/…) — single-char suffix.
  const dec = s.slice(-1)
  if (dec in DECIMAL_SUFFIX) {
    return safeRound(Number(s.slice(0, -1)) * DECIMAL_SUFFIX[dec]!)
  }
  return safeRound(Number(s))
}

function usageFrom(u: RawMetricUsage | undefined): Usage {
  return {
    cpuMillicores: parseCpuToMillicores(u?.cpu),
    memoryBytes: parseMemoryToBytes(u?.memory),
  }
}

/** Add two Usage samples. */
export function addUsage(a: Usage, b: Usage): Usage {
  return {
    cpuMillicores: a.cpuMillicores + b.cpuMillicores,
    memoryBytes: a.memoryBytes + b.memoryBytes,
  }
}

// ─────────────────────────────────────────────────────────────────────────
// Image references
// ─────────────────────────────────────────────────────────────────────────

/**
 * Split a container image string into repository / tag / digest.
 *
 *   ghcr.io/thinkpilot/mailon:v0.6.9          → repo=…/mailon, tag=v0.6.9
 *   ghcr.io/thinkpilot/mailon                 → repo=…/mailon, tag=null
 *   ghcr.io/thinkpilot/mailon@sha256:abc…     → repo=…/mailon, digest=sha256:abc…
 *   ghcr.io/thinkpilot/mailon:v0.6.9@sha256:… → repo=…/mailon, tag=v0.6.9, digest=…
 *   localhost:5000/app:1.0                    → repo=localhost:5000/app, tag=1.0
 *
 * Tag and digest are independent: a reference may carry a digest, a tag, both,
 * or neither (valid OCI). We strip the digest first, then parse the tag from
 * the remaining `name[:tag]` — so a tag+digest pin keeps the tag out of
 * `repository` (a glued-on tag would silently break exact image matching in
 * resolve.ts). The port-vs-tag ambiguity is resolved by only treating a ":" as
 * a tag separator when it appears after the last "/".
 */
export function parseImage(raw: string): ImageRef {
  // Strip an optional digest pin first: everything after "@".
  const atIdx = raw.indexOf("@")
  const digest = atIdx !== -1 ? raw.slice(atIdx + 1) || null : null
  const nameAndTag = atIdx !== -1 ? raw.slice(0, atIdx) : raw

  // Tag: a ":" that comes after the final "/" (so registry ports aren't tags).
  const lastSlash = nameAndTag.lastIndexOf("/")
  const lastColon = nameAndTag.lastIndexOf(":")
  if (lastColon > lastSlash) {
    return {
      raw,
      repository: nameAndTag.slice(0, lastColon),
      tag: nameAndTag.slice(lastColon + 1) || null,
      digest,
    }
  }

  return { raw, repository: nameAndTag, tag: null, digest }
}

// ─────────────────────────────────────────────────────────────────────────
// Namespace classification
// ─────────────────────────────────────────────────────────────────────────

/**
 * System namespaces that are cluster plumbing, not apps. Anything here (or
 * matching the `kube-*` prefix) classifies as "system" and sorts to the bottom.
 */
export const SYSTEM_NAMESPACES: readonly string[] = [
  "cert-manager",
  "dex",
  "buildkit",
  "actions-runner",
  "kube-system",
  "kube-public",
  "kube-node-lease",
]

const SYSTEM_SET = new Set(SYSTEM_NAMESPACES)

/** Classify a namespace name as user vs system plumbing. */
export function classifyNamespace(name: string): NamespaceKind {
  if (name.startsWith("kube-")) return "system"
  return SYSTEM_SET.has(name) ? "system" : "user"
}

// ─────────────────────────────────────────────────────────────────────────
// Nodes
// ─────────────────────────────────────────────────────────────────────────

const NODE_ROLE_PREFIX = "node-role.kubernetes.io/"

/** Parse `node-role.kubernetes.io/<role>` labels into a role list. */
export function parseNodeRoles(labels: Record<string, string> | undefined): string[] {
  if (!labels) return []
  return Object.keys(labels)
    .filter((k) => k.startsWith(NODE_ROLE_PREFIX))
    .map((k) => k.slice(NODE_ROLE_PREFIX.length))
    .filter(Boolean)
    .sort()
}

/**
 * Parse `get nodes -o json` items + optional node metrics into Node[].
 * Nodes are sorted control-plane-first, then by name (header presentation).
 */
export function parseNodes(items: RawNode[], metrics: RawNodeMetrics[] = []): Node[] {
  const usageByName = new Map<string, Usage>()
  for (const m of metrics) usageByName.set(m.metadata.name, usageFrom(m.usage))

  const nodes: Node[] = items.map((n) => {
    const roles = parseNodeRoles(n.metadata.labels)
    const controlPlane = roles.includes("control-plane") || roles.includes("master")
    const ready =
      n.status?.conditions?.some((c) => c.type === "Ready" && c.status === "True") ?? false
    const cap = n.status?.allocatable ?? n.status?.capacity ?? {}
    return {
      name: n.metadata.name,
      roles,
      controlPlane,
      ready,
      kubeletVersion: n.status?.nodeInfo?.kubeletVersion ?? "",
      capacity: {
        cpuMillicores: parseCpuToMillicores(cap["cpu"]),
        memoryBytes: parseMemoryToBytes(cap["memory"]),
      },
      usage: usageByName.get(n.metadata.name) ?? null,
    }
  })

  return nodes.sort((a, b) => {
    if (a.controlPlane !== b.controlPlane) return a.controlPlane ? -1 : 1
    return a.name.localeCompare(b.name)
  })
}

// ─────────────────────────────────────────────────────────────────────────
// Namespaces
// ─────────────────────────────────────────────────────────────────────────

/**
 * Parse `get ns -o json` into Namespace[], with user namespaces first
 * (alphabetical) and system plumbing sorted to the bottom (alphabetical).
 */
export function parseNamespaces(items: RawNamespace[]): Namespace[] {
  const out: Namespace[] = items.map((n) => ({
    name: n.metadata.name,
    kind: classifyNamespace(n.metadata.name),
    phase: n.status?.phase ?? "",
  }))
  return out.sort((a, b) => {
    if (a.kind !== b.kind) return a.kind === "user" ? -1 : 1
    return a.name.localeCompare(b.name)
  })
}

// ─────────────────────────────────────────────────────────────────────────
// Pod → deployment ownership mapping
// ─────────────────────────────────────────────────────────────────────────

/**
 * Build pod-name → owning-deployment-name from the ownerRef chain:
 *   Pod --(ReplicaSet ownerRef)--> ReplicaSet --(Deployment ownerRef)--> Deployment
 *
 * Pods owned directly by a Deployment (rare) or by a bare ReplicaSet with no
 * Deployment owner map to that name / null respectively. This is the spec's
 * preferred mapping (ownerRef chain), independent of label conventions.
 */
export function buildPodToDeployment(
  pods: RawPod[],
  replicaSets: RawReplicaSet[],
): Map<string, string | null> {
  // ReplicaSet name → owning Deployment name.
  const rsToDeploy = new Map<string, string | null>()
  for (const rs of replicaSets) {
    const owner = rs.metadata.ownerReferences?.find((o) => o.kind === "Deployment")
    rsToDeploy.set(rs.metadata.name, owner?.name ?? null)
  }

  const podToDeploy = new Map<string, string | null>()
  for (const p of pods) {
    const owners = p.metadata.ownerReferences ?? []
    const rsOwner = owners.find((o) => o.kind === "ReplicaSet")
    if (rsOwner) {
      podToDeploy.set(p.metadata.name, rsToDeploy.get(rsOwner.name) ?? null)
      continue
    }
    const depOwner = owners.find((o) => o.kind === "Deployment")
    podToDeploy.set(p.metadata.name, depOwner?.name ?? null)
  }
  return podToDeploy
}

// ─────────────────────────────────────────────────────────────────────────
// Pods
// ─────────────────────────────────────────────────────────────────────────

/**
 * Parse pods (+ replicasets for ownership, + pod metrics for usage) into Pod[].
 * Usage is null for any pod without a metrics entry.
 */
export function parsePods(
  pods: RawPod[],
  replicaSets: RawReplicaSet[],
  metrics: RawPodMetrics[] = [],
): Pod[] {
  const podToDeploy = buildPodToDeployment(pods, replicaSets)
  const usageByPod = podUsageByName(metrics)

  return pods.map((p) => {
    const statuses = p.status?.containerStatuses ?? []
    const restarts = statuses.reduce((sum, c) => sum + (c.restartCount ?? 0), 0)
    const ready = statuses.length > 0 && statuses.every((c) => c.ready === true)
    return {
      namespace: p.metadata.namespace ?? "",
      name: p.metadata.name,
      deployment: podToDeploy.get(p.metadata.name) ?? null,
      phase: p.status?.phase ?? "Unknown",
      ready,
      node: p.spec?.nodeName ?? "",
      restarts,
      usage: usageByPod.get(p.metadata.name) ?? null,
    }
  })
}

/** Sum each pod's per-container usage from pod metrics → pod-name → Usage. */
function podUsageByName(metrics: RawPodMetrics[]): Map<string, Usage> {
  const byPod = new Map<string, Usage>()
  for (const m of metrics) {
    let total: Usage = { cpuMillicores: 0, memoryBytes: 0 }
    for (const c of m.containers ?? []) total = addUsage(total, usageFrom(c.usage))
    byPod.set(m.metadata.name, total)
  }
  return byPod
}

// ─────────────────────────────────────────────────────────────────────────
// Deployments (with per-deployment usage rolled up from pods)
// ─────────────────────────────────────────────────────────────────────────

/**
 * Parse deployments and roll per-pod usage up to each deployment.
 *
 * Per-deployment usage = sum of the usage of every pod that maps to the
 * deployment (via {@link buildPodToDeployment}). A deployment whose pods have
 * no metrics at all gets `usage: null` (distinct from a real zero).
 */
export function parseDeployments(
  deployments: RawDeployment[],
  pods: RawPod[],
  replicaSets: RawReplicaSet[],
  metrics: RawPodMetrics[] = [],
): Deployment[] {
  const podToDeploy = buildPodToDeployment(pods, replicaSets)
  const usageByPod = podUsageByName(metrics)

  // Aggregate usage per deployment name. Track whether ANY contributing pod had
  // metrics, so we can return null when none did.
  const agg = new Map<string, { usage: Usage; sampled: boolean }>()
  for (const p of pods) {
    const dep = podToDeploy.get(p.metadata.name)
    if (!dep) continue
    const u = usageByPod.get(p.metadata.name)
    const cur = agg.get(dep) ?? { usage: { cpuMillicores: 0, memoryBytes: 0 }, sampled: false }
    if (u) {
      cur.usage = addUsage(cur.usage, u)
      cur.sampled = true
    }
    agg.set(dep, cur)
  }

  return deployments.map((d) => {
    const containers = d.spec?.template?.spec?.containers ?? []
    const images = containers.map((c) => parseImage(c.image))
    const primary: ImageRef = images[0] ?? { raw: "", repository: "", tag: null, digest: null }
    const rolled = agg.get(d.metadata.name)
    return {
      namespace: d.metadata.namespace ?? "",
      name: d.metadata.name,
      image: primary,
      images,
      readyReplicas: d.status?.readyReplicas ?? 0,
      desiredReplicas: d.spec?.replicas ?? 0,
      usage: rolled && rolled.sampled ? rolled.usage : null,
    }
  })
}
