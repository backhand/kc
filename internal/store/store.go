// Package store is the generic learning store (ActionHistory).
//
// Persists every action occurrence as (action, scope, params, ts) to a JSON
// file, keyed by cluster × app, and ranks params by recency-weighted frequency
// so views can pre-fill the most-likely choice (SPEC: "predict, then confirm").
// Generic by design — deploy is the first consumer; logs / restart / shell
// adopt it later with no rework.
//
// Design notes:
//   - Base dir is injectable. Default ~/.kc, but tests MUST pass a temp dir so
//     real ~/.kc is never touched. There is no implicit global path beyond the
//     default.
//   - No telemetry — a local file only.
//   - Distinct params are compared by a stable canonical JSON key, so
//     {a:1,b:2} and {b:2,a:1} count as the same params.
//
// Ported from tools/kc-bun/src/store/index.ts.
package store

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Params are free-form params for an action (e.g. {deployments: ["web"]}).
type Params map[string]any

// ActionRecord is one recorded action occurrence.
type ActionRecord struct {
	// Action is an arbitrary action name: "deploy" | "logs" | "restart" |
	// "shell" | …
	Action string `json:"action"`
	Params Params `json:"params"`
	// TS is epoch milliseconds.
	TS int64 `json:"ts"`
}

// Scope is the (cluster × app) scope an action's params are ranked within.
type Scope struct {
	// Cluster identifier (e.g. kube-context name).
	Cluster string `json:"cluster"`
	// App identifier (e.g. repo name or namespace).
	App string `json:"app"`
}

// ── On-disk shape ────────────────────────────────────────────────────────

type stateFile struct {
	// Version is the schema version, for forward migration.
	Version int            `json:"version"`
	Records []ActionRecord `json:"records"`
	// Scopes is the per-record scope, parallel to Records (kept flat for
	// simple appends).
	Scopes []Scope `json:"scopes"`
}

func emptyState() *stateFile {
	return &stateFile{Version: 1, Records: []ActionRecord{}, Scopes: []Scope{}}
}

// ── Canonicalisation & ranking (pure, exported for tests) ──────────────────

// CanonicalKey returns a stable string key for a value: object keys are sorted
// recursively (Go's encoding/json sorts map keys when marshalling), so key
// order does not matter. Arrays preserve their order.
func CanonicalKey(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(b)
}

// DefaultHalfLife is the recency half-life: occurrences ~2 weeks old count for
// half.
const DefaultHalfLife = 14 * 24 * time.Hour

// RankOptions configures recency weighting (injectable for deterministic
// tests).
type RankOptions struct {
	// Now in epoch ms for recency weighting; zero = time.Now().
	Now int64
	// HalfLife in ms; zero = DefaultHalfLife.
	HalfLife int64
}

// RankParams ranks distinct params by recency-weighted frequency.
//
// Each occurrence contributes weight 0.5^(age/halfLife); weights for the same
// params (by CanonicalKey) sum. Higher score ranks first; ties break by the
// most recent occurrence, then by insertion order (so "most-recent first" stays
// deterministic even when two records share a timestamp). Pure — operates on a
// record slice in insertion order (oldest → newest).
func RankParams(records []ActionRecord, opts RankOptions) []Params {
	now := opts.Now
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	halfLife := opts.HalfLife
	if halfLife == 0 {
		halfLife = int64(DefaultHalfLife / time.Millisecond)
	}

	type entry struct {
		params    Params
		score     float64
		lastTS    int64
		lastIndex int
	}
	acc := make(map[string]*entry)
	order := []*entry{} // stable iteration order = first-seen order
	for i, r := range records {
		key := CanonicalKey(r.Params)
		age := now - r.TS
		if age < 0 {
			age = 0
		}
		weight := math.Pow(0.5, float64(age)/float64(halfLife))
		if cur, ok := acc[key]; ok {
			cur.score += weight
			if r.TS > cur.lastTS {
				cur.lastTS = r.TS
			}
			cur.lastIndex = i
		} else {
			e := &entry{params: r.Params, score: weight, lastTS: r.TS, lastIndex: i}
			acc[key] = e
			order = append(order, e)
		}
	}

	sort.SliceStable(order, func(a, b int) bool {
		ea, eb := order[a], order[b]
		if ea.score != eb.score {
			return ea.score > eb.score
		}
		if ea.lastTS != eb.lastTS {
			return ea.lastTS > eb.lastTS
		}
		return ea.lastIndex > eb.lastIndex
	})

	out := make([]Params, 0, len(order))
	for _, e := range order {
		out = append(out, e.params)
	}
	return out
}

// NormalizeSet normalises a selection to a stable set: trimmed, de-duped,
// sorted.
func NormalizeSet(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := []string{}
	for _, s := range items {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ── Options ──────────────────────────────────────────────────────────────

// Options configure an ActionHistory.
type Options struct {
	// BaseDir is the base directory for state. State lives at
	// <BaseDir>/state.json. Empty defaults to ~/.kc. Tests must override this
	// with a temp dir.
	BaseDir string
	// Now is an injectable clock (epoch ms); nil defaults to time.Now.
	Now func() int64
	// HalfLife for ranking (ms); zero = DefaultHalfLife.
	HalfLife int64
}

func scopeKey(s Scope) string {
	return s.Cluster + " " + s.App
}

// ── ActionHistory ──────────────────────────────────────────────────────────

// ActionHistory is the learning store. Construct with an explicit BaseDir in
// tests; in the app the default ~/.kc is used. Load/persist is lazy and cached
// in-memory; writes are flushed to disk on each Record.
//
// Concurrency: state is a single-user local cache. Persistence is
// last-writer-wins with no file lock — two simultaneous kc processes could drop
// one record. That is an accepted trade-off: the data is non-authoritative
// ranking hints that self-heal on the next action, and a single interactive TUI
// does not write concurrently with itself.
type ActionHistory struct {
	baseDir   string
	statePath string
	now       func() int64
	halfLife  int64
	state     *stateFile
}

// New constructs an ActionHistory from Options.
func New(opts Options) *ActionHistory {
	baseDir := opts.BaseDir
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kc")
	}
	now := opts.Now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	return &ActionHistory{
		baseDir:   baseDir,
		statePath: filepath.Join(baseDir, "state.json"),
		now:       now,
		halfLife:  opts.HalfLife,
	}
}

// Path returns the resolved state file path (handy for tests / diagnostics).
func (h *ActionHistory) Path() string { return h.statePath }

// load reads state from disk into memory (idempotent). A missing file → empty
// state; a corrupt file → empty state (start clean rather than crash the tool).
func (h *ActionHistory) load() *stateFile {
	if h.state != nil {
		return h.state
	}
	data, err := os.ReadFile(h.statePath)
	if err != nil {
		h.state = emptyState()
		return h.state
	}
	var parsed stateFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		h.state = emptyState()
		return h.state
	}
	st := emptyState()
	if parsed.Records != nil {
		st.Records = parsed.Records
	}
	if parsed.Scopes != nil {
		st.Scopes = parsed.Scopes
	}
	// Defensive: keep records/scopes aligned.
	if len(st.Scopes) != len(st.Records) {
		n := min(len(st.Scopes), len(st.Records))
		st.Records = st.Records[:n]
		st.Scopes = st.Scopes[:n]
	}
	h.state = st
	return h.state
}

// flush persists in-memory state to disk, creating the base dir if needed.
func (h *ActionHistory) flush() error {
	if h.state == nil {
		return nil
	}
	if err := os.MkdirAll(h.baseDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(h.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(h.statePath, b, 0o644)
}

// Record stores one action occurrence (timestamped via the injected clock) and
// flushes to disk.
func (h *ActionHistory) Record(action string, scope Scope, params Params) error {
	st := h.load()
	st.Records = append(st.Records, ActionRecord{Action: action, Params: params, TS: h.now()})
	st.Scopes = append(st.Scopes, scope)
	return h.flush()
}

// slice returns records for one (action × scope), oldest→newest as stored.
func (h *ActionHistory) slice(action string, scope Scope) []ActionRecord {
	st := h.load()
	key := scopeKey(scope)
	out := []ActionRecord{}
	for i, r := range st.Records {
		if r.Action == action && scopeKey(st.Scopes[i]) == key {
			out = append(out, r)
		}
	}
	return out
}

// Ranked returns distinct params for an (action × scope), ranked
// recency-weighted, best first. The top entry is the pre-fill default.
func (h *ActionHistory) Ranked(action string, scope Scope) []Params {
	return RankParams(h.slice(action, scope), RankOptions{Now: h.now(), HalfLife: h.halfLife})
}

// ── Deploy-preset helpers ──────────────────────────────────────────────
//
// A "preset" is a distinct *set* of deployments deployed together. Each
// distinct set is one permutation; presets come back ranked (most-recent /
// most-used first), the top one pre-checked in the deploy modal.

// RecordDeploy records a deploy of a deployment set (set semantics: dedup +
// sort).
func (h *ActionHistory) RecordDeploy(scope Scope, deployments []string) error {
	set := NormalizeSet(deployments)
	// Store as []any so the round-tripped JSON shape matches a fresh decode
	// (canonical-key parity across record/reload).
	arr := make([]any, len(set))
	for i, s := range set {
		arr[i] = s
	}
	return h.Record("deploy", scope, Params{"deployments": arr})
}

// DeployPresets returns the ranked distinct deployment-sets for an app (the
// deploy presets). Each entry is a sorted, de-duplicated []string; the first is
// the most-likely preset.
func (h *ActionHistory) DeployPresets(scope Scope) [][]string {
	ranked := h.Ranked("deploy", scope)
	out := [][]string{}
	for _, p := range ranked {
		raw, ok := p["deployments"]
		if !ok {
			continue
		}
		arr, ok := raw.([]any)
		if !ok {
			continue
		}
		set := []string{}
		for _, v := range arr {
			if s, ok := v.(string); ok {
				set = append(set, s)
			}
		}
		out = append(out, set)
	}
	return out
}

// ── Search-history helpers ──────────────────────────────────────────────
//
// The search-everywhere modal (the `/` op) remembers past queries so an empty
// field can offer the most-recent ones. Unlike deploy presets these are ranked
// by pure RECENCY (newest first), not recency-weighted frequency — when you
// reopen search you want what you last looked for, not your most-typed term.

// RecordSearch records that the user ran a search for `query` (the query that
// led somewhere — recorded when a result is selected). Stored as
// Record("search", scope, {"q": query}); an empty/whitespace query is skipped
// so blanks never enter the recents.
func (h *ActionHistory) RecordSearch(scope Scope, query string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	return h.Record("search", scope, Params{"q": q})
}

// RecentSearches returns the n most-recent DISTINCT search queries for a scope,
// newest first (recency — NOT the frequency-weighted Ranked). It walks the
// scope's "search" records newest→oldest, collecting distinct non-empty queries
// until it has n. n <= 0 returns nil.
func (h *ActionHistory) RecentSearches(scope Scope, n int) []string {
	if n <= 0 {
		return nil
	}
	records := h.slice("search", scope) // oldest→newest
	out := make([]string, 0, n)
	seen := make(map[string]struct{})
	for i := len(records) - 1; i >= 0; i-- {
		raw, ok := records[i].Params["q"]
		if !ok {
			continue
		}
		q, ok := raw.(string)
		if !ok {
			continue
		}
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		if _, dup := seen[q]; dup {
			continue
		}
		seen[q] = struct{}{}
		out = append(out, q)
		if len(out) >= n {
			break
		}
	}
	return out
}
