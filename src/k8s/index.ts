/**
 * k8s/index.ts — typed kubectl wrappers (read-only).
 *
 * Shells out to `kubectl … -o json` (and the metrics.k8s.io raw API for usage)
 * and feeds the pure parsers in ./parse.ts. Everything here is read-only — no
 * mutations (`kubectl set image` lives in the deploy flow, step 4).
 *
 * Usage handling: we read metrics via `kubectl get --raw /apis/metrics.k8s.io/…`
 * rather than `kubectl top` — `top` has no `-o json` and only text tables,
 * whereas the metrics API returns structured per-container JSON. When
 * metrics-server is absent the raw call fails; we swallow that and report
 * `usage: null` everywhere (graceful "no metrics" degradation).
 */

import { runJson } from "../exec.ts"
import type {
  Node,
  Namespace,
  Deployment,
  Pod,
  ClusterOverview,
  NamespaceView,
  Usage,
} from "../types.ts"
import {
  parseNodes,
  parseNamespaces,
  parseDeployments,
  parsePods,
  classifyNamespace,
  addUsage,
} from "./parse.ts"
import type {
  RawList,
  RawNode,
  RawNamespace,
  RawDeployment,
  RawReplicaSet,
  RawPod,
  RawNodeMetricsList,
  RawPodMetricsList,
} from "./raw.ts"

/** Options threaded through every wrapper (kubeconfig, context, timeouts). */
export interface K8sOptions {
  /** Path to a kubeconfig (sets KUBECONFIG for the spawned kubectl). */
  kubeconfig?: string
  /** kube-context to target (`--context`). */
  context?: string
  /** Per-command timeout in ms. */
  timeoutMs?: number
}

/** Build the leading kubectl args + env from options. */
function base(opts: K8sOptions): {
  flags: string[]
  env: Record<string, string | undefined>
  timeoutMs?: number
} {
  const flags: string[] = []
  if (opts.context) flags.push("--context", opts.context)
  const env: Record<string, string | undefined> = {}
  if (opts.kubeconfig) env["KUBECONFIG"] = opts.kubeconfig
  return { flags, env, timeoutMs: opts.timeoutMs }
}

async function kjson<T>(args: string[], opts: K8sOptions): Promise<T> {
  const { flags, env, timeoutMs } = base(opts)
  return runJson<T>("kubectl", [...flags, ...args], { env, timeoutMs })
}

/**
 * Fetch node metrics, returning [] when metrics-server is unavailable.
 * Never throws on the "no metrics" path — that's an expected cluster state.
 */
async function nodeMetrics(opts: K8sOptions): Promise<RawNodeMetricsList["items"]> {
  try {
    const m = await kjson<RawNodeMetricsList>(
      ["get", "--raw", "/apis/metrics.k8s.io/v1beta1/nodes"],
      opts,
    )
    return m.items ?? []
  } catch {
    return []
  }
}

/** Fetch pod metrics for a namespace, [] when metrics are unavailable. */
async function podMetrics(ns: string, opts: K8sOptions): Promise<RawPodMetricsList["items"]> {
  try {
    const m = await kjson<RawPodMetricsList>(
      ["get", "--raw", `/apis/metrics.k8s.io/v1beta1/namespaces/${ns}/pods`],
      opts,
    )
    return m.items ?? []
  } catch {
    return []
  }
}

// ── Public wrappers ────────────────────────────────────────────────────────

/** All nodes with roles, readiness, capacity and (if available) live usage. */
export async function getNodes(opts: K8sOptions = {}): Promise<Node[]> {
  const [list, metrics] = await Promise.all([
    kjson<RawList<RawNode>>(["get", "nodes", "-o", "json"], opts),
    nodeMetrics(opts),
  ])
  return parseNodes(list.items, metrics)
}

/** All namespaces, classified user vs system, system sorted to the bottom. */
export async function getNamespaces(opts: K8sOptions = {}): Promise<Namespace[]> {
  const list = await kjson<RawList<RawNamespace>>(["get", "namespaces", "-o", "json"], opts)
  return parseNamespaces(list.items)
}

/**
 * Deployments in a namespace, each with image+tag, ready/desired, and
 * per-deployment usage summed from its pods (via the ownerRef chain).
 */
export async function getDeployments(ns: string, opts: K8sOptions = {}): Promise<Deployment[]> {
  const [deps, rs, pods, metrics] = await Promise.all([
    kjson<RawList<RawDeployment>>(["-n", ns, "get", "deployments", "-o", "json"], opts),
    kjson<RawList<RawReplicaSet>>(["-n", ns, "get", "replicasets", "-o", "json"], opts),
    kjson<RawList<RawPod>>(["-n", ns, "get", "pods", "-o", "json"], opts),
    podMetrics(ns, opts),
  ])
  return parseDeployments(deps.items, pods.items, rs.items, metrics)
}

/** Pods belonging to a deployment (status, node, restarts, usage). */
export async function getDeploymentPods(
  ns: string,
  deployment: string,
  opts: K8sOptions = {},
): Promise<Pod[]> {
  const [rs, pods, metrics] = await Promise.all([
    kjson<RawList<RawReplicaSet>>(["-n", ns, "get", "replicasets", "-o", "json"], opts),
    kjson<RawList<RawPod>>(["-n", ns, "get", "pods", "-o", "json"], opts),
    podMetrics(ns, opts),
  ])
  return parsePods(pods.items, rs.items, metrics).filter((p) => p.deployment === deployment)
}

/** Every deployment across every namespace, in one call (used by resolve.ts). */
export async function getAllDeployments(opts: K8sOptions = {}): Promise<Deployment[]> {
  const [deps, rs, pods] = await Promise.all([
    kjson<RawList<RawDeployment>>(["get", "deployments", "--all-namespaces", "-o", "json"], opts),
    kjson<RawList<RawReplicaSet>>(["get", "replicasets", "--all-namespaces", "-o", "json"], opts),
    kjson<RawList<RawPod>>(["get", "pods", "--all-namespaces", "-o", "json"], opts),
  ])
  // No metrics here — cluster-wide usage isn't needed for image resolution and
  // would mean one raw-metrics call per namespace.
  return parseDeployments(deps.items, pods.items, rs.items, [])
}

// ── Aggregates ──────────────────────────────────────────────────────────

/** Sum node usage / capacity for the cluster header. */
function totals(nodes: Node[]): ClusterOverview["totals"] {
  let capacity: Usage = { cpuMillicores: 0, memoryBytes: 0 }
  let usage: Usage | null = null
  for (const n of nodes) {
    capacity = addUsage(capacity, n.capacity)
    if (n.usage) usage = usage ? addUsage(usage, n.usage) : n.usage
  }
  return { usage, capacity }
}

/** The all-namespaces landing view: nodes + classified namespaces + totals. */
export async function getClusterOverview(opts: K8sOptions = {}): Promise<ClusterOverview> {
  const [nodes, namespaces] = await Promise.all([getNodes(opts), getNamespaces(opts)])
  return { nodes, namespaces, totals: totals(nodes) }
}

/** A single namespace view: its deployments with versions, readiness, usage. */
export async function getNamespace(ns: string, opts: K8sOptions = {}): Promise<NamespaceView> {
  const deployments = await getDeployments(ns, opts)
  return { namespace: ns, kind: classifyNamespace(ns), deployments }
}

// Re-export the classification constants/helpers for callers (e.g. views).
export { SYSTEM_NAMESPACES, classifyNamespace } from "./parse.ts"
