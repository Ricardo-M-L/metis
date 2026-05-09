package config

// staleness.go — pin a "what was the config file like when I read it?"
// snapshot, then compare against the current filesystem state on
// demand. Used by:
//
//   • metis_info introspection — surfaces "config drifted" to the LLM
//     so it knows whether `/reload` is worth suggesting.
//   • future auto-reload trigger (#13 reload exception) — when staleness
//     turns Dirty mid-session, the runtime can offer an inline reload.
//
// Mirrors crush's config/store.go:ConfigStaleness — same field names
// where they map cleanly. Differs in that we don't try to preserve
// state across CLI invocations: each `metis chat` takes a fresh
// snapshot and compares against it. Persisting between processes
// would require a side-file we'd then need to keep in sync, and the
// gain is marginal.

import (
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

// FileSnapshot is what we recorded about one config file at Load time.
// Zero ModTime / zero Size with Exists=false means "the path was
// checked at load time and was missing then" — not the same as
// "we never looked".
type FileSnapshot struct {
	Path    string
	Exists  bool
	Size    int64
	ModTime time.Time
}

// Snapshot bundles the per-file state for every config path Load()
// considered (whether or not it existed). The lock is for safe
// concurrent reads from MetisInfo / hot-reload paths; Set is called
// once at Load.
type Snapshot struct {
	mu    sync.RWMutex
	files []FileSnapshot
}

// NewSnapshot builds a Snapshot over the given paths, stat'ing each.
// Paths that don't exist are still recorded (with Exists=false) so a
// later "did anything appear?" check can flip to Dirty.
func NewSnapshot(paths []string) *Snapshot {
	s := &Snapshot{}
	out := make([]FileSnapshot, 0, len(paths))
	for _, p := range paths {
		fs := FileSnapshot{Path: p}
		if st, err := os.Stat(p); err == nil {
			fs.Exists = true
			fs.Size = st.Size()
			fs.ModTime = st.ModTime()
		}
		out = append(out, fs)
	}
	s.files = out
	return s
}

// Files returns a defensive copy of the recorded snapshot rows. Used
// by metis_info and tests; do not expose the internal slice or
// callers could mutate the snapshot's view of "what was".
func (s *Snapshot) Files() []FileSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FileSnapshot, len(s.files))
	copy(out, s.files)
	return out
}

// Staleness is the diff Snapshot.Diff returns. Dirty is the master
// bool — true when ANY file has Changed / Missing / NewlyAppeared /
// is unstat'able. The detail fields surface which paths drifted so
// callers can render specifics.
type Staleness struct {
	Dirty         bool
	Changed       []string         // path → mtime or size differs
	Missing       []string         // path was loaded, now gone
	NewlyAppeared []string         // path didn't exist at load, exists now (config init mid-session)
	Errors        map[string]error // path → stat error (perm, ENOENT race, ...)
}

// Empty reports whether the staleness diff has nothing to say.
// Equivalent to !s.Dirty when no errors are present; we keep it as a
// helper so callers don't have to remember the convention.
func (st Staleness) Empty() bool {
	return !st.Dirty && len(st.Changed) == 0 && len(st.Missing) == 0 && len(st.NewlyAppeared) == 0 && len(st.Errors) == 0
}

// Diff stat()s every recorded path and returns the difference vs the
// snapshot. Reads are concurrent-safe; each Diff allocates its own
// result so concurrent callers don't see each other's intermediate
// state.
func (s *Snapshot) Diff() Staleness {
	s.mu.RLock()
	files := make([]FileSnapshot, len(s.files))
	copy(files, s.files)
	s.mu.RUnlock()

	var st Staleness
	for _, prev := range files {
		cur, err := os.Stat(prev.Path)
		switch {
		case err != nil && errors.Is(err, os.ErrNotExist):
			if prev.Exists {
				st.Missing = append(st.Missing, prev.Path)
				st.Dirty = true
			}
			// else: didn't exist before, doesn't exist now — boring.
		case err != nil:
			if st.Errors == nil {
				st.Errors = map[string]error{}
			}
			st.Errors[prev.Path] = err
			st.Dirty = true
		default:
			if !prev.Exists {
				st.NewlyAppeared = append(st.NewlyAppeared, prev.Path)
				st.Dirty = true
				continue
			}
			if cur.Size() != prev.Size || !cur.ModTime().Equal(prev.ModTime) {
				st.Changed = append(st.Changed, prev.Path)
				st.Dirty = true
			}
		}
	}
	sort.Strings(st.Changed)
	sort.Strings(st.Missing)
	sort.Strings(st.NewlyAppeared)
	return st
}

// LoadWithSnapshot is Load + a same-time-of-day Snapshot of the paths
// it considered (loaded or not). Returning the snapshot lets the
// runtime later call Snapshot.Diff() to detect external edits.
//
// Kept as a wrapper rather than changing Load's signature so existing
// callers (metis tools / metis diag / unit tests) don't break.
func LoadWithSnapshot() (*Config, []string, *Snapshot, error) {
	cfg, loaded, err := Load()
	if err != nil {
		return cfg, loaded, nil, err
	}
	// Snapshot every path searchPaths considered, NOT just those that
	// existed at load time — so a config.toml that's created
	// mid-session shows up as NewlyAppeared on next Diff.
	snap := NewSnapshot(searchPaths())
	return cfg, loaded, snap, nil
}
