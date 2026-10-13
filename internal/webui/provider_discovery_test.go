package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func TestProviderModelDiscoveryRetainsNewSlugCapabilitiesAndRouteCache(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("client_version") != "pi" || r.Header.Get("Authorization") != "Bearer capability-secret" {
			t.Errorf("model capabilities did not use the rich authenticated catalog")
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"never-seen-before","display_name":"New Model","context_window":272000,"max_context_window":872000,"max_input_tokens":144000,"max_tokens":128000}]}`))
	}))
	defer upstream.Close()
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{ID: "gateway", Transport: "openai_chat", BaseURL: upstream.URL + "/v1", Model: "old-model"}); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("gateway", "capability-secret", "openai_chat", upstream.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/models", strings.NewReader(`{"id":"gateway"}`)))
	var response struct {
		Models []struct {
			ID, Name, Source                                                 string
			ContextWindow, MaxContextWindow, MaxInputTokens, MaxOutputTokens int
		}
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || requests != 1 || len(response.Models) != 1 {
		t.Fatalf("discovery=%d requests=%d body=%s", rr.Code, requests, rr.Body.String())
	}
	m := response.Models[0]
	if m.ID != "never-seen-before" || m.Name != "New Model" || m.ContextWindow != 272000 || m.MaxContextWindow != 872000 || m.MaxInputTokens != 144000 || m.MaxOutputTokens != 128000 || m.Source != modelinfo.SourceEndpointPI {
		t.Fatalf("discovery dropped or conflated capacity metadata: %+v", m)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("capability-secret")) {
		t.Fatal("credential echoed")
	}
	c, err := modelinfo.New(filepath.Join(config.Home(), "cache", "provider-models"))
	if err != nil {
		t.Fatal(err)
	}
	b := modelinfo.Binding{ProviderID: "gateway", Transport: "openai_chat", BaseURL: upstream.URL + "/v1"}
	cached, ok := c.Get(b, m.ID)
	if !ok || cached.ContextWindow != 272000 || cached.MaxContextWindow != 872000 || cached.MaxInputTokens != 144000 || cached.MaxOutputTokens != 128000 {
		t.Fatalf("runtime cache=%+v ok=%v", cached, ok)
	}
	b.BaseURL += "/different"
	if _, ok := c.Get(b, m.ID); ok {
		t.Fatal("capacity cache crossed endpoint binding")
	}
}

func TestProviderModelDiscoveryInputCeilingDoesNotInventDefaultWindow(t *testing.T) {
	models, err := parseProviderModels([]byte(`{"data":[{"id":"input-only-new","max_input_tokens":500000}]}`), "openai_chat")
	if err != nil || len(models) != 1 || models[0].MaxInputTokens != 500000 || models[0].ContextWindow != 0 || models[0].MaxContextWindow != 0 || models[0].MaxOutputTokens != 0 {
		t.Fatalf("input ceiling was promoted to a context window: %+v err=%v", models, err)
	}
}

func TestDiscoveredProviderModelsKeepClientProfileSemantics(t *testing.T) {
	models := discoveredProviderModels([]modelinfo.Model{{ID: "new-profile-model", ContextWindow: 272000, MaxContextWindow: 872000, MaxOutputTokens: 128000, WindowKind: "client_profile", Source: modelinfo.SourceEndpointPI}})
	if len(models) != 1 || models[0].WindowKind != "client_profile" || models[0].ContextWindow != 272000 || models[0].MaxContextWindow != 872000 {
		t.Fatalf("client profile was promoted to model capacity: %+v", models)
	}
	wire, err := json.Marshal(models)
	if err != nil || !bytes.Contains(wire, []byte(`"windowKind":"client_profile"`)) {
		t.Fatalf("profile semantics absent from model selection response: %s err=%v", wire, err)
	}
}

func TestProviderModelDiscoveryUsesSavedEndpointWithoutEchoingCredential(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	const secret = "discovery-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("unexpected discovery request: method=%s path=%s authenticated=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") == "Bearer "+secret)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b","name":"Model B"},{"id":"model-a"},{"id":"model-b","name":"Model B"}]}`))
	}))
	defer upstream.Close()
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{ID: "gateway", Transport: "openai_responses", BaseURL: upstream.URL + "/v1", Model: "old-model"}); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("gateway", secret, "openai_responses", upstream.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/models", strings.NewReader(`{"id":"gateway"}`)))
	if rr.Code != http.StatusOK || bytes.Contains(rr.Body.Bytes(), []byte(secret)) {
		t.Fatalf("discovery = %d, leaked=%t: %s", rr.Code, bytes.Contains(rr.Body.Bytes(), []byte(secret)), rr.Body.String())
	}
	var got struct {
		Models []struct{ ID, Name string } `json:"models"`
		Source string                      `json:"source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "provider" || len(got.Models) != 2 || got.Models[0].ID != "model-a" || got.Models[1].ID != "model-b" {
		t.Fatalf("discovered models = %+v", got)
	}
}

func TestProviderModelDiscoveryDraftNeverForwardsOldKeyToChangedEndpoint(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("stored credential crossed to a draft endpoint")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"new-model"}]}`))
	}))
	defer upstream.Close()
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{ID: "gateway", Transport: "openai_chat", BaseURL: "http://127.0.0.1:1/v1", Model: "old-model"}); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("gateway", "old-secret", "openai_chat", "http://127.0.0.1:1/v1"); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	body, _ := json.Marshal(map[string]string{"id": "gateway", "transport": "openai_chat", "baseUrl": upstream.URL + "/v1"})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/models", bytes.NewReader(body)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("new-model")) {
		t.Fatalf("draft discovery = %d: %s", rr.Code, rr.Body.String())
	}
	cfg, _, err := config.Load()
	if err != nil || cfg.Provider.Custom["gateway"].Model != "old-model" {
		t.Fatalf("draft discovery changed config: %+v %v", cfg, err)
	}
}

func TestProviderModelDiscoveryCustomAnthropicUsesGatewayCatalogAuthentication(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/v1/models" || r.URL.Query().Get("client_version") != "pi" || r.Header.Get("Authorization") != "Bearer gateway-key" || r.Header.Get("x-api-key") != "gateway-key" {
			t.Errorf("custom Anthropic model discovery used an incompatible route or auth header")
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"new-model","context_window":272000}]}`))
	}))
	defer upstream.Close()
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{ID: "gateway", Transport: "anthropic_messages", BaseURL: upstream.URL + "/proxy", Model: "new-model"}); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("gateway", "gateway-key", "anthropic_messages", upstream.URL+"/proxy"); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/models", strings.NewReader(`{"id":"gateway"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"contextWindow":272000`)) {
		t.Fatalf("discovery=%d: %s", rr.Code, rr.Body.String())
	}
	client, err := modelinfo.New(filepath.Join(config.Home(), "cache", "provider-models"))
	if err != nil {
		t.Fatal(err)
	}
	binding := modelinfo.Binding{ProviderID: "gateway", Transport: "anthropic_messages", BaseURL: upstream.URL + "/proxy"}
	if _, ok := client.Get(binding, "new-model"); !ok {
		t.Fatal("discovery cache identity differs from the configured runtime endpoint")
	}
	binding.Transport = "openai_chat"
	if _, ok := client.Get(binding, "new-model"); ok {
		t.Fatal("capabilities crossed a transport binding")
	}
}

func TestProviderModelDiscoveryViewKeepsRuntimeDefaultTransport(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := &config.Config{Provider: config.ProviderSet{Custom: map[string]config.ProviderRaw{
		"default-wire": {BaseURL: "http://127.0.0.1:1", Model: "new"},
	}}}
	for _, view := range configuredProviderViews(cfg) {
		if view.ID == "default-wire" {
			if view.Transport != "anthropic_messages" {
				t.Fatalf("UI route differs from runtime default: %+v", view)
			}
			return
		}
	}
	t.Fatal("default route missing")
}

func TestProviderModelDiscoveryNativeOpenAIViewUsesConfiguredWireProtocol(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for _, tc := range []struct{ wire, want string }{
		{"", "openai_chat"}, {"chat", "openai_chat"}, {"responses", "openai_responses"},
	} {
		cfg := &config.Config{Provider: config.ProviderSet{OpenAI: config.ProviderOpenAI{WireProtocol: tc.wire}, Custom: map[string]config.ProviderRaw{}}}
		found := false
		for _, view := range configuredProviderViews(cfg) {
			if view.ID == "openai" {
				found = true
				if view.Transport != tc.want {
					t.Fatalf("wire=%q transport=%q want=%q", tc.wire, view.Transport, tc.want)
				}
			}
		}
		if !found {
			t.Fatal("native OpenAI route absent")
		}
	}
}

func TestProviderModelDiscoveryUsesCodexLocalCatalog(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	putWebCodexOAuth(t)
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/models", strings.NewReader(`{"id":"openai-codex"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"source":"local_catalog"`)) || !bytes.Contains(rr.Body.Bytes(), []byte("gpt-6-astra")) {
		t.Fatalf("Codex discovery = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestProviderModelSelectionPersistsWithoutChangingDefault(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{ID: "gateway", Transport: "openai_chat", BaseURL: "https://example.test/v1", Model: "old-model"}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveUserProviderDefault("openai-codex"); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/model", strings.NewReader(`{"id":"gateway","model":"new-model"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("save discovered model = %d: %s", rr.Code, rr.Body.String())
	}
	cfg, _, err := config.Load()
	if err != nil || cfg.Provider.Custom["gateway"].Model != "new-model" || cfg.Provider.Default != "openai-codex" {
		t.Fatalf("saved model/default = %+v, err=%v", cfg, err)
	}
}

func TestProviderModelSelectionPersistsBuiltInCodexModel(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	putWebCodexOAuth(t)
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/model", strings.NewReader(`{"id":"openai-codex","model":"gpt-6-astra"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"restartRequired":true`)) {
		t.Fatalf("save built-in model = %d: %s", rr.Code, rr.Body.String())
	}
	cfg, _, err := config.Load()
	if err != nil || cfg.Provider.OpenAICodex.Model != "gpt-6-astra" {
		t.Fatalf("saved Codex model = %+v, err=%v", cfg, err)
	}
}

func TestParseProviderModelsAcceptsSupportedListingShapes(t *testing.T) {
	for _, tc := range []struct {
		name, transport, body, wantID string
	}{
		{"anthropic", "anthropic_messages", `{"data":[{"id":"claude-new","display_name":"Claude New"}]}`, "claude-new"},
		{"gemini", "gemini_native", `{"models":[{"name":"models/gemini-new","displayName":"Gemini New","supportedGenerationMethods":["generateContent"]},{"name":"models/embed-only","supportedGenerationMethods":["embedContent"]}]}`, "gemini-new"},
		{"gateway map", "openai_chat", `{"models":{"alias-model":{"id":"upstream-model","name":"Alias"}}}`, "alias-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProviderModels([]byte(tc.body), tc.transport)
			if err != nil || len(got) != 1 || got[0].ID != tc.wantID {
				t.Fatalf("parsed = %+v, err = %v", got, err)
			}
		})
	}
	for _, body := range []string{`not json`, `{}`, `{"data":{}}`, `{"models":null}`} {
		if _, err := parseProviderModels([]byte(body), "openai_chat"); err == nil {
			t.Fatalf("accepted malformed model listing %q", body)
		}
	}
}

func TestProviderModelDiscoveryRefusesRedirectBeforeForwardingKey(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	redirected := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
	}))
	defer other.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/models", http.StatusFound)
	}))
	defer upstream.Close()
	s, _ := testServer(t)
	body, _ := json.Marshal(map[string]string{"transport": "openai_chat", "baseUrl": upstream.URL, "apiKey": "draft-secret"})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/models", bytes.NewReader(body)))
	if rr.Code != http.StatusBadGateway || redirected || bytes.Contains(rr.Body.Bytes(), []byte("draft-secret")) {
		t.Fatalf("redirect discovery = %d redirected=%t leaked=%t", rr.Code, redirected, bytes.Contains(rr.Body.Bytes(), []byte("draft-secret")))
	}
}

func TestProviderConnectionProbeRejectsNonSuccessStatus(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{ID: "gateway", Transport: "openai_chat", BaseURL: upstream.URL + "/v1", Model: "model"}); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("gateway", "secret", "openai_chat", upstream.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", strings.NewReader(`{"id":"gateway","confirm":true}`)))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("non-2xx probe claimed success: %d %s", rr.Code, rr.Body.String())
	}
}
