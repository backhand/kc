// Command kc is a keyboard-driven, Midnight-Commander-style CLI for daily
// Kubernetes operations. See SPEC.md for the design.
//
// This entrypoint is deliberately thin: it resolves the runtime context (kube
// context for the cache key, repo context for the entry point), wires the data
// layer's caches + learning store into the TUI model, and runs the Bubble Tea
// program. All views / navigation / optimistic-render logic live in internal/tui;
// all Kubernetes/git/GitHub access lives in internal/*.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/backhand/kc/internal/cache"
	xexec "github.com/backhand/kc/internal/exec"
	"github.com/backhand/kc/internal/git"
	"github.com/backhand/kc/internal/k8s"
	"github.com/backhand/kc/internal/resolve"
	"github.com/backhand/kc/internal/store"
	"github.com/backhand/kc/internal/tui"
)

// Build metadata, injected at release time by GoReleaser's ldflags
// (-X main.version=… -X main.commit=… -X main.date=…). Defaults are for a plain
// `go build` / `go install`.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Printf("kc %s (commit %s, built %s)\n", version, commit, date)
			return
		case "--help", "-h", "help":
			fmt.Print(usage)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kc: %v\n", err)
		os.Exit(1)
	}
}

const usage = `kc — keyboard-driven, Midnight-Commander-style Kubernetes operations TUI

Usage:
  kc             launch the TUI (uses the ambient KUBECONFIG / current context)
  kc --version   print version
  kc --help      print this help

In a git repo whose GHCR image runs on the cluster, kc opens at that app's
namespace; elsewhere it opens at all-namespaces. Set KC_NO_ALTSCREEN=1 for a
linear (pipeable) render. Full docs: https://github.com/backhand/kc
`

func run() error {
	ctx := context.Background()

	// Kube options come from the ambient KUBECONFIG (kubectl resolves auth,
	// including Dex/OIDC, for free — SPEC). A generous per-command timeout keeps
	// a slow apiserver from wedging the background fetches.
	opts := k8s.Options{Timeout: 20 * time.Second}

	// The cluster key for the cache is the current kube-context name; fall back
	// to a stable literal if kubectl can't tell us (cache still works, just
	// shared across contexts).
	cluster := currentContext(ctx, opts)
	if cluster == "" {
		cluster = "default"
	}

	// Resolve the contextual entry point: in a repo, map its GHCR image to the
	// running namespaces; remember the last-viewed namespace for that app.
	// Resolution failures degrade to the plain all-namespaces entry — kc must
	// still start.
	entry, app := resolveEntry(ctx, opts)

	history := store.New(store.Options{}) // default ~/.kc
	if app != "" {
		entry.PreferNamespace = lastNamespace(history, cluster, app)
	}

	deps := tui.Deps{
		KubeOpts:       opts,
		Cluster:        cluster,
		OverviewCache:  cache.New[k8s.ClusterOverview](cache.Options{Namespace: "overview"}),
		NamespaceCache: cache.New[k8s.NamespaceView](cache.Options{Namespace: "namespace"}),
		PodsCache:      cache.New[[]k8s.Pod](cache.Options{Namespace: "pods"}),
		AllDeployCache: cache.New[[]k8s.Deployment](cache.Options{Namespace: "alldeploy"}),
		History:        history,
		App:            app,
		Entry:          entry,
	}

	// Alt-screen by default; KC_NO_ALTSCREEN=1 disables it so the output is a
	// linear append stream (useful for piping / pty capture / CI smoke runs).
	var progOpts []tea.ProgramOption
	if os.Getenv("KC_NO_ALTSCREEN") == "" {
		progOpts = append(progOpts, tea.WithAltScreen())
	}
	p := tea.NewProgram(tui.New(deps), progOpts...)
	if _, err := p.Run(); err != nil {
		return err
	}
	return nil
}

// currentContext returns the active kube-context name, or "" if it can't be
// determined (no kubeconfig, kubectl absent). Read-only, short timeout.
func currentContext(ctx context.Context, opts k8s.Options) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ro := xexec.RunOptions{AllowNonZero: true}
	if opts.Kubeconfig != "" {
		ro.Env = []string{"KUBECONFIG=" + opts.Kubeconfig}
	}
	res, err := xexec.Run(cctx, "kubectl", []string{"config", "current-context"}, ro)
	if err != nil || res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// resolveEntry derives the contextual entry point from the cwd's git repo. It
// returns the resolved namespaces/groups and the app name (repo name) for the
// learning store, or a zero Entry + "" when not in a repo / nothing resolved.
func resolveEntry(ctx context.Context, opts k8s.Options) (tui.Entry, string) {
	rc, err := git.GetRepoContext(ctx, "")
	if err != nil || !rc.InRepo || rc.GHCRImage == "" {
		return tui.Entry{}, ""
	}
	res, err := resolve.Namespaces(ctx, rc.GHCRImage, opts)
	if err != nil {
		return tui.Entry{}, ""
	}
	app := ""
	if rc.Remote != nil {
		app = rc.Remote.Repo
	}
	return tui.Entry{Resolution: res}, app
}

// lastNamespace reads the most-recently-viewed namespace for an app from the
// learning store (recorded by the TUI on entry). Empty when none remembered.
func lastNamespace(h *store.ActionHistory, cluster, app string) string {
	ranked := h.Ranked("view-namespace", store.Scope{Cluster: cluster, App: app})
	for _, p := range ranked {
		if ns, ok := p["namespace"].(string); ok && ns != "" {
			return ns
		}
	}
	return ""
}
