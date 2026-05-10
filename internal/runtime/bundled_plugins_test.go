package runtime

// bundled_plugins_test.go — covers the bundled marketplace registry:
// embedded defaults load, user overrides take precedence, save round-
// trips, builtin entries are not persisted to disk.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMarketplaceRegistry_BundledDefaults(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	r := LoadMarketplaceRegistry()
	if len(r.Entries) == 0 {
		t.Fatalf("expected bundled marketplaces; got 0 entries")
	}
	// anthropic-agent-skills must be in the bundle (the headline entry
	// from default_marketplaces.json).
	e, ok := r.Entries["anthropic-agent-skills"]
	if !ok {
		t.Fatalf("expected anthropic-agent-skills bundled; got entries=%v", r.ListNames())
	}
	if !e.Builtin {
		t.Errorf("anthropic-agent-skills should be flagged Builtin=true")
	}
	if e.Source.Source != "github" || e.Source.Repo != "anthropics/skills" {
		t.Errorf("source mismatch: got %+v, want github:anthropics/skills", e.Source)
	}
	if got := e.CloneURL(); got != "https://github.com/anthropics/skills.git" {
		t.Errorf("CloneURL = %q, want https://github.com/anthropics/skills.git", got)
	}
}

func TestLoadMarketplaceRegistry_UserOverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	// Hand-write a user override that points anthropic-agent-skills at
	// a different repo. Bundled defaults must yield to user intent.
	user := map[string]MarketplaceEntry{
		"anthropic-agent-skills": {
			Source: MarketplaceSource{Source: "github", Repo: "myfork/skills"},
		},
		"third-party": {
			Source: MarketplaceSource{Source: "github", Repo: "someone/marketplace"},
		},
	}
	data, _ := json.Marshal(user)
	if err := os.WriteFile(filepath.Join(dir, "marketplaces.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	r := LoadMarketplaceRegistry()
	if got := r.Entries["anthropic-agent-skills"].Source.Repo; got != "myfork/skills" {
		t.Errorf("user override didn't win: got repo %q, want myfork/skills", got)
	}
	if r.Entries["anthropic-agent-skills"].Builtin {
		t.Errorf("overridden entry should be flagged Builtin=false")
	}
	if _, ok := r.Entries["third-party"]; !ok {
		t.Errorf("user-only marketplace 'third-party' missing from registry")
	}
}

func TestSaveUserMarketplaces_OmitsBundled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	r := LoadMarketplaceRegistry()
	// Add a user marketplace alongside the bundled set.
	r.Add("my-private", MarketplaceSource{Source: "github", Repo: "me/private-skills"})
	if err := r.SaveUserMarketplaces(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "marketplaces.json"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "my-private") {
		t.Errorf("user entry missing from saved file:\n%s", body)
	}
	if strings.Contains(body, "anthropic-agent-skills") {
		t.Errorf("bundled entries should NOT be saved to disk:\n%s", body)
	}
}

func TestRemove_DeletesEntry(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	r := LoadMarketplaceRegistry()
	r.Add("temp", MarketplaceSource{Source: "github", Repo: "x/y"})
	if _, ok := r.Entries["temp"]; !ok {
		t.Fatalf("Add didn't land")
	}
	r.Remove("temp")
	if _, ok := r.Entries["temp"]; ok {
		t.Errorf("Remove didn't delete entry")
	}
}

func TestEnsureBundledPlugins_NoOp(t *testing.T) {
	// The shim must always return empty. The bundled-skill embed
	// approach was abandoned 2026-05-10 in favour of bundled
	// marketplaces — callers (main.go, cmdPluginList) still invoke
	// EnsureBundledPlugins for compat but expect a no-op.
	installed, errs := EnsureBundledPlugins()
	if len(installed) != 0 || len(errs) != 0 {
		t.Errorf("expected no-op; got installed=%v errs=%v", installed, errs)
	}
}

func TestMarketplaceClonePath_HonorsHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	got := MarketplaceClonePath("anthropic-agent-skills")
	want := filepath.Join(dir, "plugins", "marketplaces", "anthropic-agent-skills")
	if got != want {
		t.Errorf("MarketplaceClonePath = %q, want %q", got, want)
	}
}
