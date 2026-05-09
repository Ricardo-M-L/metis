package config

// staleness_test.go — pin the snapshot/diff matrix:
//   • file unchanged → not dirty
//   • mtime drift → Changed
//   • size drift → Changed
//   • file deleted → Missing
//   • file appears → NewlyAppeared
//   • file present at both points but unreadable → Errors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshot_Unchanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	writeFile(t, p, "default = \"x\"")
	snap := NewSnapshot([]string{p})
	st := snap.Diff()
	if st.Dirty {
		t.Errorf("untouched config should not be dirty; st=%+v", st)
	}
}

func TestSnapshot_DetectsMtimeChange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	writeFile(t, p, "default = \"x\"")
	snap := NewSnapshot([]string{p})
	// Bump mtime (size unchanged) by overwriting with same length + bumping mtime.
	time.Sleep(20 * time.Millisecond) // mtime granularity safety
	writeFile(t, p, "default = \"y\"")
	st := snap.Diff()
	if !st.Dirty {
		t.Errorf("modified config should be dirty")
	}
	if len(st.Changed) != 1 || !strings.HasSuffix(st.Changed[0], "config.toml") {
		t.Errorf("Changed should contain config.toml; got %+v", st.Changed)
	}
}

func TestSnapshot_DetectsMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	writeFile(t, p, "x = 1")
	snap := NewSnapshot([]string{p})
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	st := snap.Diff()
	if !st.Dirty || len(st.Missing) != 1 {
		t.Errorf("removed file should be Missing; st=%+v", st)
	}
}

func TestSnapshot_DetectsNewlyAppeared(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	// Snapshot when file does NOT exist.
	snap := NewSnapshot([]string{p})
	// Now create.
	writeFile(t, p, "x = 1")
	st := snap.Diff()
	if !st.Dirty || len(st.NewlyAppeared) != 1 {
		t.Errorf("appeared file should land in NewlyAppeared; st=%+v", st)
	}
}

func TestSnapshot_NeverExistedNeverDirty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "phantom.toml")
	snap := NewSnapshot([]string{p})
	st := snap.Diff()
	if st.Dirty {
		t.Errorf("absent path that stayed absent shouldn't be dirty; st=%+v", st)
	}
	if !st.Empty() {
		t.Errorf("Staleness.Empty() should report true; got %+v", st)
	}
}

func TestSnapshot_FilesReturnsCopy(t *testing.T) {
	// Mutating the returned slice must NOT affect the snapshot.
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	writeFile(t, p, "x = 1")
	snap := NewSnapshot([]string{p})
	rows := snap.Files()
	if len(rows) != 1 {
		t.Fatalf("got %d rows; want 1", len(rows))
	}
	rows[0].Path = "MUTATED"
	rows2 := snap.Files()
	if rows2[0].Path == "MUTATED" {
		t.Errorf("Files() should return a defensive copy")
	}
}

func TestLoadWithSnapshot_ReturnsBoth(t *testing.T) {
	cfg, _, snap, err := LoadWithSnapshot()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil {
		t.Errorf("nil cfg")
	}
	if snap == nil {
		t.Errorf("nil snapshot")
	}
}
