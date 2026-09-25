package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestProviderModelDiscoveryUsesSavedEndpointWithoutEchoingCredential(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	const secret = "discovery-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("unexpected discovery request: method=%s path=%s authenticated=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") == "Bearer "+secret)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b","name":"Model B"},{"id":"model-a"},{"id":"model-b"},{"name":"missing-id"}]}`))
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
	if got.Source != "provider" || len(got.Models) != 2 || got.Models[0].ID != "model-b" || got.Models[1].ID != "model-a" {
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
