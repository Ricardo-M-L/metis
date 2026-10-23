package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type settingsEnvelope struct {
	Revision     string         `json:"revision"`
	Providers    []providerView `json:"providers"`
	SavedDefault struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"savedDefault"`
	Current struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"current"`
}

func settingsAPI(t *testing.T, h http.Handler, method, path string, body any) (*httptest.ResponseRecorder, settingsEnvelope) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, path, bytes.NewReader(data)))
	var response settingsEnvelope
	if rr.Code >= 200 && rr.Code < 300 {
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return rr, response
}

func TestProviderSettingsCASLifecycleAndDefaultRoutes(t *testing.T) {
	isolatedWebProviderConfig(t)
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{ID: "existing-default", Transport: "openai_chat", BaseURL: "https://existing.example/v1", Model: "existing-model"}); err != nil {
		t.Fatal(err)
	}
	s, store := testServer(t)
	s.activeProviderName, s.activeModel = "startup", "startup-model"
	s.activeSessionID = "before-settings"
	h := s.handler()
	rr, initial := settingsAPI(t, h, http.MethodGet, "/api/providers", nil)
	if rr.Code != http.StatusOK || initial.Revision == "" || len(initial.Providers) != 1 {
		t.Fatalf("snapshot %d %s", rr.Code, rr.Body.String())
	}
	const secret = "test-provider-settings-secret"
	body := map[string]any{"expectedRevision": initial.Revision, "id": "settings-gateway", "displayName": "Settings gateway", "transport": "openai_chat", "baseUrl": "https://settings.example/v1", "model": "first", "models": []map[string]any{{"id": "first", "name": "First", "contextWindow": 1000000, "maxOutputTokens": 4096, "outputBudget": 1024}, {"id": "second", "contextWindow": 200000, "outputBudget": 1536}}, "credentialAction": "replace", "apiKey": secret}
	rr, created := settingsAPI(t, h, http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	if created.Revision == initial.Revision || created.SavedDefault != initial.SavedDefault || created.Current.Provider != "startup" {
		t.Fatalf("save changed runtime/default: %+v", created)
	}
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatal("credential leaked in save response")
	}
	modelsRR, _ := settingsAPI(t, h, http.MethodGet, "/api/models", nil)
	if !strings.Contains(modelsRR.Body.String(), `"model":"first"`) || !strings.Contains(modelsRR.Body.String(), `"model":"second"`) || !strings.Contains(modelsRR.Body.String(), `"outputBudget":1024`) {
		t.Fatalf("composer lost saved models: %s", modelsRR.Body.String())
	}
	rr, _ = settingsAPI(t, h, http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale save %d %s", rr.Code, rr.Body.String())
	}
	key, _ := auth.Get("settings-gateway")
	if key != secret {
		t.Fatal("stale save changed key")
	}
	if err := store.WriteHeaderFull(session.Header{ID: "before-settings", Provider: "startup", Model: "startup-model", System: "original"}); err != nil {
		t.Fatal(err)
	}
	rr, selected := settingsAPI(t, h, http.MethodPost, "/api/providers/default", map[string]any{"id": "settings-gateway", "model": "second", "expectedRevision": created.Revision})
	if rr.Code != http.StatusOK || selected.SavedDefault.Provider != "settings-gateway" || selected.SavedDefault.Model != "second" {
		t.Fatalf("default %d %s", rr.Code, rr.Body.String())
	}
	if selected.Current.Provider != "startup" || selected.Current.Model != "startup-model" {
		t.Fatal("default silently changed current session")
	}
	s.freshSystem = "identity\nobsolete-provider-hint"
	s.freshSystemSections = []llm.SystemSection{{Name: "identity", Body: "identity"}, {Name: "provider_hint", Body: "obsolete-provider-hint"}}
	s.buildProvider = func(provider, model string) (*rtpkg.ProviderBuild, error) {
		return &rtpkg.ProviderBuild{Model: model}, nil
	}
	newRR, _ := settingsAPI(t, h, http.MethodPost, "/api/sessions", map[string]any{})
	if newRR.Code != http.StatusCreated {
		t.Fatalf("new session %d %s", newRR.Code, newRR.Body.String())
	}
	var item sessionItem
	_ = json.Unmarshal(newRR.Body.Bytes(), &item)
	header, _, err := store.LoadHeader(item.ID)
	if err != nil || header.Provider != "settings-gateway" || header.Model != "second" {
		t.Fatalf("new session did not adopt saved default: %+v %v", header, err)
	}
	if strings.Contains(header.System, "obsolete-provider-hint") || !strings.Contains(header.System, "identity") {
		t.Fatalf("new conversation kept startup provider prompt: %q", header.System)
	}
	old, _, _ := store.LoadHeader("before-settings")
	if old.Provider != "startup" || old.Model != "startup-model" || old.System != "original" {
		t.Fatal("saved default rewrote old conversation")
	}
	rr, _ = settingsAPI(t, h, http.MethodDelete, "/api/providers", map[string]any{"id": "settings-gateway", "expectedRevision": selected.Revision})
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete default %d %s", rr.Code, rr.Body.String())
	}
	rr, other := settingsAPI(t, h, http.MethodPost, "/api/providers/default", map[string]any{"id": initial.SavedDefault.Provider, "expectedRevision": selected.Revision})
	if rr.Code != http.StatusOK {
		t.Fatalf("restore default %d %s", rr.Code, rr.Body.String())
	}
	rr, deleted := settingsAPI(t, h, http.MethodDelete, "/api/providers", map[string]any{"id": "settings-gateway", "expectedRevision": other.Revision})
	if rr.Code != http.StatusOK || deleted.Revision == other.Revision {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if key, _ := auth.Get("settings-gateway"); key != "" {
		t.Fatal("delete left managed key")
	}
}

func TestProviderSettingsBuiltinKeyAndRevisionRequiredForNewForm(t *testing.T) {
	isolatedWebProviderConfig(t)
	s, _ := testServer(t)
	h := s.handler()
	_, initial := settingsAPI(t, h, http.MethodGet, "/api/providers", nil)
	body := map[string]any{"id": "openai", "displayName": "My OpenAI", "transport": "openai_chat", "baseUrl": "https://api.openai.com/v1", "model": "gpt-4o", "models": []config.ProviderModelSettings{{ID: "gpt-4o"}}, "credentialAction": "replace", "apiKey": "builtin-key-test"}
	rr, _ := settingsAPI(t, h, http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("new form without revision %d %s", rr.Code, rr.Body.String())
	}
	body["expectedRevision"] = initial.Revision
	rr, saved := settingsAPI(t, h, http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("builtin edit %d %s", rr.Code, rr.Body.String())
	}
	if saved.SavedDefault != initial.SavedDefault || strings.Contains(rr.Body.String(), "builtin-key-test") {
		t.Fatal("builtin save changed default/leaked key")
	}
	for _, provider := range saved.Providers {
		if provider.ID == "openai" && (provider.DisplayName != "My OpenAI" || provider.CredentialSource != "managed" || !provider.CredentialConfigured) {
			t.Fatalf("builtin view %+v", provider)
		}
	}
}

func TestProviderCatalogUsesLocalDetachedSnapshotWithoutNetwork(t *testing.T) {
	isolatedWebProviderConfig(t)
	var network atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer remote.Close()
	t.Setenv("METIS_MODELS_URL", remote.URL)
	path := filepath.Join(config.Home(), "cache", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"sample":{"id":"sample","name":"Sample","api":"https://sample.example/v1","npm":"@ai-sdk/openai-compatible","models":{"model-a":{"id":"model-a","name":"Model A","limit":{"context":123456,"output":7890},"modalities":{"input":["text","image"]}}}},"unsupported":{"id":"unsupported","npm":"unknown","models":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr, _ := settingsAPI(t, s.handler(), http.MethodGet, "/api/providers/catalog", nil)
	if rr.Code != http.StatusOK || network.Load() != 0 || !strings.Contains(rr.Body.String(), `"contextWindow":123456`) || strings.Contains(rr.Body.String(), `"id":"unsupported"`) {
		t.Fatalf("catalog %d %s network=%d", rr.Code, rr.Body.String(), network.Load())
	}
}

func TestProviderCatalogExplicitRefreshWarmsEmptyCacheWithoutChangingSettings(t *testing.T) {
	isolatedWebProviderConfig(t)
	configPath := filepath.Join(config.Home(), "config.toml")
	configBytes := []byte("[provider]\ndefault = \"openai\"\n")
	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := auth.Set("openai", "private-key-never-forwarded"); err != nil {
		t.Fatal(err)
	}
	credentialBefore, err := os.ReadFile(auth.Path())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var fail atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" || r.Header.Get("x-goog-api-key") != "" || r.ContentLength > 0 {
			t.Error("public catalog refresh used model credentials or a prompt")
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"standard":{"id":"standard","name":"Standard provider","api":"https://public.example/v1","npm":"@ai-sdk/openai-compatible","models":{"public-model":{"id":"public-model","name":"Public model","limit":{"context":987654,"output":4567}}}}}`))
	}))
	defer remote.Close()
	t.Setenv("METIS_MODELS_URL", remote.URL)
	s, _ := testServer(t)
	h := s.handler()
	rr, _ := settingsAPI(t, h, http.MethodGet, "/api/providers/catalog", nil)
	if rr.Code != http.StatusOK || calls.Load() != 0 || !bytes.Contains(rr.Body.Bytes(), []byte(`"providers":[]`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"networkRequested":false`)) {
		t.Fatalf("empty-cache GET: %d %s calls=%d", rr.Code, rr.Body.String(), calls.Load())
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rr, _ = settingsAPI(t, h, method, "/api/providers/catalog/refresh", nil)
		if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" || calls.Load() != 0 {
			t.Fatalf("refresh method gate: %s %d calls=%d", method, rr.Code, calls.Load())
		}
	}
	rr, _ = settingsAPI(t, h, http.MethodPost, "/api/providers/catalog/refresh", nil)
	if rr.Code != http.StatusOK || calls.Load() != 1 || !bytes.Contains(rr.Body.Bytes(), []byte(`"source":"refreshed_catalog"`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"networkRequested":true`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"id":"public-model"`)) {
		t.Fatalf("explicit refresh: %d %s calls=%d", rr.Code, rr.Body.String(), calls.Load())
	}
	cachePath := filepath.Join(config.Home(), "cache", "models.json")
	cacheBefore, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	rr, _ = settingsAPI(t, h, http.MethodGet, "/api/providers/catalog", nil)
	if rr.Code != http.StatusOK || calls.Load() != 1 || !bytes.Contains(rr.Body.Bytes(), []byte(`"id":"public-model"`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"source":"local_catalog"`)) {
		t.Fatalf("cached GET after refresh: %d %s calls=%d", rr.Code, rr.Body.String(), calls.Load())
	}
	fail.Store(true)
	rr, _ = settingsAPI(t, h, http.MethodPost, "/api/providers/catalog/refresh", nil)
	if rr.Code != http.StatusBadGateway || calls.Load() != 2 || !bytes.Contains(rr.Body.Bytes(), []byte("Cached entries are unchanged")) {
		t.Fatalf("failed refresh: %d %s calls=%d", rr.Code, rr.Body.String(), calls.Load())
	}
	cacheAfter, err := os.ReadFile(cachePath)
	if err != nil || !bytes.Equal(cacheAfter, cacheBefore) {
		t.Fatalf("failed refresh changed cache: %v", err)
	}
	rr, _ = settingsAPI(t, h, http.MethodGet, "/api/providers/catalog", nil)
	if rr.Code != http.StatusOK || calls.Load() != 2 || !bytes.Contains(rr.Body.Bytes(), []byte(`"id":"public-model"`)) {
		t.Fatalf("failed refresh lost cached templates: %d %s", rr.Code, rr.Body.String())
	}
	credentialAfter, err := os.ReadFile(auth.Path())
	if err != nil || !bytes.Equal(credentialAfter, credentialBefore) {
		t.Fatalf("catalog refresh changed credentials: %v", err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(configAfter, configBytes) {
		t.Fatalf("catalog refresh changed configuration: %v", err)
	}
}

func TestProviderSettingsSaveIntentPreventsAccidentalProviderReplacement(t *testing.T) {
	isolatedWebProviderConfig(t)
	for _, profile := range []config.ProviderSettingsProvider{{ID: "openai", Transport: "openai_chat", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o", Models: []config.ProviderModelSettings{{ID: "gpt-4o"}}}, {ID: "gemini", Transport: "gemini_native", BaseURL: "https://generativelanguage.googleapis.com", Model: "gemini-2.5-pro", Models: []config.ProviderModelSettings{{ID: "gemini-2.5-pro"}}}} {
		snapshot, err := config.ReadProviderSettings()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := config.SaveProviderSettings(config.ProviderSettingsMutation{Action: "upsert", ExpectedRevision: snapshot.Revision, Provider: &profile}); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := testServer(t)
	h := s.handler()
	_, initial := settingsAPI(t, h, http.MethodGet, "/api/providers", nil)
	body := map[string]any{"intent": "create", "expectedRevision": initial.Revision, "id": "intent-gateway", "transport": "openai_chat", "baseUrl": "https://intent.example/v1", "model": "original-model", "models": []map[string]any{{"id": "original-model"}, {"id": "second-model"}}, "credentialAction": "replace", "apiKey": "original-intent-key"}
	rr, created := settingsAPI(t, h, http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("explicit create: %d %s", rr.Code, rr.Body.String())
	}
	guard := map[string][]byte{}
	for _, path := range []string{filepath.Join(config.Home(), "config.toml"), filepath.Join(config.Home(), "provider-settings.json"), auth.Path()} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		guard[path] = data
	}
	for _, test := range []struct {
		name, intent, id string
		status           int
	}{{"custom collision", "create", "intent-gateway", http.StatusConflict}, {"built-in collision", "create", "openai", http.StatusConflict}, {"canonical alias collision", "create", "google", http.StatusConflict}, {"update missing", "update", "missing-gateway", http.StatusConflict}, {"unknown intent", "replace", "intent-gateway", http.StatusBadRequest}} {
		t.Run(test.name, func(t *testing.T) {
			request := map[string]any{"intent": test.intent, "expectedRevision": created.Revision, "id": test.id, "transport": "openai_chat", "baseUrl": "https://replacement.example/v1", "model": "replacement-model", "models": []map[string]any{{"id": "replacement-model"}}, "credentialAction": "replace", "apiKey": "replacement-intent-key"}
			rr, _ := settingsAPI(t, h, http.MethodPost, "/api/providers", request)
			if rr.Code != test.status {
				t.Fatalf("intent rejected incorrectly: %d %s", rr.Code, rr.Body.String())
			}
			for path, before := range guard {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("rejected intent modified configuration or credentials: %s err=%v", filepath.Base(path), err)
				}
			}
			snapshot, err := config.ReadProviderSettings()
			if err != nil || snapshot.Revision != created.Revision {
				t.Fatalf("rejected intent changed revision: %v", err)
			}
		})
	}
	body["intent"], body["expectedRevision"], body["credentialAction"] = "update", created.Revision, "keep"
	delete(body, "apiKey")
	body["displayName"] = "Explicitly updated"
	rr, updated := settingsAPI(t, h, http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusOK || updated.Revision == created.Revision {
		t.Fatalf("explicit update rejected: %d %s", rr.Code, rr.Body.String())
	}
	for _, provider := range updated.Providers {
		if provider.ID == "intent-gateway" && (provider.DisplayName != "Explicitly updated" || len(provider.Models) != 2) {
			t.Fatalf("explicit update lost intended model list: %+v", provider)
		}
	}
	if key, err := auth.Get("intent-gateway"); err != nil || key != "original-intent-key" {
		t.Fatalf("explicit update changed preserved key: %v", err)
	}
}
