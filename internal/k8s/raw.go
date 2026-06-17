package k8s

// Minimal structural types for the kubectl `-o json` payloads we read.
//
// Intentionally partial — only the fields kc parses. Pointers / omitted fields
// are fine where Kubernetes may omit them (e.g. a Deployment with 0 ready
// replicas has no .status.readyReplicas). Parsers in parse.go narrow these into
// the clean domain types; nothing else should touch the raw shapes.
//
// Ported from tools/kc-bun/src/k8s/raw.ts.

// rawOwnerRef is one entry of metadata.ownerReferences.
type rawOwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// rawObjectMeta is the subset of metadata kc reads.
type rawObjectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels"`
	OwnerReferences []rawOwnerRef     `json:"ownerReferences"`
}

// rawList is the generic {items: [...]} envelope kubectl returns for lists.
type rawList[T any] struct {
	Items []T `json:"items"`
}

// ── Nodes ────────────────────────────────────────────────────────────────

type rawNodeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type rawNodeInfo struct {
	KubeletVersion string `json:"kubeletVersion"`
}

type rawNodeStatus struct {
	Capacity    map[string]string  `json:"capacity"`
	Allocatable map[string]string  `json:"allocatable"`
	Conditions  []rawNodeCondition `json:"conditions"`
	NodeInfo    *rawNodeInfo       `json:"nodeInfo"`
}

type rawNode struct {
	Metadata rawObjectMeta  `json:"metadata"`
	Status   *rawNodeStatus `json:"status"`
}

// ── metrics.k8s.io ─────────────────────────────────────────────────────────

type rawMetricUsage struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type rawNodeMetrics struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Usage *rawMetricUsage `json:"usage"`
}

type rawNodeMetricsList struct {
	Items []rawNodeMetrics `json:"items"`
}

type rawPodMetricsContainer struct {
	Name  string          `json:"name"`
	Usage *rawMetricUsage `json:"usage"`
}

type rawPodMetrics struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Containers []rawPodMetricsContainer `json:"containers"`
}

type rawPodMetricsList struct {
	Items []rawPodMetrics `json:"items"`
}

// ── Namespaces ──────────────────────────────────────────────────────────

type rawNamespace struct {
	Metadata rawObjectMeta `json:"metadata"`
	Status   *struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// ── Deployments ───────────────────────────────────────────────────────────

type rawContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type rawDeployment struct {
	Metadata rawObjectMeta `json:"metadata"`
	Spec     *struct {
		Replicas *int `json:"replicas"`
		Template *struct {
			Spec *struct {
				Containers []rawContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status *struct {
		ReadyReplicas     int `json:"readyReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
	} `json:"status"`
}

// ── ReplicaSets ─────────────────────────────────────────────────────────

type rawReplicaSet struct {
	Metadata rawObjectMeta `json:"metadata"`
}

// ── Pods ─────────────────────────────────────────────────────────────────

type rawContainerStatus struct {
	Ready        bool `json:"ready"`
	RestartCount int  `json:"restartCount"`
}

type rawPod struct {
	Metadata rawObjectMeta `json:"metadata"`
	Spec     *struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status *struct {
		Phase             string               `json:"phase"`
		ContainerStatuses []rawContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}
