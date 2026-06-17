//go:build live

// Package live holds opt-in integration tests that hit the real cluster / repo
// / GitHub. They are excluded from the default `go test ./...` by the `live`
// build tag; run them with:
//
//	KC_KUBECONFIG=/path/to/config \
//	KC_REPO=/path/to/mailon \
//	go test -tags live -v ./internal/live/
//
// Read-only throughout — no mutations, no writes to the real ~/.kc (store/cache
// tests use t.TempDir).
package live

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/cache"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/git"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/github"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/resolve"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/store"
)

func kubeOpts(t *testing.T) k8s.Options {
	kc := os.Getenv("KC_KUBECONFIG")
	if kc == "" {
		t.Skip("set KC_KUBECONFIG to run live k8s checks")
	}
	return k8s.Options{Kubeconfig: kc, Timeout: 20 * time.Second}
}

func repoDir(t *testing.T) string {
	d := os.Getenv("KC_REPO")
	if d == "" {
		t.Skip("set KC_REPO to run live git/github checks")
	}
	return d
}

func TestLive_ClusterOverview(t *testing.T) {
	ctx := context.Background()
	ov, err := k8s.GetClusterOverview(ctx, kubeOpts(t))
	if err != nil {
		t.Fatalf("GetClusterOverview: %v", err)
	}
	t.Logf("nodes=%d namespaces=%d capacity=%dm CPU / %d MiB",
		len(ov.Nodes), len(ov.Namespaces),
		ov.Totals.Capacity.CPUMillicores, ov.Totals.Capacity.MemoryBytes/(1<<20))
	for _, n := range ov.Nodes {
		usage := "no-metrics"
		if n.Usage != nil {
			usage = formatUsage(*n.Usage)
		}
		t.Logf("  node %-26s cp=%-5v ready=%-5v %s cap=%s usage=%s",
			n.Name, n.ControlPlane, n.Ready, n.KubeletVersion, formatUsage(n.Capacity), usage)
	}
	for _, ns := range ov.Namespaces {
		t.Logf("  ns %-18s [%s]", ns.Name, ns.Kind)
	}
	if len(ov.Nodes) == 0 {
		t.Error("expected at least one node")
	}
}

func TestLive_Deployments(t *testing.T) {
	ctx := context.Background()
	opts := kubeOpts(t)
	for _, ns := range []string{"mailon", "cert-manager", "kube-system"} {
		view, err := k8s.GetNamespace(ctx, ns, opts)
		if err != nil {
			t.Errorf("GetNamespace(%s): %v", ns, err)
			continue
		}
		t.Logf("namespace %s [%s] — %d deployments", view.Namespace, view.Kind, len(view.Deployments))
		for _, d := range view.Deployments {
			usage := "no-metrics"
			if d.Usage != nil {
				usage = formatUsage(*d.Usage)
			}
			t.Logf("  %-22s %-40s %d/%d %s", d.Name, d.Image.Raw, d.ReadyReplicas, d.DesiredReplicas, usage)
		}
	}
}

func TestLive_GitContext(t *testing.T) {
	ctx := context.Background()
	rc, err := git.GetRepoContext(ctx, repoDir(t))
	if err != nil {
		t.Fatalf("GetRepoContext: %v", err)
	}
	t.Logf("inRepo=%v root=%s remote=%+v ghcr=%s", rc.InRepo, rc.Root, rc.Remote, rc.GHCRImage)
	if !rc.InRepo || rc.Remote == nil {
		t.Fatal("expected a git repo with an origin remote")
	}
	if rc.Remote.Owner != "thinkpilot" || rc.Remote.Repo != "mailon" {
		t.Errorf("remote = %+v, want {thinkpilot mailon}", *rc.Remote)
	}
	if rc.GHCRImage != "ghcr.io/thinkpilot/mailon" {
		t.Errorf("ghcr = %q, want ghcr.io/thinkpilot/mailon", rc.GHCRImage)
	}
}

func TestLive_Releases(t *testing.T) {
	ctx := context.Background()
	repo := git.RepoRef{Owner: "thinkpilot", Repo: "mailon"}
	// Probe GHCR availability live too (token likely lacks read:packages → unknown).
	rels := github.GetReleases(ctx, repo, github.Options{
		Limit:     5,
		GHCRImage: "ghcr.io/thinkpilot/mailon",
		Timeout:   20 * time.Second,
	})
	if len(rels) == 0 {
		t.Fatal("expected at least one release for thinkpilot/mailon")
	}
	for _, r := range rels {
		t.Logf("  %-10s latest=%-5v pre=%-5v build=%-8s avail=%s run=%d",
			r.Tag, r.Latest, r.Prerelease, r.Build, availStr(r.ImageAvailable), r.BuildRunID)
	}
}

func TestLive_ResolveRepoToNamespace(t *testing.T) {
	ctx := context.Background()
	res, err := resolve.Namespaces(ctx, "ghcr.io/thinkpilot/mailon", kubeOpts(t))
	if err != nil {
		t.Fatalf("resolve.Namespaces: %v", err)
	}
	t.Logf("image=%s namespaces=%+v groups=%+v", res.Image, res.Namespaces, res.Groups)
	found := false
	for _, n := range res.Namespaces {
		if n.Namespace == "mailon" {
			found = true
		}
	}
	if !found {
		t.Error("expected the mailon repo image to resolve to the mailon namespace")
	}
}

func TestLive_StoreAndCacheRoundTrip(t *testing.T) {
	// Temp dirs only — never the real ~/.kc.
	base := t.TempDir()

	h := store.New(store.Options{BaseDir: base})
	scope := store.Scope{Cluster: "thinkpilot-k3s", App: "mailon"}
	if err := h.RecordDeploy(scope, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordDeploy(scope, []string{"responder", "sender"}); err != nil {
		t.Fatal(err)
	}
	presets := store.New(store.Options{BaseDir: base}).DeployPresets(scope) // fresh instance reloads
	t.Logf("deploy presets (most-recent first): %v", presets)
	if len(presets) != 2 || presets[0][0] != "responder" {
		t.Errorf("presets = %v, want [[responder sender] [web]]", presets)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	c := cache.New[k8s.ClusterOverview](cache.Options{BaseDir: base, Namespace: "overview", Now: clock})
	snap := k8s.ClusterOverview{Namespaces: []k8s.Namespace{{Name: "mailon", Kind: k8s.KindUser}}}
	if err := c.Put("thinkpilot-k3s", snap); err != nil {
		t.Fatal(err)
	}
	now = now.Add(8 * time.Second)
	got, age, found := c.Get("thinkpilot-k3s")
	t.Logf("cache: found=%v age=%v namespaces=%d", found, age, len(got.Namespaces))
	if !found || age != 8*time.Second || len(got.Namespaces) != 1 {
		t.Errorf("cache round-trip: found=%v age=%v got=%+v", found, age, got)
	}
}

func formatUsage(u k8s.Usage) string {
	return strconv.FormatInt(u.CPUMillicores, 10) + "m/" +
		strconv.FormatInt(u.MemoryBytes/(1<<20), 10) + "Mi"
}

func availStr(a github.Availability) string {
	switch a {
	case github.AvailPresent:
		return "present"
	case github.AvailAbsent:
		return "absent"
	default:
		return "unknown"
	}
}
