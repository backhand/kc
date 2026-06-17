//go:build live

// Live TUI smoke tests — opt-in (build tag `live`), hit the real cluster via the
// ambient/`KC_KUBECONFIG` kubeconfig and drive the model headlessly through
// teatest, printing the rendered frames. Read-only; caches use t.TempDir so the
// real ~/.kc is never touched.
//
//	KC_KUBECONFIG=/path/to/config go test -tags live -v -run Live ./internal/tui/
//	KC_REPO=/path/to/mailon       (optional) exercises repo-entry-at-namespace
package tui

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/cache"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/git"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
	"github.com/thinkpilot/infrastructure/tools/kc/internal/resolve"
)

func liveDeps(t *testing.T) Deps {
	t.Helper()
	kc := os.Getenv("KC_KUBECONFIG")
	if kc == "" {
		t.Skip("set KC_KUBECONFIG to run live TUI checks")
	}
	base := t.TempDir()
	opts := k8s.Options{Kubeconfig: kc, Timeout: 20 * time.Second}
	return Deps{
		KubeOpts:       opts,
		Cluster:        "live",
		OverviewCache:  cache.New[k8s.ClusterOverview](cache.Options{BaseDir: base, Namespace: "overview"}),
		NamespaceCache: cache.New[k8s.NamespaceView](cache.Options{BaseDir: base, Namespace: "namespace"}),
		PodsCache:      cache.New[[]k8s.Pod](cache.Options{BaseDir: base, Namespace: "pods"}),
		AllDeployCache: cache.New[[]k8s.Deployment](cache.Options{BaseDir: base, Namespace: "alldeploy"}),
		Fetch:          realFetchers(opts),
	}
}

// dumpFinal renders the final model's View() — the actual frame the user would
// see with the live data still in the model — which is more legible than the
// post-quit teardown bytes left in the output stream.
func dumpFinal(t *testing.T, tm *teatest.TestModel, label string) {
	t.Helper()
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(10*time.Second))
	m, ok := fm.(Model)
	if !ok {
		t.Fatalf("final model type %T", fm)
	}
	m.quitting = false // View() blanks while quitting; show the last real frame
	t.Logf("\n===== %s =====\n%s", label, m.View())
}

// TestLive_TopView renders the all-namespaces view against the real cluster.
func TestLive_TopView(t *testing.T) {
	tm := teatest.NewTestModel(t, New(liveDeps(t)), teatest.WithInitialTermSize(132, 40))
	// Wait for the overview AND the version-hint feed (the all-deployments fetch
	// populates the VERSION column with running image tags, e.g. v0.6.x).
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("NAMESPACE")) &&
			bytes.Contains(b, []byte("updated")) &&
			bytes.Contains(b, []byte("v0.6"))
	}, teatest.WithDuration(15*time.Second))
	tm.Send(runeMsg('q'))
	dumpFinal(t, tm, "LIVE top view")
}

// TestLive_DrillMailon drills all-namespaces → mailon → first deployment → pods,
// printing the namespace + pods frames.
func TestLive_DrillMailon(t *testing.T) {
	deps := liveDeps(t)
	tm := teatest.NewTestModel(t, New(deps), teatest.WithInitialTermSize(132, 40))

	// Wait for the overview, then move the cursor onto the "mailon" row.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("mailon"))
	}, teatest.WithDuration(15*time.Second))

	// Find mailon's row index from a fresh overview fetch (deterministic order).
	ov, err := k8s.GetClusterOverview(context.Background(), deps.KubeOpts)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	idx := -1
	for i, ns := range ov.Namespaces {
		if ns.Name == "mailon" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Skip("no mailon namespace on this cluster")
	}
	for i := 0; i < idx; i++ {
		tm.Send(runeMsg('j'))
	}
	tm.Send(keyMsg(tea.KeyEnter)) // -> mailon namespace
	// Wait for the deployment rows to actually LOAD before drilling further —
	// otherwise Enter on an empty list is a no-op. "deployments" in the header
	// flips from "0 deployments" to "N deployments" once the fetch lands.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("DEPLOYMENT")) &&
			bytes.Contains(b, []byte("web")) // a known mailon deployment row
	}, teatest.WithDuration(15*time.Second))

	tm.Send(keyMsg(tea.KeyEnter)) // -> first deployment's pods
	// Wait until pods actually populate (a "Running" status cell), not just the
	// column header — so the dumped frame shows real pod rows.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("POD")) && bytes.Contains(b, []byte("Running"))
	}, teatest.WithDuration(15*time.Second))

	tm.Send(runeMsg('q'))
	dumpFinal(t, tm, "LIVE mailon pods")
}

// TestLive_RepoEntry verifies the contextual entry point: resolving the mailon
// repo's GHCR image lands the model on the mailon namespace at launch.
func TestLive_RepoEntry(t *testing.T) {
	repo := os.Getenv("KC_REPO")
	if repo == "" {
		t.Skip("set KC_REPO (mailon checkout) to run the repo-entry check")
	}
	deps := liveDeps(t)
	rc, err := git.GetRepoContext(context.Background(), repo)
	if err != nil || rc.GHCRImage == "" {
		t.Fatalf("repo context: %v (ghcr=%q)", err, rc.GHCRImage)
	}
	res, err := resolve.Namespaces(context.Background(), rc.GHCRImage, deps.KubeOpts)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	deps.Entry = Entry{Resolution: res}

	tm := teatest.NewTestModel(t, New(deps), teatest.WithInitialTermSize(132, 40))
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		// Breadcrumb must include the mailon namespace at entry (not just the
		// overview), and a real deployment row must have loaded.
		return bytes.Contains(b, []byte("all-namespaces › mailon")) &&
			bytes.Contains(b, []byte("DEPLOYMENT")) &&
			bytes.Contains(b, []byte("web"))
	}, teatest.WithDuration(15*time.Second))
	tm.Send(runeMsg('q'))
	dumpFinal(t, tm, "LIVE repo entry (mailon)")
}
