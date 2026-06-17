package deploy

import (
	"sort"
	"strings"

	"github.com/backhand/kc/internal/git"
	"github.com/backhand/kc/internal/k8s"
)

// Pure planning helpers: derive the release repo from running deployments and
// compute exactly what a deploy changes (per deployment: which container, from
// which tag, to which image). No I/O — unit-testable, and the basis for the
// confirm screen's "from→to" lines.

// DeriveRepo finds the GitHub repo to pull releases from by inspecting the
// deployments' container images. The first ghcr.io image
// (`ghcr.io/<owner>/<app>[:tag]`) wins; its `<owner>/<app>` becomes the repo
// ref. This lets the deploy flow list releases even when kc was NOT launched
// from the app's checkout (SPEC: "Derive the repo from the deployments' GHCR
// image").
//
// Returns (ref, true) on the first match, or (zero, false) when no deployment
// runs a ghcr.io image (e.g. a non-GHCR app — the version list is then empty).
func DeriveRepo(deployments []k8s.Deployment) (git.RepoRef, bool) {
	for _, d := range deployments {
		for _, img := range d.Images {
			if ref, ok := repoFromGHCR(img.Repository); ok {
				return ref, true
			}
		}
	}
	return git.RepoRef{}, false
}

// repoFromGHCR turns a GHCR repository path into a RepoRef:
// `ghcr.io/<owner>/<app>` → {Owner: <owner>, Repo: <app>}. The match is
// case-insensitive on the host; owner/repo casing is preserved (GitHub orgs may
// carry caps even though the GHCR path is lowercase).
func repoFromGHCR(repository string) (git.RepoRef, bool) {
	rest, ok := cutPrefixFold(repository, "ghcr.io/")
	if !ok {
		return git.RepoRef{}, false
	}
	segs := splitNonEmpty(rest, "/")
	if len(segs) < 2 {
		return git.RepoRef{}, false
	}
	// owner is the first segment; repo is the last (handles nested paths like
	// ghcr.io/<owner>/<group>/<app> by taking the final component as the repo).
	return git.RepoRef{Owner: segs[0], Repo: segs[len(segs)-1]}, true
}

// Change is one deployment's image change for a deploy: which container gets set
// to which image, and the tag it currently runs (for the from→to confirm line).
type Change struct {
	// Namespace / Deployment identify the workload.
	Namespace  string
	Deployment string
	// Container is the container whose image is set. Empty means "every
	// container" (the "*" wildcard) — used for single-container deployments or
	// when no container matched the target repo.
	Container string
	// FromTag is the tag currently deployed on that container (empty when the
	// running image carried no tag, shown as "—").
	FromTag string
	// ToTag is the release tag being deployed (e.g. "v0.6.10").
	ToTag string
	// Image is the full target image (`<repo>:<ToTag>`) handed to `set image`.
	Image string
}

// NoOp reports whether the change leaves the container on its current tag (a
// redeploy of the same version). Still a valid deploy (re-pull / restart), just
// flagged so the confirm screen can say so.
func (c Change) NoOp() bool { return c.FromTag == c.ToTag }

// PlanChanges computes the per-deployment image changes for deploying tag onto
// the chosen deployments.
//
// For each selected deployment it picks the container to update by matching the
// derived release image's repository against the deployment's containers:
//   - the matching container's name is used (so a sidecar is never touched);
//   - if exactly one container, the "*" wildcard is used regardless;
//   - if several containers but none match the repo, "*" is used as a last
//     resort (the deployment likely is single-purpose under a different repo).
//
// The target image is `<repo>:<tag>` where <repo> is the matched container's
// repository, falling back to the derived release image. selected names that
// aren't present in deployments are skipped. Output is sorted by deployment name
// for a stable confirm screen.
func PlanChanges(deployments []k8s.Deployment, selected []string, repoImage, tag string) []Change {
	want := make(map[string]struct{}, len(selected))
	for _, s := range selected {
		want[s] = struct{}{}
	}
	byName := make(map[string]k8s.Deployment, len(deployments))
	for _, d := range deployments {
		byName[d.Name] = d
	}

	out := make([]Change, 0, len(selected))
	for name := range want {
		d, ok := byName[name]
		if !ok {
			continue
		}
		container, fromTag, repo := matchContainer(d, repoImage)
		image := repo + ":" + tag
		out = append(out, Change{
			Namespace:  d.Namespace,
			Deployment: d.Name,
			Container:  container,
			FromTag:    fromTag,
			ToTag:      tag,
			Image:      image,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Deployment < out[j].Deployment })
	return out
}

// matchContainer chooses which container of a deployment to set, returning its
// name (empty = the "*" wildcard), the tag it currently runs, and the repository
// to build the target image from.
//
//   - Single container → ("", its tag, its repo): use "*" (no name needed).
//   - Multiple containers → the one whose repository matches repoImage
//     (case-insensitive): (name, its tag, its repo).
//   - Multiple but none match → ("", primary's tag, repoImage): fall back to "*"
//     on the supplied release image.
func matchContainer(d k8s.Deployment, repoImage string) (container, fromTag, repo string) {
	switch len(d.Images) {
	case 0:
		return "", "", repoImage
	case 1:
		img := d.Images[0]
		return "", img.Tag, img.Repository
	}
	want := strings.ToLower(repoImage)
	for _, img := range d.Images {
		if strings.ToLower(img.Repository) == want {
			return img.Name, img.Tag, img.Repository
		}
	}
	// No container matched the release repo — fall back to the wildcard on the
	// release image, reporting the primary container's tag as "from".
	return "", d.Images[0].Tag, repoImage
}

// ── small string helpers (local to keep the package dependency-free) ─────────

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

func splitNonEmpty(s, sep string) []string {
	out := []string{}
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
