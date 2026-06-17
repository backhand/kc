// Package k8s holds the kc data layer's Kubernetes domain types and the
// read-only kubectl wrappers + pure parsers that produce them.
//
// Everything here is plain data — TUI-agnostic. The views (step 3) and the
// deploy flow (step 4) consume these; nothing here knows about rendering.
//
// Ported from tools/kc-bun/src/types.ts and tools/kc-bun/src/k8s/.
package k8s

// ─────────────────────────────────────────────────────────────────────────
// Resource usage
// ─────────────────────────────────────────────────────────────────────────

// Usage is a point-in-time resource sample, normalised to base units:
//   - CPUMillicores — CPU in millicores (1000m = 1 core).
//   - MemoryBytes   — memory in bytes.
//
// A nil *Usage means "no metrics available" (e.g. metrics-server absent /
// metrics empty), which we treat distinctly from a genuine zero.
type Usage struct {
	CPUMillicores int64 `json:"cpuMillicores"`
	MemoryBytes   int64 `json:"memoryBytes"`
}

// ─────────────────────────────────────────────────────────────────────────
// Nodes
// ─────────────────────────────────────────────────────────────────────────

// Node is a cluster node with its roles, readiness, capacity and live usage.
type Node struct {
	Name string `json:"name"`
	// Roles parsed from node-role.kubernetes.io/* labels
	// (e.g. ["control-plane","etcd"]).
	Roles []string `json:"roles"`
	// ControlPlane is true when the node is a control-plane / master
	// (sorted/styled distinctly upstream).
	ControlPlane   bool   `json:"controlPlane"`
	Ready          bool   `json:"ready"`
	KubeletVersion string `json:"kubeletVersion"`
	// Capacity is schedulable capacity, normalised.
	Capacity Usage `json:"capacity"`
	// Usage is live usage from metrics, or nil when metrics are unavailable.
	Usage *Usage `json:"usage"`
}

// ─────────────────────────────────────────────────────────────────────────
// Namespaces
// ─────────────────────────────────────────────────────────────────────────

// NamespaceKind is "user" (an app namespace you operate on) or "system"
// (cluster plumbing: kube-*, cert-manager, dex, buildkit, actions-runner, …)
// sorted to the bottom.
type NamespaceKind string

const (
	// KindUser is an app namespace you operate on.
	KindUser NamespaceKind = "user"
	// KindSystem is cluster plumbing, sorted to the bottom.
	KindSystem NamespaceKind = "system"
)

// Namespace is a classified namespace row.
type Namespace struct {
	Name string        `json:"name"`
	Kind NamespaceKind `json:"kind"`
	// Phase from .status.phase (usually "Active").
	Phase string `json:"phase"`
}

// ─────────────────────────────────────────────────────────────────────────
// Deployments & pods
// ─────────────────────────────────────────────────────────────────────────

// ImageRef is a container image split into its repository, tag and digest.
type ImageRef struct {
	// Name is the owning container's name (from the pod spec). Empty when the
	// ref was parsed standalone (e.g. via ParseImage) rather than off a
	// deployment's containers. The deploy flow targets `kubectl set image
	// deployment/<d> <Name>=<image>` so a sidecar is never clobbered.
	Name string `json:"name,omitempty"`
	// Raw is the full image string as deployed,
	// e.g. "ghcr.io/thinkpilot/mailon:v0.6.9".
	Raw string `json:"raw"`
	// Repository is everything left of the tag,
	// e.g. "ghcr.io/thinkpilot/mailon".
	Repository string `json:"repository"`
	// Tag is the tag, e.g. "v0.6.9"; empty if the image carried no tag
	// (implicitly :latest).
	Tag string `json:"tag"`
	// Digest is the @sha256:… pin if present, else empty.
	Digest string `json:"digest"`
}

// Deployment is a workload with its image, readiness and rolled-up usage.
type Deployment struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Image is the primary container's image; Images holds every container.
	Image ImageRef `json:"image"`
	// Images is every container's image (primary first).
	Images          []ImageRef `json:"images"`
	ReadyReplicas   int        `json:"readyReplicas"`
	DesiredReplicas int        `json:"desiredReplicas"`
	// Usage is the sum of this deployment's pods' usage, or nil if no metrics
	// for any pod.
	Usage *Usage `json:"usage"`
}

// Pod is a single pod with status, scheduling and usage.
type Pod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Deployment is the owning deployment name, resolved via the ReplicaSet
	// ownerRef chain; empty when a pod maps to no deployment.
	Deployment string `json:"deployment"`
	// Phase from .status.phase (Pending|Running|Succeeded|Failed|Unknown|…).
	Phase string `json:"phase"`
	// Ready is true when every container reports ready.
	Ready bool `json:"ready"`
	// Node the pod is scheduled on (empty for unscheduled pods).
	Node string `json:"node"`
	// Restarts is the total restarts across all containers.
	Restarts int `json:"restarts"`
	// Usage from metrics, or nil when unavailable.
	Usage *Usage `json:"usage"`
}

// ─────────────────────────────────────────────────────────────────────────
// Aggregates returned by the public API
// ─────────────────────────────────────────────────────────────────────────

// Totals is summed node usage / capacity for the cluster header.
type Totals struct {
	// Usage is nil when no node reported metrics.
	Usage    *Usage `json:"usage"`
	Capacity Usage  `json:"capacity"`
}

// ClusterOverview is the all-namespaces landing view: node header + classified
// namespace rows + totals.
type ClusterOverview struct {
	Nodes      []Node      `json:"nodes"`
	Namespaces []Namespace `json:"namespaces"`
	Totals     Totals      `json:"totals"`
}

// NamespaceView is a single namespace's deployments with versions, readiness
// and usage.
type NamespaceView struct {
	Namespace   string        `json:"namespace"`
	Kind        NamespaceKind `json:"kind"`
	Deployments []Deployment  `json:"deployments"`
}
