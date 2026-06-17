/**
 * Minimal structural types for the kubectl `-o json` payloads we read.
 *
 * Intentionally partial — only the fields kc parses. Fields are optional where
 * Kubernetes may omit them (e.g. a Deployment with 0 ready replicas has no
 * `.status.readyReplicas`). Parsers in ./parse.ts narrow these into our clean
 * domain types; nothing else should touch the raw shapes.
 */

export interface RawOwnerRef {
  kind: string
  name: string
}

export interface RawObjectMeta {
  name: string
  namespace?: string
  labels?: Record<string, string>
  ownerReferences?: RawOwnerRef[]
}

export interface RawList<T> {
  items: T[]
}

// ── Nodes ────────────────────────────────────────────────────────────────

export interface RawNode {
  metadata: RawObjectMeta
  status?: {
    capacity?: Record<string, string>
    allocatable?: Record<string, string>
    conditions?: { type: string; status: string }[]
    nodeInfo?: { kubeletVersion?: string }
  }
}

// ── metrics.k8s.io ─────────────────────────────────────────────────────────

export interface RawMetricUsage {
  cpu?: string
  memory?: string
}

export interface RawNodeMetrics {
  metadata: { name: string }
  usage?: RawMetricUsage
}

export interface RawNodeMetricsList {
  items: RawNodeMetrics[]
}

export interface RawPodMetrics {
  metadata: { name: string; namespace?: string }
  containers?: { name: string; usage?: RawMetricUsage }[]
}

export interface RawPodMetricsList {
  items: RawPodMetrics[]
}

// ── Namespaces ──────────────────────────────────────────────────────────

export interface RawNamespace {
  metadata: RawObjectMeta
  status?: { phase?: string }
}

// ── Deployments ───────────────────────────────────────────────────────────

export interface RawContainer {
  name: string
  image: string
}

export interface RawDeployment {
  metadata: RawObjectMeta
  spec?: {
    replicas?: number
    template?: { spec?: { containers?: RawContainer[] } }
  }
  status?: {
    readyReplicas?: number
    availableReplicas?: number
  }
}

// ── ReplicaSets ─────────────────────────────────────────────────────────

export interface RawReplicaSet {
  metadata: RawObjectMeta
}

// ── Pods ─────────────────────────────────────────────────────────────────

export interface RawContainerStatus {
  ready?: boolean
  restartCount?: number
}

export interface RawPod {
  metadata: RawObjectMeta
  spec?: { nodeName?: string }
  status?: {
    phase?: string
    containerStatuses?: RawContainerStatus[]
  }
}
