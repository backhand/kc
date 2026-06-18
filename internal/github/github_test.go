package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Unit tests for release annotation: build-status cross-ref + image
// availability, against captured live `gh` fixtures (mailon).
// Ported from tools/kc-bun/test/github.test.ts.

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

func annotateBuildRuns() []RawRun {
	return []RawRun{
		{DatabaseID: 1, Event: "release", HeadBranch: "v1.0.0", Status: "completed", Conclusion: "success", WorkflowName: "Build & push container image"},
		{DatabaseID: 2, Event: "release", HeadBranch: "v1.1.0", Status: "completed", Conclusion: "failure", WorkflowName: "Build & push container image"},
		{DatabaseID: 3, Event: "release", HeadBranch: "v1.2.0", Status: "in_progress", Conclusion: "", WorkflowName: "Build & push container image"},
		// A non-release push run that mentions a tag in its title — must NOT match.
		{DatabaseID: 4, Event: "push", HeadBranch: "master", DisplayTitle: "chore(release): 2.0.0", Status: "completed", Conclusion: "success", WorkflowName: "CI"},
	}
}

func TestAnnotateBuild(t *testing.T) {
	runs := annotateBuildRuns()
	check := func(tag string, wantBuild BuildStatus, wantID int64) {
		t.Helper()
		b, id := AnnotateBuild(tag, runs)
		if b != wantBuild || id != wantID {
			t.Errorf("AnnotateBuild(%q) = (%s, %d), want (%s, %d)", tag, b, id, wantBuild, wantID)
		}
	}
	check("v1.0.0", BuildReady, 1)
	check("v1.1.0", BuildFailed, 2)
	check("v1.2.0", BuildBuilding, 3)
	check("v9.9.9", BuildNone, 0)
	// "2.0.0" only appears in a push run's title → none.
	check("2.0.0", BuildNone, 0)
}

func TestAnnotateBuild_MatchesViaDisplayTitle(t *testing.T) {
	runs := []RawRun{
		{DatabaseID: 9, Event: "release", HeadBranch: "refs/tags/v3.0.0", DisplayTitle: "v3.0.0", Status: "completed", Conclusion: "success"},
	}
	if b, id := AnnotateBuild("v3.0.0", runs); b != BuildReady || id != 9 {
		t.Errorf("got (%s, %d), want (ready, 9)", b, id)
	}
}

func TestAnnotateBuild_PicksMostRecentRun(t *testing.T) {
	// A failed build (older id) re-run to success (newer id) for the same tag.
	runs := []RawRun{
		{DatabaseID: 50, Event: "release", HeadBranch: "v4.0.0", Status: "completed", Conclusion: "success"},
		{DatabaseID: 40, Event: "release", HeadBranch: "v4.0.0", Status: "completed", Conclusion: "failure"},
	}
	if b, id := AnnotateBuild("v4.0.0", runs); b != BuildReady || id != 50 {
		t.Errorf("got (%s, %d), want (ready, 50)", b, id)
	}
	// Reversed order → still the newest (id 50) wins.
	rev := []RawRun{runs[1], runs[0]}
	if b, id := AnnotateBuild("v4.0.0", rev); b != BuildReady || id != 50 {
		t.Errorf("reversed: got (%s, %d), want (ready, 50)", b, id)
	}
}

func TestAnnotateReleases_FlagsAndAvailability(t *testing.T) {
	releases := []RawRelease{
		{TagName: "v2.0.0", IsLatest: true, IsPrerelease: false},
		{TagName: "v2.1.0-rc.1", IsPrerelease: true},
		{TagName: "draft-x", IsDraft: true}, // dropped
	}
	runs := []RawRun{
		{DatabaseID: 1, Event: "release", HeadBranch: "v2.0.0", Status: "completed", Conclusion: "success"},
	}
	availability := map[string]Availability{
		"v2.0.0":      AvailPresent,
		"v2.1.0-rc.1": AvailUnknown,
	}
	out := AnnotateReleases(releases, runs, availability)
	if len(out) != 2 {
		t.Fatalf("got %d annotations, want 2 (draft filtered)", len(out))
	}
	byTag := map[string]ReleaseAnnotation{}
	for _, r := range out {
		byTag[r.Tag] = r
	}
	stable := byTag["v2.0.0"]
	if !stable.Latest || stable.Prerelease || stable.Build != BuildReady || stable.ImageAvailable != AvailPresent {
		t.Errorf("v2.0.0 = %+v, want latest+ready+present", stable)
	}
	rc := byTag["v2.1.0-rc.1"]
	if !rc.Prerelease || rc.Build != BuildNone || rc.ImageAvailable != AvailUnknown {
		t.Errorf("v2.1.0-rc.1 = %+v, want prerelease+none+unknown", rc)
	}
}

func TestAnnotateReleases_DefaultsUnknownAndNameFallback(t *testing.T) {
	out := AnnotateReleases([]RawRelease{{TagName: "v1.0.0"}}, nil, nil)
	if out[0].ImageAvailable != AvailUnknown {
		t.Errorf("imageAvailable = %v, want unknown when no availability map", out[0].ImageAvailable)
	}
	if out[0].Name != "v1.0.0" {
		t.Errorf("name = %q, want fallback to tag", out[0].Name)
	}
}

func TestAnnotateReleases_LiveMailonFixtures(t *testing.T) {
	releases := loadFixture[[]RawRelease](t, "mailon-releases.json")
	runs := loadFixture[[]RawRun](t, "mailon-runs.json")
	out := AnnotateReleases(releases, runs, nil)
	byTag := map[string]ReleaseAnnotation{}
	for _, r := range out {
		byTag[r.Tag] = r
	}
	// From the captured run fixture: v0.6.9 failed, v0.6.5 ready, v0.6.10 none.
	if byTag["v0.6.9"].Build != BuildFailed {
		t.Errorf("v0.6.9 build = %s, want failed", byTag["v0.6.9"].Build)
	}
	if byTag["v0.6.5"].Build != BuildReady {
		t.Errorf("v0.6.5 build = %s, want ready", byTag["v0.6.5"].Build)
	}
	if byTag["v0.6.10"].Build != BuildNone {
		t.Errorf("v0.6.10 build = %s, want none", byTag["v0.6.10"].Build)
	}
	if !byTag["v0.6.10"].Latest {
		t.Error("v0.6.10 should be the latest published release")
	}
	for _, r := range out {
		if r.ImageAvailable != AvailUnknown {
			t.Errorf("%s availability = %v, want unknown (none supplied)", r.Tag, r.ImageAvailable)
		}
	}
}

// TestReduceRun covers the (status, conclusion) → BuildStatus reduction shared by
// AnnotateBuild and the single-run RunStatus poll (the deploy "wait for build").
func TestReduceRun(t *testing.T) {
	cases := []struct {
		status, conclusion string
		want               BuildStatus
	}{
		{"queued", "", BuildBuilding},
		{"in_progress", "", BuildBuilding},
		{"waiting", "", BuildBuilding},
		{"completed", "success", BuildReady},
		{"completed", "failure", BuildFailed},
		{"completed", "cancelled", BuildFailed},
		{"completed", "timed_out", BuildFailed},
		{"completed", "", BuildFailed},
	}
	for _, c := range cases {
		if got := reduceRun(c.status, c.conclusion); got != c.want {
			t.Errorf("reduceRun(%q,%q) = %v, want %v", c.status, c.conclusion, got, c.want)
		}
	}
}
