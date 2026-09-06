package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	runtimepkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

func TestBuildProviderKeepsFrozenConfigAndCredentialHomeTogether(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	parent := t.TempDir()
	homeA := filepath.Join(parent, "home-a")
	homeB := filepath.Join(parent, "home-b")
	for _, dir := range []string{homeA, homeB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig := func(home, baseURL, model string) {
		t.Helper()
		body := `[provider]
default = "route"

[provider.custom.route]
transport = "openai_responses"
base_url = "` + baseURL + `"
model = "` + model + `"
`
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(homeA, "https://a.example/v1", "model-a")
	writeConfig(homeB, "https://b.example/collect", "model-b")

	link := filepath.Join(parent, "current")
	if err := os.Symlink(homeA, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("METIS_HOME", link)
	if err := auth.ActivateAPIKeyBound("route", "credential-from-a", "openai_responses", "https://a.example/v1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(homeB, link); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	built, err := runtimepkg.BuildProviderWithoutPreconnect(cfg, "route", "")
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := built.Provider.(*openai.Responses)
	if !ok {
		t.Fatalf("provider type = %T, want *openai.Responses", built.Provider)
	}
	if provider.BaseURL != "https://a.example/v1" || provider.APIKey != "credential-from-a" || built.Model != "model-a" {
		t.Fatalf("provider mixed roots: base_url=%q api_key=%q model=%q", provider.BaseURL, provider.APIKey, built.Model)
	}
}

func TestConfigLoadAndWriteRejectReplacedCredentialHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory replacement semantics differ on Windows")
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "metis-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider]\ndefault = \"anthropic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", home)
	if _, _, err := config.Load(); err != nil {
		t.Fatal(err)
	}

	original := home + ".original"
	if err := os.Rename(home, original); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("Load after root replacement = %v, want identity failure", err)
	}
	if err := config.SaveUserProviderDefault("openai"); err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("SaveUserProviderDefault after root replacement = %v, want identity failure", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement root received config: %v", err)
	}
}
