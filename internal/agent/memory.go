package agent

// Package memory implements the auto-memory and skill-nudge system.
// Inspired by Hermes' AIAgent memory nudge (every N turns) and skill nudge
// (every M iterations when skill_manage tool was used). Tracks when to
// suggest memory updates and skill creation based on agent activity patterns.
//
// G.6 (2026-05-12) — three-layer scoping: a Store can carry an
// ordered `Scopes []string` list naming directories (highest priority
// first). When set, Query merges entries across scopes with the
// higher-priority scope winning on (Type, Key) conflicts; Save writes
// to scopes[0]. Mirrors the user/project/local cascade that
// internal/config/config.go uses for searchPaths.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Scope name constants — used as the human-friendly label returned in
// Entry.Scope on Query results so the UI can render "from: project".
// These are conventional names; callers can pass anything via
// NewScopedStore, but stick to these for cross-tool consistency.
const (
	ScopeLocal   = "local"
	ScopeProject = "project"
	ScopeUser    = "user"
)

// Store persists memory entries to JSONL under a root directory.
//
// G.6 — when Scopes is non-empty, Query reads from every scope
// directory (priority order: scopes[0] highest) and merges by
// (Type, Key). Save writes to scopes[0] only — explicit cross-scope
// writes go through SaveTo(scope, entry).
type Store struct {
	// Root is the single-directory legacy path. Used when Scopes is
	// empty, and as the fallback for paths in Scopes.
	Root string

	// Scopes is the ordered list of {label, dir} pairs the Store
	// reads from. Highest-priority scope first. Empty = single-root
	// legacy mode.
	Scopes []ScopeRoot
}

// ScopeRoot pairs a human label with a directory. The label
// propagates onto Entry.Scope so callers know where a memory came
// from without manual path inspection.
type ScopeRoot struct {
	Label string
	Dir   string
}

// NewStore returns a Store that persists under dir.
func NewStore(dir string) *Store {
	os.MkdirAll(dir, 0o755)
	return &Store{Root: dir}
}

// NewScopedStore returns a Store that reads from `scopes` in priority
// order (scopes[0] = highest). Save writes to scopes[0]. Each
// directory is auto-created on first call. Empty input is treated as
// "no scoping" — caller should use NewStore for that case but we
// don't panic.
//
// Typical wiring (chat REPL):
//
//	NewScopedStore([]ScopeRoot{
//	    {Label: ScopeLocal,   Dir: filepath.Join(cwd, ".metis/memory-local")},
//	    {Label: ScopeProject, Dir: filepath.Join(cwd, ".metis/memory")},
//	    {Label: ScopeUser,    Dir: filepath.Join(home, ".metis/memory")},
//	})
//
// Mirrors the user/project/local cascade in internal/config/searchPaths
// — the precedent the user-validated reuse-score 9/10 in the plan call.
func NewScopedStore(scopes []ScopeRoot) *Store {
	if len(scopes) == 0 {
		return &Store{}
	}
	s := &Store{
		Root:   scopes[0].Dir, // writes target the top-priority scope
		Scopes: append([]ScopeRoot(nil), scopes...),
	}
	for _, sc := range s.Scopes {
		if sc.Dir != "" {
			_ = os.MkdirAll(sc.Dir, 0o755)
		}
	}
	return s
}

// Entry is one memory item.
type Entry struct {
	Type      string   `json:"type"` // "fact" | "preference" | "context"
	Key       string   `json:"key"`
	Value     string   `json:"value"`
	Source    string   `json:"source,omitempty"` // which tool / turn produced it
	CreatedAt string   `json:"created_at"`
	Tags      []string `json:"tags,omitempty"`
	// Scope is populated by Query results (G.6, 2026-05-12) — it
	// tells the caller which scope this entry came from
	// ("local"/"project"/"user"/etc). NOT serialized to JSONL —
	// derived at read time from the file that produced the entry.
	Scope string `json:"-"`
}

// Fact records a factual finding.
type Fact struct {
	Content string `json:"content"`
	Tags    []string
}

// Preference records a user preference.
type Preference struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Save persists an entry to the store. When the Store is scoped,
// Save writes to scopes[0] (highest priority). The serialized JSON
// does NOT include Scope — it's set at Query time from whichever
// directory the entry was read out of.
func (s *Store) Save(e Entry) error {
	dir := s.writeDir()
	if dir == "" {
		return os.ErrInvalid
	}
	f, err := os.OpenFile(filepath.Join(dir, e.Type+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	e.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	// Encode never emits the Scope field (struct tag `json:"-"`).
	return json.NewEncoder(f).Encode(e)
}

// SaveTo persists an entry into a specific named scope (G.6,
// 2026-05-12). The scope must be one of the labels passed to
// NewScopedStore; unknown labels return an error so misspellings
// surface immediately instead of writing to /dev/null.
func (s *Store) SaveTo(scope string, e Entry) error {
	dir := s.dirForScope(scope)
	if dir == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, e.Type+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	e.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	return json.NewEncoder(f).Encode(e)
}

// writeDir resolves the default write directory:
//   - scopes[0] when Scopes is set (G.6 scoped mode)
//   - Root otherwise (single-directory legacy mode)
func (s *Store) writeDir() string {
	if len(s.Scopes) > 0 {
		return s.Scopes[0].Dir
	}
	return s.Root
}

// dirForScope returns the directory associated with `label`, or ""
// when the label isn't part of this Store's Scopes.
func (s *Store) dirForScope(label string) string {
	for _, sc := range s.Scopes {
		if sc.Label == label {
			return sc.Dir
		}
	}
	return ""
}

// Query searches memory entries by type/tag/key substring. Returns newest first.
//
// In scoped mode (G.6) reads from every scope in priority order and
// dedups by (Type, Key) — the highest-priority scope wins on
// conflicts. Each returned Entry carries its source scope label in
// Entry.Scope so callers can render "from: local" / "from: project".
func (s *Store) Query(typ, keySubstr string, limit int) ([]Entry, error) {
	if len(s.Scopes) == 0 {
		return s.queryLegacy(typ, keySubstr, limit)
	}
	return s.queryScoped(typ, keySubstr, limit)
}

// queryLegacy preserves the single-root behavior used by callers that
// didn't opt into scoping. Identical to the pre-G.6 Query path.
func (s *Store) queryLegacy(typ, keySubstr string, limit int) ([]Entry, error) {
	var entries []Entry
	if typ == "" {
		types := []string{"fact", "preference", "context"}
		for _, t := range types {
			ents, err := s.loadFile(filepath.Join(s.Root, t+".jsonl"), keySubstr, "", limit)
			if err != nil {
				continue
			}
			entries = append(entries, ents...)
		}
	} else {
		var err error
		entries, err = s.loadFile(filepath.Join(s.Root, typ+".jsonl"), keySubstr, "", limit)
		if err != nil {
			return nil, err
		}
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// queryScoped walks every scope's JSONL files and applies the
// (Type, Key) precedence rule: the entry from the highest-priority
// scope wins. Within a single scope, entries are newest-first (file
// append order reversed); across scopes the merge preserves the
// per-(Type, Key) record but the overall ordering is then
// re-sorted newest-first across scopes too so a freshly-written
// local memory shows up first even if older project memories exist.
func (s *Store) queryScoped(typ, keySubstr string, limit int) ([]Entry, error) {
	seen := make(map[string]struct{})
	var winners []Entry
	types := []string{typ}
	if typ == "" {
		types = []string{"fact", "preference", "context"}
	}
	// Walk scopes high → low priority. For each (Type, Key) we keep
	// the FIRST occurrence so the higher-priority scope wins.
	for _, sc := range s.Scopes {
		for _, t := range types {
			ents, err := s.loadFile(filepath.Join(sc.Dir, t+".jsonl"), keySubstr, sc.Label, 0)
			if err != nil {
				continue
			}
			for _, e := range ents {
				key := e.Type + "\x00" + e.Key
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				winners = append(winners, e)
			}
		}
	}
	// Newest-first across scopes — use CreatedAt for ordering.
	sortEntriesNewestFirst(winners)
	if limit > 0 && len(winners) > limit {
		winners = winners[:limit]
	}
	return winners, nil
}

// sortEntriesNewestFirst sorts in-place by Entry.CreatedAt descending.
// Ties (same timestamp) keep their relative order — stable sort would
// be ideal but sort.Slice's stability isn't guaranteed; for our use
// case (rare timestamp collisions) the simple comparison is fine.
func sortEntriesNewestFirst(entries []Entry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j-1].CreatedAt < entries[j].CreatedAt; j-- {
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
}

// loadFile decodes a single JSONL file. `scopeLabel` is stamped onto
// every returned Entry's Scope field so scoped Query results can
// render the source. Empty scopeLabel leaves Entry.Scope as "" (the
// pre-G.6 behavior).
func (s *Store) loadFile(path, keySubstr, scopeLabel string, limit int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []Entry
	d := json.NewDecoder(f)
	for d.More() {
		var e Entry
		if err := d.Decode(&e); err != nil {
			continue
		}
		if keySubstr != "" && !strings.Contains(strings.ToLower(e.Key+e.Value), strings.ToLower(keySubstr)) {
			continue
		}
		e.Scope = scopeLabel
		entries = append(entries, e)
	}
	// newest first within this file (append-order reversed).
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// Render returns a compact memory summary suitable for injection into system prompt.
func (s *Store) Render(facts, prefs, ctx int) (string, error) {
	var parts []string
	if facts > 0 {
		ents, _ := s.Query("fact", "", facts)
		if len(ents) > 0 {
			var lines []string
			for _, e := range ents {
				lines = append(lines, "- "+e.Value)
			}
			parts = append(parts, "## Facts\n"+strings.Join(lines, "\n"))
		}
	}
	if prefs > 0 {
		ents, _ := s.Query("preference", "", prefs)
		if len(ents) > 0 {
			var lines []string
			for _, e := range ents {
				lines = append(lines, "- "+e.Key+": "+e.Value)
			}
			parts = append(parts, "## Preferences\n"+strings.Join(lines, "\n"))
		}
	}
	if ctx > 0 {
		ents, _ := s.Query("context", "", ctx)
		if len(ents) > 0 {
			var lines []string
			for _, e := range ents {
				lines = append(lines, "- "+e.Value)
			}
			parts = append(parts, "## Recent Context\n"+strings.Join(lines, "\n"))
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "## Memory\n" + strings.Join(parts, "\n\n"), nil
}
