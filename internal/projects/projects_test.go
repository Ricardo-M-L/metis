package projects

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempHome scopes METIS_HOME to a tempdir so tests don't touch
// the developer's real ~/.metis/projects.json.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	return dir
}

func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	withTempHome(t)
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.Projects) != 0 {
		t.Errorf("missing file should yield empty registry; got %d", len(r.Projects))
	}
}

func TestRegister_AddsNewProject(t *testing.T) {
	dir := withTempHome(t)
	if err := Register("/p/api", dir); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list, _ := List()
	if len(list) != 1 || list[0].Path != "/p/api" {
		t.Fatalf("registry: %+v", list)
	}
	if list[0].LastAccessed.IsZero() {
		t.Error("LastAccessed should be set")
	}
	if list[0].DataDir != dir {
		t.Errorf("DataDir: got %q, want %q", list[0].DataDir, dir)
	}
}

func TestRegister_BumpsExistingProject(t *testing.T) {
	dir := withTempHome(t)
	Register("/p/api", dir)
	first, _ := List()
	firstTime := first[0].LastAccessed

	time.Sleep(5 * time.Millisecond) // ensure timestamp diverges
	if err := Register("/p/api", dir); err != nil {
		t.Fatal(err)
	}
	second, _ := List()
	if len(second) != 1 {
		t.Errorf("re-register should NOT duplicate; got %d entries", len(second))
	}
	if !second[0].LastAccessed.After(firstTime) {
		t.Error("re-register should bump LastAccessed forward")
	}
}

func TestList_SortsByRecency(t *testing.T) {
	dir := withTempHome(t)
	Register("/p/old", dir)
	time.Sleep(5 * time.Millisecond)
	Register("/p/new", dir)

	list, _ := List()
	if list[0].Path != "/p/new" {
		t.Errorf("most-recent should sort first; got %s", list[0].Path)
	}
}

func TestSave_Atomic(t *testing.T) {
	withTempHome(t)
	if err := Register("/p/api", "/d"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path()); err != nil {
		t.Errorf("projects.json should exist post-register: %v", err)
	}
	// .tmp file should NOT exist after a successful Save (atomic rename).
	if _, err := os.Stat(Path() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file should be cleaned up by atomic rename")
	}
}

func TestSave_FilePermissionsAreOwnerOnly(t *testing.T) {
	withTempHome(t)
	if err := Register("/p/api", "/d"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("projects.json should be 0o600 (private); got %o", mode)
	}
}

func TestRegister_EmptyPathSkipped(t *testing.T) {
	withTempHome(t)
	if err := Register("", "/d"); err != nil {
		t.Errorf("empty path should be silent no-op, not error; got %v", err)
	}
	list, _ := List()
	if len(list) != 0 {
		t.Errorf("empty registry expected; got %d entries", len(list))
	}
}

func TestRemove_DropsEntry(t *testing.T) {
	withTempHome(t)
	Register("/p/api", "/d")
	Register("/p/web", "/d")
	removed, err := Remove("/p/api")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("Remove should report removed=true on hit")
	}
	list, _ := List()
	if len(list) != 1 || list[0].Path != "/p/web" {
		t.Errorf("post-remove list: %+v", list)
	}
}

func TestRemove_MissingNoOp(t *testing.T) {
	withTempHome(t)
	Register("/p/api", "/d")
	removed, err := Remove("/p/notfound")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("Remove should report removed=false when entry missing")
	}
}

func TestPath_ResolvesUnderMetisHome(t *testing.T) {
	dir := withTempHome(t)
	want := filepath.Join(dir, "projects.json")
	if got := Path(); got != want {
		t.Errorf("Path: got %q, want %q", got, want)
	}
}
