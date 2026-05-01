package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowedDirs_AddRoundTrip(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)
	tmp := t.TempDir()

	if err := d.Add(tmp, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := d.All()
	if len(got) != 1 || got[0] != tmp {
		t.Errorf("All() = %v, want [%q]", got, tmp)
	}

	// Re-add same path is idempotent.
	if err := d.Add(tmp, false); err != nil {
		t.Errorf("re-Add should be idempotent, got: %v", err)
	}
	if len(d.All()) != 1 {
		t.Errorf("re-Add caused dup: %v", d.All())
	}
}

func TestAllowedDirs_PersistAcrossNew(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	tmp := t.TempDir()

	d1 := NewAllowedDirs(nil)
	if err := d1.Add(tmp, true); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d2 := NewAllowedDirs(nil)
	got := d2.All()
	if len(got) != 1 || got[0] != tmp {
		t.Errorf("after reload All() = %v, want [%q]", got, tmp)
	}
}

func TestAllowedDirs_RejectsNonDir(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)

	tmpfile := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(tmpfile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Add(tmpfile, false); err == nil {
		t.Errorf("Add(file) should error, got nil")
	}
	if err := d.Add("/nonexistent/path/should/never/exist", false); err == nil {
		t.Errorf("Add(missing) should error, got nil")
	}
}

func TestAllowedDirs_SystemPromptAddendum(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)
	if got := d.SystemPromptAddendum(); got != "" {
		t.Errorf("empty list should give empty addendum, got %q", got)
	}
	tmp := t.TempDir()
	_ = d.Add(tmp, false)
	got := d.SystemPromptAddendum()
	if !strings.Contains(got, tmp) {
		t.Errorf("addendum should mention %q, got %q", tmp, got)
	}
	if !strings.Contains(got, "Additional accessible directories") {
		t.Errorf("addendum missing header: %q", got)
	}
}

func TestAllowedDirs_RemoveErrorsOnMissing(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)
	if err := d.Remove("/tmp"); err == nil {
		t.Errorf("Remove of unknown dir should error")
	}
}
