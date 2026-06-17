// Package git derives repo context from the cwd.
//
// Is the cwd in a git work tree? Parse the `origin` remote → {owner, repo}
// (both ssh and https forms) → derive the GHCR image `ghcr.io/<owner>/<repo>`.
// The entry-point view and repo→namespace resolution build on this.
//
// Ported from tools/kc-bun/src/git.ts.
package git

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/exec"
)

// RepoRef is a parsed git origin → GitHub coordinates.
type RepoRef struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// RepoContext is full repo context derived from a directory.
type RepoContext struct {
	// InRepo is true when the dir resolves into a git work tree.
	InRepo bool `json:"inRepo"`
	// Root is the absolute path to the repo root, or empty when not in a repo.
	Root string `json:"root"`
	// Remote is the parsed owner/repo from the origin remote, or nil.
	Remote *RepoRef `json:"remote"`
	// GHCRImage is the derived `ghcr.io/<owner>/<repo>`, or empty.
	GHCRImage string `json:"ghcrImage"`
}

// scpLike matches the scp-like ssh syntax: [user@]host:owner/repo(.git).
// It is distinguished from a URL by having ":" but no "://".
var scpLike = regexp.MustCompile(`^[^/]+@[^/:]+:(.+)$`)

// ParseRemote parses a git remote URL into a RepoRef.
//
// Handles the forms the team actually uses:
//   - ssh scp-like : git@github.com:thinkpilot/mailon.git
//   - ssh url      : ssh://git@github.com/thinkpilot/mailon.git
//   - https        : https://github.com/thinkpilot/mailon.git
//   - https w/ user: https://user@github.com/thinkpilot/mailon
//   - no .git suffix, optional trailing slash
//
// Returns nil for anything it can't confidently parse.
func ParseRemote(rawURL string) *RepoRef {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil
	}

	var path string
	if m := scpLike.FindStringSubmatch(trimmed); m != nil && !strings.Contains(trimmed, "://") {
		path = m[1]
	} else {
		u, err := url.Parse(trimmed)
		if err != nil {
			return nil
		}
		path = u.Path
	}

	// Normalise: strip leading/trailing slashes and a trailing ".git".
	cleaned := strings.TrimLeft(path, "/")
	cleaned = stripGitSuffix(cleaned)
	cleaned = strings.TrimRight(cleaned, "/")

	segments := splitNonEmpty(cleaned, "/")
	if len(segments) < 2 {
		return nil
	}
	owner := segments[0]
	repo := segments[len(segments)-1]
	if owner == "" || repo == "" {
		return nil
	}
	return &RepoRef{Owner: owner, Repo: repo}
}

// GHCRImageFor derives the GHCR image path for a repo:
// `ghcr.io/<owner>/<repo>` (lowercased — GHCR/OCI image names are lowercase
// while GitHub orgs/repos may carry caps).
func GHCRImageFor(ref RepoRef) string {
	return "ghcr.io/" + strings.ToLower(ref.Owner) + "/" + strings.ToLower(ref.Repo)
}

// GetRepoContext resolves full repo context for a directory (empty dir = the
// process cwd).
//
// Degrades gracefully: outside a repo → {InRepo:false, …}; in a repo with no
// origin → {InRepo:true, Remote:nil, GHCRImage:""}.
func GetRepoContext(ctx context.Context, dir string) (RepoContext, error) {
	ro := exec.RunOptions{Dir: dir}

	// 1) Are we inside a work tree, and where is its root?
	res, err := exec.Run(ctx, "git", []string{"rev-parse", "--show-toplevel"}, ro)
	if err != nil {
		if exec.IsExecError(err) {
			// Not a repo (git exits 128) — a normal, expected outcome.
			return RepoContext{}, nil
		}
		return RepoContext{}, err
	}
	root := strings.TrimSpace(res.Stdout)
	if root == "" {
		return RepoContext{}, nil
	}

	// 2) origin URL → {owner, repo}. Missing origin is fine.
	var remote *RepoRef
	if r, err := exec.Run(ctx, "git", []string{"remote", "get-url", "origin"}, ro); err == nil {
		remote = ParseRemote(r.Stdout)
	}

	ghcr := ""
	if remote != nil {
		ghcr = GHCRImageFor(*remote)
	}
	return RepoContext{InRepo: true, Root: root, Remote: remote, GHCRImage: ghcr}, nil
}

// stripGitSuffix removes a trailing ".git" (case-insensitive).
func stripGitSuffix(s string) string {
	if len(s) >= 4 && strings.EqualFold(s[len(s)-4:], ".git") {
		return s[:len(s)-4]
	}
	return s
}

// splitNonEmpty splits s on sep, dropping empty segments.
func splitNonEmpty(s, sep string) []string {
	out := []string{}
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
