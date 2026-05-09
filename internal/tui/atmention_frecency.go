package tui

// atmention_frecency.go — frecency layer for the @file picker.
//
// The base @-mention system (atmention.go) returns files in
// filesystem walk order. That's fine for a fresh repo, but in a
// project the user's already worked on, the most-accessed files
// should bubble to the top of the dropdown. This file adds:
//
//   - A persistent store at ~/.metis/atmention-frecency.json
//     mapping each accepted file path to {count, lastAccessAt}.
//   - frecencyScore() computing a hybrid frequency × decay score so
//     a file accessed yesterday outranks one accessed last month
//     even if both have similar counts.
//   - sortByFrecency() applied after fuzzyMatch in matchAtMention.
//
// Mirrors opencode's `prompt/frecency.tsx`. Score formula is the
// classic Mozilla bookmark frecency:
//
//	score = count × max(0.1, 1.0 − hoursAgo / halfLife)
//
// halfLife = 24h (after 24h since last access, score is roughly
// halved). Floor at 0.1 so old-but-much-used files don't drop to
// zero — they should still rank above never-seen ones.
//
// Persistence is lazy: the JSON file is read once per process, kept
// in memory, and rewritten only on explicit RecordMentionAccess
// calls. A 1000-entry cap (oldest evicted by lastAccessAt) keeps
// the file from growing without bound on multi-year projects.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

const (
	frecencyMaxEntries = 1000
	frecencyHalfLifeH  = 24.0
)

// frecencyEntry tracks how many times a path has been picked from
// the @-mention dropdown and when it was most recently picked.
type frecencyEntry struct {
	Count        int       `json:"count"`
	LastAccessAt time.Time `json:"last_access_at"`
}

// frecencyStore is the in-memory representation of the on-disk JSON.
// The whole map is keyed by the relative file path the user picked
// (matching what matchAtMention returns from atMentionIndex).
type frecencyStore struct {
	mu      sync.Mutex
	entries map[string]frecencyEntry
	loaded  bool
	dirty   bool
}

// frecencySingleton — module-private store. Tests reset via
// resetFrecencyStore().
var frecencySingleton = &frecencyStore{}

// frecencyPath returns ~/.metis/atmention-frecency.json. Resolved
// each call so a test that fakes config.Home() picks up the new
// path without bouncing the singleton.
func frecencyPath() string {
	return filepath.Join(config.Home(), "atmention-frecency.json")
}

// load reads the on-disk store into memory. Idempotent — second
// calls are a no-op until reset(). Missing/corrupt file → empty
// store, no error returned (a bad frecency file should never break
// the @-picker UX).
func (s *frecencyStore) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	s.loaded = true
	s.entries = map[string]frecencyEntry{}
	b, err := os.ReadFile(frecencyPath())
	if err != nil {
		return
	}
	var m map[string]frecencyEntry
	if err := json.Unmarshal(b, &m); err != nil {
		return
	}
	s.entries = m
}

// save writes the in-memory store back to disk. Best-effort — a
// write failure must never crash a chat session, so errors are
// swallowed. Called from RecordMentionAccess after every increment.
//
// Mode 0o600 because frecency leaks "what files this user works
// on most" — useful diagnostic, but not for cohabiting users to
// see.
func (s *frecencyStore) save() {
	if !s.dirty {
		return
	}
	b, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return
	}
	path := frecencyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, b, 0o600); err == nil {
		s.dirty = false
	}
}

// RecordMentionAccess increments the count and stamps the access
// time for path. Called from acceptAtMention every time the user
// picks a file from the dropdown.
//
// Eviction policy: when entries grow past frecencyMaxEntries, drop
// the oldest-by-LastAccessAt half. Cheap to do — runs maybe once a
// year on a heavy user.
func RecordMentionAccess(path string) {
	if path == "" {
		return
	}
	frecencySingleton.load()
	frecencySingleton.mu.Lock()
	defer frecencySingleton.mu.Unlock()

	e := frecencySingleton.entries[path]
	e.Count++
	e.LastAccessAt = time.Now()
	frecencySingleton.entries[path] = e
	frecencySingleton.dirty = true

	if len(frecencySingleton.entries) > frecencyMaxEntries {
		evictOldestHalf(frecencySingleton.entries)
	}
	// Save outside the lock would race with another writer; we hold
	// it through save() — call rate is bounded by user keystrokes
	// (≤ a few per second) so contention is irrelevant.
	frecencySingleton.save()
}

// evictOldestHalf removes the half of m with the oldest
// LastAccessAt. Linear-time + a sort; called rarely so the cost is
// fine.
func evictOldestHalf(m map[string]frecencyEntry) {
	type pair struct {
		path string
		at   time.Time
	}
	pairs := make([]pair, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, pair{k, v.LastAccessAt})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].at.Before(pairs[j].at)
	})
	for i := 0; i < len(pairs)/2; i++ {
		delete(m, pairs[i].path)
	}
}

// frecencyScore returns a non-negative score for path. Higher means
// "rank earlier in the dropdown." Zero means "never picked"; such
// paths sort below any path the user has touched.
//
// Mozilla bookmark formula:
//
//	score = count × max(0.1, 1.0 − hoursAgo / halfLife)
//
// The 0.1 floor preserves long-but-stale files above brand-new ones
// the user just learned exist (still rare to outrank a fresh hit
// because the count multiplier is small).
func frecencyScore(path string) float64 {
	frecencySingleton.load()
	frecencySingleton.mu.Lock()
	defer frecencySingleton.mu.Unlock()
	e, ok := frecencySingleton.entries[path]
	if !ok || e.Count == 0 {
		return 0
	}
	hoursAgo := time.Since(e.LastAccessAt).Hours()
	decay := 1.0 - hoursAgo/frecencyHalfLifeH
	if decay < 0.1 {
		decay = 0.1
	}
	return float64(e.Count) * decay
}

// sortByFrecency returns matches reordered with higher-frecency
// entries first. Stable: ties keep their input order so fuzzyMatch's
// own ordering survives where frecency is equal (typical for files
// the user has never picked).
//
// Allocates a new slice — caller's input is not mutated, matching
// matchAtMention's pure-function expectation.
func sortByFrecency(matches []string) []string {
	if len(matches) <= 1 {
		return matches
	}
	scored := make([]struct {
		path  string
		score float64
		idx   int
	}, len(matches))
	for i, m := range matches {
		scored[i].path = m
		scored[i].score = frecencyScore(m)
		scored[i].idx = i
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].idx < scored[j].idx
	})
	out := make([]string, len(matches))
	for i, s := range scored {
		out[i] = s.path
	}
	return out
}

// resetFrecencyStore is for tests only — clears the singleton's
// loaded state so the next call re-reads from disk (or sees an
// empty fresh store under a t.TempDir() HOME override).
func resetFrecencyStore() {
	frecencySingleton.mu.Lock()
	defer frecencySingleton.mu.Unlock()
	frecencySingleton.entries = nil
	frecencySingleton.loaded = false
	frecencySingleton.dirty = false
}
