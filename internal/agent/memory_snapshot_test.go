package agent

// memory_snapshot_test.go — locks Phase G.10 (2026-05-12) snapshot
// contracts:
//
//   1. Snapshot(name) creates an on-disk copy of the current memory
//      under <writeDir>/snapshots/<name>/.
//   2. RestoreSnapshot(name) appends those entries back into the
//      live store (idempotent: ListSnapshots stays in tact).
//   3. ListSnapshots enumerates created snapshots.
//   4. Empty name / invalid name / unknown restore name → error.
//   5. Works for both legacy (single Root) and scoped stores —
//      snapshots use the writeDir, so scoped stores snapshot from
//      scopes[0] only (per design — see Snapshot doc).

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStore_SnapshotRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(Entry{Type: "fact", Key: "lang", Value: "go"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Entry{Type: "preference", Key: "editor", Value: "nvim"}); err != nil {
		t.Fatal(err)
	}

	if err := s.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Verify files exist under snapshots/first/.
	snapDir := filepath.Join(dir, "snapshots", "first")
	if _, err := os.Stat(filepath.Join(snapDir, "fact.jsonl")); err != nil {
		t.Errorf("snapshot fact.jsonl not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapDir, "preference.jsonl")); err != nil {
		t.Errorf("snapshot preference.jsonl not written: %v", err)
	}

	// Now wipe the live store and restore.
	os.Remove(filepath.Join(dir, "fact.jsonl"))
	os.Remove(filepath.Join(dir, "preference.jsonl"))

	if err := s.RestoreSnapshot("first"); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	facts, err := s.Query("fact", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Value != "go" {
		t.Errorf("Restored facts wrong: %v", facts)
	}
	prefs, err := s.Query("preference", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefs) != 1 || prefs[0].Value != "nvim" {
		t.Errorf("Restored prefs wrong: %v", prefs)
	}
}

func TestStore_SnapshotListsNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(dir)
	s.Save(Entry{Type: "fact", Key: "a", Value: "1"})

	if err := s.Snapshot("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot("beta"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"alpha": true, "beta": true}
	if len(got) != 2 {
		t.Fatalf("expected 2 snapshots, got %d (%v)", len(got), got)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected snapshot %q", name)
		}
	}
}

func TestStore_SnapshotEmptyDirReturnsNil(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	got, err := s.ListSnapshots()
	if err != nil {
		t.Errorf("empty dir should not error: %v", err)
	}
	if got != nil {
		t.Errorf("empty dir should return nil; got %v", got)
	}
}

func TestStore_SnapshotInvalidNames(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	cases := []string{
		"",           // empty
		".hidden",    // starts with dot
		"path/sep",   // contains /
		"with space", // contains space
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.Snapshot(name); !errors.Is(err, os.ErrInvalid) {
				t.Errorf("Snapshot(%q) should return os.ErrInvalid; got %v", name, err)
			}
		})
	}
}

func TestStore_RestoreSnapshotMissing(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	err := s.RestoreSnapshot("never-taken")
	if !os.IsNotExist(err) {
		t.Errorf("RestoreSnapshot of missing name should return IsNotExist; got %v", err)
	}
}

func TestStore_SnapshotWithScopedStoreUsesWriteDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scopes := []ScopeRoot{
		{Label: ScopeLocal, Dir: filepath.Join(root, "local")},
		{Label: ScopeProject, Dir: filepath.Join(root, "project")},
	}
	s := NewScopedStore(scopes)
	// Write a fact into scopes[0] via the default Save (which targets local).
	s.Save(Entry{Type: "fact", Key: "k", Value: "v"})
	if err := s.Snapshot("snap1"); err != nil {
		t.Fatal(err)
	}
	// Snapshot dir must be under scopes[0].
	wantSnap := filepath.Join(root, "local", "snapshots", "snap1", "fact.jsonl")
	if _, err := os.Stat(wantSnap); err != nil {
		t.Errorf("scoped snapshot file should be under scopes[0]: %v", err)
	}
	// It should NOT be under scopes[1].
	dontWant := filepath.Join(root, "project", "snapshots", "snap1")
	if _, err := os.Stat(dontWant); err == nil {
		t.Errorf("scoped snapshot leaked to scopes[1]: %s", dontWant)
	}
}

func TestStore_SnapshotEmptyTypeIsSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(dir)
	// Save only fact; preference + context not written.
	s.Save(Entry{Type: "fact", Key: "a", Value: "1"})

	if err := s.Snapshot("sparse"); err != nil {
		t.Fatal(err)
	}
	// Only fact.jsonl exists in the snapshot dir.
	snapDir := filepath.Join(dir, "snapshots", "sparse")
	if _, err := os.Stat(filepath.Join(snapDir, "fact.jsonl")); err != nil {
		t.Errorf("fact should be snapshotted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapDir, "preference.jsonl")); !os.IsNotExist(err) {
		t.Errorf("preference should NOT be snapshotted when empty; got err=%v", err)
	}
}
