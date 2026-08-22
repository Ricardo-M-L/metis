package pluginmarket

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogShowsBundledMarketplacesBeforeFirstSync(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	catalog := NewManager().Catalog()
	if len(catalog.Marketplaces) != 3 {
		t.Fatalf("bundled marketplaces = %d, want 3: %+v", len(catalog.Marketplaces), catalog.Marketplaces)
	}
	if !catalog.NeedsSync || len(catalog.Plugins) != 0 {
		t.Fatalf("first-run catalog = %+v, want unsynced empty plugin list", catalog)
	}
}

func TestInstallRejectsMarketplaceSymlinkAndDoesNotCreatePlugin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "marketplaces.json"), []byte(`{
  "fixture": {"source": {"source": "github", "repo": "example/plugins"}}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "plugins", "marketplaces", "fixture")
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("do not copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-plugin")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	manifest := `{"plugins":[{"name":"linked-plugin","source":"./linked-plugin"}]}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewManager().Install(context.Background(), "linked-plugin", "fixture")
	if err == nil {
		t.Fatal("symlinked marketplace source was installed")
	}
	if _, statErr := os.Stat(filepath.Join(home, "plugins", "linked-plugin")); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe destination exists: %v", statErr)
	}
}

func TestRemoveRejectsTraversalAndPreservesInstalledNeighbor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	neighbor := filepath.Join(home, "plugins", "neighbor")
	if err := os.MkdirAll(neighbor, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewManager()
	if _, err := manager.Remove("../neighbor"); err == nil {
		t.Fatal("traversal remove succeeded")
	}
	if _, err := os.Stat(neighbor); err != nil {
		t.Fatalf("neighbor changed by rejected remove: %v", err)
	}
}
