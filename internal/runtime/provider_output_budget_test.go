package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

func outputBudgetCatalogFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_CATALOG_DISABLE", "")
	models := catalog.Catalog{
		"deepseek": {Models: map[string]catalog.Model{
			"deepseek-flash": {ID: "deepseek-flash", Reasoning: true, Limit: catalog.Limit{Context: 1_048_576, Output: 393_216}},
			"deepseek-chat":  {ID: "deepseek-chat", Limit: catalog.Limit{Context: 131_072, Output: 65_536}},
		}},
		// A reseller's differing total prevents generic model-only context
		// resolution; the exact issuer entry still establishes a recommendation.
		"reseller": {Models: map[string]catalog.Model{
			"deepseek-flash": {ID: "deepseek-flash", Reasoning: true, Limit: catalog.Limit{Context: 1_000_000, Output: 384_000}},
		}},
		"openai": {Models: map[string]catalog.Model{
			"gpt-6.1-sol": {ID: "gpt-6.1-sol", Reasoning: true, Limit: catalog.Limit{Context: 1_050_000, Output: 128_000}},
		}},
		"moonshotai": {Models: map[string]catalog.Model{
			"kimi-k3": {ID: "kimi-k3", Reasoning: true, Limit: catalog.Limit{Context: 1_000_000, Output: 64_000}},
		}},
	}
	data, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "cache", "models.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func outputBudgetRaw(transportName, model string) config.ProviderRaw {
	return config.ProviderRaw{Transport: transportName, APIKey: "fixture-key", BaseURL: "https://budget-fixture.invalid/v1", Model: model}
}

func TestCustomDeepSeekDefaultOutputBudgetUsesExactModelRecommendation(t *testing.T) {
	for _, transportName := range []string{"openai_chat", "openai_responses"} {
		t.Run(transportName, func(t *testing.T) {
			outputBudgetCatalogFixture(t)
			raw := outputBudgetRaw(transportName, "deepseek-flash")
			built, err := BuildProviderWithoutPreconnect(newCfgWithCustom("friendly-proxy", raw), "friendly-proxy", "")
			if err != nil {
				t.Fatal(err)
			}
			if built.MaxOutputTokens != 65_536 {
				t.Fatalf("reasoning model retained low generic default: got %d, want 65536; caps=%+v", built.MaxOutputTokens, built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities())
			}
			if got := llm.ProviderDefaultOutputBudget(built.Provider); got != built.MaxOutputTokens {
				t.Fatalf("adapter default %d differs from build reservation %d", got, built.MaxOutputTokens)
			}
			bound := llm.BindExecutionProvider(built.Provider, context.Background())
			if got := llm.ProviderDefaultOutputBudget(bound); got != built.MaxOutputTokens {
				t.Fatalf("execution binding lost the adapter budget: %d", got)
			}
		})
	}
}

func TestCustomOutputBudgetPreservesOtherModelsAndExplicitChoices(t *testing.T) {
	for _, tc := range []struct {
		name, transport, model string
		explicit, want         int
	}{
		{name: "explicit low budget", model: "deepseek-flash", explicit: 1024, want: 1024},
		{name: "explicit high budget", model: "deepseek-flash", explicit: 100_000, want: 100_000},
		{name: "GPT default unchanged", model: "gpt-6.1-sol", want: 8192},
		{name: "Kimi default unchanged", model: "kimi-k3", want: 8192},
		{name: "unknown model unchanged", model: "deepseek-new-private", want: 8192},
		{name: "similar model unchanged", model: "deepseek-flash-copy", want: 8192},
		{name: "nonreasoning model unchanged", model: "deepseek-chat", want: 8192},
		{name: "other protocol unchanged", transport: "anthropic_messages", model: "deepseek-flash", want: 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputBudgetCatalogFixture(t)
			if tc.transport == "" {
				tc.transport = "openai_chat"
			}
			raw := outputBudgetRaw(tc.transport, tc.model)
			raw.MaxTokens = tc.explicit
			built, err := BuildProviderWithoutPreconnect(newCfgWithCustom("budget-fixture", raw), "budget-fixture", "")
			if err != nil {
				t.Fatal(err)
			}
			if built.MaxOutputTokens != tc.want || llm.ProviderDefaultOutputBudget(built.Provider) != tc.want {
				t.Fatalf("budget changed: build=%d adapter=%d want=%d", built.MaxOutputTokens, llm.ProviderDefaultOutputBudget(built.Provider), tc.want)
			}
		})
	}
}

func TestCustomDeepSeekOutputBudgetHonorsModelSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings config.ProviderModelSettings
		want     int
	}{
		{name: "explicit model budget", settings: config.ProviderModelSettings{OutputBudget: 4096}, want: 4096},
		{name: "smaller output capacity", settings: config.ProviderModelSettings{MaxOutputTokens: 16_384}, want: 16_384},
		{name: "small working window", settings: config.ProviderModelSettings{ContextWindow: 12_000}, want: 6000},
		{name: "reasoning disabled", settings: config.ProviderModelSettings{SupportsReasoning: boolPtr(false)}, want: 8192},
		{name: "vision wrapper preserves budget", settings: config.ProviderModelSettings{SupportsVision: boolPtr(true)}, want: 65_536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputBudgetCatalogFixture(t)
			raw := outputBudgetRaw("openai_chat", "deepseek-flash")
			built, err := buildCustomProvider(newCfgWithCustom("budget-fixture", raw), "budget-fixture", raw, "", false, &tc.settings)
			if err != nil {
				t.Fatal(err)
			}
			if built.MaxOutputTokens != tc.want || llm.ProviderDefaultOutputBudget(built.Provider) != tc.want {
				t.Fatalf("model setting not preserved: build=%d adapter=%d want=%d", built.MaxOutputTokens, llm.ProviderDefaultOutputBudget(built.Provider), tc.want)
			}
		})
	}
}

func TestCustomDeepSeekOutputBudgetUsesServingCapAndActualWire(t *testing.T) {
	home := outputBudgetCatalogFixture(t)
	var payload map[string]any
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"data":[{"id":"deepseek-flash","context_window":1048576,"max_output_tokens":16384}]}`)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	client, err := modelinfo.New(filepath.Join(home, "cache", "provider-models"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Refresh(context.Background(), modelinfo.Binding{ProviderID: "budget-fixture", Transport: "openai_chat", BaseURL: server.URL, AuthKind: modelinfo.AuthBearer}, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	before := calls
	raw := outputBudgetRaw("openai_chat", "deepseek-flash")
	raw.BaseURL = server.URL
	built, err := BuildProviderWithoutPreconnect(newCfgWithCustom("budget-fixture", raw), "budget-fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("provider construction unexpectedly performed network discovery")
	}
	if built.MaxOutputTokens != 16_384 {
		t.Fatalf("default did not respect serving cap: %d", built.MaxOutputTokens)
	}
	for _, budget := range []int{0, 1024} {
		_, err = built.Provider.Complete(context.Background(), llm.Request{Effort: "high", MaxTokens: budget, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "OK"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		want := budget
		if want == 0 {
			want = 16_384
		}
		if payload["max_tokens"] != float64(want) || payload["reasoning_effort"] != "high" {
			t.Fatalf("wire budget/effort differs from reservation: %+v", payload)
		}
		if llm.ProviderDefaultOutputBudget(built.Provider) != 16_384 {
			t.Fatal("per-request cap mutated the shared adapter")
		}
	}
}

func TestCustomDeepSeekDefaultOutputBudgetOnWire(t *testing.T) {
	for _, protocol := range []string{"openai_chat", "openai_responses"} {
		t.Run(protocol, func(t *testing.T) {
			outputBudgetCatalogFixture(t)
			var payload map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("construction performed discovery: %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				if protocol == "openai_chat" {
					io.WriteString(w, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				} else {
					io.WriteString(w, `{"id":"fixture","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}
			}))
			defer server.Close()
			raw := outputBudgetRaw(protocol, "deepseek-flash")
			raw.BaseURL = server.URL
			built, err := BuildProviderWithoutPreconnect(newCfgWithCustom("budget-fixture", raw), "budget-fixture", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = built.Provider.Complete(context.Background(), llm.Request{Effort: "high", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "OK"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			field := "max_tokens"
			if protocol == "openai_responses" {
				field = "max_output_tokens"
			}
			if payload[field] != float64(65_536) {
				t.Fatalf("wire retained a different default: %s=%v", field, payload[field])
			}
		})
	}
}

func TestCustomDeepSeekAutomaticBudgetKeepsIndependentInputWindow(t *testing.T) {
	home := outputBudgetCatalogFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"deepseek-flash","max_input_tokens":2048,"max_output_tokens":32768}]}`)
	}))
	defer server.Close()
	client, err := modelinfo.New(filepath.Join(home, "cache", "provider-models"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Refresh(context.Background(), modelinfo.Binding{ProviderID: "budget-fixture", Transport: "openai_chat", BaseURL: server.URL, AuthKind: modelinfo.AuthBearer}, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	raw := outputBudgetRaw("openai_chat", "deepseek-flash")
	raw.BaseURL = server.URL
	built, err := BuildProviderWithoutPreconnect(newCfgWithCustom("budget-fixture", raw), "budget-fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
	if capabilities.WindowKind != llm.ContextWindowInput || capabilities.Window != 2048 || built.MaxOutputTokens != 32768 {
		t.Fatalf("input-only capacity incorrectly shrank output budget: caps=%+v budget=%d", capabilities, built.MaxOutputTokens)
	}
}

func TestDefaultOutputBudgetSubscriptionRemainsServerManaged(t *testing.T) {
	p := &openai.Responses{ProviderName: "openai-codex", MaxTokens: 64_000}
	if got := llm.ProviderDefaultOutputBudget(p); got != 0 {
		t.Fatalf("subscription must not claim an enforced request budget: %d", got)
	}
}
