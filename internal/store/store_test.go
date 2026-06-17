package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Unit tests for the learning store: canonicalisation, recency-weighted
// ranking, deploy presets, and a round-trip to a TEMP dir.
//
// Every persistence test injects an explicit temp BaseDir — real ~/.kc is never
// written.
// Ported from tools/kc-bun/test/store.test.ts.

var scope = Scope{Cluster: "thinkpilot-k3s", App: "mailon"}

// ── Pure helpers ────────────────────────────────────────────────────────────

func TestCanonicalKey(t *testing.T) {
	if CanonicalKey(map[string]any{"a": 1, "b": 2}) != CanonicalKey(map[string]any{"b": 2, "a": 1}) {
		t.Error("key order should not matter")
	}
	if CanonicalKey(map[string]any{"x": map[string]any{"p": 1, "q": 2}, "list": []any{1, 2}}) !=
		CanonicalKey(map[string]any{"list": []any{1, 2}, "x": map[string]any{"q": 2, "p": 1}}) {
		t.Error("nested objects/arrays should normalise")
	}
	if CanonicalKey(map[string]any{"deployments": []any{"web"}}) ==
		CanonicalKey(map[string]any{"deployments": []any{"web", "sender"}}) {
		t.Error("distinct values should differ")
	}
}

func TestNormalizeSet(t *testing.T) {
	got := NormalizeSet([]string{" web ", "sender", "web", "", "sender"})
	if !reflect.DeepEqual(got, []string{"sender", "web"}) {
		t.Errorf("NormalizeSet = %v, want [sender web]", got)
	}
}

func TestRankParams_FrequencyAndRecency(t *testing.T) {
	const now int64 = 1_000_000_000_000
	const day int64 = 24 * 60 * 60 * 1000

	t.Run("more frequent ranks higher", func(t *testing.T) {
		records := []ActionRecord{
			{Action: "deploy", Params: Params{"d": []any{"web"}}, TS: now - day},
			{Action: "deploy", Params: Params{"d": []any{"web"}}, TS: now - 2*day},
			{Action: "deploy", Params: Params{"d": []any{"sender"}}, TS: now - day},
		}
		ranked := RankParams(records, RankOptions{Now: now})
		if len(ranked) != 2 {
			t.Fatalf("got %d distinct, want 2", len(ranked))
		}
		if !reflect.DeepEqual(ranked[0]["d"], []any{"web"}) {
			t.Errorf("top = %v, want web", ranked[0]["d"])
		}
	})

	t.Run("recency outweighs old single with short half-life", func(t *testing.T) {
		records := []ActionRecord{
			{Action: "deploy", Params: Params{"d": []any{"old"}}, TS: now - 60*day},
			{Action: "deploy", Params: Params{"d": []any{"fresh"}}, TS: now},
		}
		ranked := RankParams(records, RankOptions{Now: now, HalfLife: day})
		if !reflect.DeepEqual(ranked[0]["d"], []any{"fresh"}) {
			t.Errorf("top = %v, want fresh", ranked[0]["d"])
		}
	})

	t.Run("ties broken by most-recent occurrence", func(t *testing.T) {
		records := []ActionRecord{
			{Action: "deploy", Params: Params{"d": []any{"a"}}, TS: now - 10*day},
			{Action: "deploy", Params: Params{"d": []any{"b"}}, TS: now - 1*day},
		}
		ranked := RankParams(records, RankOptions{Now: now, HalfLife: 10_000 * day})
		if !reflect.DeepEqual(ranked[0]["d"], []any{"b"}) {
			t.Errorf("top = %v, want b (more recent)", ranked[0]["d"])
		}
	})
}

// ── Persistence round-trip (TEMP dir) ───────────────────────────────────────

func TestActionHistory_NeverResolvesRealHomeWhenBaseDirInjected(t *testing.T) {
	dir := t.TempDir()
	h := New(Options{BaseDir: dir})
	if !strings.HasPrefix(h.Path(), dir) {
		t.Errorf("path %q does not start with temp dir %q", h.Path(), dir)
	}
	home, _ := os.UserHomeDir()
	if strings.Contains(h.Path(), filepath.Join(home, ".kc")) {
		t.Errorf("path %q must not touch real ~/.kc", h.Path())
	}
}

func TestActionHistory_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	const t0 int64 = 1_700_000_000_000
	write := New(Options{BaseDir: dir, Now: func() int64 { return t0 }})
	if err := write.Record("deploy", scope, Params{"deployments": []any{"web"}}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := os.Stat(write.Path()); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	// A brand-new instance reads the same persisted state.
	read := New(Options{BaseDir: dir})
	ranked := read.Ranked("deploy", scope)
	if len(ranked) != 1 || !reflect.DeepEqual(ranked[0]["deployments"], []any{"web"}) {
		t.Errorf("reloaded ranked = %v, want [{deployments:[web]}]", ranked)
	}
}

func TestActionHistory_ScopeIsolation(t *testing.T) {
	dir := t.TempDir()
	h := New(Options{BaseDir: dir})
	_ = h.RecordDeploy(Scope{Cluster: "c1", App: "mailon"}, []string{"web"})
	_ = h.RecordDeploy(Scope{Cluster: "c1", App: "other"}, []string{"api"})
	_ = h.RecordDeploy(Scope{Cluster: "c2", App: "mailon"}, []string{"sender"})

	check := func(s Scope, want [][]string) {
		t.Helper()
		if got := h.DeployPresets(s); !reflect.DeepEqual(got, want) {
			t.Errorf("DeployPresets(%+v) = %v, want %v", s, got, want)
		}
	}
	check(Scope{Cluster: "c1", App: "mailon"}, [][]string{{"web"}})
	check(Scope{Cluster: "c1", App: "other"}, [][]string{{"api"}})
	check(Scope{Cluster: "c2", App: "mailon"}, [][]string{{"sender"}})
}

func TestActionHistory_MissingStateFile(t *testing.T) {
	h := New(Options{BaseDir: filepath.Join(t.TempDir(), "does-not-exist-yet")})
	if got := h.Ranked("deploy", scope); len(got) != 0 {
		t.Errorf("ranked = %v, want empty", got)
	}
	if got := h.DeployPresets(scope); len(got) != 0 {
		t.Errorf("presets = %v, want empty", got)
	}
}

func TestActionHistory_CorruptStateFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{ not valid json "), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(Options{BaseDir: dir})
	if got := h.Ranked("deploy", scope); len(got) != 0 {
		t.Errorf("ranked = %v, want empty (corrupt → clean start)", got)
	}
}

// ── Deploy presets — the spec's headline scenario ───────────────────────────

func TestDeployPresets_TwoPresetsMostRecentFirst(t *testing.T) {
	dir := t.TempDir()
	clock := int64(1_700_000_000_000)
	h := New(Options{BaseDir: dir, Now: func() int64 { return clock }})

	clock += 1000
	_ = h.RecordDeploy(scope, []string{"web"})
	clock += 1000
	_ = h.RecordDeploy(scope, []string{"responder", "sender"})

	presets := h.DeployPresets(scope)
	if len(presets) != 2 {
		t.Fatalf("got %d presets, want 2", len(presets))
	}
	if !reflect.DeepEqual(presets[0], []string{"responder", "sender"}) {
		t.Errorf("preset[0] = %v, want [responder sender] (most recent)", presets[0])
	}
	if !reflect.DeepEqual(presets[1], []string{"web"}) {
		t.Errorf("preset[1] = %v, want [web]", presets[1])
	}
}

func TestDeployPresets_MostRecentFirstWhenTimestampsTie(t *testing.T) {
	// Two deploys within the same millisecond → identical ts. Insertion order
	// (the lastIndex tie-break) must still decide "most recent".
	dir := t.TempDir()
	const frozen int64 = 1_700_000_000_000
	h := New(Options{BaseDir: dir, Now: func() int64 { return frozen }})
	_ = h.RecordDeploy(scope, []string{"web"})
	_ = h.RecordDeploy(scope, []string{"responder", "sender"})

	presets := h.DeployPresets(scope)
	if !reflect.DeepEqual(presets[0], []string{"responder", "sender"}) {
		t.Errorf("preset[0] = %v, want [responder sender]", presets[0])
	}
	if !reflect.DeepEqual(presets[1], []string{"web"}) {
		t.Errorf("preset[1] = %v, want [web]", presets[1])
	}
}

func TestDeployPresets_RedeploySameSetRanksNotCounts(t *testing.T) {
	dir := t.TempDir()
	clock := int64(1_700_000_000_000)
	h := New(Options{BaseDir: dir, Now: func() int64 { return clock }})

	// [web] deployed twice, [sender] once but more recently.
	clock += 1000
	_ = h.RecordDeploy(scope, []string{"web"})
	clock += 1000
	_ = h.RecordDeploy(scope, []string{"web"})
	clock += 1000
	_ = h.RecordDeploy(scope, []string{"sender"})

	presets := h.DeployPresets(scope)
	if len(presets) != 2 {
		t.Fatalf("got %d presets, want 2", len(presets))
	}
	// [web] has higher frequency → ranks first under the default half-life.
	if !reflect.DeepEqual(presets[0], []string{"web"}) {
		t.Errorf("preset[0] = %v, want [web] (higher frequency)", presets[0])
	}

	// Order-insensitive: [sender,responder] then [responder,sender] is the same
	// permutation.
	clock += 1000
	_ = h.RecordDeploy(scope, []string{"sender", "responder"})
	clock += 1000
	_ = h.RecordDeploy(scope, []string{"responder", "sender"})
	presets2 := h.DeployPresets(scope)
	count := 0
	for _, p := range presets2 {
		if len(p) == 2 && p[0] == "responder" && p[1] == "sender" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("[responder sender] permutation appears %d times, want 1", count)
	}
}

// ── Default half-life sanity (uses real DefaultHalfLife) ─────────────────────

func TestRankParams_DefaultHalfLife(t *testing.T) {
	now := time.Now().UnixMilli()
	day := int64(24 * time.Hour / time.Millisecond)
	records := []ActionRecord{
		{Action: "deploy", Params: Params{"d": []any{"recent"}}, TS: now - day},
		{Action: "deploy", Params: Params{"d": []any{"ancient"}}, TS: now - 365*day},
	}
	ranked := RankParams(records, RankOptions{Now: now}) // default half-life
	if !reflect.DeepEqual(ranked[0]["d"], []any{"recent"}) {
		t.Errorf("top = %v, want recent under the default 2-week half-life", ranked[0]["d"])
	}
}
