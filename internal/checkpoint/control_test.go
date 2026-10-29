package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlHomeScopePersistsAndDoesNotCaptureSiblings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	project := filepath.Join(home, "project with spaces")
	other := filepath.Join(home, "other")
	for _, dir := range []string{project, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager("scoped", home, "")
	if state, err := m.Command(context.Background(), "on"); err != nil || !state.Enabled || !m.Sparse() {
		t.Fatalf("home session switch: %+v %v", state, err)
	}
	state, err := m.Command(context.Background(), `on "project with spaces"`)
	if err != nil || !state.Enabled || len(state.Roots) != 1 {
		t.Fatalf("enable: %+v %v", state, err)
	}
	hash, err := m.BaselineContext(context.Background(), "test", "", "baseline")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := m.treePaths(hash)
	if err != nil || len(paths) != 1 {
		t.Fatalf("snapshot scope: %+v %v", paths, err)
	}
	if _, ok := paths["project with spaces/file.txt"]; !ok {
		t.Fatalf("wrong files: %+v", paths)
	}
	next := NewManager("scoped", home, "")
	if next.DisabledByPolicy() || !next.ControlStatus().Enabled {
		t.Fatal("scope did not survive reopening the same session")
	}
	if _, err := m.Command(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snap("test", "", "off"); err == nil {
		t.Fatal("off still snapshots")
	}
	if list, err := m.List(10); err != nil || len(list) == 0 {
		t.Fatalf("off removed old checkpoints: %+v %v", list, err)
	}
	if _, err := m.Command(context.Background(), "on other"); err != nil {
		t.Fatalf("change selected project: %v", err)
	}
	now, err := m.BaselineContext(context.Background(), "test", "", "changed scope")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := m.treePaths(now)
	if err != nil || len(selected) != 1 {
		t.Fatalf("changed scope retained foreign files: %+v %v", selected, err)
	}
	if _, ok := selected["other/file.txt"]; !ok {
		t.Fatalf("wrong changed scope: %+v", selected)
	}
}

func TestControlRejectsEscapesSymlinksAndActiveRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	m := NewManager("scope-reject", home, "")
	for _, args := range []string{"on /", "on ..", "on .", "on .metis", "on link", "off unexpected", "on \"unclosed"} {
		if _, err := m.Command(context.Background(), args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if _, err := m.Command(context.Background(), "on project"); err != nil {
		t.Fatal(err)
	}
	release, _, err := m.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	other := NewManager("scope-reject", home, "")
	if _, err := other.Command(context.Background(), "off"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("changed active settings: %v", err)
	}
	release()
	if _, err := other.Command(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	end, _, err := m.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	if m.ControlStatus().Enabled || !m.AutomaticDisabled() {
		t.Fatal("running CLI failed to load Desktop switch")
	}
}
