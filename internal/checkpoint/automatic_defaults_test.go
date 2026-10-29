package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHomeDefaultsToSparseWithoutScanningOrPersistingPreference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	unrelated := filepath.Join(home, "unrelated")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "private.txt"), []byte("not a target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unrelated, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unrelated, 0o700) })
	m := NewManager("automatic-home", home, "")
	status := m.ControlStatus()
	if !status.Enabled || !m.Sparse() || m.AutomaticDisabled() || m.Disabled() || len(status.Roots) != 0 || status.Revision != "auto" {
		t.Fatalf("home must automatically protect explicit file targets: %+v sparse=%v disabled=%v", status, m.Sparse(), m.Disabled())
	}
	if _, err := os.Stat(m.shadowDir); !os.IsNotExist(err) {
		t.Fatalf("construction initialized the home snapshot tree: %v", err)
	}
	_, control, err := controlPaths("automatic-home", home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(control); !os.IsNotExist(err) {
		t.Fatalf("automatic default persisted a user preference: %v", err)
	}
	file := filepath.Join(home, "edited.txt")
	if err := os.WriteFile(file, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := m.BaselinePathsContext(context.Background(), []string{file}, "", "Write", "", "automatic edit")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := m.treePaths(hash)
	_, captured := paths["edited.txt"]
	if err != nil || len(paths) != 1 || !captured {
		t.Fatalf("default home snapshot captured non-target paths: %v %v", paths, err)
	}
	if m.Protects(filepath.Join(unrelated, "private.txt")) {
		t.Fatal("unrelated home file became a managed rewind target")
	}
}

func TestHomeExplicitOffSurvivesNewAutomaticDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	m := NewManager("explicit-off", home, "")
	if _, err := m.Command(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	reopened := NewManager("explicit-off", home, "")
	if reopened.ControlStatus().Enabled || !reopened.AutomaticDisabled() || reopened.DisabledByPolicy() {
		t.Fatalf("explicit session preference was replaced by the new default: %+v", reopened.ControlStatus())
	}
	if other := NewManager("unconfigured-session", home, ""); !other.ControlStatus().Enabled || !other.Sparse() {
		t.Fatalf("an unrelated session inherited the explicit off preference: %+v", other.ControlStatus())
	}
}

func TestHomeAliasDefaultsToSparseWhileFilesystemRootStaysOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m := NewManager("home-alias", alias, "")
	if !m.ControlStatus().Enabled || !m.Sparse() || m.DisabledByPolicy() {
		t.Fatalf("home alias bypassed automatic sparse policy: %+v", m.ControlStatus())
	}
	root := filepath.VolumeName(home) + string(filepath.Separator)
	rootAlias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{root, rootAlias, ""} {
		m := NewManager("root-stays-off", cwd, "")
		if !m.DisabledByPolicy() || !m.AutomaticDisabled() || !m.Disabled() {
			t.Fatalf("filesystem/empty root unexpectedly enabled: %q", cwd)
		}
	}
}

func TestRewindMetadataPathIsPrivateAndSeparatesSessionWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	workspace := t.TempDir()
	shadow := t.TempDir()
	m := NewManager("rewind-owner", workspace, shadow)
	path := m.RewindMetadataPath()
	if filepath.Dir(path) != filepath.Join(m.shadowDir, ".git") {
		t.Fatalf("rewind metadata is not private shadow metadata: %s", path)
	}
	if path == NewManager("rewind-owner", t.TempDir(), shadow).RewindMetadataPath() || path == NewManager("other-owner", workspace, shadow).RewindMetadataPath() {
		t.Fatal("rewind metadata identity leaked across a session or workspace")
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(workspace, alias); err == nil {
		if path != NewManager("rewind-owner", alias, shadow).RewindMetadataPath() {
			t.Fatal("workspace aliases selected different rewind metadata")
		}
	}
	if _, err := os.Stat(m.shadowDir); !os.IsNotExist(err) {
		t.Fatalf("reading the metadata path initialized shadow storage: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const body = `{"test":"barrier saved before the first successful file snapshot"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "code.txt"), []byte("code"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := m.BaselineContext(context.Background(), "Write", "", "baseline after rewind metadata")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := m.treePaths(hash)
	if err != nil || len(paths) != 1 {
		t.Fatalf("rewind metadata leaked into file snapshots: %v %v", paths, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != body {
		t.Fatalf("snapshot initialization discarded rewind metadata: %q %v", got, err)
	}
}
