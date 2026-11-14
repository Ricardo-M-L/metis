package runtime

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestProviderProfileIdentityReachesLoopAndSurvivesSwitchRollback(t *testing.T) {
	for _, visionOverride := range []bool{false, true} {
		name := "direct"
		if visionOverride {
			name = "vision-override"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			t.Setenv("METIS_CATALOG_DISABLE", "1")
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			cfg := &config.Config{}
			cfg.Provider.Custom = make(map[string]config.ProviderRaw)
			for _, profile := range []string{"easycliproxyapi", "other-custom-profile"} {
				raw := config.ProviderRaw{
					Transport: "openai_chat", BaseURL: server.URL, APIKey: "fixture-not-a-credential",
					Model: "configured-model", CatalogProvider: "different-catalog-issuer", MaxTokens: 1024,
				}
				if visionOverride {
					raw.SupportsVision = &visionOverride
				}
				cfg.Provider.Custom[profile] = raw
			}
			// This is also the detached worker's construction path: the queued
			// profile and model override must survive transport/catalog defaults.
			source, err := BuildProviderWithoutPreconnect(cfg, "easycliproxyapi", "frozen-source-model")
			if err != nil {
				t.Fatal(err)
			}
			if source.Provider.Name() != "openai" {
				t.Fatalf("fixture needs a distinct adapter name, got %q", source.Provider.Name())
			}
			assertRuntimeProviderProfile(t, source.Provider, "easycliproxyapi", "frozen-source-model")
			loop := BuildAgentLoop(cfg, AgentLoopOptions{
				Provider: source.Provider, Model: source.Model, MaxOutputTokens: source.MaxOutputTokens,
				Registry: tools.NewRegistry(), MemoryManagerProvided: true,
			})
			if got := loop.ContextMetadataSnapshot(); got.ProviderID != "easycliproxyapi" || got.Model != "frozen-source-model" {
				t.Fatalf("initial loop lost the configured profile: %+v", got)
			}
			before := loop.ProviderRuntimeState()
			target, err := BuildProviderWithoutPreconnect(cfg, "other-custom-profile", "frozen-target-model")
			if err != nil {
				t.Fatal(err)
			}
			loop.RebindProviderRuntime(target.Provider, target.Model, target.MaxOutputTokens, before.System, before.SystemSections)
			if got := loop.ContextMetadataSnapshot(); got.ProviderID != "other-custom-profile" || got.Model != "frozen-target-model" {
				t.Fatalf("switch retained the source profile: %+v", got)
			}
			loop.RebindProviderRuntime(before.Provider, before.Model, before.MaxOutputTokens, before.System, before.SystemSections)
			if got := loop.ContextMetadataSnapshot(); got.ProviderID != "easycliproxyapi" || got.Model != "frozen-source-model" {
				t.Fatalf("rollback retained the target profile: %+v", got)
			}
			if requests.Load() != 0 {
				t.Fatalf("identity reads unexpectedly made %d HTTP requests", requests.Load())
			}
		})
	}
}

func assertRuntimeProviderProfile(t *testing.T, provider llm.Provider, profile, model string) {
	t.Helper()
	metadata, ok := provider.(llm.ContextCapabilitiesProvider)
	if !ok {
		t.Fatal("provider wrapper lost context metadata")
	}
	got := metadata.ContextCapabilities()
	if got.ProviderID != profile || got.Model != model || got.RouteKey == "" {
		t.Fatalf("profile/model were not stamped into route metadata: %+v", got)
	}
}
