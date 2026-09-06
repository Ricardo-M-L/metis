package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tui"
)

func TestCustomTransportAliasLoginBuildsWithManagedKey(t *testing.T) {
	for _, transport := range []string{"anthropic", "openai", "gemini", "azure"} {
		t.Run(transport, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Setenv("METIS_CATALOG_DISABLE", "1")
			t.Chdir(t.TempDir())
			const id = "alias-route"
			const endpoint = "https://provider.example.test/v1"
			body := fmt.Sprintf("[provider.custom.%s]\ntransport = %q\nbase_url = %q\nmodel = %q\n", id, transport, endpoint, "test-model")
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := validateExplicitLoginProvider(id); err != nil {
				t.Errorf("explicit login rejected a runtime-supported alias: %v", err)
			}
			providers, err := configuredLoginProviders()
			if err != nil {
				t.Fatal(err)
			}
			if len(providers) != 1 || providers[0].ID != id {
				t.Errorf("login picker = %#v, want %s", providers, id)
			}
			result := tui.AuthResult{
				Provider: id, Key: "fake-alias-key", Method: tui.AuthMethodAPIKey,
				Custom: &tui.CustomProviderResult{Transport: transport, BaseURL: endpoint, Model: "test-model", Existing: true},
			}
			if err := completeAPIKeyLogin(result); err != nil {
				t.Fatalf("complete alias login: %v", err)
			}
			cfg, err := loadAuthProviderConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Provider.Default != id {
				t.Fatalf("selected provider = %q", cfg.Provider.Default)
			}
			if key, err := cfg.ResolveAPIKey(id); err != nil || key != result.Key {
				t.Fatalf("resolve bound alias key: present=%v err=%v", key != "", err)
			}
			built, err := rtpkg.BuildProviderWithoutPreconnect(cfg, id, "")
			if err != nil || built == nil || built.Provider == nil {
				t.Fatalf("construct alias provider: %v", err)
			}
		})
	}
}

func TestManagedAPIKeySupportExcludesUnregisteredAndCloudAliases(t *testing.T) {
	for _, transport := range []string{"chat", "responses", "vertex", "vertex_anthropic", "bedrock", "bedrock_anthropic", "unknown"} {
		if config.CustomProviderSupportsManagedAPIKey(config.ProviderRaw{Transport: transport}) {
			t.Errorf("single-key login accepted unsupported transport %q", transport)
		}
	}
}

func TestAuthKeysPutDoesNotSaveAfterCancellation(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := acquireSearchBackendKey
	t.Cleanup(func() { acquireSearchBackendKey = original })
	acquireSearchBackendKey = func(context.Context) (string, error) {
		cancel()
		return "fake-must-not-be-saved", nil
	}
	err := cmdAuth(ctx, []string{"keys", "put", "tavily"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("keys put error = %v, want cancellation", err)
	}
	if key, err := auth.GetSearchKey("tavily"); err != nil || key != "" {
		t.Fatalf("cancelled input persisted a key: present=%v err=%v", key != "", err)
	}
}
