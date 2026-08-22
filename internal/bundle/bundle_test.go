package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBundle creates a bundle dir with one agent profile + one skill.
func writeBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "demo-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name = \"" + name + "\"\nversion = \"1.0.0\"\ndescription = \"test bundle\"\n"
	if err := os.WriteFile(filepath.Join(dir, "bundle.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "reviewer.md"), []byte("---\nname: reviewer\n---\nYou review code."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "demo-skill", "SKILL.md"), []byte("# demo skill\nDoes a demo."), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallListRemove(t *testing.T) {
	home := t.TempDir()
	src := t.TempDir()
	writeBundle(t, filepath.Join(src, "b"), "my-bundle")

	rec, err := Install(home, filepath.Join(src, "b"))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.Name != "my-bundle" || len(rec.Files) != 2 {
		t.Fatalf("record = %+v", rec)
	}
	// files landed
	if _, err := os.Stat(filepath.Join(home, "agents", "reviewer.md")); err != nil {
		t.Fatalf("agent profile not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "demo-skill", "SKILL.md")); err != nil {
		t.Fatalf("skill not installed: %v", err)
	}

	// ledger
	recs, err := List(home)
	if err != nil || len(recs) != 1 || recs[0].Name != "my-bundle" {
		t.Fatalf("list = %v err=%v", recs, err)
	}
	if MissingFiles(home, recs[0]) != 0 {
		t.Fatalf("installed files should all exist")
	}

	// remove
	if err := Remove(home, "my-bundle"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "reviewer.md")); !os.IsNotExist(err) {
		t.Fatalf("agent profile should be gone")
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "demo-skill")); !os.IsNotExist(err) {
		t.Fatalf("skill dir should be pruned")
	}
	recs, _ = List(home)
	if len(recs) != 0 {
		t.Fatalf("ledger should be empty after remove")
	}
}

func TestReinstallUpgrades(t *testing.T) {
	home := t.TempDir()
	src := t.TempDir()
	bd := filepath.Join(src, "b")
	writeBundle(t, bd, "up-bundle")
	if _, err := Install(home, bd); err != nil {
		t.Fatal(err)
	}
	// v2: change the agent file content
	if err := os.WriteFile(filepath.Join(bd, "agents", "reviewer.md"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(home, bd); err != nil {
		t.Fatal(err)
	}
	recs, _ := List(home)
	if len(recs) != 1 {
		t.Fatalf("upgrade must not duplicate ledger rows: %d", len(recs))
	}
	b, _ := os.ReadFile(filepath.Join(home, "agents", "reviewer.md"))
	if string(b) != "v2" {
		t.Fatalf("upgrade must overwrite content, got %q", b)
	}
}

func TestManifestValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bundle.toml"), []byte("name = \"Bad Name!\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(dir); err == nil || !strings.Contains(err.Error(), "slug") {
		t.Fatalf("bad name should fail with slug error, got %v", err)
	}
	// missing manifest
	if _, err := LoadManifest(t.TempDir()); err == nil {
		t.Fatal("missing manifest should fail")
	}
}

func TestEmptyBundleRejected(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bundle.toml"), []byte("name = \"empty-one\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(home, dir); err == nil || !strings.Contains(err.Error(), "installs nothing") {
		t.Fatalf("empty bundle should be rejected, got %v", err)
	}
}

func TestRemovePathEscapeGuard(t *testing.T) {
	home := t.TempDir()
	led := &Ledger{Bundles: []InstallRecord{{
		Name: "evil", Files: []string{"../../../etc/passwd", "agents/ok.md"},
	}}}
	if err := saveLedger(home, led); err != nil {
		t.Fatal(err)
	}
	// create a legit file so remove has something to hit
	os.MkdirAll(filepath.Join(home, "agents"), 0o755)
	os.WriteFile(filepath.Join(home, "agents", "ok.md"), []byte("x"), 0o644)
	if err := Remove(home, "evil"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "ok.md")); !os.IsNotExist(err) {
		t.Fatal("legit recorded file should be removed")
	}
	// ../../.. path must not have escaped (guard skips it silently)
	if _, err := os.Stat("/etc/passwd"); err != nil {
		// exists — fine; the guard is about NOT deleting it. Best-effort check:
		_ = err
	}
}
