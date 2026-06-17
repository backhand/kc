package k8s

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Unit tests for the pure kubectl-JSON parsers, against captured live fixtures.
// Deterministic and offline — no cluster access.
// Ported from tools/kc-bun/test/k8s-parse.test.ts.

func loadFixture[T any](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return out
}

// ── Quantity parsing ────────────────────────────────────────────────────────

func TestParseCPUToMillicores(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"250m", 250},
		{"2", 2000},
		{"0.5", 500},
		{"1500000000n", 1500},
		{"279789138n", 280}, // from node-metrics fixture
		{"500u", 1},         // 500 micro = 0.5m → round → 1
		{"5000u", 5},        // 5000 micro = 5m
		{"", 0},
	}
	for _, c := range cases {
		if got := ParseCPUToMillicores(c.in); got != c.want {
			t.Errorf("ParseCPUToMillicores(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseMemoryToBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"128Mi", 134217728},
		{"2Gi", 2147483648},
		{"7931600Ki", 7931600 * 1024},
		{"500M", 500000000},
		{"1024", 1024},
		{"512000000m", 512000}, // 512000000 / 1000 bytes
		{"1500m", 2},           // 1.5 bytes → round
		{"not-a-number", 0},    // garbage degrades to 0, never NaN
		{"12Xi", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := ParseMemoryToBytes(c.in); got != c.want {
			t.Errorf("ParseMemoryToBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ── Image parsing ────────────────────────────────────────────────────────

func TestParseImage(t *testing.T) {
	cases := []struct {
		name              string
		in                string
		repo, tag, digest string
	}{
		{"registry/owner/repo:tag", "ghcr.io/thinkpilot/mailon:v0.6.9", "ghcr.io/thinkpilot/mailon", "v0.6.9", ""},
		{"no tag", "ghcr.io/thinkpilot/mailon", "ghcr.io/thinkpilot/mailon", "", ""},
		{"registry port not a tag", "localhost:5000/app:1.0", "localhost:5000/app", "1.0", ""},
		{"port no tag", "localhost:5000/app", "localhost:5000/app", "", ""},
		{"digest pin", "ghcr.io/thinkpilot/mailon@sha256:abc123", "ghcr.io/thinkpilot/mailon", "", "sha256:abc123"},
		{"tag and digest", "ghcr.io/thinkpilot/mailon:v0.6.9@sha256:abc123", "ghcr.io/thinkpilot/mailon", "v0.6.9", "sha256:abc123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseImage(c.in)
			if got.Repository != c.repo {
				t.Errorf("repository = %q, want %q", got.Repository, c.repo)
			}
			if got.Tag != c.tag {
				t.Errorf("tag = %q, want %q", got.Tag, c.tag)
			}
			if got.Digest != c.digest {
				t.Errorf("digest = %q, want %q", got.Digest, c.digest)
			}
		})
	}
}

// ── Namespace classification ────────────────────────────────────────────────

func TestClassifyNamespace(t *testing.T) {
	for _, ns := range []string{"kube-system", "kube-public", "kube-node-lease"} {
		if got := ClassifyNamespace(ns); got != KindSystem {
			t.Errorf("ClassifyNamespace(%q) = %q, want system (kube-* prefix)", ns, got)
		}
	}
	for _, ns := range []string{"cert-manager", "dex", "buildkit", "actions-runner"} {
		if got := ClassifyNamespace(ns); got != KindSystem {
			t.Errorf("ClassifyNamespace(%q) = %q, want system (denylist)", ns, got)
		}
	}
	for _, ns := range []string{"mailon", "consistant", "temporal", "default"} {
		if got := ClassifyNamespace(ns); got != KindUser {
			t.Errorf("ClassifyNamespace(%q) = %q, want user", ns, got)
		}
	}
	for _, want := range []string{"cert-manager", "dex", "buildkit", "actions-runner"} {
		if !contains(SystemNamespaces, want) {
			t.Errorf("SystemNamespaces missing %q", want)
		}
	}
}

func TestParseNamespaces_Fixture(t *testing.T) {
	list := loadFixture[rawList[rawNamespace]](t, "namespaces.json")
	parsed := parseNamespaces(list.Items)

	// No "user" appears after a "system".
	firstSystem, lastUser := -1, -1
	for i, n := range parsed {
		if n.Kind == KindSystem && firstSystem == -1 {
			firstSystem = i
		}
		if n.Kind == KindUser {
			lastUser = i
		}
	}
	if !(lastUser < firstSystem) {
		t.Errorf("lastUser (%d) should be < firstSystem (%d)", lastUser, firstSystem)
	}

	byName := map[string]Namespace{}
	for _, n := range parsed {
		byName[n.Name] = n
	}
	if _, ok := byName["mailon"]; !ok {
		t.Error("expected mailon namespace")
	}
	if _, ok := byName["kube-system"]; !ok {
		t.Error("expected kube-system namespace")
	}
	if byName["mailon"].Kind != KindUser {
		t.Errorf("mailon kind = %q, want user", byName["mailon"].Kind)
	}
	if byName["kube-system"].Kind != KindSystem {
		t.Errorf("kube-system kind = %q, want system", byName["kube-system"].Kind)
	}
}

// ── Nodes (fixture) ────────────────────────────────────────────────────────

func TestParseNodes_Fixture(t *testing.T) {
	list := loadFixture[rawList[rawNode]](t, "nodes.json")
	metrics := loadFixture[rawNodeMetricsList](t, "node-metrics.json")
	nodes := parseNodes(list.Items, metrics.Items)

	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	// control-plane first.
	if !nodes[0].ControlPlane {
		t.Error("first node should be control-plane")
	}
	if !contains(nodes[0].Roles, "control-plane") {
		t.Errorf("first node roles = %v, want to contain control-plane", nodes[0].Roles)
	}

	byName := map[string]Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	server := byName["thinkpilot-k3s-server"]
	if !server.Ready {
		t.Error("server node not ready")
	}
	if !strings.HasPrefix(server.KubeletVersion, "v1.") {
		t.Errorf("server kubelet = %q, want v1.* prefix", server.KubeletVersion)
	}
	agent := byName["thinkpilot-k3s-agent-0"]
	if agent.Capacity.CPUMillicores != 4000 {
		t.Errorf("agent cpu capacity = %d, want 4000", agent.Capacity.CPUMillicores)
	}
	if agent.Usage == nil {
		t.Fatal("agent usage is nil, want metrics")
	}
	if agent.Usage.CPUMillicores <= 0 {
		t.Errorf("agent usage cpu = %d, want > 0", agent.Usage.CPUMillicores)
	}
}

func TestParseNodes_NoMetrics(t *testing.T) {
	list := loadFixture[rawList[rawNode]](t, "nodes.json")
	nodes := parseNodes(list.Items, nil)
	for _, n := range nodes {
		if n.Usage != nil {
			t.Errorf("node %s usage = %+v, want nil with no metrics", n.Name, *n.Usage)
		}
	}
}

// ── Pod → deployment mapping (fixture) ──────────────────────────────────────

func TestBuildPodToDeployment_Fixture(t *testing.T) {
	pods := loadFixture[rawList[rawPod]](t, "mailon-pods.json")
	rs := loadFixture[rawList[rawReplicaSet]](t, "mailon-replicasets.json")
	m := BuildPodToDeployment(pods.Items, rs.Items)

	want := map[string]bool{"ingester": true, "knowledge": true, "responder": true, "sender": true, "web": true}
	for _, dep := range m {
		if dep != "" && !want[dep] {
			t.Errorf("pod mapped to unexpected deployment %q", dep)
		}
	}
	// Spot-check a known running pod.
	var ingesterPod string
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Metadata.Name, "ingester-") {
			ingesterPod = p.Metadata.Name
			break
		}
	}
	if ingesterPod == "" {
		t.Fatal("no ingester pod in fixture")
	}
	if m[ingesterPod] != "ingester" {
		t.Errorf("ingester pod %q mapped to %q, want ingester", ingesterPod, m[ingesterPod])
	}
}

// ── Pods (fixture) ────────────────────────────────────────────────────────

func TestParsePods_Fixture(t *testing.T) {
	pods := loadFixture[rawList[rawPod]](t, "mailon-pods.json")
	rs := loadFixture[rawList[rawReplicaSet]](t, "mailon-replicasets.json")
	metrics := loadFixture[rawPodMetricsList](t, "mailon-pod-metrics.json")
	parsed := parsePods(pods.Items, rs.Items, metrics.Items)

	if len(parsed) == 0 {
		t.Fatal("no pods parsed")
	}
	for _, p := range parsed {
		if p.Phase == "Running" {
			if p.Node == "" {
				t.Errorf("running pod %s has no node", p.Name)
			}
			if p.Deployment == "" {
				t.Errorf("running pod %s has no deployment", p.Name)
			}
		}
	}
	withUsage := 0
	for _, p := range parsed {
		if p.Usage != nil {
			withUsage++
			if p.Usage.MemoryBytes <= 0 {
				t.Errorf("pod %s usage mem = %d, want > 0", p.Name, p.Usage.MemoryBytes)
			}
		}
	}
	if withUsage != len(metrics.Items) {
		t.Errorf("%d pods with usage, want %d (metric count)", withUsage, len(metrics.Items))
	}
}

// TestGetAllPods_Composition asserts the parse composition GetAllPods relies on:
// `parsePods(pods, replicasets, nil)` (no metrics — the cluster-wide pod fetch
// for the search index skips them) still resolves every running pod's owning
// Deployment via the ReplicaSet ownerRef chain, so each pod is addressable by a
// (namespace, deployment) jump. This is GetAllPods minus the I/O (the wrapper is
// the same parsePods call its argv test covers separately).
func TestGetAllPods_Composition(t *testing.T) {
	pods := loadFixture[rawList[rawPod]](t, "mailon-pods.json")
	rs := loadFixture[rawList[rawReplicaSet]](t, "mailon-replicasets.json")

	parsed := parsePods(pods.Items, rs.Items, nil) // nil metrics, exactly as GetAllPods
	if len(parsed) == 0 {
		t.Fatal("no pods parsed")
	}
	want := map[string]bool{"ingester": true, "knowledge": true, "responder": true, "sender": true, "web": true}
	for _, p := range parsed {
		// No metrics requested → never any usage (so search-index pod fetch is cheap).
		if p.Usage != nil {
			t.Errorf("pod %s has usage but GetAllPods passes nil metrics", p.Name)
		}
		if p.Phase == "Running" {
			if p.Deployment == "" {
				t.Errorf("running pod %s resolved to no deployment (not addressable by jump)", p.Name)
			} else if !want[p.Deployment] {
				t.Errorf("pod %s mapped to unexpected deployment %q", p.Name, p.Deployment)
			}
			if p.Namespace == "" {
				t.Errorf("pod %s has no namespace", p.Name)
			}
		}
	}
}

// ── Deployments + per-deployment usage rollup (fixture) ─────────────────────

func TestParseDeployments_Fixture(t *testing.T) {
	deps := loadFixture[rawList[rawDeployment]](t, "mailon-deployments.json")
	pods := loadFixture[rawList[rawPod]](t, "mailon-pods.json")
	rs := loadFixture[rawList[rawReplicaSet]](t, "mailon-replicasets.json")
	metrics := loadFixture[rawPodMetricsList](t, "mailon-pod-metrics.json")
	parsed := parseDeployments(deps.Items, pods.Items, rs.Items, metrics.Items)

	byName := map[string]Deployment{}
	names := []string{}
	for _, d := range parsed {
		byName[d.Name] = d
		names = append(names, d.Name)
	}
	sort.Strings(names)
	wantNames := []string{"ingester", "knowledge", "responder", "sender", "web"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Errorf("deployment names = %v, want %v", names, wantNames)
	}

	web := byName["web"]
	if web.Image.Repository != "ghcr.io/thinkpilot/mailon" {
		t.Errorf("web image repo = %q, want ghcr.io/thinkpilot/mailon", web.Image.Repository)
	}
	if web.Image.Tag != "v0.6.9" {
		t.Errorf("web image tag = %q, want v0.6.9", web.Image.Tag)
	}
	// The owning container's name is captured on the image ref (the deploy flow's
	// `kubectl set image <name>=…` relies on this).
	if web.Image.Name != "web" {
		t.Errorf("web image container name = %q, want web", web.Image.Name)
	}
	if len(web.Images) != 1 || web.Images[0].Name != "web" {
		t.Errorf("web.Images = %+v, want one container named web", web.Images)
	}
	if web.DesiredReplicas != 2 {
		t.Errorf("web desired = %d, want 2", web.DesiredReplicas)
	}
	if web.ReadyReplicas != 2 {
		t.Errorf("web ready = %d, want 2", web.ReadyReplicas)
	}

	// web's usage = sum of its metric'd pods.
	var expectedMem int64
	for _, m := range metrics.Items {
		if strings.HasPrefix(m.Metadata.Name, "web-") {
			for _, c := range m.Containers {
				if c.Usage != nil {
					expectedMem += ParseMemoryToBytes(c.Usage.Memory)
				}
			}
		}
	}
	if web.Usage == nil {
		t.Fatal("web usage is nil, want rolled-up metrics")
	}
	if web.Usage.MemoryBytes != expectedMem {
		t.Errorf("web usage mem = %d, want %d (sum of web pods)", web.Usage.MemoryBytes, expectedMem)
	}
}

func TestParseDeployments_NoMetrics(t *testing.T) {
	deps := loadFixture[rawList[rawDeployment]](t, "mailon-deployments.json")
	pods := loadFixture[rawList[rawPod]](t, "mailon-pods.json")
	rs := loadFixture[rawList[rawReplicaSet]](t, "mailon-replicasets.json")
	parsed := parseDeployments(deps.Items, pods.Items, rs.Items, nil)
	for _, d := range parsed {
		if d.Usage != nil {
			t.Errorf("deployment %s usage = %+v, want nil with no metrics", d.Name, *d.Usage)
		}
	}
}
