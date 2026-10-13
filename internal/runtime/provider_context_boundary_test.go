package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestProviderContextBudgetInputWindowDoesNotReserveOutputInBuild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
			w.Write([]byte(`{"name":"models/private-model","inputTokenLimit":65536,"outputTokenLimit":131072}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"private-model","context_window":65536,"max_output_tokens":131072}]}`))
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.Provider.Gemini.APIKey, cfg.Provider.Gemini.BaseURL, cfg.Provider.Gemini.Model = "fixture-key", server.URL, "private-model"
	cfg.Provider.Gemini.MaxTokens = 4096
	bootstrap, err := BuildProviderWithoutPreconnect(cfg, "gemini", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Provider.(llm.ContextCapabilitiesProvider).RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.Provider.Gemini.MaxTokens = 131072
	built, err := BuildProviderWithoutPreconnect(cfg, "gemini", "")
	if err != nil {
		t.Fatalf("independent Gemini output allowance was subtracted from input window: %v", err)
	}
	capabilities := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
	if capabilities.Window != 65536 || capabilities.WindowKind != llm.ContextWindowInput || capabilities.MaxOutputTokens != 131072 || built.MaxOutputTokens != 131072 {
		t.Fatalf("input/output metadata changed during finalization: %+v output=%d", capabilities, built.MaxOutputTokens)
	}
	cfg.Provider.Gemini.MaxTokens = 131073
	if _, err := BuildProviderWithoutPreconnect(cfg, "gemini", ""); err == nil || !strings.Contains(err.Error(), "advertised output limit") {
		t.Fatalf("input-only model bypassed its independent output ceiling: %v", err)
	}
	// A total context window continues to reserve the assistant allowance,
	// even if that same endpoint reports a larger separate output ceiling.
	client, err := modelinfo.New(filepath.Join(home, "cache", "provider-models"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Refresh(context.Background(), modelinfo.Binding{ProviderID: "openai", Transport: "openai_chat", BaseURL: server.URL, AuthKind: modelinfo.AuthBearer}, "fixture-key"); err != nil {
		t.Fatal(err)
	}
	openAICfg := configWithKey(t, "openai", "fixture-key", "private-model")
	openAICfg.Provider.OpenAI.BaseURL, openAICfg.Provider.OpenAI.MaxTokens = server.URL, 131072
	if _, err := BuildProviderWithoutPreconnect(openAICfg, "openai", ""); err == nil || !strings.Contains(err.Error(), "smaller than context_window") {
		t.Fatalf("combined total context window did not reserve output: %v", err)
	}
}

func TestProviderContextBudgetLegacyNativeDefaultDoesNotLeakToOverride(t *testing.T) {
	for _, name := range []string{"anthropic", "openai", "openai-responses", "gemini", "openai-codex"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			t.Setenv("METIS_CATALOG_DISABLE", "1")
			const configuredModel, otherModel = "fixture-default-model", "fixture-other-model"
			cfg := &config.Config{}
			providerID := name
			switch name {
			case "anthropic":
				cfg.Provider.Anthropic.APIKey, cfg.Provider.Anthropic.Model, cfg.Provider.Anthropic.ContextWindow, cfg.Provider.Anthropic.MaxTokens = "fixture-key", configuredModel, 100000, 4096
			case "openai", "openai-responses":
				providerID = "openai"
				cfg.Provider.OpenAI.APIKey, cfg.Provider.OpenAI.Model, cfg.Provider.OpenAI.ContextWindow, cfg.Provider.OpenAI.MaxTokens = "fixture-key", configuredModel, 100000, 4096
				if name == "openai-responses" {
					cfg.Provider.OpenAI.WireProtocol = "responses"
				}
			case "gemini":
				cfg.Provider.Gemini.APIKey, cfg.Provider.Gemini.Model, cfg.Provider.Gemini.ContextWindow, cfg.Provider.Gemini.MaxTokens = "fixture-key", configuredModel, 100000, 4096
			case "openai-codex":
				cfg.Provider.OpenAICodex.Model, cfg.Provider.OpenAICodex.ContextWindow, cfg.Provider.OpenAICodex.MaxTokens = configuredModel, 100000, 4096
				if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{AccessToken: "fixture-token", RefreshToken: "fixture-refresh", AccountID: "fixture-account", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			check := func(override, source string, requested int) llm.ContextCapabilities {
				t.Helper()
				built, err := BuildProviderWithoutPreconnect(cfg, providerID, override)
				if err != nil {
					t.Fatal(err)
				}
				got := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
				if got.BudgetSource != source || got.RequestedWindow != requested || built.Model != built.Provider.ModelID() {
					t.Fatalf("override %q borrowed legacy budget or wrong wire id: %+v build=%q wire=%q", override, got, built.Model, built.Provider.ModelID())
				}
				return got
			}
			check("", llm.ContextBudgetLegacyProvider, 100000)
			check(configuredModel, llm.ContextBudgetLegacyProvider, 100000)
			check(otherModel, llm.ContextBudgetAutomatic, 0)
			route, err := ProviderContextBudgetRoute(cfg, providerID)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.SetContextBudget(route, otherModel, 230000); err != nil {
				t.Fatal(err)
			}
			check(otherModel, llm.ContextBudgetPerModel, 230000)
			check("", llm.ContextBudgetLegacyProvider, 100000)
		})
	}
}

func TestProviderContextBudgetLegacyCustomAndCloudDefaultDoesNotLeakToOverride(t *testing.T) {
	for _, transportName := range []string{"anthropic_messages", "openai_chat", "openai_responses", "gemini_native", "azure_openai", "vertex_anthropic", "bedrock_anthropic"} {
		t.Run(transportName, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			t.Setenv("METIS_CATALOG_DISABLE", "1")
			const id, configuredModel, otherModel = "legacy-profile", "fixture-default-model", "fixture-other-model"
			raw := config.ProviderRaw{Transport: transportName, APIKey: "fixture-key", Model: configuredModel, ContextWindow: 100000, MaxTokens: 4096}
			if transportName == "vertex_anthropic" {
				raw.ServiceAccountFile, raw.Project = writeRuntimeTestServiceAccount(t), "fixture-project"
			}
			if transportName == "bedrock_anthropic" {
				t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture-secret")
			}
			if transportName == "azure_openai" {
				raw.BaseURL = "https://azure-fixture.invalid"
			}
			cfg := &config.Config{}
			cfg.Provider.Custom = map[string]config.ProviderRaw{id: raw}
			check := func(override, source string, requested int) {
				t.Helper()
				built, err := BuildProviderWithoutPreconnect(cfg, id, override)
				if err != nil {
					t.Fatal(err)
				}
				got := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
				if got.BudgetSource != source || got.RequestedWindow != requested || built.Model != built.Provider.ModelID() {
					t.Fatalf("override %q borrowed legacy budget or wrong wire id: %+v build=%q wire=%q", override, got, built.Model, built.Provider.ModelID())
				}
			}
			check("", llm.ContextBudgetLegacyProvider, 100000)
			check(configuredModel, llm.ContextBudgetLegacyProvider, 100000)
			check(otherModel, llm.ContextBudgetAutomatic, 0)
			route, err := ProviderContextBudgetRoute(cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.SetContextBudget(route, otherModel, 230000); err != nil {
				t.Fatal(err)
			}
			check(otherModel, llm.ContextBudgetPerModel, 230000)
			check("", llm.ContextBudgetLegacyProvider, 100000)
			if got := cfg.Provider.Custom[id]; got.ContextWindow != 100000 || got.Model != configuredModel {
				t.Fatal("legacy provider configuration was rewritten")
			}
		})
	}
}

func TestProviderContextBudgetLegacyEmptyConfigMatchesConstructorWireDefault(t *testing.T) {
	for _, transportName := range []string{"anthropic_messages", "openai_chat", "openai_responses", "gemini_native"} {
		t.Run(transportName, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			t.Setenv("METIS_CATALOG_DISABLE", "1")
			cfg := &config.Config{}
			cfg.Provider.Custom = map[string]config.ProviderRaw{"legacy-profile": {Transport: transportName, APIKey: "fixture-key", ContextWindow: 100000, MaxTokens: 4096}}
			defaultModel := defaultWireModelForTransport(transportName)
			for _, override := range []string{"", defaultModel, "other-private-model"} {
				built, err := BuildProviderWithoutPreconnect(cfg, "legacy-profile", override)
				if err != nil {
					t.Fatal(err)
				}
				got := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
				wantSource, wantRequested := llm.ContextBudgetLegacyProvider, 100000
				if override == "other-private-model" {
					wantSource, wantRequested = llm.ContextBudgetAutomatic, 0
				}
				if got.BudgetSource != wantSource || got.RequestedWindow != wantRequested || built.Model != built.Provider.ModelID() || (override != "other-private-model" && built.Model != defaultModel) {
					t.Fatalf("empty default did not match constructor's actual wire model: %+v build=%q wire=%q expected=%q", got, built.Model, built.Provider.ModelID(), defaultModel)
				}
			}
		})
	}
}

func TestProviderContextBudgetAnthropicOAuthUsesStableAccountAndDiskOnlyCatalog(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("METIS_CATALOG_DISABLE", "")
	var catalogGets, credentialHeaders, wireCalls atomic.Int32
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogGets.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			credentialHeaders.Add(1)
		}
		w.Write([]byte(`{"anthropic":{"models":{"private-claude-model":{"limit":{"context":1000000,"output":128000}}}}}`))
	}))
	defer catalogServer.Close()
	t.Setenv("METIS_MODELS_URL", catalogServer.URL)
	credential := auth.OAuthCredential{AccessToken: "fixture-token-a", RefreshToken: "fixture-refresh-a", AccountID: "fixture-account-a", ExpiresAt: time.Now().Add(time.Hour)}
	if err := auth.PutOAuth("anthropic", credential); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Provider.Anthropic.Model, cfg.Provider.Anthropic.MaxTokens, cfg.Provider.Anthropic.ContextWindow = "private-claude-model", 4096, 100000
	built, err := BuildProviderWithoutPreconnect(cfg, "anthropic", "")
	if err != nil {
		t.Fatal(err)
	}
	provider := built.Provider.(*anthropic.Anthropic)
	if provider.ContextResolver == nil || provider.OAuthTokenSource == nil || provider.APIKey != "" {
		t.Fatal("OAuth route did not receive a disk-only catalog resolver")
	}
	for range 100 {
		_ = provider.ContextCapabilities()
	}
	if catalogGets.Load() != 0 {
		t.Fatal("construction/status warmed the public catalog")
	}
	loop := BuildAgentLoop(cfg, AgentLoopOptions{Provider: provider, Model: built.Model, MaxOutputTokens: built.MaxOutputTokens, Registry: tools.NewRegistry()})
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalogGets.Load() != 1 || credentialHeaders.Load() != 0 {
		t.Fatalf("OAuth reached an unrelated capability endpoint: gets=%d credentialHeaders=%d", catalogGets.Load(), credentialHeaders.Load())
	}
	credential.AccessToken, credential.RefreshToken = "fixture-rotated-token", "fixture-rotated-refresh"
	if err := auth.PutOAuth("anthropic", credential); err != nil {
		t.Fatal(err)
	}
	if token, err := provider.OAuthTokenSource(context.Background()); err != nil || token != credential.AccessToken {
		t.Fatalf("token rotation changed stable account binding: %v", err)
	}
	credential.AccountID = "fixture-account-b"
	if err := auth.PutOAuth("anthropic", credential); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.OAuthTokenSource(context.Background()); !errors.Is(err, llm.ErrContextBudgetPolicy) || strings.Contains(err.Error(), credential.AccountID) || strings.Contains(err.Error(), credential.AccessToken) {
		t.Fatalf("wire callback accepted changed account or leaked identity: %v", err)
	}
	if err := loop.RefreshCapabilities(context.Background()); !errors.Is(err, llm.ErrContextBudgetPolicy) {
		t.Fatalf("request-boundary budget policy accepted changed OAuth account: %v", err)
	}
	wireServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wireCalls.Add(1); w.WriteHeader(500) }))
	defer wireServer.Close()
	provider.BaseURL = wireServer.URL
	loop.AppendUser("must not use another account with the old operating budget")
	if err := loop.Run(context.Background(), make(chan agent.Event, 64)); !errors.Is(err, llm.ErrContextBudgetPolicy) || wireCalls.Load() != 0 {
		t.Fatalf("changed account made generation requests: error=%v calls=%d", err, wireCalls.Load())
	}
	routeB, err := ProviderContextBudgetRoute(cfg, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetContextBudget(routeB, cfg.Provider.Anthropic.Model, 230000); err != nil {
		t.Fatal(err)
	}
	fresh, err := BuildProviderWithoutPreconnect(cfg, "anthropic", "")
	if err != nil {
		t.Fatal(err)
	}
	got := fresh.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
	if got.Window != 230000 || got.BudgetSource != llm.ContextBudgetPerModel || got.RouteKey == provider.ContextCapabilities().RouteKey || catalogGets.Load() != 1 {
		t.Fatalf("fresh OAuth provider did not safely bind B's exact budget: %+v catalogGets=%d", got, catalogGets.Load())
	}
}
