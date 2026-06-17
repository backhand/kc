package k8s

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Pure parsers: raw kubectl JSON → kc domain types.
//
// No I/O — every function here is deterministic and offline-testable against
// captured fixtures. The wrappers in k8s.go feed these.
//
// Ported from tools/kc-bun/src/k8s/parse.ts.

// ─────────────────────────────────────────────────────────────────────────
// Quantity parsing (Kubernetes resource.Quantity → base units)
// ─────────────────────────────────────────────────────────────────────────

// safeRound rounds n, coercing a non-finite result (NaN from a bad parse) to 0.
// Mirrors the TS reference's Number.isFinite guard so a single garbage sample
// degrades to 0 rather than poisoning a usage sum.
func safeRound(n float64) int64 {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	return int64(math.Round(n))
}

// parseFloat parses s as a float64; a parse failure yields NaN so safeRound
// degrades it to 0 (matching JS Number("garbage") === NaN).
func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// ParseCPUToMillicores parses a Kubernetes CPU quantity into millicores.
//
//	"250m" → 250 · "2" → 2000 · "1500000000n" → 1500 · "0.5" → 500
//
// Suffixes: n (nano), u (micro), m (milli); bare = cores.
func ParseCPUToMillicores(q string) int64 {
	s := strings.TrimSpace(q)
	if s == "" {
		return 0
	}
	switch {
	case strings.HasSuffix(s, "n"):
		return safeRound(parseFloat(s[:len(s)-1]) / 1e6)
	case strings.HasSuffix(s, "u"):
		return safeRound(parseFloat(s[:len(s)-1]) / 1e3)
	case strings.HasSuffix(s, "m"):
		return safeRound(parseFloat(s[:len(s)-1]))
	default:
		return safeRound(parseFloat(s) * 1000)
	}
}

var binarySuffix = map[string]float64{
	"Ki": math.Exp2(10),
	"Mi": math.Exp2(20),
	"Gi": math.Exp2(30),
	"Ti": math.Exp2(40),
	"Pi": math.Exp2(50),
	"Ei": math.Exp2(60),
}

var decimalSuffix = map[string]float64{
	"k": 1e3,
	"M": 1e6,
	"G": 1e9,
	"T": 1e12,
	"P": 1e15,
	"E": 1e18,
}

// ParseMemoryToBytes parses a Kubernetes memory quantity into bytes.
//
//	"128Mi" → 134217728 · "2Gi" → 2147483648 · "500M" → 500000000 · "1024" → 1024
//
// Also accepts the milli suffix `m` (resource.Quantity is one type for CPU and
// memory; some kubelets emit fractional-byte memory like "512000000m" = bytes ×
// 1/1000). Any unrecognised/garbage value degrades to 0 (via safeRound) rather
// than NaN — a single bad sample must not poison usage sums.
func ParseMemoryToBytes(q string) int64 {
	s := strings.TrimSpace(q)
	if s == "" {
		return 0
	}
	// Binary (Ki/Mi/…) — check the 2-char suffix first.
	if len(s) >= 2 {
		bin := s[len(s)-2:]
		if mult, ok := binarySuffix[bin]; ok {
			return safeRound(parseFloat(s[:len(s)-2]) * mult)
		}
	}
	// Milli suffix: value is in thousandths of a byte.
	if strings.HasSuffix(s, "m") {
		return safeRound(parseFloat(s[:len(s)-1]) / 1000)
	}
	// Decimal (k/M/G/…) — single-char suffix.
	dec := s[len(s)-1:]
	if mult, ok := decimalSuffix[dec]; ok {
		return safeRound(parseFloat(s[:len(s)-1]) * mult)
	}
	return safeRound(parseFloat(s))
}

// usageFrom builds a Usage from a raw metric sample (nil-safe).
func usageFrom(u *rawMetricUsage) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		CPUMillicores: ParseCPUToMillicores(u.CPU),
		MemoryBytes:   ParseMemoryToBytes(u.Memory),
	}
}

// AddUsage returns the sum of two Usage samples.
func AddUsage(a, b Usage) Usage {
	return Usage{
		CPUMillicores: a.CPUMillicores + b.CPUMillicores,
		MemoryBytes:   a.MemoryBytes + b.MemoryBytes,
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Image references
// ─────────────────────────────────────────────────────────────────────────

// ParseImage splits a container image string into repository / tag / digest.
//
//	ghcr.io/thinkpilot/mailon:v0.6.9          → repo=…/mailon, tag=v0.6.9
//	ghcr.io/thinkpilot/mailon                 → repo=…/mailon, tag=""
//	ghcr.io/thinkpilot/mailon@sha256:abc…     → repo=…/mailon, digest=sha256:abc…
//	ghcr.io/thinkpilot/mailon:v0.6.9@sha256:… → repo=…/mailon, tag=v0.6.9, digest=…
//	localhost:5000/app:1.0                    → repo=localhost:5000/app, tag=1.0
//
// Tag and digest are independent. The digest is stripped first, then the tag is
// parsed from the remaining name[:tag] — so a tag+digest pin keeps the tag out
// of Repository (a glued-on tag would silently break exact image matching in
// resolve). The port-vs-tag ambiguity is resolved by only treating a ":" as a
// tag separator when it appears after the last "/".
func ParseImage(raw string) ImageRef {
	// Strip an optional digest pin first: everything after "@".
	digest := ""
	nameAndTag := raw
	if at := strings.Index(raw, "@"); at != -1 {
		digest = raw[at+1:]
		nameAndTag = raw[:at]
	}

	lastSlash := strings.LastIndex(nameAndTag, "/")
	lastColon := strings.LastIndex(nameAndTag, ":")
	if lastColon > lastSlash {
		return ImageRef{
			Raw:        raw,
			Repository: nameAndTag[:lastColon],
			Tag:        nameAndTag[lastColon+1:],
			Digest:     digest,
		}
	}
	return ImageRef{Raw: raw, Repository: nameAndTag, Tag: "", Digest: digest}
}

// ─────────────────────────────────────────────────────────────────────────
// Namespace classification
// ─────────────────────────────────────────────────────────────────────────

// SystemNamespaces are cluster plumbing, not apps. Anything here (or matching
// the kube-* prefix) classifies as "system" and sorts to the bottom.
var SystemNamespaces = []string{
	"cert-manager",
	"dex",
	"buildkit",
	"actions-runner",
	"kube-system",
	"kube-public",
	"kube-node-lease",
}

var systemSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(SystemNamespaces))
	for _, n := range SystemNamespaces {
		m[n] = struct{}{}
	}
	return m
}()

// ClassifyNamespace classifies a namespace name as user vs system plumbing.
func ClassifyNamespace(name string) NamespaceKind {
	if strings.HasPrefix(name, "kube-") {
		return KindSystem
	}
	if _, ok := systemSet[name]; ok {
		return KindSystem
	}
	return KindUser
}

// ─────────────────────────────────────────────────────────────────────────
// Nodes
// ─────────────────────────────────────────────────────────────────────────

const nodeRolePrefix = "node-role.kubernetes.io/"

// ParseNodeRoles parses node-role.kubernetes.io/<role> labels into a sorted
// role list.
func ParseNodeRoles(labels map[string]string) []string {
	roles := []string{}
	for k := range labels {
		if strings.HasPrefix(k, nodeRolePrefix) {
			if role := k[len(nodeRolePrefix):]; role != "" {
				roles = append(roles, role)
			}
		}
	}
	sort.Strings(roles)
	return roles
}

// parseNodes parses `get nodes -o json` items + optional node metrics into
// []Node. Nodes are sorted control-plane-first, then by name.
func parseNodes(items []rawNode, metrics []rawNodeMetrics) []Node {
	usageByName := make(map[string]Usage, len(metrics))
	for _, m := range metrics {
		usageByName[m.Metadata.Name] = usageFrom(m.Usage)
	}

	nodes := make([]Node, 0, len(items))
	for _, n := range items {
		roles := ParseNodeRoles(n.Metadata.Labels)
		controlPlane := contains(roles, "control-plane") || contains(roles, "master")

		ready := false
		kubelet := ""
		var cap map[string]string
		if n.Status != nil {
			for _, c := range n.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					ready = true
					break
				}
			}
			if n.Status.NodeInfo != nil {
				kubelet = n.Status.NodeInfo.KubeletVersion
			}
			if n.Status.Allocatable != nil {
				cap = n.Status.Allocatable
			} else {
				cap = n.Status.Capacity
			}
		}

		var usage *Usage
		if u, ok := usageByName[n.Metadata.Name]; ok {
			usage = &u
		}

		nodes = append(nodes, Node{
			Name:           n.Metadata.Name,
			Roles:          roles,
			ControlPlane:   controlPlane,
			Ready:          ready,
			KubeletVersion: kubelet,
			Capacity: Usage{
				CPUMillicores: ParseCPUToMillicores(cap["cpu"]),
				MemoryBytes:   ParseMemoryToBytes(cap["memory"]),
			},
			Usage: usage,
		})
	}

	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].ControlPlane != nodes[j].ControlPlane {
			return nodes[i].ControlPlane // control-plane first
		}
		return nodes[i].Name < nodes[j].Name
	})
	return nodes
}

// ─────────────────────────────────────────────────────────────────────────
// Namespaces
// ─────────────────────────────────────────────────────────────────────────

// parseNamespaces parses `get ns -o json` into []Namespace, with user
// namespaces first (alphabetical) and system plumbing sorted to the bottom
// (alphabetical).
func parseNamespaces(items []rawNamespace) []Namespace {
	out := make([]Namespace, 0, len(items))
	for _, n := range items {
		phase := ""
		if n.Status != nil {
			phase = n.Status.Phase
		}
		out = append(out, Namespace{
			Name:  n.Metadata.Name,
			Kind:  ClassifyNamespace(n.Metadata.Name),
			Phase: phase,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == KindUser // user before system
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// Pod → deployment ownership mapping
// ─────────────────────────────────────────────────────────────────────────

// BuildPodToDeployment maps pod-name → owning-deployment-name via the ownerRef
// chain:
//
//	Pod --(ReplicaSet ownerRef)--> ReplicaSet --(Deployment ownerRef)--> Deployment
//
// Pods owned directly by a Deployment (rare) map to that name. A pod owned by a
// bare ReplicaSet with no Deployment owner — or by nothing — maps to "" (the
// zero value, equivalent to the TS null). This is the spec's preferred mapping,
// independent of label conventions.
func BuildPodToDeployment(pods []rawPod, replicaSets []rawReplicaSet) map[string]string {
	// ReplicaSet name → owning Deployment name ("" if none).
	rsToDeploy := make(map[string]string, len(replicaSets))
	for _, rs := range replicaSets {
		owner := findOwner(rs.Metadata.OwnerReferences, "Deployment")
		rsToDeploy[rs.Metadata.Name] = owner
	}

	podToDeploy := make(map[string]string, len(pods))
	for _, p := range pods {
		if rsOwner := findOwner(p.Metadata.OwnerReferences, "ReplicaSet"); rsOwner != "" {
			podToDeploy[p.Metadata.Name] = rsToDeploy[rsOwner]
			continue
		}
		podToDeploy[p.Metadata.Name] = findOwner(p.Metadata.OwnerReferences, "Deployment")
	}
	return podToDeploy
}

// findOwner returns the name of the first ownerReference of the given kind, or
// "" if none.
func findOwner(refs []rawOwnerRef, kind string) string {
	for _, o := range refs {
		if o.Kind == kind {
			return o.Name
		}
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────
// Pods
// ─────────────────────────────────────────────────────────────────────────

// parsePods parses pods (+ replicasets for ownership, + pod metrics for usage)
// into []Pod. Usage is nil for any pod without a metrics entry.
func parsePods(pods []rawPod, replicaSets []rawReplicaSet, metrics []rawPodMetrics) []Pod {
	podToDeploy := BuildPodToDeployment(pods, replicaSets)
	usageByPod := podUsageByName(metrics)

	out := make([]Pod, 0, len(pods))
	for _, p := range pods {
		var statuses []rawContainerStatus
		phase := "Unknown"
		node := ""
		if p.Status != nil {
			statuses = p.Status.ContainerStatuses
			if p.Status.Phase != "" {
				phase = p.Status.Phase
			}
		}
		if p.Spec != nil {
			node = p.Spec.NodeName
		}

		restarts := 0
		ready := len(statuses) > 0
		for _, c := range statuses {
			restarts += c.RestartCount
			if !c.Ready {
				ready = false
			}
		}

		var usage *Usage
		if u, ok := usageByPod[p.Metadata.Name]; ok {
			usage = &u
		}

		out = append(out, Pod{
			Namespace:  p.Metadata.Namespace,
			Name:       p.Metadata.Name,
			Deployment: podToDeploy[p.Metadata.Name],
			Phase:      phase,
			Ready:      ready,
			Node:       node,
			Restarts:   restarts,
			Usage:      usage,
		})
	}
	return out
}

// podUsageByName sums each pod's per-container usage from pod metrics into a
// pod-name → Usage map.
func podUsageByName(metrics []rawPodMetrics) map[string]Usage {
	byPod := make(map[string]Usage, len(metrics))
	for _, m := range metrics {
		total := Usage{}
		for _, c := range m.Containers {
			total = AddUsage(total, usageFrom(c.Usage))
		}
		byPod[m.Metadata.Name] = total
	}
	return byPod
}

// ─────────────────────────────────────────────────────────────────────────
// Deployments (with per-deployment usage rolled up from pods)
// ─────────────────────────────────────────────────────────────────────────

// parseDeployments parses deployments and rolls per-pod usage up to each
// deployment.
//
// Per-deployment usage = sum of the usage of every pod that maps to the
// deployment (via BuildPodToDeployment). A deployment whose pods have no
// metrics at all gets nil usage (distinct from a real zero).
func parseDeployments(deployments []rawDeployment, pods []rawPod, replicaSets []rawReplicaSet, metrics []rawPodMetrics) []Deployment {
	podToDeploy := BuildPodToDeployment(pods, replicaSets)
	usageByPod := podUsageByName(metrics)

	// Aggregate usage per deployment name; track whether ANY contributing pod
	// had metrics, so we can return nil when none did.
	type agg struct {
		usage   Usage
		sampled bool
	}
	byDep := make(map[string]*agg)
	for _, p := range pods {
		dep := podToDeploy[p.Metadata.Name]
		if dep == "" {
			continue
		}
		cur := byDep[dep]
		if cur == nil {
			cur = &agg{}
			byDep[dep] = cur
		}
		if u, ok := usageByPod[p.Metadata.Name]; ok {
			cur.usage = AddUsage(cur.usage, u)
			cur.sampled = true
		}
	}

	out := make([]Deployment, 0, len(deployments))
	for _, d := range deployments {
		var containers []rawContainer
		replicas := 0
		if d.Spec != nil {
			if d.Spec.Replicas != nil {
				replicas = *d.Spec.Replicas
			}
			if d.Spec.Template != nil && d.Spec.Template.Spec != nil {
				containers = d.Spec.Template.Spec.Containers
			}
		}
		images := make([]ImageRef, 0, len(containers))
		for _, c := range containers {
			ref := ParseImage(c.Image)
			ref.Name = c.Name // owning container name (for `set image <name>=…`)
			images = append(images, ref)
		}
		primary := ImageRef{}
		if len(images) > 0 {
			primary = images[0]
		}
		ready := 0
		if d.Status != nil {
			ready = d.Status.ReadyReplicas
		}

		var usage *Usage
		if cur := byDep[d.Metadata.Name]; cur != nil && cur.sampled {
			u := cur.usage
			usage = &u
		}

		out = append(out, Deployment{
			Namespace:       d.Metadata.Namespace,
			Name:            d.Metadata.Name,
			Image:           primary,
			Images:          images,
			ReadyReplicas:   ready,
			DesiredReplicas: replicas,
			Usage:           usage,
		})
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
