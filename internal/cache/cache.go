// Package cache is the typed snapshot cache that makes kc startup feel instant.
//
// SPEC ("Startup & data freshness — optimistic caching"): on launch kc loads
// the last-known snapshot from ~/.kc/cache/ and renders it immediately (marked
// stale) while firing fresh fetches in the background; when fresh data lands it
// rewrites the cache. This package is the pure, testable read/write half of
// that — the optimistic orchestration lives in the app (step 3).
//
// A Cache[T] stores JSON-serialisable snapshots of type T keyed by a string
// (e.g. cluster, or cluster×repo), each stamped with a write time. Get returns
// the value, its age, and a found flag so the app can render-stale-then-refresh.
//
// NET-NEW for the Go rewrite (no TypeScript reference) — see SPEC.md.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// entry is the on-disk envelope: the snapshot plus the time it was written.
type entry[T any] struct {
	// Version is the schema version, for forward migration.
	Version int `json:"version"`
	// Key is the (raw) cache key, stored for diagnostics / collision sanity.
	Key string `json:"key"`
	// StoredAt is the write time (RFC3339 / time.Time JSON).
	StoredAt time.Time `json:"storedAt"`
	// Value is the cached snapshot.
	Value T `json:"value"`
}

// Cache is a typed, file-backed snapshot store generic over the snapshot type
// T. Construct with New. Each instance owns a namespace subdirectory under the
// base dir, so caches for different snapshot types never collide.
type Cache[T any] struct {
	dir string
	now func() time.Time
}

// Options configure a Cache.
type Options struct {
	// BaseDir is the base directory for cached snapshots. Files live under
	// <BaseDir>/cache/<namespace>/. Empty defaults to ~/.kc. Tests must
	// override this with a temp dir so real ~/.kc is never touched.
	BaseDir string
	// Namespace separates snapshot types under the cache dir (e.g. "overview",
	// "namespace", "releases"). Empty means files live directly under
	// <BaseDir>/cache/.
	Namespace string
	// Now is an injectable clock; nil defaults to time.Now. Tests use this to
	// make staleness deterministic.
	Now func() time.Time
}

// New constructs a typed Cache[T]. The base dir defaults to ~/.kc when empty.
func New[T any](opts Options) *Cache[T] {
	baseDir := opts.BaseDir
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kc")
	}
	dir := filepath.Join(baseDir, "cache")
	if opts.Namespace != "" {
		dir = filepath.Join(dir, opts.Namespace)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[T]{dir: dir, now: now}
}

// Dir returns the resolved directory where snapshots are written (handy for
// tests / diagnostics).
func (c *Cache[T]) Dir() string { return c.dir }

// fileFor maps a key to its snapshot file path. Keys are hashed so arbitrary
// strings (including ones with path separators, e.g. "cluster/repo") map to a
// single safe filename; a short readable prefix aids diagnostics.
func (c *Cache[T]) fileFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, slug(key)+"-"+hex.EncodeToString(sum[:8])+".json")
}

// Put writes value under key, stamped with the current time. The cache dir is
// created if needed. Writes are atomic (temp file + rename) so a crash mid-write
// never leaves a half-written snapshot that would fail to decode on the next
// startup.
func (c *Cache[T]) Put(key string, value T) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	e := entry[T]{Version: 1, Key: key, StoredAt: c.now(), Value: value}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	path := c.fileFor(key)
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Get reads the snapshot for key. It returns the value, its age (now − StoredAt
// via the injected clock), and a found flag. found is false (and age is zero)
// when nothing is cached for the key or the cached file is unreadable/corrupt —
// so a corrupt cache degrades to a normal cold start rather than an error.
func (c *Cache[T]) Get(key string) (value T, age time.Duration, found bool) {
	var zero T
	data, err := os.ReadFile(c.fileFor(key))
	if err != nil {
		return zero, 0, false
	}
	var e entry[T]
	if err := json.Unmarshal(data, &e); err != nil {
		return zero, 0, false
	}
	age = c.now().Sub(e.StoredAt)
	if age < 0 {
		age = 0
	}
	return e.Value, age, true
}

// slug reduces a key to a short, filesystem-safe, lowercase prefix for the file
// name (the hash suffix guarantees uniqueness).
func slug(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "key"
	}
	return s
}
