package agent

// memory_scope_test.go — locks the G.6 three-layer memory scoping
// behavior added 2026-05-12. Pairs with internal/agent/memory.go
// Store / ScopeRoot / NewScopedStore / SaveTo / queryScoped.
//
// Eight contracts:
//
//   1. Legacy (single-Root) Store unchanged — Save+Query roundtrip
//      preserves all fields, Entry.Scope is empty.
//   2. NewScopedStore creates each scope directory on disk so the
//      caller doesn't need to mkdir manually.
//   3. Save targets scopes[0] (highest priority) by default.
//   4. SaveTo("project", ...) lands in the project directory and
//      Query returns it with Scope=="project".
//   5. Conflict resolution: same (Type, Key) in user + project +
//      local resolves to local (the highest scope).
//   6. SaveTo with an unknown scope label errors instead of
//      silently writing to /dev/null.
//   7. Query returns Entry.Scope correctly for every result.
//   8. limit clamping works across scopes (we accumulate matches
//      from all scopes and trim afterwards, not per-scope).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// scopeRoots constructs a 3-layer tree under t.TempDir() and returns
// the {label, dir} slice and the absolute paths for inspection.
func scopeRoots(t *testing.T) ([]ScopeRoot, [3]string) {
	t.Helper()
	root := t.TempDir()
	local := filepath.Join(root, "local")
	project := filepath.Join(root, "project")
	user := filepath.Join(root, "user")
	return []ScopeRoot{
		{Label: ScopeLocal, Dir: local},
		{Label: ScopeProject, Dir: project},
		{Label: ScopeUser, Dir: user},
	}, [3]string{local, project, user}
}

func TestStore_LegacyRoundtripUnaffected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(Entry{Type: "fact", Key: "lang", Value: "go"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Query("fact", "", 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].Value != "go" {
		t.Errorf("Value = %q, want go", got[0].Value)
	}
	if got[0].Scope != "" {
		t.Errorf("legacy mode should leave Scope empty; got %q", got[0].Scope)
	}
}

func TestNewScopedStore_CreatesDirs(t *testing.T) {
	t.Parallel()
	scopes, paths := scopeRoots(t)
	_ = NewScopedStore(scopes)
	for i, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("scope %d (%s) not created: %v", i, p, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("scope %d (%s) is not a directory", i, p)
		}
	}
}

func TestStore_SaveTargetsHighestPriority(t *testing.T) {
	t.Parallel()
	scopes, paths := scopeRoots(t)
	s := NewScopedStore(scopes)

	if err := s.Save(Entry{Type: "preference", Key: "editor", Value: "nvim"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File should appear under the local (priority 0) directory only.
	mustExist(t, filepath.Join(paths[0], "preference.jsonl"))
	mustNotExist(t, filepath.Join(paths[1], "preference.jsonl"))
	mustNotExist(t, filepath.Join(paths[2], "preference.jsonl"))
}

func TestStore_SaveToProjectScope(t *testing.T) {
	t.Parallel()
	scopes, paths := scopeRoots(t)
	s := NewScopedStore(scopes)

	if err := s.SaveTo(ScopeProject, Entry{Type: "fact", Key: "k1", Value: "from project"}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	mustExist(t, filepath.Join(paths[1], "fact.jsonl"))
	mustNotExist(t, filepath.Join(paths[0], "fact.jsonl"))

	got, err := s.Query("fact", "", 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].Scope != ScopeProject {
		t.Errorf("Scope = %q, want %q", got[0].Scope, ScopeProject)
	}
	if got[0].Value != "from project" {
		t.Errorf("Value = %q, want %q", got[0].Value, "from project")
	}
}

func TestStore_ConflictResolutionHighestScopeWins(t *testing.T) {
	t.Parallel()
	scopes, _ := scopeRoots(t)
	s := NewScopedStore(scopes)

	// Same (Type, Key) at three layers — local should win on read.
	if err := s.SaveTo(ScopeUser, Entry{Type: "fact", Key: "k1", Value: "user value"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTo(ScopeProject, Entry{Type: "fact", Key: "k1", Value: "project value"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTo(ScopeLocal, Entry{Type: "fact", Key: "k1", Value: "local value"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Query("fact", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("conflict resolution should dedup to 1 entry; got %d", len(got))
	}
	if got[0].Value != "local value" {
		t.Errorf("Value = %q, want %q (local should win)", got[0].Value, "local value")
	}
	if got[0].Scope != ScopeLocal {
		t.Errorf("Scope = %q, want %q", got[0].Scope, ScopeLocal)
	}
}

func TestStore_SaveTo_UnknownScopeErrors(t *testing.T) {
	t.Parallel()
	scopes, _ := scopeRoots(t)
	s := NewScopedStore(scopes)
	err := s.SaveTo("does-not-exist", Entry{Type: "fact", Key: "k", Value: "v"})
	if !errors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid for unknown scope; got %v", err)
	}
}

func TestStore_QueryAcrossScopes_NoConflict(t *testing.T) {
	t.Parallel()
	scopes, _ := scopeRoots(t)
	s := NewScopedStore(scopes)

	if err := s.SaveTo(ScopeUser, Entry{Type: "fact", Key: "only_user", Value: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTo(ScopeProject, Entry{Type: "fact", Key: "only_project", Value: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTo(ScopeLocal, Entry{Type: "fact", Key: "only_local", Value: "l"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Query("fact", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries (one per scope, no conflicts); got %d", len(got))
	}
	// Confirm scope labels propagate.
	seen := make(map[string]string)
	for _, e := range got {
		seen[e.Key] = e.Scope
	}
	want := map[string]string{
		"only_user":    ScopeUser,
		"only_project": ScopeProject,
		"only_local":   ScopeLocal,
	}
	for k, v := range want {
		if got, ok := seen[k]; !ok {
			t.Errorf("missing entry with key=%q", k)
		} else if got != v {
			t.Errorf("entry %q: Scope=%q, want %q", k, got, v)
		}
	}
}

func TestStore_QueryLimitClampsAcrossScopes(t *testing.T) {
	t.Parallel()
	scopes, _ := scopeRoots(t)
	s := NewScopedStore(scopes)

	for i, sc := range []string{ScopeUser, ScopeProject, ScopeLocal} {
		for j := 0; j < 3; j++ {
			_ = s.SaveTo(sc, Entry{
				Type:  "fact",
				Key:   fmt.Sprintf("s%d-k%d", i, j),
				Value: "v",
			})
		}
	}
	got, err := s.Query("fact", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("limit=5 should yield 5 entries; got %d", len(got))
	}
}

// helpers ----------------------------------------------------------

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist: %s (%v)", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("file should NOT exist: %s", path)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error: %v", err)
	}
}

