// Package github wraps the `gh` CLI for the deploy flow's version list.
//
// The N latest *releases* for a repo (pre-releases included and flagged), each
// annotated with:
//   - build status — cross-referenced from the release's "build & push" run
//     (`gh run list`): ready / building / failed / none.
//   - image availability — does the tag exist in GHCR?
//
// GHCR note: an authoritative registry check needs read:packages-scoped
// credentials. When the registry can't be queried (no creds / private package
// → 401/403), the probe returns AvailUnknown ("unknown") and callers fall back
// to build == BuildReady as a proxy. The probe is injectable so tests stay
// offline.
//
// Ported from tools/kc-bun/src/github.ts.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backhand/kc/internal/exec"
	"github.com/backhand/kc/internal/git"
)

// BuildStatus is the build status of a release's image, cross-referenced from
// Actions.
type BuildStatus string

const (
	// BuildReady — a publishing run for this tag completed successfully.
	BuildReady BuildStatus = "ready"
	// BuildBuilding — a publishing run for this tag is queued / in progress.
	BuildBuilding BuildStatus = "building"
	// BuildFailed — the publishing run for this tag failed / was cancelled.
	BuildFailed BuildStatus = "failed"
	// BuildNone — no publishing run found for this tag.
	BuildNone BuildStatus = "none"
)

// Availability is a tri-state for "does the tagged image exist in GHCR?":
// confirmed present, confirmed absent, or could-not-tell.
type Availability int

const (
	// AvailUnknown — could not be determined (no registry creds / probe
	// disabled). Callers may fall back to build == BuildReady as a proxy.
	AvailUnknown Availability = iota
	// AvailPresent — confirmed present in the registry.
	AvailPresent
	// AvailAbsent — confirmed absent.
	AvailAbsent
)

// ReleaseAnnotation is a release annotated with build status + image
// availability.
type ReleaseAnnotation struct {
	Tag string `json:"tag"`
	// Name is the release display name (falls back to tag).
	Name       string `json:"name"`
	Prerelease bool   `json:"prerelease"`
	// Latest is whether GitHub flags this as the latest non-prerelease.
	Latest      bool        `json:"latest"`
	PublishedAt string      `json:"publishedAt"`
	Build       BuildStatus `json:"build"`
	// BuildRunID is the Actions run id behind Build, when one was matched (0 if
	// none).
	BuildRunID int64 `json:"buildRunId"`
	// ImageAvailable is whether the tagged image exists in GHCR (tri-state).
	ImageAvailable Availability `json:"imageAvailable"`
}

// ── Raw `gh` JSON shapes ────────────────────────────────────────────────────

// RawRelease mirrors `gh release list --json
// tagName,name,isPrerelease,isDraft,isLatest,publishedAt`.
type RawRelease struct {
	TagName      string `json:"tagName"`
	Name         string `json:"name"`
	IsPrerelease bool   `json:"isPrerelease"`
	IsDraft      bool   `json:"isDraft"`
	IsLatest     bool   `json:"isLatest"`
	PublishedAt  string `json:"publishedAt"`
}

// RawRun mirrors `gh run list --json
// databaseId,headBranch,status,conclusion,workflowName,event,displayTitle`.
type RawRun struct {
	DatabaseID   int64  `json:"databaseId"`
	HeadBranch   string `json:"headBranch"`
	Status       string `json:"status"`     // queued | in_progress | completed | …
	Conclusion   string `json:"conclusion"` // success | failure | cancelled | … | ""
	WorkflowName string `json:"workflowName"`
	Event        string `json:"event"` // release | push | …
	DisplayTitle string `json:"displayTitle"`
}

// ── Build-status cross-referencing (pure) ──────────────────────────────────

// AnnotateBuild finds the Actions run that builds/publishes the image for tag
// and reduces it to a BuildStatus + run id (0 if none).
//
// Matching heuristic (robust to workflow renames): prefer a run triggered by
// the `release` event whose HeadBranch (the ref) or DisplayTitle equals the
// tag. The team's release build runs on event=release with HeadBranch set to
// the tag (e.g. v0.6.9).
//
// We pick the MOST RECENT matching run (highest DatabaseID), not the first:
// `gh run list` ordering is not contractually newest-first, and a failed build
// is often re-run to success for the same tag. GitHub run ids increase
// monotonically with time.
//
//	completed + success            → ready
//	completed + failure/cancelled… → failed
//	queued / in_progress           → building
//	no matching run                → none
func AnnotateBuild(tag string, runs []RawRun) (BuildStatus, int64) {
	var match *RawRun
	for i := range runs {
		r := &runs[i]
		if r.Event != "release" {
			continue
		}
		if r.HeadBranch != tag && r.DisplayTitle != tag {
			continue
		}
		if match == nil || r.DatabaseID > match.DatabaseID {
			match = r
		}
	}
	if match == nil {
		return BuildNone, 0
	}
	id := match.DatabaseID
	if match.Status != "completed" {
		// queued / in_progress / waiting / requested / pending → still building.
		return BuildBuilding, id
	}
	if match.Conclusion == "success" {
		return BuildReady, id
	}
	// failure | cancelled | timed_out | action_required | startup_failure | ""
	return BuildFailed, id
}

// AnnotateReleases combines releases + runs into annotations (pure). Image
// availability is filled in separately by GetReleases via the async probe; here
// it defaults to AvailUnknown unless an availability map is supplied.
func AnnotateReleases(releases []RawRelease, runs []RawRun, availability map[string]Availability) []ReleaseAnnotation {
	out := make([]ReleaseAnnotation, 0, len(releases))
	for _, r := range releases {
		if r.IsDraft {
			continue // drafts aren't deployable versions
		}
		build, runID := AnnotateBuild(r.TagName, runs)
		name := strings.TrimSpace(r.Name)
		if name == "" {
			name = r.TagName
		}
		avail := AvailUnknown
		if availability != nil {
			if a, ok := availability[r.TagName]; ok {
				avail = a
			}
		}
		out = append(out, ReleaseAnnotation{
			Tag:            r.TagName,
			Name:           name,
			Prerelease:     r.IsPrerelease,
			Latest:         r.IsLatest,
			PublishedAt:    r.PublishedAt,
			Build:          build,
			BuildRunID:     runID,
			ImageAvailable: avail,
		})
	}
	return out
}

// ── GHCR image availability (injectable probe) ──────────────────────────────

// ImageProbe resolves whether image:tag exists in a registry.
// AvailUnknown = couldn't tell.
type ImageProbe func(ctx context.Context, image, tag string) Availability

// ghcrTokenURL is the GHCR token-mint endpoint; var so tests can redirect it.
var ghcrTokenURL = "https://ghcr.io/token"

// ghcrRegistryURL is the GHCR v2 registry base; var so tests can redirect it.
var ghcrRegistryURL = "https://ghcr.io/v2"

// GHCRManifestProbe is the default GHCR probe: HEAD the OCI manifest for
// <image>:<tag>.
//
// Mints a pull token from the gh CLI's token and queries
// <registry>/<path>/manifests/<tag>:
//
//	200       → AvailPresent
//	404       → AvailAbsent
//	401 / 403 → AvailUnknown (no package-read permission — fall back)
//
// Returns AvailUnknown on any error so availability never blocks the version
// list.
func GHCRManifestProbe(ctx context.Context, image, tag string) Availability {
	// image looks like "ghcr.io/<owner>/<repo>"; strip the registry host.
	path, ok := strings.CutPrefix(image, "ghcr.io/")
	if !ok || path == "" {
		return AvailUnknown
	}

	res, err := exec.Run(ctx, "gh", []string{"auth", "token"}, exec.RunOptions{Timeout: 5 * time.Second})
	if err != nil {
		return AvailUnknown
	}
	token := strings.TrimSpace(res.Stdout)
	if token == "" {
		return AvailUnknown
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// GHCR mints a registry pull token when presented a GH token as the
	// password (Basic x:<token>).
	tokReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ghcrTokenURL+"?service=ghcr.io&scope=repository:"+path+":pull", nil)
	if err != nil {
		return AvailUnknown
	}
	tokReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x:"+token)))
	tokRes, err := client.Do(tokReq)
	if err != nil {
		return AvailUnknown
	}
	defer tokRes.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(tokRes.Body, 1<<20))
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &tok)
	if tok.Token == "" {
		return AvailUnknown
	}

	manifestURL := ghcrRegistryURL + "/" + path + "/manifests/" + url.PathEscape(tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return AvailUnknown
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	res2, err := client.Do(req)
	if err != nil {
		return AvailUnknown
	}
	defer res2.Body.Close()
	switch res2.StatusCode {
	case http.StatusOK:
		return AvailPresent
	case http.StatusNotFound:
		return AvailAbsent
	default:
		return AvailUnknown // 401/403/5xx → unknown
	}
}

// ── Async API ──────────────────────────────────────────────────────────────

// Options configures GetReleases.
type Options struct {
	// Limit is how many latest releases to fetch. Zero = 5 (the deploy modal
	// shows 5).
	Limit int
	// RunLimit is how many recent runs to scan for build status. Zero = 50.
	RunLimit int
	// Timeout is the per-command timeout (zero = exec.DefaultTimeout).
	Timeout time.Duration
	// GHCRImage is the image path to probe availability against
	// (e.g. ghcr.io/thinkpilot/mailon). Empty → availability stays
	// AvailUnknown for every release.
	GHCRImage string
	// Probe overrides the image-availability probe (tests pass a stub; default
	// is GHCRManifestProbe).
	Probe ImageProbe
}

func repoSlug(repo git.RepoRef) string {
	return repo.Owner + "/" + repo.Repo
}

// GetReleases returns the latest releases for a repo, annotated with build
// status + image availability.
//
// Degrades gracefully throughout: a repo with no releases → empty; a `gh` error
// (auth, network, repo gone) → empty rather than an error; build/availability
// annotation never fails on its own. The deploy modal can render whatever comes
// back without guarding.
func GetReleases(ctx context.Context, repo git.RepoRef, opts Options) []ReleaseAnnotation {
	slug := repoSlug(repo)
	limit := opts.Limit
	if limit <= 0 {
		limit = 5
	}
	runLimit := opts.RunLimit
	if runLimit <= 0 {
		runLimit = 50
	}

	var releases []RawRelease
	var runs []RawRun
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); releases = fetchReleases(ctx, slug, limit, opts.Timeout) }()
	go func() { defer wg.Done(); runs = fetchRuns(ctx, slug, runLimit, opts.Timeout) }()
	wg.Wait()

	// Probe image availability per tag (only when a GHCR image is supplied).
	var availability map[string]Availability
	if opts.GHCRImage != "" {
		probe := opts.Probe
		if probe == nil {
			probe = GHCRManifestProbe
		}
		availability = make(map[string]Availability, len(releases))
		var mu sync.Mutex
		var pg sync.WaitGroup
		for _, r := range releases {
			pg.Add(1)
			go func(tag string) {
				defer pg.Done()
				a := probe(ctx, opts.GHCRImage, tag)
				mu.Lock()
				availability[tag] = a
				mu.Unlock()
			}(r.TagName)
		}
		pg.Wait()
	}

	return AnnotateReleases(releases, runs, availability)
}

// fetchReleases runs `gh release list` → raw releases; nil on any error
// (e.g. no releases / gh hiccup).
func fetchReleases(ctx context.Context, slug string, limit int, timeout time.Duration) []RawRelease {
	var out []RawRelease
	err := ghJSON(ctx, []string{
		"release", "list",
		"--repo", slug,
		"--limit", strconv.Itoa(limit),
		"--json", "tagName,name,isPrerelease,isDraft,isLatest,publishedAt",
	}, timeout, &out)
	if err != nil {
		return nil
	}
	return out
}

// fetchRuns runs `gh run list` → raw runs; nil on any error.
func fetchRuns(ctx context.Context, slug string, runLimit int, timeout time.Duration) []RawRun {
	var out []RawRun
	err := ghJSON(ctx, []string{
		"run", "list",
		"--repo", slug,
		"--limit", strconv.Itoa(runLimit),
		"--json", "databaseId,headBranch,status,conclusion,workflowName,event,displayTitle",
	}, timeout, &out)
	if err != nil {
		return nil
	}
	return out
}

func ghJSON(ctx context.Context, args []string, timeout time.Duration, out any) error {
	return exec.RunJSON(ctx, "gh", args, exec.RunOptions{Timeout: timeout}, out)
}

// SortReleasesByPublished sorts annotations newest-first by PublishedAt; a
// convenience for callers that want deterministic order regardless of `gh`'s
// output ordering. (Not used by GetReleases, which preserves `gh`'s order.)
func SortReleasesByPublished(rs []ReleaseAnnotation) {
	sort.SliceStable(rs, func(i, j int) bool {
		return rs[i].PublishedAt > rs[j].PublishedAt
	})
}
