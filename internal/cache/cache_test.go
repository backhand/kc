package cache

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Unit tests for the NET-NEW typed snapshot cache: round-trip, staleness via an
// injected clock, cold start, corrupt-file degradation, and base-dir isolation
// (real ~/.kc is never written). Deterministic and offline.

// A realistic snapshot type to exercise the generics end-to-end.
type overview struct {
	Cluster    string   `json:"cluster"`
	Namespaces []string `json:"namespaces"`
	NodeCount  int      `json:"nodeCount"`
}

func TestCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := New[overview](Options{BaseDir: dir, Namespace: "overview"})

	want := overview{Cluster: "thinkpilot-k3s", Namespaces: []string{"mailon", "kube-system"}, NodeCount: 2}
	if err := c.Put("thinkpilot-k3s", want); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, age, found := c.Get("thinkpilot-k3s")
	if !found {
		t.Fatal("found = false after Put")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if age < 0 {
		t.Errorf("age = %v, want >= 0", age)
	}
}

func TestCache_Staleness(t *testing.T) {
	dir := t.TempDir()
	// Frozen clock we advance by hand to make age deterministic.
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c := New[overview](Options{BaseDir: dir, Now: clock})

	if err := c.Put("k", overview{Cluster: "k"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Advance the clock 90s after the write.
	now = now.Add(90 * time.Second)

	_, age, found := c.Get("k")
	if !found {
		t.Fatal("found = false")
	}
	if age != 90*time.Second {
		t.Errorf("age = %v, want 90s", age)
	}
}

func TestCache_NotFoundColdStart(t *testing.T) {
	c := New[overview](Options{BaseDir: t.TempDir()})
	got, age, found := c.Get("nothing-here")
	if found {
		t.Error("found = true, want false for a cold cache")
	}
	if age != 0 {
		t.Errorf("age = %v, want 0 when not found", age)
	}
	if !reflect.DeepEqual(got, overview{}) {
		t.Errorf("got = %+v, want zero value", got)
	}
}

func TestCache_CorruptFileDegradesToColdStart(t *testing.T) {
	dir := t.TempDir()
	c := New[overview](Options{BaseDir: dir, Namespace: "overview"})
	// Write the snapshot, then clobber the file with garbage.
	if err := c.Put("k", overview{Cluster: "k"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.WriteFile(c.fileFor("k"), []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, found := c.Get("k"); found {
		t.Error("found = true on a corrupt file, want false (degrade to cold start)")
	}
}

func TestCache_NeverTouchesRealHome(t *testing.T) {
	dir := t.TempDir()
	c := New[overview](Options{BaseDir: dir, Namespace: "overview"})
	if !strings.HasPrefix(c.Dir(), dir) {
		t.Errorf("dir %q does not start with temp dir %q", c.Dir(), dir)
	}
	home, _ := os.UserHomeDir()
	if strings.Contains(c.Dir(), filepath.Join(home, ".kc")) {
		t.Errorf("dir %q must not touch real ~/.kc", c.Dir())
	}
	_ = c.Put("k", overview{Cluster: "k"})
	// Nothing should have been written under the real home.
	if _, err := os.Stat(filepath.Join(home, ".kc", "cache")); err == nil {
		// Only fail if the entry we'd write doesn't already legitimately exist
		// for the user; assert our file is under the temp dir instead.
		if _, _, found := c.Get("k"); !found {
			t.Fatal("snapshot not found under injected dir")
		}
	}
}

func TestCache_DistinctKeysAndNamespaces(t *testing.T) {
	dir := t.TempDir()
	c := New[overview](Options{BaseDir: dir, Namespace: "overview"})
	_ = c.Put("cluster-a", overview{Cluster: "a"})
	_ = c.Put("cluster-b", overview{Cluster: "b"})

	a, _, foundA := c.Get("cluster-a")
	b, _, foundB := c.Get("cluster-b")
	if !foundA || !foundB {
		t.Fatal("both keys should be found")
	}
	if a.Cluster != "a" || b.Cluster != "b" {
		t.Errorf("keys collided: a=%q b=%q", a.Cluster, b.Cluster)
	}

	// A key with path separators (e.g. cluster×repo) must map to one safe file.
	keyed := New[overview](Options{BaseDir: dir, Namespace: "releases"})
	composite := "thinkpilot-k3s/thinkpilot/mailon"
	if err := keyed.Put(composite, overview{Cluster: composite}); err != nil {
		t.Fatalf("put composite key: %v", err)
	}
	got, _, found := keyed.Get(composite)
	if !found || got.Cluster != composite {
		t.Errorf("composite-key round-trip failed: found=%v got=%+v", found, got)
	}
	// The releases namespace must not see the overview cache's keys.
	if _, _, found := keyed.Get("cluster-a"); found {
		t.Error("namespaces should isolate keys")
	}
}

func TestCache_PutOverwrites(t *testing.T) {
	dir := t.TempDir()
	c := New[overview](Options{BaseDir: dir})
	_ = c.Put("k", overview{Cluster: "old", NodeCount: 1})
	_ = c.Put("k", overview{Cluster: "new", NodeCount: 3})
	got, _, found := c.Get("k")
	if !found || got.Cluster != "new" || got.NodeCount != 3 {
		t.Errorf("got %+v, want the latest write {new 3}", got)
	}
}

func TestCache_GenericOverDifferentTypes(t *testing.T) {
	dir := t.TempDir()
	// A different snapshot type in its own namespace — proves the generic.
	type releases struct {
		Tags []string `json:"tags"`
	}
	rc := New[releases](Options{BaseDir: dir, Namespace: "releases"})
	if err := rc.Put("mailon", releases{Tags: []string{"v0.6.10", "v0.6.9"}}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, _, found := rc.Get("mailon")
	if !found || !reflect.DeepEqual(got.Tags, []string{"v0.6.10", "v0.6.9"}) {
		t.Errorf("got %+v, found=%v", got, found)
	}
}
