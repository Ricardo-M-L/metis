package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/azure"
	"github.com/Ricardo-M-L/metis/internal/llm/bedrock"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/vertex"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestCustomProviderCachedCapabilitiesSurviveVisionOverrideAndReachLoop(t *testing.T) {
	for _, transportName := range []string{"anthropic_messages", "anthropic", "", "openai_chat", "openai_responses"} {
		t.Run(transportName, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Write([]byte(`{"models":[{"slug":"unlisted-private-model","context_window":272000,"max_context_window":872000,"max_tokens":128000}]}`))
			}))
			defer server.Close()
			bindingTransport := transportName
			if bindingTransport == "" {
				bindingTransport = "anthropic_messages"
			}
			client, err := modelinfo.New(filepath.Join(home, "cache", "provider-models"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Refresh(context.Background(), modelinfo.Binding{ProviderID: "new-profile", Transport: bindingTransport, BaseURL: server.URL, AuthKind: modelinfo.AuthBearer}, "fixture-key")
			if err != nil {
				t.Fatal(err)
			}
			vision := true
			cfg := &config.Config{}
			cfg.Provider.Custom = map[string]config.ProviderRaw{"new-profile": {Transport: transportName, BaseURL: server.URL, APIKey: "fixture-key", Model: "unlisted-private-model", MaxTokens: 8192, SupportsVision: &vision}}
			built, err := BuildProviderWithoutPreconnect(cfg, "new-profile", "")
			if err != nil {
				t.Fatal(err)
			}
			capProvider, ok := built.Provider.(llm.ContextCapabilitiesProvider)
			if !ok {
				t.Fatal("vision override erased metadata capability")
			}
			got := capProvider.ContextCapabilities()
			if got.Window != 272000 || got.MaxWindow != 872000 || got.Source != "endpoint_pi" {
				t.Fatalf("cached capability lost: %+v", got)
			}
			loop := BuildAgentLoop(cfg, AgentLoopOptions{Provider: built.Provider, Model: built.Model, MaxOutputTokens: built.MaxOutputTokens, Registry: tools.NewRegistry()})
			metadata, threshold, trigger := loop.ContextDetailsSnapshot()
			if metadata != got || loop.ContextWindow != got.Window || loop.Compactor.MaxContextTokens != got.Window {
				t.Fatalf("loop/context/compactor split: %+v window=%d compactor=%d", metadata, loop.ContextWindow, loop.Compactor.MaxContextTokens)
			}
			if trigger <= 0 || float64(trigger) >= threshold*float64(metadata.MaxWindow) {
				t.Fatalf("trigger uses extended maximum: %d threshold=%v", trigger, threshold)
			}
			if err := loop.RefreshCapabilities(context.Background()); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatal("cached construction/status/boundary unexpectedly made HTTP requests")
			}
		})
	}
}

func TestProviderContextBudgetMatchesExactRouteAndModel(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"private-a","context_window":400000,"max_context_window":600000},{"id":"private-b","context_window":300000}]}`))
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{"gateway": {Transport: " OPENAI_CHAT ", BaseURL: server.URL + "/", APIKey: "fixture-key", Model: "private-a", MaxTokens: 4096}}
	route, err := ProviderContextBudgetRoute(cfg, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetContextBudget(route, "private-a", 550000); err != nil {
		t.Fatal(err)
	}
	built, err := BuildProviderWithoutPreconnect(cfg, "gateway", "")
	if err != nil {
		t.Fatal(err)
	}
	provider := built.Provider.(llm.ContextCapabilitiesProvider)
	if err := provider.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := provider.ContextCapabilities()
	if got.Window != 550000 || got.DefaultWindow != 400000 || got.MaxWindow != 600000 || got.RequestedWindow != 550000 || got.BudgetSource != llm.ContextBudgetPerModel {
		t.Fatalf("precise budget did not reach shared snapshot: %+v", got)
	}
	// Rebuilding with another exact model must not carry private-a's budget.
	other, err := BuildProviderWithoutPreconnect(cfg, "gateway", "private-b")
	if err != nil {
		t.Fatal(err)
	}
	if got := other.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities(); got.Window != 300000 || got.BudgetSource != llm.ContextBudgetAutomatic || got.RequestedWindow != 0 {
		t.Fatalf("model switch borrowed a different model's budget: %+v", got)
	}
	// Changing either endpoint or actual wire protocol is a different route.
	for _, change := range []string{"endpoint", "protocol", "provider"} {
		clone := *cfg
		raw := cfg.Provider.Custom["gateway"]
		id := "gateway"
		switch change {
		case "endpoint":
			raw.BaseURL = server.URL + "/other"
		case "protocol":
			raw.Transport = "openai_responses"
		case "provider":
			id = "other-gateway"
		}
		clone.Provider.Custom = map[string]config.ProviderRaw{id: raw}
		other, err := BuildProviderWithoutPreconnect(&clone, id, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := other.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities(); got.BudgetSource != llm.ContextBudgetAutomatic || got.RequestedWindow != 0 {
			t.Fatalf("%s switch borrowed another route's budget: %+v", change, got)
		}
	}
}

func TestProviderContextBudgetAutoResetCoversLegacyAndClampsAllConsumers(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"private-model","context_window":400000,"max_context_window":600000}]}`))
	}))
	defer server.Close()
	cfg := configWithKey(t, "openai", "fixture-key", "private-model")
	cfg.Provider.OpenAI.BaseURL, cfg.Provider.OpenAI.ContextWindow = server.URL, 100000
	route, err := ProviderContextBudgetRoute(cfg, "openai")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := BuildProviderWithoutPreconnect(cfg, "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := legacy.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities(); got.Window != 100000 || got.BudgetSource != llm.ContextBudgetLegacyProvider {
		t.Fatalf("legacy compatibility lost: %+v", got)
	}
	if err := config.SetContextBudget(route, "private-model", 0); err != nil {
		t.Fatal(err)
	}
	auto, err := BuildProviderWithoutPreconnect(cfg, "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	provider := auto.Provider.(llm.ContextCapabilitiesProvider)
	if err := provider.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := provider.ContextCapabilities(); got.Window != 400000 || got.BudgetSource != llm.ContextBudgetAutomatic || got.RequestedWindow != 0 {
		t.Fatalf("zero record did not override global legacy budget: %+v", got)
	}
	if err := config.SetContextBudget(route, "private-model", 700000); err != nil {
		t.Fatal(err)
	}
	manual, err := BuildProviderWithoutPreconnect(cfg, "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	got := manual.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
	if got.Window != 600000 || got.RequestedWindow != 700000 || got.Source != "configured_clamped" || got.BudgetSource != llm.ContextBudgetPerModel {
		t.Fatalf("manual budget bypassed actual route ceiling: %+v", got)
	}
	loop := BuildAgentLoop(cfg, AgentLoopOptions{Provider: manual.Provider, Model: manual.Model, MaxOutputTokens: manual.MaxOutputTokens, Registry: tools.NewRegistry()})
	metadata, _, _ := loop.ContextDetailsSnapshot()
	if metadata != got || loop.ContextWindow != got.Window || loop.Compactor.MaxContextTokens != got.Window {
		t.Fatalf("UI/Loop/Compactor context split: %+v %+v %d %d", got, metadata, loop.ContextWindow, loop.Compactor.MaxContextTokens)
	}
}

func TestProviderContextBudgetCodexAccountStableAcrossTokenRefresh(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	credential := auth.OAuthCredential{AccessToken: "fixture-token", RefreshToken: "fixture-refresh", AccountID: "fixture-account-a", ExpiresAt: time.Now().Add(time.Hour)}
	if err := auth.PutOAuth("openai-codex", credential); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Provider.OpenAICodex.Model = "gpt-fixture-model"
	route, err := ProviderContextBudgetRoute(cfg, "openai-codex")
	if err != nil {
		t.Fatal(err)
	}
	if route.AccountScope == credential.AccountID {
		t.Fatal("settings route exposes raw OAuth account")
	}
	if err := config.SetContextBudget(route, cfg.Provider.OpenAICodex.Model, 100000); err != nil {
		t.Fatal(err)
	}
	build := func() *openai.Responses {
		t.Helper()
		built, err := BuildProviderWithoutPreconnect(cfg, "openai-codex", "")
		if err != nil {
			t.Fatal(err)
		}
		return built.Provider.(*openai.Responses)
	}
	first := build()
	if first.ContextWindow != 100000 || first.ContextBudgetSource != llm.ContextBudgetPerModel || first.CodexContextResolver == nil || first.ContextResolver != nil {
		t.Fatal("Codex budget/discovery bypassed subscription boundary")
	}
	credential.AccessToken, credential.RefreshToken = "rotated-fixture-token", "rotated-fixture-refresh"
	if err := auth.PutOAuth("openai-codex", credential); err != nil {
		t.Fatal(err)
	}
	if rotated := build(); rotated.ContextWindow != 100000 || rotated.ContextBudgetSource != llm.ContextBudgetPerModel {
		t.Fatal("token refresh discarded the stable account budget")
	}
	credential.AccountID = "fixture-account-b"
	if err := auth.PutOAuth("openai-codex", credential); err != nil {
		t.Fatal(err)
	}
	if switched := build(); switched.ContextWindow != 0 || switched.ContextBudgetSource != llm.ContextBudgetAutomatic {
		t.Fatal("new OAuth account inherited the previous account's budget")
	}
}

func TestProviderContextBudgetGeminiAliasSharesNativeRoute(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Provider.Gemini.APIKey, cfg.Provider.Gemini.Model = "fixture-key", "gemini-fixture"
	route, err := ProviderContextBudgetRoute(cfg, "google")
	if err != nil {
		t.Fatal(err)
	}
	if route.ProviderID != "gemini" || route.Transport != "gemini_native" {
		t.Fatalf("native alias identity split: %+v", route)
	}
	if err := config.SetContextBudget(route, cfg.Provider.Gemini.Model, 500000); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gemini", "google"} {
		built, err := BuildProviderWithoutPreconnect(cfg, name, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities(); got.Window != 500000 || got.BudgetSource != llm.ContextBudgetPerModel {
			t.Fatalf("%q alias lost exact budget: %+v", name, got)
		}
	}
}

func TestProviderContextBudgetRejectsCorruptStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "context-budgets.json"), []byte("broken-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildProviderWithoutPreconnect(configWithKey(t, "openai", "fixture-key", "private-model"), "openai", ""); err == nil {
		t.Fatal("corrupt model budget silently fell back to provider-global configuration")
	}
}

func TestProviderContextBudgetExistingLoopAdoptsSavedBudgetAtRequestBoundary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"data":[{"id":"private-model","context_window":400000,"max_context_window":600000}]}`))
	}))
	defer server.Close()
	cfg := configWithKey(t, "openai", "fixture-key", "private-model")
	cfg.Provider.OpenAI.BaseURL, cfg.Provider.OpenAI.ContextWindow = server.URL, 100000
	route, err := ProviderContextBudgetRoute(cfg, "openai")
	if err != nil {
		t.Fatal(err)
	}
	routeKey, err := config.ContextBudgetRouteKey(route, "private-model")
	if err != nil {
		t.Fatal(err)
	}
	built, err := BuildProviderWithoutPreconnect(cfg, "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	provider := built.Provider.(*openai.OpenAI)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{Provider: provider, Model: built.Model, MaxOutputTokens: built.MaxOutputTokens, Registry: tools.NewRegistry()})
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := loop.ContextMetadataSnapshot()
	if before.ProviderID != "openai" || before.Model != "private-model" || before.RouteKey != routeKey || before.Window != 100000 {
		t.Fatalf("bound snapshot has wrong ownership: %+v", before)
	}
	if err := config.SetContextBudget(route, "private-model", 550000); err != nil {
		t.Fatal(err)
	}
	// A UI sampler cannot trigger storage I/O or prematurely change the bound
	// budget. The existing Loop and provider remain exactly the same objects.
	if got := provider.ContextCapabilities(); got != before {
		t.Fatalf("view read adopted settings before request boundary: %+v", got)
	}
	if got := loop.ContextMetadataSnapshot(); got != before {
		t.Fatalf("installed UI snapshot changed before request boundary: %+v", got)
	}
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := loop.ContextMetadataSnapshot()
	if after.Window != 550000 || after.BudgetSource != llm.ContextBudgetPerModel || after.RouteKey != routeKey || loop.Compactor.MaxContextTokens != 550000 {
		t.Fatalf("existing task did not adopt saved budget: %+v compactor=%d", after, loop.Compactor.MaxContextTokens)
	}
	if provider.ContextWindow != 100000 {
		t.Fatal("request boundary mutated the shared provider's constructor field")
	}
	if err := config.SetContextBudget(route, "private-model", 0); err != nil {
		t.Fatal(err)
	}
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	auto := loop.ContextMetadataSnapshot()
	if auto.Window != 400000 || auto.BudgetSource != llm.ContextBudgetAutomatic || auto.RequestedWindow != 0 {
		t.Fatalf("existing task did not apply explicit auto over legacy: %+v", auto)
	}
	if err := os.WriteFile(filepath.Join(home, "context-budgets.json"), []byte("broken-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := provider.ContextCapabilities(); got != auto {
		t.Fatalf("view inspected corrupt storage: %+v", got)
	}
	if err := loop.RefreshCapabilities(context.Background()); !errors.Is(err, llm.ErrContextBudgetPolicy) {
		t.Fatalf("unsafe saved policy was ignored: %v", err)
	}
	if got := loop.ContextMetadataSnapshot(); got != auto {
		t.Fatalf("failed lookup replaced the installed snapshot: %+v", got)
	}
	loop.AppendUser("must not issue a model request with corrupt saved settings")
	events := make(chan agent.Event, 64)
	if err := loop.Run(context.Background(), events); !errors.Is(err, llm.ErrContextBudgetPolicy) {
		t.Fatalf("Run did not stop before provider request: %v", err)
	}
	if posts.Load() != 0 {
		t.Fatalf("Run sent %d provider requests despite corrupt saved policy", posts.Load())
	}
}

func TestProviderContextBudgetParallelStatusAndRefreshDoNotRace(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	cfg := configWithKey(t, "openai", "fixture-key", "private-model")
	cfg.Provider.OpenAI.BaseURL = "https://fixture.invalid/v1"
	route, err := ProviderContextBudgetRoute(cfg, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetContextBudget(route, "private-model", 300000); err != nil {
		t.Fatal(err)
	}
	built, err := BuildProviderWithoutPreconnect(cfg, "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	provider := built.Provider.(*openai.OpenAI)
	// Isolate saved-budget I/O from unrelated authenticated discovery, while
	// exercising the real adapter getter/refresh with a shared provider.
	provider.ContextResolver = nil
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 1000 {
				got := provider.ContextCapabilities()
				if got.Window < 300000 || got.Window > 300031 || got.BudgetSource != llm.ContextBudgetPerModel || got.ProviderID != "openai" || got.Model != "private-model" || got.RouteKey == "" {
					t.Errorf("parallel sampler received incoherent budget: %+v", got)
					return
				}
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		for i := range 32 {
			if err := config.SetContextBudget(route, "private-model", 300000+i); err != nil {
				t.Error(err)
				return
			}
			if err := provider.RefreshContextCapabilities(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	group.Wait()
	if provider.ContextWindow != 300000 {
		t.Fatal("refresh modified shared constructor state")
	}
}

func TestProviderContextBudgetCloudPolicyBridgeRemainsCapacityUnknown(t *testing.T) {
	for _, transportName := range []string{"azure_openai", "vertex_anthropic", "bedrock_anthropic"} {
		t.Run(transportName, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			const model = "private-cloud-model"
			cfg := &config.Config{}
			raw := config.ProviderRaw{Transport: transportName, Model: model, ContextWindow: 100000}
			var provider, sibling llm.Provider
			switch transportName {
			case "azure_openai":
				raw.BaseURL = "https://azure-fixture.invalid"
				provider = &azure.Azure{BaseURL: raw.BaseURL, Model: model, ContextWindow: 100000}
				sibling = &azure.Azure{BaseURL: "https://other-azure-fixture.invalid", Model: model, ContextWindow: 100000}
			case "vertex_anthropic":
				raw.Project, raw.Region = "fixture-project", "fixture-east"
				provider = &vertex.Vertex{Project: raw.Project, Region: raw.Region, Model: model, ContextWindow: 100000}
				sibling = &vertex.Vertex{Project: "other-project", Region: raw.Region, Model: model, ContextWindow: 100000}
			case "bedrock_anthropic":
				raw.Region = "fixture-east-1"
				provider = &bedrock.Bedrock{Region: raw.Region, Model: model, ContextWindow: 100000}
				sibling = &bedrock.Bedrock{Region: "other-region", Model: model, ContextWindow: 100000}
			}
			if provider.MaxContextTokens() != 100000 {
				t.Fatal("nil policy changed legacy cloud constructor behavior")
			}
			cfg.Provider.Custom = map[string]config.ProviderRaw{"cloud-fixture": raw}
			route, err := ProviderContextBudgetRoute(cfg, "cloud-fixture")
			if err != nil {
				t.Fatal(err)
			}
			if err := config.SetContextBudget(route, model, 500000); err != nil {
				t.Fatal(err)
			}
			if err := bindProviderContext(provider, "cloud-fixture", transportName, false); err != nil {
				t.Fatal(err)
			}
			capProvider := provider.(llm.ContextCapabilitiesProvider)
			got := capProvider.ContextCapabilities()
			if got.Window != 500000 || got.WindowKind != llm.ContextWindowTotal || got.BudgetSource != llm.ContextBudgetPerModel || got.ModelWindow != 0 || got.DefaultWindow != 0 || got.MaxWindow != 0 || got.MaxInputTokens != 0 || got.MaxOutputTokens != 0 || got.Model != model || got.ProviderID != "cloud-fixture" || got.RouteKey == "" {
				t.Fatalf("cloud bridge fabricated capacity or lost budget: %+v", got)
			}
			if err := bindProviderContext(sibling, "cloud-fixture", transportName, false); err != nil {
				t.Fatal(err)
			}
			if got := sibling.(llm.ContextCapabilitiesProvider).ContextCapabilities(); got.Window != 100000 || got.BudgetSource != llm.ContextBudgetLegacyProvider {
				t.Fatalf("cloud project/region/endpoint borrowed budget: %+v", got)
			}
			loop := BuildAgentLoop(cfg, AgentLoopOptions{Provider: provider, Model: model, MaxOutputTokens: 4096, Registry: tools.NewRegistry()})
			if err := config.SetContextBudget(route, model, 550000); err != nil {
				t.Fatal(err)
			}
			if got := capProvider.ContextCapabilities(); got.Window != 500000 {
				t.Fatal("cloud view triggered saved-budget I/O")
			}
			if err := loop.RefreshCapabilities(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := loop.ContextMetadataSnapshot(); got.Window != 550000 || got.BudgetSource != llm.ContextBudgetPerModel || loop.Compactor.MaxContextTokens != 550000 {
				t.Fatalf("existing cloud Loop did not refresh policy: %+v", got)
			}
			if err := config.SetContextBudget(route, model, 0); err != nil {
				t.Fatal(err)
			}
			if err := loop.RefreshCapabilities(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := loop.ContextMetadataSnapshot(); got.Window != 0 || got.BudgetSource != llm.ContextBudgetAutomatic || loop.Compactor.MaxContextTokens != 0 {
				t.Fatalf("cloud auto retained an obsolete manual budget: %+v compactor=%d", got, loop.Compactor.MaxContextTokens)
			}
		})
	}
}

func TestProviderContextBudgetCodexExistingProviderRejectsChangedAccount(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	credential := auth.OAuthCredential{AccessToken: "fixture-token", RefreshToken: "fixture-refresh", AccountID: "fixture-account-a", ExpiresAt: time.Now().Add(time.Hour)}
	if err := auth.PutOAuth("openai-codex", credential); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Provider.OpenAICodex.Model = "private-model"
	routeA, err := ProviderContextBudgetRoute(cfg, "openai-codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetContextBudget(routeA, "private-model", 100000); err != nil {
		t.Fatal(err)
	}
	built, err := BuildProviderWithoutPreconnect(cfg, "openai-codex", "")
	if err != nil {
		t.Fatal(err)
	}
	provider := built.Provider.(*openai.Responses)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{Provider: provider, Model: built.Model, MaxOutputTokens: built.MaxOutputTokens, Registry: tools.NewRegistry()})
	credential.AccountID = "fixture-account-b"
	if err := auth.PutOAuth("openai-codex", credential); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.OAuthTokenSource(context.Background()); !errors.Is(err, llm.ErrContextBudgetPolicy) {
		t.Fatalf("generation token callback accepted another account: %v", err)
	}
	if err := loop.RefreshCapabilities(context.Background()); !errors.Is(err, llm.ErrContextBudgetPolicy) {
		t.Fatalf("metadata callback accepted another account: %v", err)
	}
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	// The fixed token callback is shared by subscription discovery and the
	// generation wire. A changed account is rejected before either can send.
	provider.BaseURL = server.URL
	loop.AppendUser("must not spend account B with account A's saved budget")
	if err := loop.Run(context.Background(), make(chan agent.Event, 64)); !errors.Is(err, llm.ErrContextBudgetPolicy) {
		t.Fatalf("account switch did not stop the live Loop: %v", err)
	}
	if posts.Load() != 0 {
		t.Fatalf("account switch made %d generation requests", posts.Load())
	}
	routeB, err := ProviderContextBudgetRoute(cfg, "openai-codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetContextBudget(routeB, "private-model", 150000); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := BuildProviderWithoutPreconnect(cfg, "openai-codex", "")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := rebuilt.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
	if capabilities.Window != 150000 || capabilities.BudgetSource != llm.ContextBudgetPerModel || capabilities.RouteKey == provider.ContextCapabilities().RouteKey {
		t.Fatalf("fresh provider did not bind new-account budget: %+v", capabilities)
	}
}

func TestNativeOpenAIOutputReservationAndProtocolCacheMatchWire(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte(`{"models":[{"slug":"wire-private-model","context_window":400000,"max_tokens":16000}]}`))
	}))
	defer server.Close()
	for _, protocol := range []string{"", " responses "} {
		cfg := configWithKey(t, "openai", "fixture-key", "wire-private-model")
		cfg.Provider.OpenAI.BaseURL = server.URL
		cfg.Provider.OpenAI.MaxTokens = 0
		cfg.Provider.OpenAI.WireProtocol = protocol
		built, err := BuildProviderWithoutPreconnect(cfg, "openai", "")
		if err != nil {
			t.Fatal(err)
		}
		if built.MaxOutputTokens != 4096 {
			t.Fatalf("protocol %q metadata budget differs from constructor's wire default: %d", protocol, built.MaxOutputTokens)
		}
		if err := built.Provider.(llm.ContextCapabilitiesProvider).RefreshContextCapabilities(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := built.Provider.MaxContextTokens(); got != 400000 {
			t.Fatalf("protocol %q did not bind endpoint capability: %d", protocol, got)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("chat and Responses shared a protocol cache: %d requests", requests.Load())
	}
	client, err := modelinfo.New(filepath.Join(config.Home(), "cache", "provider-models"))
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"openai_chat", "openai_responses"} {
		if _, ok := client.Get(modelinfo.Binding{ProviderID: "openai", Transport: protocol, BaseURL: server.URL}, "wire-private-model"); !ok {
			t.Fatalf("cache missing protocol %q", protocol)
		}
	}
}

func TestProviderContextUsesConfiguredHomeCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_MODELS_URL", "http://127.0.0.1:1")
	const model = "split-home-full-capacity-fixture"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"slug":"split-home-full-capacity-fixture","context_window":272000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`))
	}))
	defer server.Close()
	directory := filepath.Join(home, "cache")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	metadata := catalog.Catalog{"openai": {Models: map[string]catalog.Model{model: {Limit: catalog.Limit{Context: 1050000, Input: 922000, Output: 128000}}}}}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{"private-gateway": {Transport: "anthropic_messages", BaseURL: server.URL, APIKey: "fixture-key", Model: model, MaxTokens: 4096}}
	built, err := BuildProviderWithoutPreconnect(cfg, "private-gateway", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := built.Provider.(llm.ContextCapabilitiesProvider).RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
	if got.Window != 1050000 || got.MaxInputTokens != 922000 || got.Source != "catalog_profile" || got.ProfileDefaultWindow != 272000 {
		t.Fatalf("resolver ignored configured home and borrowed unrelated HOME: %+v", got)
	}
}
