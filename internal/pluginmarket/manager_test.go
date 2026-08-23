package pluginmarket

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogShowsBundledMarketplacesBeforeFirstSync(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	catalog := NewManager().Catalog()
	if len(catalog.Marketplaces) != 4 {
		t.Fatalf("bundled marketplaces = %d, want 4: %+v", len(catalog.Marketplaces), catalog.Marketplaces)
	}
	if !catalog.NeedsSync || len(catalog.Plugins) != 0 {
		t.Fatalf("first-run catalog = %+v, want unsynced empty plugin list", catalog)
	}
}

func TestCatalogExposesEcosystemBridgesSeparatelyFromMarketplaces(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	catalog := NewManager().Catalog()

	byID := map[string]Ecosystem{}
	for _, ecosystem := range catalog.Ecosystems {
		byID[ecosystem.ID] = ecosystem
	}
	if byID["codex"].Mode != "adapter" || byID["deepseek-harness"].Mode != "profile" {
		t.Fatalf("ecosystem bridges = %+v", catalog.Ecosystems)
	}
	if len(byID["codex"].Components) == 0 || len(byID["deepseek-harness"].Components) == 0 {
		t.Fatalf("ecosystem component contracts missing: %+v", catalog.Ecosystems)
	}
	for _, market := range catalog.Marketplaces {
		if market.Name == "codex-local" || strings.Contains(strings.ToLower(market.DisplayName), "on this mac") {
			t.Fatalf("local runtime cache must not be presented as a marketplace: %+v", market)
		}
	}
}

func TestCatalogAndInstallCompatibleCodexCachePlugin(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	root := filepath.Join(codexHome, "plugins", "cache", "openai-primary-runtime", "presentations", "1.2.3")
	if err := os.MkdirAll(filepath.Join(root, ".codex-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "skills", "presentations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name":"presentations","version":"1.2.3",
  "description":"Create PowerPoint and PPTX slide decks",
  "keywords":["ppt","pptx","slides"],"skills":"./skills/","mcpServers":"./.mcp.json",
  "interface":{"displayName":"Presentations","developerName":"OpenAI","category":"Productivity","logo":"./assets/logo.png","brandColor":"#c43e1c"}
}`
	if err := os.WriteFile(filepath.Join(root, ".codex-plugin", "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "presentations", "SKILL.md"), []byte("---\nname: presentations\ndescription: slides\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "logo.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpManifest := `{"mcpServers":{"slides":{"command":"./bin/slides-mcp","args":["--stdio"],"cwd":".","env":{"TOKEN":"${SLIDES_TOKEN}"}}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(mcpManifest), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager()
	catalog := manager.Catalog()
	var found *Plugin
	for i := range catalog.Plugins {
		if catalog.Plugins[i].Name == "presentations" && catalog.Plugins[i].Ecosystem == "codex" {
			found = &catalog.Plugins[i]
			break
		}
	}
	if found == nil || !found.Installable || found.DisplayName != "Presentations" || found.Icon == "" {
		t.Fatalf("compatible Codex plugin missing metadata: %+v", found)
	}
	if len(found.Skills) != 1 || found.Skills[0] != filepath.Join("skills", "presentations", "SKILL.md") {
		t.Fatalf("skills = %#v", found.Skills)
	}
	icon, err := manager.PluginIconPath("codex-local", "presentations")
	if err != nil || icon != filepath.Join(root, "assets", "logo.png") {
		t.Fatalf("icon = %q, err=%v", icon, err)
	}
	result, err := manager.Install(context.Background(), "presentations", "codex-local")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(result.Path, "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if body := string(data); !strings.Contains(body, `version = "1.2.3"`) || !strings.Contains(body, `skills/presentations/SKILL.md`) ||
		!strings.Contains(body, `[[mcp_servers]]`) || !strings.Contains(body, `name = "slides"`) || !strings.Contains(body, `working_dir = "."`) {
		t.Fatalf("generated plugin.toml:\n%s", body)
	}
}

func TestCatalogAndInstallPortablePartsOfDSHBundle(t *testing.T) {
	home := t.TempDir()
	dshHome := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("DSH_HOME", dshHome)
	profile := filepath.Join(dshHome, "profiles", "web")
	packageRoot := filepath.Join(profile, "node_modules", "@acme", "vision")
	if err := os.MkdirAll(filepath.Join(packageRoot, "skills", "vision"), 0o700); err != nil {
		t.Fatal(err)
	}
	profileManifest := `{"name":"dsh-profile-web","dependencies":{"@acme/vision":"1.2.0"},"dsh":{"profile":{"bundles":["@deepseek-ai/dsh-base","@acme/vision"]}}}`
	if err := os.WriteFile(filepath.Join(profile, "package.json"), []byte(profileManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	packageManifest := `{"name":"@acme/vision","version":"1.2.0","description":"Vision for DSH","keywords":["dsh-plugin"],"dsh":{"bundle":{"patch":"./cordis.patch.yml"}}}`
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(packageManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "cordis.patch.yml"), []byte("- insert:\n  - id: vision\n    name: '@acme/vision'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "skills", "vision", "SKILL.md"), []byte("---\nname: vision\ndescription: inspect images\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager()
	catalog := manager.Catalog()
	var found *Plugin
	for i := range catalog.Plugins {
		if catalog.Plugins[i].PackageName == "@acme/vision" {
			found = &catalog.Plugins[i]
			break
		}
	}
	if found == nil || found.Ecosystem != "deepseek-harness" || found.Compatibility != "partial" || !found.Installable {
		t.Fatalf("DSH bundle adapter result = %+v", found)
	}
	if found.Marketplace != "dsh-profile-web" {
		t.Fatalf("DSH origin = %q", found.Marketplace)
	}
	result, err := manager.Install(context.Background(), found.Name, found.Marketplace)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(result.Path, "plugin.toml"))
	if err != nil || !strings.Contains(string(body), `skills/vision/SKILL.md`) {
		t.Fatalf("translated DSH manifest: %v\n%s", err, body)
	}
}

func TestCatalogReadsCodexMarketplaceLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	root := filepath.Join(home, "plugins", "marketplaces", "codex-plugins-official")
	pluginRoot := filepath.Join(root, "plugins", "canva")
	if err := os.MkdirAll(filepath.Join(root, ".agents", "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".codex-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginRoot, "skills", "slides"), 0o700); err != nil {
		t.Fatal(err)
	}
	marketplace := `{"name":"openai-curated","interface":{"displayName":"Codex official"},"plugins":[{"name":"canva","source":{"source":"local","path":"./plugins/canva"},"category":"Design"}]}`
	if err := os.WriteFile(filepath.Join(root, ".agents", "plugins", "marketplace.json"), []byte(marketplace), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := `{"name":"canva","description":"Create presentations","keywords":["ppt"],"skills":"./skills/","interface":{"displayName":"Canva","logo":"./assets/icon.png"}}`
	if err := os.WriteFile(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), []byte(plugin), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "skills", "slides", "SKILL.md"), []byte("---\nname: slides\ndescription: slides\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := NewManager().Catalog()
	for _, item := range catalog.Plugins {
		if item.Name == "canva" && item.Marketplace == "codex-plugins-official" {
			if !item.Installable || item.DisplayName != "Canva" || item.Category != "Design" {
				t.Fatalf("Codex marketplace plugin = %+v", item)
			}
			return
		}
	}
	t.Fatal("Codex marketplace plugin not found")
}

func TestCatalogMarksPinnedHTTPSGitPluginForInstallInspection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "marketplaces.json"), []byte(`{
  "fixture": {"source": {"source": "github", "repo": "example/plugins"}}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "plugins", "marketplaces", "fixture")
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"plugins":[{"name":"pptx-deck","description":"Create PowerPoint decks","source":{"source":"git-subdir","url":"https://github.com/example/pptx.git","path":"plugin","sha":"0123456789abcdef0123456789abcdef01234567"}}]}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := NewManager().Catalog()
	for _, item := range catalog.Plugins {
		if item.Name == "pptx-deck" && item.Marketplace == "fixture" {
			if !item.Installable || item.Compatibility != "remote" {
				t.Fatalf("remote plugin = %+v", item)
			}
			return
		}
	}
	t.Fatal("remote plugin not found")
}

func TestInstallPinnedHTTPSGitSubdirPlugin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "marketplaces.json"), []byte(`{
  "fixture": {"source": {"source": "github", "repo": "example/plugins"}}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "plugins", "marketplaces", "fixture")
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"plugins":[{"name":"pptx-deck","description":"Create PowerPoint decks","source":{"source":"git-subdir","url":"https://github.com/example/pptx.git","path":"plugin","sha":"0123456789abcdef0123456789abcdef01234567"}}]}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager()
	manager.runGit = func(_ context.Context, args ...string) error {
		if len(args) >= 2 && args[0] == "clone" {
			checkout := args[len(args)-1]
			skillDir := filepath.Join(checkout, "plugin", "skills", "pptx")
			if err := os.MkdirAll(skillDir, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: pptx\ndescription: Create slides\n---\n"), 0o600)
		}
		return nil
	}

	result, err := manager.Install(context.Background(), "pptx-deck", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "skills", "pptx", "SKILL.md")); err != nil {
		t.Fatalf("installed remote skill missing: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(result.Path, "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `skills/pptx/SKILL.md`) {
		t.Fatalf("generated plugin.toml:\n%s", data)
	}
	downloadRoot := filepath.Join(home, "plugins", ".marketplace-downloads")
	entries, err := os.ReadDir(downloadRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary remote checkout was not removed: %+v", entries)
	}
}

func TestCatalogAcceptsPinnedGitHubSlug(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "marketplaces.json"), []byte(`{
  "fixture": {"source": {"source": "github", "repo": "example/plugins"}}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "plugins", "marketplaces", "fixture")
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"plugins":[{"name":"genpptx","description":"Create PPTX","source":{"source":"git-subdir","url":"yn01/claude-plugins","path":"genpptx","sha":"646368548add6433ed9dd676fdbcc7d65f97a863"}}]}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, item := range NewManager().Catalog().Plugins {
		if item.Name == "genpptx" && item.Marketplace == "fixture" {
			if !item.Installable || item.Compatibility != "remote" {
				t.Fatalf("GitHub slug plugin = %+v", item)
			}
			return
		}
	}
	t.Fatal("GitHub slug plugin not found")
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
