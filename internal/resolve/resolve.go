// Package resolve maps a repo's GHCR image to the namespaces running it.
//
// Given a repo's GHCR image, find the running deployments using it and return
// their namespace(s). An app may span several namespaces (mailon,
// mailon-staging); we also collapse them into `<app>-*` groups so the views can
// zoom out from a namespace to its app group. The entry-point view uses this to
// land the user on the right namespace when kc is started inside a repo.
//
// Ported from tools/kc-bun/src/resolve.ts.
package resolve

import (
	"context"
	"sort"
	"strings"

	"github.com/backhand/kc/internal/k8s"
)

// ResolvedNamespace is a namespace resolved from a repo's image, with the
// matching deployment names.
type ResolvedNamespace struct {
	Namespace string `json:"namespace"`
	// Deployments are the deployment names in this namespace using the repo's
	// image (sorted).
	Deployments []string `json:"deployments"`
}

// Group is namespaces collapsed by their `<app>-*` prefix
// (e.g. mailon + mailon-staging → group "mailon").
type Group struct {
	App        string   `json:"app"`
	Namespaces []string `json:"namespaces"`
}

// Resolution is the result of resolving a GHCR image to running namespaces.
type Resolution struct {
	// Image is the GHCR image we resolved against (e.g.
	// "ghcr.io/thinkpilot/mailon"), or empty when none was supplied.
	Image string `json:"image"`
	// Namespaces running that image. Empty when nothing matches.
	Namespaces []ResolvedNamespace `json:"namespaces"`
	// Groups are namespaces collapsed into `<app>-*` groups.
	Groups []Group `json:"groups"`
}

// deploymentMatchesImage reports whether any of a deployment's container images
// match the target GHCR repository (case-insensitive, tag-insensitive).
func deploymentMatchesImage(dep k8s.Deployment, image string) bool {
	want := strings.ToLower(image)
	for _, img := range dep.Images {
		if strings.ToLower(img.Repository) == want {
			return true
		}
	}
	return false
}

// FromDeployments is the pure core: given all deployments and a target image,
// group matching deployments by namespace and roll namespaces up into `<app>-*`
// groups.
//
// Group key = the namespace name truncated at the first "-" (so `mailon` and
// `mailon-staging` share group "mailon"). A namespace with no "-" is its own
// group. Groups and their namespaces are returned sorted for stable output.
func FromDeployments(deployments []k8s.Deployment, image string) Resolution {
	byNamespace := make(map[string][]string)
	for _, dep := range deployments {
		if !deploymentMatchesImage(dep, image) {
			continue
		}
		byNamespace[dep.Namespace] = append(byNamespace[dep.Namespace], dep.Name)
	}

	namespaces := make([]ResolvedNamespace, 0, len(byNamespace))
	for ns, deps := range byNamespace {
		sort.Strings(deps)
		namespaces = append(namespaces, ResolvedNamespace{Namespace: ns, Deployments: deps})
	}
	sort.SliceStable(namespaces, func(i, j int) bool {
		return namespaces[i].Namespace < namespaces[j].Namespace
	})

	// Collapse namespaces into <app>-* groups.
	byApp := make(map[string][]string)
	for _, rn := range namespaces {
		app := rn.Namespace
		if i := strings.Index(app, "-"); i != -1 {
			app = app[:i]
		}
		byApp[app] = append(byApp[app], rn.Namespace)
	}
	groups := make([]Group, 0, len(byApp))
	for app, ns := range byApp {
		sort.Strings(ns)
		groups = append(groups, Group{App: app, Namespaces: ns})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].App < groups[j].App
	})

	return Resolution{Image: image, Namespaces: namespaces, Groups: groups}
}

// Namespaces resolves a GHCR image to the namespaces running it, live.
//
// image is typically RepoContext.GHCRImage. An empty image (not in a repo, or
// no origin) yields an empty resolution rather than erroring.
func Namespaces(ctx context.Context, image string, opts k8s.Options) (Resolution, error) {
	if image == "" {
		return Resolution{}, nil
	}
	deployments, err := k8s.GetAllDeployments(ctx, opts)
	if err != nil {
		return Resolution{}, err
	}
	return FromDeployments(deployments, image), nil
}
