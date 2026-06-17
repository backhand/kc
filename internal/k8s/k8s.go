package k8s

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/exec"
)

// Read-only kubectl wrappers.
//
// Shells out to `kubectl … -o json` (and the metrics.k8s.io raw API for usage)
// and feeds the pure parsers in parse.go. Everything here is read-only — no
// mutations (`kubectl set image` lives in the deploy flow, step 4).
//
// Usage handling: we read metrics via `kubectl get --raw /apis/metrics.k8s.io/…`
// rather than `kubectl top` — top has no -o json and only text tables, whereas
// the metrics API returns structured per-container JSON. When metrics-server is
// absent the raw call fails; we swallow that and report nil usage everywhere
// (graceful "no metrics" degradation).
//
// Ported from tools/kc-bun/src/k8s/index.ts.

// Options are threaded through every wrapper (kubeconfig, context, timeouts).
type Options struct {
	// Kubeconfig sets KUBECONFIG for the spawned kubectl.
	Kubeconfig string
	// Context targets a kube-context (--context).
	Context string
	// Timeout is the per-command timeout (zero = exec.DefaultTimeout).
	Timeout time.Duration
}

// runOpts builds the exec.RunOptions (env + timeout) from Options.
func (o Options) runOpts() exec.RunOptions {
	ro := exec.RunOptions{Timeout: o.Timeout}
	if o.Kubeconfig != "" {
		ro.Env = []string{"KUBECONFIG=" + o.Kubeconfig}
	}
	return ro
}

// args prepends the context flag (if any) to a kubectl invocation.
func (o Options) args(rest ...string) []string {
	if o.Context != "" {
		return append([]string{"--context", o.Context}, rest...)
	}
	return rest
}

// kjson runs `kubectl <args>` and decodes the JSON stdout into out.
func kjson(ctx context.Context, opts Options, out any, args ...string) error {
	return exec.RunJSON(ctx, "kubectl", opts.args(args...), opts.runOpts(), out)
}

// nodeMetrics fetches node metrics, returning nil when metrics-server is
// unavailable. Never errors on the "no metrics" path — an expected state.
func nodeMetrics(ctx context.Context, opts Options) []rawNodeMetrics {
	var m rawNodeMetricsList
	if err := kjson(ctx, opts, &m, "get", "--raw", "/apis/metrics.k8s.io/v1beta1/nodes"); err != nil {
		return nil
	}
	return m.Items
}

// podMetrics fetches pod metrics for a namespace, nil when unavailable.
func podMetrics(ctx context.Context, ns string, opts Options) []rawPodMetrics {
	var m rawPodMetricsList
	path := "/apis/metrics.k8s.io/v1beta1/namespaces/" + ns + "/pods"
	if err := kjson(ctx, opts, &m, "get", "--raw", path); err != nil {
		return nil
	}
	return m.Items
}

// ── Public wrappers ────────────────────────────────────────────────────────

// GetNodes returns all nodes with roles, readiness, capacity and (if available)
// live usage.
func GetNodes(ctx context.Context, opts Options) ([]Node, error) {
	var list rawList[rawNode]
	var metrics []rawNodeMetrics
	err := parallel(
		func() error { return kjson(ctx, opts, &list, "get", "nodes", "-o", "json") },
		func() error { metrics = nodeMetrics(ctx, opts); return nil },
	)
	if err != nil {
		return nil, err
	}
	return parseNodes(list.Items, metrics), nil
}

// GetNamespaces returns all namespaces, classified user vs system, with system
// sorted to the bottom.
func GetNamespaces(ctx context.Context, opts Options) ([]Namespace, error) {
	var list rawList[rawNamespace]
	if err := kjson(ctx, opts, &list, "get", "namespaces", "-o", "json"); err != nil {
		return nil, err
	}
	return parseNamespaces(list.Items), nil
}

// GetDeployments returns the deployments in a namespace, each with image+tag,
// ready/desired, and per-deployment usage summed from its pods (via the
// ownerRef chain).
func GetDeployments(ctx context.Context, ns string, opts Options) ([]Deployment, error) {
	var deps rawList[rawDeployment]
	var rs rawList[rawReplicaSet]
	var pods rawList[rawPod]
	var metrics []rawPodMetrics
	err := parallel(
		func() error { return kjson(ctx, opts, &deps, "-n", ns, "get", "deployments", "-o", "json") },
		func() error { return kjson(ctx, opts, &rs, "-n", ns, "get", "replicasets", "-o", "json") },
		func() error { return kjson(ctx, opts, &pods, "-n", ns, "get", "pods", "-o", "json") },
		func() error { metrics = podMetrics(ctx, ns, opts); return nil },
	)
	if err != nil {
		return nil, err
	}
	return parseDeployments(deps.Items, pods.Items, rs.Items, metrics), nil
}

// GetDeploymentPods returns the pods belonging to a deployment (status, node,
// restarts, usage).
func GetDeploymentPods(ctx context.Context, ns, deployment string, opts Options) ([]Pod, error) {
	var rs rawList[rawReplicaSet]
	var pods rawList[rawPod]
	var metrics []rawPodMetrics
	err := parallel(
		func() error { return kjson(ctx, opts, &rs, "-n", ns, "get", "replicasets", "-o", "json") },
		func() error { return kjson(ctx, opts, &pods, "-n", ns, "get", "pods", "-o", "json") },
		func() error { metrics = podMetrics(ctx, ns, opts); return nil },
	)
	if err != nil {
		return nil, err
	}
	all := parsePods(pods.Items, rs.Items, metrics)
	out := make([]Pod, 0, len(all))
	for _, p := range all {
		if p.Deployment == deployment {
			out = append(out, p)
		}
	}
	return out, nil
}

// GetAllDeployments returns every deployment across every namespace in one
// call (used by the resolve package). No metrics here — cluster-wide usage
// isn't needed for image resolution and would mean one raw-metrics call per
// namespace.
func GetAllDeployments(ctx context.Context, opts Options) ([]Deployment, error) {
	var deps rawList[rawDeployment]
	var rs rawList[rawReplicaSet]
	var pods rawList[rawPod]
	err := parallel(
		func() error {
			return kjson(ctx, opts, &deps, "get", "deployments", "--all-namespaces", "-o", "json")
		},
		func() error {
			return kjson(ctx, opts, &rs, "get", "replicasets", "--all-namespaces", "-o", "json")
		},
		func() error {
			return kjson(ctx, opts, &pods, "get", "pods", "--all-namespaces", "-o", "json")
		},
	)
	if err != nil {
		return nil, err
	}
	return parseDeployments(deps.Items, pods.Items, rs.Items, nil), nil
}

// allResourceArgs is the pure argv for a cluster-wide `kubectl get <resource>`,
// context threaded exactly like the read-only wrappers. Exposed so the exact
// command is unit-testable without spawning kubectl (SPEC: "Build all kubectl
// argv with pure functions") — GetAllPods uses it.
func allResourceArgs(opts Options, resource string) []string {
	return opts.args("get", resource, "--all-namespaces", "-o", "json")
}

// GetAllPodsArgs returns the kubectl argv GetAllPods issues for pods — pure.
//
//	kubectl [--context <c>] get pods --all-namespaces -o json
//
// Exposed for argv assertions in tests (the search-everywhere index's
// cluster-wide pod fetch).
func GetAllPodsArgs(opts Options) []string { return allResourceArgs(opts, "pods") }

// GetAllPods returns every pod across every namespace in one call, each with its
// owning Deployment resolved via the ReplicaSet ownerRef chain (`get pods` +
// `get replicasets`, both --all-namespaces). It feeds the search-everywhere
// index (jump to any pod cluster-wide). No metrics here — the index keys on
// name/namespace/deployment only, and a per-namespace raw-metrics fan-out across
// the whole cluster would be far too costly for a fast-open modal.
func GetAllPods(ctx context.Context, opts Options) ([]Pod, error) {
	var rs rawList[rawReplicaSet]
	var pods rawList[rawPod]
	err := parallel(
		func() error {
			return exec.RunJSON(ctx, "kubectl", GetAllPodsArgs(opts), opts.runOpts(), &pods)
		},
		func() error {
			return kjson(ctx, opts, &rs, "get", "replicasets", "--all-namespaces", "-o", "json")
		},
	)
	if err != nil {
		return nil, err
	}
	return parsePods(pods.Items, rs.Items, nil), nil
}

// ── Aggregates ──────────────────────────────────────────────────────────

// computeTotals sums node usage / capacity for the cluster header.
func computeTotals(nodes []Node) Totals {
	capacity := Usage{}
	var usage *Usage
	for _, n := range nodes {
		capacity = AddUsage(capacity, n.Capacity)
		if n.Usage != nil {
			if usage == nil {
				u := *n.Usage
				usage = &u
			} else {
				sum := AddUsage(*usage, *n.Usage)
				usage = &sum
			}
		}
	}
	return Totals{Usage: usage, Capacity: capacity}
}

// GetClusterOverview returns the all-namespaces landing view: nodes +
// classified namespaces + totals.
func GetClusterOverview(ctx context.Context, opts Options) (ClusterOverview, error) {
	var nodes []Node
	var namespaces []Namespace
	err := parallel(
		func() error {
			n, e := GetNodes(ctx, opts)
			nodes = n
			return e
		},
		func() error {
			ns, e := GetNamespaces(ctx, opts)
			namespaces = ns
			return e
		},
	)
	if err != nil {
		return ClusterOverview{}, err
	}
	return ClusterOverview{Nodes: nodes, Namespaces: namespaces, Totals: computeTotals(nodes)}, nil
}

// GetNamespace returns a single namespace view: its deployments with versions,
// readiness and usage.
func GetNamespace(ctx context.Context, ns string, opts Options) (NamespaceView, error) {
	deps, err := GetDeployments(ctx, ns, opts)
	if err != nil {
		return NamespaceView{}, err
	}
	return NamespaceView{Namespace: ns, Kind: ClassifyNamespace(ns), Deployments: deps}, nil
}

// ── Interactive / streaming argv builders (logs, exec) ──────────────────────
//
// kc's logs (`L`) and shell (`s`) ops are NOT shell-outs that capture output:
// they hand the terminal to a long-lived kubectl via tea.ExecProcess (suspend
// the TUI → stream/interact → resume). So the data layer only owns the pure
// argv assembly here (context threaded exactly like the read-only wrappers); the
// TUI builds the *exec.Cmd (with the KUBECONFIG env from Options.runOpts) and
// hands it to Bubble Tea. Pure → the exact argv is unit-testable without
// spawning anything (SPEC: "Build all kubectl argv with pure functions").
//
// These are read-only / read-mostly: `logs` only reads; `exec -it -- sh` opens
// an interactive session but is not a declarative mutation, so it stays out of
// the mutating deploy package.

// LogsArgs builds the kubectl argv for streaming logs — pure.
//
//	kubectl [--context <c>] -n <ns> logs <target> --all-containers --tail=<n> [-f]
//
// target is a kubectl resource reference, e.g. "pod/responder-aaa" or
// "deployment/responder". --all-containers includes sidecars; tail bounds the
// backlog; follow appends -f for a live stream (the TUI streams with follow,
// a path-check probe omits it).
func LogsArgs(opts Options, ns, target string, tail int, follow bool) []string {
	args := opts.args("-n", ns, "logs", target, "--all-containers")
	if tail >= 0 {
		args = append(args, "--tail="+strconv.Itoa(tail))
	}
	if follow {
		args = append(args, "-f")
	}
	return args
}

// ExecArgs builds the kubectl argv for an interactive shell into a pod — pure.
//
//	kubectl [--context <c>] -n <ns> exec -it <pod> -- <command…>
//
// command defaults to ["sh"] when empty (the canonical busybox/distroless-ish
// fallback shell). -it allocates a TTY + keeps stdin open for an interactive
// session; the TUI runs this via tea.ExecProcess so the real terminal is handed
// to kubectl.
func ExecArgs(opts Options, ns, pod string, command ...string) []string {
	if len(command) == 0 {
		command = []string{"sh"}
	}
	args := opts.args("-n", ns, "exec", "-it", pod, "--")
	return append(args, command...)
}

// ExecOptions returns the exec.RunOptions (KUBECONFIG env + timeout) for a
// kubectl invocation built from Options. Exposed so the TUI can thread the same
// kubeconfig/context env into the *exec.Cmd it hands to tea.ExecProcess for the
// streaming/interactive ops, matching the read-only wrappers.
func (o Options) ExecOptions() exec.RunOptions { return o.runOpts() }

// parallel runs fns concurrently and returns the first non-nil error (after all
// have finished). A stdlib stand-in for the TS Promise.all fan-out — keeps the
// data layer dependency-free.
func parallel(fns ...func() error) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, fn := range fns {
		wg.Add(1)
		go func(fn func() error) {
			defer wg.Done()
			if err := fn(); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(fn)
	}
	wg.Wait()
	return firstErr
}
