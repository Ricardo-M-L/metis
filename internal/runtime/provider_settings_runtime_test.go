package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

func prepareProviderSettingsRuntimeTest(t *testing.T) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Chdir(t.TempDir())
}

func saveProviderSettingsRuntimeTest(t *testing.T, spec config.ProviderSettingsProvider) *config.Config {
	t.Helper()
	snapshot, err := config.ReadProviderSettings()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveProviderSettings(config.ProviderSettingsMutation{
		Action: "upsert", ExpectedRevision: snapshot.Revision, Provider: &spec, APIKey: "fixture-key",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProviderModelSettingsActualWireAndCapabilities(t *testing.T) {
	for _, transportName := range []string{"openai_chat", "openai_responses", "anthropic_messages", "gemini_native"} {
		t.Run(transportName, func(t *testing.T) {
			prepareProviderSettingsRuntimeTest(t)
			var calls atomic.Int32
			var payload map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost {
					t.Errorf("unexpected pre-generation request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch transportName {
				case "openai_chat":
					io.WriteString(w, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				case "openai_responses":
					io.WriteString(w, `{"id":"fixture","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				case "anthropic_messages":
					io.WriteString(w, `{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "gemini_native":
					io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
				}
			}))
			defer server.Close()
			vision := true
			entry := config.ProviderModelSettings{ID: "fixture-private-model", ContextWindow: 1_000_000, MaxOutputTokens: 16_384, OutputBudget: 1_337}
			if transportName != "gemini_native" {
				entry.SupportsVision = &vision
			}
			cfg := saveProviderSettingsRuntimeTest(t, config.ProviderSettingsProvider{
				ID: "runtime-fixture", Kind: "custom", DisplayName: "Runtime fixture", Transport: transportName,
				BaseURL: server.URL, Model: entry.ID, Models: []config.ProviderModelSettings{entry},
			})
			built, err := BuildProviderWithoutPreconnect(cfg, "runtime-fixture", "")
			if err != nil {
				t.Fatal(err)
			}
			caps := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
			if built.MaxOutputTokens != 1_337 || built.Provider.MaxContextTokens() != 1_000_000 || caps.ModelWindow != 1_000_000 || caps.ModelSource != "user" || caps.MaxOutputTokens != 16_384 || caps.MaxInputTokens != 0 {
				t.Fatalf("settings did not reach runtime: build=%+v caps=%+v", built, caps)
			}
			if entry.SupportsVision != nil && pubprovider.ProviderVisionCapability(built.Provider) != pubprovider.VisionSupported {
				t.Fatal("per-model vision declaration was lost")
			}
			loop := &agent.Loop{Provider: built.Provider, Model: built.Model, Compactor: &agent.Compactor{MaxOutputTokens: built.MaxOutputTokens}}
			loop.SyncContextCapabilities()
			if window, _, _ := loop.ContextStatusSnapshot(); window != 1_000_000 {
				t.Fatalf("loop retained stale context: %d", window)
			}
			if loop.Compactor.MaxContextTokens != 1_000_000 || loop.Compactor.MaxOutputTokens != 1_337 {
				t.Fatalf("compactor retained stale settings: %+v", loop.Compactor)
			}
			if calls.Load() != 0 {
				t.Fatalf("construction/status contacted endpoint: %d", calls.Load())
			}
			_, err = built.Provider.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "OK"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			var wireOutput any
			switch transportName {
			case "openai_responses":
				wireOutput = payload["max_output_tokens"]
			case "gemini_native":
				wireOutput = payload["generationConfig"].(map[string]any)["maxOutputTokens"]
			default:
				wireOutput = payload["max_tokens"]
			}
			if wireOutput != float64(1_337) || calls.Load() != 1 {
				t.Fatalf("wire output budget=%v calls=%d payload=%v", wireOutput, calls.Load(), payload)
			}
		})
	}
}

func TestProviderModelSettingsDoNotLeakAcrossModelOrRoute(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	t.Setenv("METIS_RUNTIME_FIXTURE_ROUTE_KEY", "fixture-key")
	entry := config.ProviderModelSettings{ID: "fixture-private-model", ContextWindow: 1_000_000, OutputBudget: 1_337}
	cfg := saveProviderSettingsRuntimeTest(t, config.ProviderSettingsProvider{
		ID: "runtime-fixture", Kind: "custom", DisplayName: "Runtime fixture", Transport: "openai_chat",
		BaseURL: "http://127.0.0.1:18627/v1", Model: entry.ID, Models: []config.ProviderModelSettings{entry},
	})
	for _, change := range []string{"model", "endpoint", "transport", "provider"} {
		t.Run(change, func(t *testing.T) {
			copied := *cfg
			copied.Provider.Custom = make(map[string]config.ProviderRaw)
			for key, value := range cfg.Provider.Custom {
				copied.Provider.Custom[key] = value
			}
			raw := copied.Provider.Custom["runtime-fixture"]
			raw.APIKeyEnv = "METIS_RUNTIME_FIXTURE_ROUTE_KEY"
			id, selected := "runtime-fixture", ""
			switch change {
			case "model":
				selected = "fixture-other-model"
			case "endpoint":
				raw.BaseURL = "http://127.0.0.1:18628/v1"
			case "transport":
				raw.Transport = "anthropic_messages"
			case "provider":
				id = "runtime-other-fixture"
			}
			copied.Provider.Custom[id] = raw
			built, err := BuildProviderWithoutPreconnect(&copied, id, selected)
			if err != nil {
				t.Fatal(err)
			}
			caps := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
			if caps.ModelWindow == 1_000_000 || caps.ModelSource == "user" || built.MaxOutputTokens == 1_337 {
				t.Fatalf("settings escaped exact binding: caps=%+v output=%d", caps, built.MaxOutputTokens)
			}
		})
	}
}

func TestProviderModelSettingsKnownLimitsAndWorkingBudgets(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		base                   llm.ContextCapabilities
		settings               config.ProviderModelSettings
		wantWindow, wantOutput int
	}{
		{"unknown fallback", llm.ContextCapabilities{Window: 128_000, Source: "fallback"}, config.ProviderModelSettings{ContextWindow: 1_000_000, MaxOutputTokens: 16_384}, 1_000_000, 16_384},
		{"known hard limit", llm.ContextCapabilities{Window: 128_000, MaxWindow: 128_000, ModelWindow: 128_000, ModelSource: "catalog_route", MaxOutputTokens: 16_384, ModelMaxOutputTokens: 16_384, Source: "catalog_route", LimitSource: "catalog_route"}, config.ProviderModelSettings{ContextWindow: 1_000_000, MaxOutputTokens: 32_768}, 128_000, 16_384},
		{"smaller user declaration", llm.ContextCapabilities{Window: 128_000, MaxWindow: 128_000, ModelWindow: 128_000}, config.ProviderModelSettings{ContextWindow: 64_000}, 64_000, 0},
		{"working budget retained", llm.ContextCapabilities{Window: 150_000, RequestedWindow: 150_000, BudgetSource: llm.ContextBudgetPerModel}, config.ProviderModelSettings{ContextWindow: 1_000_000}, 150_000, 0},
		{"independent input ceiling", llm.ContextCapabilities{Window: 65_536, WindowKind: llm.ContextWindowInput, MaxInputTokens: 65_536, MaxOutputTokens: 131_072}, config.ProviderModelSettings{ContextWindow: 1_000_000}, 65_536, 131_072},
		{"output budget is not capacity", llm.ContextCapabilities{Window: 128_000, Source: "fallback"}, config.ProviderModelSettings{OutputBudget: 1_337}, 128_000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyProviderModelCapabilitySettings(tc.base, tc.settings)
			if got.Window != tc.wantWindow || got.MaxOutputTokens != tc.wantOutput {
				t.Fatalf("incorrect merged limits: %+v", got)
			}
			if tc.settings.ContextWindow > 0 {
				wantModel := smallestKnownModelLimit(tc.base.ModelWindow, tc.settings.ContextWindow)
				wantSource := tc.base.ModelSource
				if tc.base.ModelWindow <= 0 || tc.settings.ContextWindow < tc.base.ModelWindow {
					wantSource = "user"
				}
				if got.ModelWindow != wantModel || got.ModelSource != wantSource {
					t.Fatalf("model capacity/provenance incorrect: %+v", got)
				}
			}
			if tc.settings.ContextWindow == 0 && (got.MaxWindow != tc.base.MaxWindow || got.ModelWindow != tc.base.ModelWindow) {
				t.Fatalf("output budget manufactured capacity: %+v", got)
			}
			if got.RequestedWindow != tc.base.RequestedWindow || got.BudgetSource != tc.base.BudgetSource {
				t.Fatalf("working policy changed: %+v", got)
			}
		})
	}
}

func TestProviderModelSettingsRejectImpossibleOutputBeforeGeneration(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	for _, settings := range []config.ProviderModelSettings{
		{ContextWindow: 4_096, OutputBudget: 4_096},
		{ContextWindow: 100_000, MaxOutputTokens: 1_024, OutputBudget: 2_048},
	} {
		provider := openai.New("fixture-key", "http://127.0.0.1:18627/v1", "fixture-private-model", 8_192, 0, 0)
		if _, err := finalizeProviderModelSettingsBuild(provider, provider.ModelID(), provider.MaxTokens, &settings); err == nil {
			t.Fatalf("accepted impossible output budget: %+v", settings)
		}
	}
}

func TestProviderModelSettingsInheritedOutputAndUnknownVision(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	provider := openai.New("fixture-key", "http://127.0.0.1:18627/v1", "fixture-private-model", 0, 0, 0)
	baseVision := pubprovider.ProviderVisionCapability(provider)
	built, err := finalizeProviderModelSettingsBuild(provider, provider.ModelID(), 0, &config.ProviderModelSettings{ContextWindow: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if built.MaxOutputTokens != provider.MaxTokens || pubprovider.ProviderVisionCapability(built.Provider) != baseVision {
		t.Fatalf("inherited defaults changed: output=%d expected=%d vision=%v", built.MaxOutputTokens, provider.MaxTokens, pubprovider.ProviderVisionCapability(built.Provider))
	}
}

func TestProviderModelSettingsRequestBudgetCannotBypassLimit(t *testing.T) {
	p := providerModelSettingsProvider{settings: config.ProviderModelSettings{OutputBudget: 1_024}}
	// The fake is the base OpenAI client only; no request is sent here.
	p.Provider = openai.New("fixture-key", "http://127.0.0.1:18627/v1", "fixture-private-model", 1_024, 0, 0)
	for _, requested := range []int{0, 512, 2_048} {
		req, err := p.prepareRequest(llm.Request{MaxTokens: requested})
		if err != nil || req.MaxTokens != map[int]int{0: 1_024, 512: 512, 2_048: 1_024}[requested] {
			t.Fatalf("request budget not bounded: %+v err=%v", req, err)
		}
	}
	p.settings.MaxOutputTokens = 512
	if _, err := p.prepareRequest(llm.Request{}); !errors.Is(err, llm.ErrContextOutputBudget) {
		t.Fatalf("impossible refreshed output was sent: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.RefreshContextCapabilities(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh ignored cancellation: %v", err)
	}
}

func TestProviderModelSettingsStreamUsesBoundedOutput(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	var output atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		output.Store(int64(body["max_tokens"].(float64)))
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	provider := openai.New("fixture-key", server.URL, "fixture-private-model", 8_192, 0, 0)
	built, err := finalizeProviderModelSettingsBuild(provider, provider.ModelID(), provider.MaxTokens, &config.ProviderModelSettings{OutputBudget: 1_337})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := built.Provider.Stream(context.Background(), llm.Request{MaxTokens: 4_096, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "OK"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for {
		_, err := reader.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if output.Load() != 1_337 {
		t.Fatalf("stream encoder retained old output: %d", output.Load())
	}
}

func TestProviderModelSettingsPreservesOptionalContracts(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	for _, responses := range []bool{false, true} {
		var provider llm.Provider
		if responses {
			provider = openai.NewResponses("fixture-key", "http://127.0.0.1:18627/v1", "fixture-private-model", 4_096, 0, 0)
		} else {
			provider = openai.New("fixture-key", "http://127.0.0.1:18627/v1", "fixture-private-model", 4_096, 0, 0)
		}
		_, beforeHistory := provider.(llm.ContextHistoryPolicy)
		built, err := finalizeProviderModelSettingsBuild(provider, provider.ModelID(), 4_096, &config.ProviderModelSettings{ContextWindow: 1_000_000})
		if err != nil {
			t.Fatal(err)
		}
		_, afterHistory := built.Provider.(llm.ContextHistoryPolicy)
		if beforeHistory != afterHistory {
			t.Fatal("history policy interface changed")
		}
		managed, _ := built.Provider.(interface{ ManagesRecoverySession() bool })
		if managed == nil || managed.ManagesRecoverySession() != responses {
			t.Fatal("recovery marker changed")
		}
	}
}

func TestProviderModelSettingsNoDeclarationPreservesConcreteClient(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	provider := openai.New("fixture-key", "http://127.0.0.1:18627/v1", "fixture-private-model", 4_096, 0, 0)
	built, err := finalizeProviderModelSettingsBuild(provider, provider.ModelID(), 4_096, &config.ProviderModelSettings{ID: provider.ModelID(), Name: "Display only"})
	if err != nil || built.Provider != provider {
		t.Fatalf("display name wrapped/changed runtime: %+v err=%v", built, err)
	}
	if strings.Contains(built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities().ModelSource, "user") {
		t.Fatal("display name manufactured a user capacity declaration")
	}
}

func TestProviderModelSettingsWorkingBudgetStillRefreshes(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	entry := config.ProviderModelSettings{ID: "fixture-private-model", ContextWindow: 1_000_000, OutputBudget: 1_337}
	cfg := saveProviderSettingsRuntimeTest(t, config.ProviderSettingsProvider{ID: "runtime-fixture", Kind: "custom", Transport: "openai_chat", BaseURL: server.URL, Model: entry.ID, Models: []config.ProviderModelSettings{entry}})
	route, err := ProviderContextBudgetRoute(cfg, "runtime-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetContextBudget(route, entry.ID, 150_000); err != nil {
		t.Fatal(err)
	}
	built, err := BuildProviderWithoutPreconnect(cfg, "runtime-fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	metadata := built.Provider.(llm.ContextCapabilitiesProvider)
	before := metadata.ContextCapabilities()
	if before.Window != 150_000 || before.RequestedWindow != 150_000 || before.BudgetSource != llm.ContextBudgetPerModel || before.ModelWindow != 1_000_000 {
		t.Fatalf("initial working policy lost: %+v", before)
	}
	if err := config.SetContextBudget(route, entry.ID, 650_000); err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{Provider: built.Provider, Model: built.Model, Compactor: &agent.Compactor{MaxOutputTokens: built.MaxOutputTokens}}
	loop.SyncContextCapabilities()
	if calls.Load() != 0 {
		t.Fatal("cached status did discovery")
	}
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := metadata.ContextCapabilities()
	if after.Window != 650_000 || after.RequestedWindow != 650_000 || after.RouteKey != before.RouteKey || after.BudgetSource != llm.ContextBudgetPerModel {
		t.Fatalf("refresh replaced declaration or policy ownership: %+v", after)
	}
	if loop.Compactor.MaxContextTokens != 650_000 || loop.Compactor.MaxOutputTokens != 1_337 {
		t.Fatalf("compactor did not adopt refreshed working policy: %+v", loop.Compactor)
	}
}

func TestProviderModelSettingsOutputProvenanceAndClamp(t *testing.T) {
	base := llm.ContextCapabilities{Window: 800_000, ModelWindow: 1_000_000, MaxWindow: 1_000_000, ModelSource: "catalog_route", LimitSource: "catalog_route", RequestedWindow: 800_000, BudgetSource: llm.ContextBudgetPerModel, Source: "configured"}
	output := applyProviderModelCapabilitySettings(base, config.ProviderModelSettings{MaxOutputTokens: 64_000})
	if output.ModelSource != "catalog_route" || output.ModelWindow != 1_000_000 || output.ModelOutputSource != "user" {
		t.Fatalf("output declaration relabelled full model capacity: %+v", output)
	}
	clamped := applyProviderModelCapabilitySettings(base, config.ProviderModelSettings{ContextWindow: 500_000})
	if clamped.Window != 500_000 || clamped.RequestedWindow != 800_000 || clamped.Source != "configured_clamped" || clamped.LimitSource != "user" {
		t.Fatalf("clamped budget misreported: %+v", clamped)
	}
}

type providerSettingsDynamicCaps struct {
	llm.Provider
	caps  atomic.Pointer[llm.ContextCapabilities]
	calls atomic.Int32
}

func (p *providerSettingsDynamicCaps) ContextCapabilities() llm.ContextCapabilities {
	return *p.caps.Load()
}
func (p *providerSettingsDynamicCaps) RefreshContextCapabilities(context.Context) error { return nil }
func (p *providerSettingsDynamicCaps) Complete(context.Context, llm.Request) (*llm.Response, error) {
	p.calls.Add(1)
	return &llm.Response{}, nil
}
func (p *providerSettingsDynamicCaps) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls.Add(1)
	return nil, errors.New("unexpected stream dispatch")
}

func TestProviderModelSettingsDefaultOutputCannotBypassRefreshedHardLimit(t *testing.T) {
	prepareProviderSettingsRuntimeTest(t)
	fake := &providerSettingsDynamicCaps{Provider: openai.New("fixture-key", "http://127.0.0.1:18627/v1", "fixture-private-model", 64_000, 0, 0)}
	fake.caps.Store(&llm.ContextCapabilities{Window: 1_000_000, MaxOutputTokens: 128_000})
	built, err := finalizeProviderModelSettingsBuild(fake, fake.ModelID(), 64_000, &config.ProviderModelSettings{MaxOutputTokens: 128_000})
	if err != nil {
		t.Fatal(err)
	}
	fake.caps.Store(&llm.ContextCapabilities{Window: 1_000_000, MaxOutputTokens: 32_000})
	if _, err := built.Provider.Complete(context.Background(), llm.Request{}); !errors.Is(err, llm.ErrContextOutputBudget) {
		t.Fatalf("zero request budget bypassed lower hard limit: %v", err)
	}
	if _, err := built.Provider.Stream(context.Background(), llm.Request{}); !errors.Is(err, llm.ErrContextOutputBudget) {
		t.Fatalf("stream zero request budget bypassed lower hard limit: %v", err)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("invalid generation reached provider")
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			for j := 0; j < 50; j++ {
				caps := built.Provider.(llm.ContextCapabilitiesProvider).ContextCapabilities()
				if caps.MaxOutputTokens != 32_000 {
					t.Errorf("torn capabilities: %+v", caps)
					return
				}
			}
		})
	}
	workers.Wait()
}

func TestProviderModelSettingsReasoningReachesActualWire(t *testing.T) {
	for _, tc := range []struct {
		name, transport string
		enabled         bool
		mapping         map[string]string
	}{
		{"chat disabled", "openai_chat", false, nil},
		{"responses disabled", "openai_responses", false, nil},
		{"anthropic disabled", "anthropic_messages", false, nil},
		{"chat mapped", "openai_chat", true, map[string]string{"low": "minimal", "medium": "medium", "high": "xhigh"}},
		{"responses mapped", "openai_responses", true, map[string]string{"low": "minimal", "medium": "medium", "high": "xhigh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepareProviderSettingsRuntimeTest(t)
			var wire map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch tc.transport {
				case "openai_chat":
					io.WriteString(w, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
				case "openai_responses":
					io.WriteString(w, `{"id":"fixture","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`)
				case "anthropic_messages":
					io.WriteString(w, `{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`)
				}
			}))
			defer server.Close()
			entry := config.ProviderModelSettings{ID: "fixture-private-model", ContextWindow: 1_000_000, OutputBudget: 1_337, SupportsReasoning: &tc.enabled, ReasoningEfforts: tc.mapping}
			cfg := saveProviderSettingsRuntimeTest(t, config.ProviderSettingsProvider{ID: "runtime-fixture", Kind: "custom", Transport: tc.transport, BaseURL: server.URL, Model: entry.ID, Models: []config.ProviderModelSettings{entry}})
			built, err := BuildProviderWithoutPreconnect(cfg, "runtime-fixture", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := built.Provider.Complete(context.Background(), llm.Request{Effort: llm.EffortHigh, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "OK"}}}}}); err != nil {
				t.Fatal(err)
			}
			if !tc.enabled {
				for _, key := range []string{"reasoning", "reasoning_effort", "thinking"} {
					if wire[key] != nil {
						t.Fatalf("disabled reasoning leaked %s=%v", key, wire[key])
					}
				}
			} else if tc.transport == "openai_chat" {
				if wire["reasoning_effort"] != "xhigh" {
					t.Fatalf("declared effort did not reach chat wire: %v", wire)
				}
			} else if wire["reasoning"].(map[string]any)["effort"] != "xhigh" {
				t.Fatalf("declared effort did not reach Responses wire: %v", wire)
			}
		})
	}
}
