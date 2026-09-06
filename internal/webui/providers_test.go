package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

func TestOpenAICodexProviderUsesOAuthWithoutExposingAPIKeyEditing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	const accessToken = "oauth-access-must-not-be-rendered"
	if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{
		AccessToken: accessToken, RefreshToken: "refresh-must-not-be-rendered", AccountID: "acct-test",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	_, store := testServer(t)
	s := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{
		BuildProvider: func(provider, model string) (*rtpkg.ProviderBuild, error) {
			return &rtpkg.ProviderBuild{Provider: &activationTestProvider{name: provider, model: model}, Model: model}, nil
		},
	})
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("provider list: %d %s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(accessToken)) || bytes.Contains(rr.Body.Bytes(), []byte("refresh-must-not-be-rendered")) {
		t.Fatalf("provider list leaked OAuth credential: %s", rr.Body.String())
	}
	var listed struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var codex *providerView
	for i := range listed.Providers {
		if listed.Providers[i].ID == "openai-codex" {
			codex = &listed.Providers[i]
			break
		}
	}
	if codex == nil {
		t.Fatal("OpenAI Codex missing from Desktop provider list")
	}
	if codex.Custom || !codex.CredentialConfigured || codex.CredentialKind != "oauth" || codex.SetupCommand != "metis login openai-codex" {
		t.Fatalf("OpenAI Codex view = %+v", *codex)
	}

	// The custom-provider API-key form must not be able to shadow or replace
	// the built-in OAuth-only provider.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewBufferString(
		`{"id":"openai-codex","transport":"openai_responses","baseUrl":"https://evil.invalid/v1","model":"gpt-test","apiKey":"must-not-be-stored"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("API-key overwrite status = %d: %s", rr.Code, rr.Body.String())
	}
	if key, err := auth.Get("openai-codex"); err != nil || key != "" {
		t.Fatalf("API key was stored for OAuth-only provider: %q, %v", key, err)
	}
	credential, err := auth.GetOAuth("openai-codex")
	if err != nil || credential == nil || credential.AccessToken != accessToken {
		t.Fatalf("OAuth credential changed after rejected API-key edit: %+v, %v", credential, err)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/default", bytes.NewBufferString(`{"id":"openai-codex"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("set default: %d %s", rr.Code, rr.Body.String())
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.Provider.Default != "openai-codex" {
		t.Fatalf("saved config = %+v", cfg)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/validate", bytes.NewBufferString(`{"id":"openai-codex"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"valid":true`)) {
		t.Fatalf("validate OAuth provider: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", bytes.NewBufferString(`{"id":"openai-codex","confirm":true}`)))
	if rr.Code != http.StatusBadRequest || bytes.Contains(rr.Body.Bytes(), []byte(accessToken)) {
		t.Fatalf("OAuth probe = %d %s", rr.Code, rr.Body.String())
	}
}

func TestOpenAICodexProviderRejectsLegacyOrIncompleteCredentials(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.Set("openai-codex", "legacy-api-key-must-not-count"); err == nil {
		t.Fatal("OAuth-only OpenAI Codex provider accepted an API-key credential")
	}
	if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{AccessToken: "access-without-account"}); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	h := s.handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("provider list: %d %s", rr.Code, rr.Body.String())
	}
	var listed struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, view := range listed.Providers {
		if view.ID == "openai-codex" {
			found = true
			if view.CredentialConfigured || view.CredentialKind != "oauth" || view.SetupCommand != "metis login openai-codex" {
				t.Fatalf("incomplete OAuth credential reported ready: %+v", view)
			}
		}
	}
	if !found {
		t.Fatal("OpenAI Codex missing from provider list")
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/validate", bytes.NewBufferString(`{"id":"openai-codex"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("incomplete credential validation = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAnthropicProviderViewMatchesRuntimeCredentialRules(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		apiKey     string
		configured bool
		kind       string
	}{
		{name: "official OAuth", baseURL: "https://api.anthropic.com", configured: true, kind: "oauth"},
		{name: "custom gateway rejects OAuth", baseURL: "https://gateway.example/v1", configured: false, kind: "api_key"},
		{name: "API key wins on custom gateway", baseURL: "https://gateway.example/v1", apiKey: "gateway-key", configured: true, kind: "api_key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			if err := auth.PutOAuth("anthropic", auth.OAuthCredential{AccessToken: "oauth-access"}); err != nil {
				t.Fatal(err)
			}
			if test.apiKey != "" {
				if err := auth.ActivateAPIKeyBound("anthropic", test.apiKey, "anthropic_messages", test.baseURL); err != nil {
					t.Fatal(err)
				}
			}
			cfg, _, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Provider.Anthropic.BaseURL = test.baseURL
			var got *providerView
			for _, view := range configuredProviderViews(cfg) {
				if view.ID == "anthropic" {
					copy := view
					got = &copy
					break
				}
			}
			if got == nil {
				t.Fatal("Anthropic missing from provider views")
			}
			if got.CredentialConfigured != test.configured || got.CredentialKind != test.kind {
				t.Fatalf("Anthropic view = %+v, want configured=%t kind=%q", *got, test.configured, test.kind)
			}
			if !test.configured && got.SetupCommand != "metis login anthropic" {
				t.Fatalf("setup command = %q", got.SetupCommand)
			}
		})
	}
}

func TestProviderSettingsRendersOAuthAsExternalLogin(t *testing.T) {
	js, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{
		[]byte("p.credentialKind === 'oauth'"),
		[]byte("p.credentialKind === 'api_key'"),
		[]byte("escHtml(p.setupCommand)"),
		[]byte("const probeButton = probeable ?"),
		[]byte("OAuth sign-in is completed with metis login in a terminal."),
	} {
		if !bytes.Contains(js, want) {
			t.Fatalf("chat.js missing OAuth provider surface %q", want)
		}
	}
}

func TestOpenAICodexValidateRejectsExpiredUnrefreshableOAuth(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{
		AccessToken: "expired-access", AccountID: "account-1", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("provider list = %d: %s", rr.Code, rr.Body.String())
	}
	var listed struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, view := range listed.Providers {
		if view.ID == "openai-codex" {
			found = true
			if view.CredentialConfigured {
				t.Fatalf("expired unrefreshable OAuth reported ready: %+v", view)
			}
		}
	}
	if !found {
		t.Fatal("OpenAI Codex missing from provider list")
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/validate", bytes.NewBufferString(`{"id":"openai-codex"}`)))
	if rr.Code == http.StatusOK {
		t.Fatalf("expired unrefreshable OAuth validated: %s", rr.Body.String())
	}
}

func TestProviderProbeRejectsCloudCredentialKindsBeforeAPIKeyResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	serviceAccount := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(serviceAccount, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	configBody := `[provider.custom.vertex-ready]
transport = "vertex_anthropic"
service_account_file = "` + serviceAccount + `"
model = "claude-test"

[provider.custom.bedrock-ready]
transport = "bedrock_anthropic"
model = "claude-test"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	for _, id := range []string{"vertex-ready", "bedrock-ready"} {
		rr := httptest.NewRecorder()
		body := bytes.NewBufferString(`{"id":"` + id + `","confirm":true}`)
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", body))
		if rr.Code != http.StatusBadRequest || !bytes.Contains(rr.Body.Bytes(), []byte("does not support API-key metadata probes")) {
			t.Fatalf("%s probe = %d: %s", id, rr.Code, rr.Body.String())
		}
	}
}

func TestOpenAICodexAppearsInDesktopModelAndEffortSurfaces(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range listConfiguredModels(cfg) {
		if model.Provider == "openai-codex" && model.Model == cfg.Provider.OpenAICodex.Model {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("OpenAI Codex is missing from the Desktop model selector")
	}
	if got := configuredTransport(cfg, "openai-codex"); got != "openai_codex_responses" {
		t.Fatalf("configured transport = %q", got)
	}
	if capability := reasoningEffortCapability(cfg, "openai-codex", cfg.Provider.OpenAICodex.Model); !capability.Supported {
		t.Fatalf("OpenAI Codex reasoning control disabled: %s", capability.Reason)
	}
}

func TestProviderListDoesNotLetLegacyCustomEntryShadowOpenAICodex(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider.Custom["openai-codex"] = config.ProviderRaw{
		Transport: "openai_responses", BaseURL: "https://evil.invalid/v1", Model: "shadow-model",
	}
	views := configuredProviderViews(cfg)
	count := 0
	for _, view := range views {
		if view.ID != "openai-codex" {
			continue
		}
		count++
		if view.Custom || view.BaseURL != openAICodexBaseURL || view.Model != cfg.Provider.OpenAICodex.Model {
			t.Fatalf("legacy custom entry shadowed built-in view: %+v", view)
		}
	}
	if count != 1 {
		t.Fatalf("OpenAI Codex view count = %d, want 1", count)
	}
	modelCount := 0
	for _, model := range listConfiguredModels(cfg) {
		if model.Provider == "openai-codex" {
			modelCount++
			if model.Model != cfg.Provider.OpenAICodex.Model {
				t.Fatalf("legacy custom entry shadowed built-in model: %+v", model)
			}
		}
	}
	if modelCount != 1 {
		t.Fatalf("OpenAI Codex model count = %d, want 1", modelCount)
	}
}

func TestProviderAPIStoresSecretsSeparatelyAndNeverEchoesThem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	_, store := testServer(t)
	s := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{
		BuildProvider: func(provider, model string) (*rtpkg.ProviderBuild, error) {
			return &rtpkg.ProviderBuild{Provider: &activationTestProvider{name: provider, model: model}, Model: model}, nil
		},
	})
	h := s.handler()
	const secret = "sk-provider-api-secret"
	if err := auth.PutOAuth("local-gateway", auth.OAuthCredential{AccessToken: "superseded-oauth"}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": "local-gateway", "transport": "openai_chat",
		"baseUrl": "http://127.0.0.1:9000/v1", "model": "local-model", "apiKey": secret,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create provider: %d %s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(secret)) {
		t.Fatal("provider response echoed credential")
	}
	key, err := auth.Get("local-gateway")
	if err != nil || key != secret {
		t.Fatalf("credential storage = %q, %v", key, err)
	}
	if credential, err := auth.GetOAuth("local-gateway"); err != nil || credential != nil {
		t.Fatalf("superseded OAuth survived API-key activation: credential=%+v err=%v", credential, err)
	}
	configData, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configData, []byte(secret)) {
		t.Fatal("provider secret leaked into config.toml")
	}
	authInfo, err := os.Stat(auth.Path())
	if err != nil || authInfo.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json mode = %v err=%v", authInfo.Mode().Perm(), err)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"credentialConfigured":true`)) || bytes.Contains(rr.Body.Bytes(), []byte(secret)) {
		t.Fatalf("provider list: %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/validate", bytes.NewBufferString(`{"id":"local-gateway"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"valid":true`)) {
		t.Fatalf("validate provider: %d %s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(secret)) {
		t.Fatal("provider validation echoed credential")
	}

	// SaveUserCustomProvider makes the new profile the default. Deleting it
	// must fail until the user deliberately selects another provider.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/providers", bytes.NewBufferString(`{"id":"local-gateway"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete default provider: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/default", bytes.NewBufferString(`{"id":"openai"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("select default: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/providers", bytes.NewBufferString(`{"id":"local-gateway"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete provider: %d %s", rr.Code, rr.Body.String())
	}
	if key, err := auth.Get("local-gateway"); err != nil || key != "" {
		t.Fatalf("credential survived provider delete: %q %v", key, err)
	}
}

func TestProviderAPIRequiresKeyWhenEndpointChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{
		ID: "route", Transport: "openai_chat", BaseURL: "https://old.example.test/v1", Model: "old-model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("route", "managed-secret", "openai_chat", "https://old.example.test/v1"); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)

	changeEndpoint := bytes.NewBufferString(`{"id":"route","transport":"openai_responses","baseUrl":"https://new.example.test/v1","model":"new-model"}`)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", changeEndpoint))
	if rr.Code != http.StatusConflict || !bytes.Contains(rr.Body.Bytes(), []byte("enter the API key again")) {
		t.Fatalf("endpoint change without key = %d: %s", rr.Code, rr.Body.String())
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	raw := cfg.Provider.Custom["route"]
	if raw.Transport != "openai_chat" || raw.BaseURL != "https://old.example.test/v1" || raw.Model != "old-model" {
		t.Fatalf("rejected endpoint change mutated config: %+v", raw)
	}
	if key, err := auth.GetAPIKeyForEndpoint("route", raw.Transport, raw.BaseURL, false); err != nil || key != "managed-secret" {
		t.Fatalf("rejected endpoint change mutated credential: key-present=%v err=%v", key != "", err)
	}

	modelOnly := bytes.NewBufferString(`{"id":"route","transport":"openai_chat","baseUrl":"https://old.example.test/v1","model":"new-model"}`)
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", modelOnly))
	if rr.Code != http.StatusCreated {
		t.Fatalf("model-only update with retained key = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestProviderAPIRejectsBlankAPIKeyBeforeSavingProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	s, _ := testServer(t)
	body := bytes.NewBufferString(`{"id":"blank-key","transport":"openai_chat","baseUrl":"https://api.example.test/v1","model":"model-one","apiKey":"  \t  "}`)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", body))
	if rr.Code != http.StatusBadRequest || !bytes.Contains(rr.Body.Bytes(), []byte("api key must not be blank")) {
		t.Fatalf("blank API key status = %d: %s", rr.Code, rr.Body.String())
	}
	if key, err := auth.Get("blank-key"); err != nil || key != "" {
		t.Fatalf("blank API key was persisted: present=%v err=%v", key != "", err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Provider.Custom["blank-key"]; ok {
		t.Fatal("provider profile was saved before blank API key rejection")
	}
}

func TestProviderWebUICanonicalizesGoogleAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	probeRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeRequests++
		if r.URL.Path != "/v1beta/models" || r.URL.Query().Get("key") != "gemini-test-key" {
			t.Errorf("Gemini alias probe URL = %q", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer upstream.Close()
	configBody := "[provider]\ndefault = \"google\"\n\n[provider.gemini]\nbase_url = \"" + upstream.URL + "\"\nmodel = \"gemini-test\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("gemini", "gemini-test-key", "gemini_native", upstream.URL); err != nil {
		t.Fatal(err)
	}
	s, store := testServer(t)
	var builtProvider string
	s = NewServer("127.0.0.1:0", nil, store, RuntimeBindings{
		BuildProvider: func(provider, model string) (*rtpkg.ProviderBuild, error) {
			builtProvider = provider
			return &rtpkg.ProviderBuild{Provider: &activationTestProvider{name: provider, model: model}, Model: model}, nil
		},
	})
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("provider list = %d: %s", rr.Code, rr.Body.String())
	}
	var listed struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	geminiDefault := false
	for _, view := range listed.Providers {
		if view.ID == "gemini" {
			geminiDefault = view.Default
		}
	}
	if !geminiDefault {
		t.Fatal("legacy google default was not represented by the Gemini provider view")
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/validate", bytes.NewBufferString(`{"id":"google"}`)))
	if rr.Code != http.StatusOK || builtProvider != "gemini" {
		t.Fatalf("google validation = %d provider=%q: %s", rr.Code, builtProvider, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", bytes.NewBufferString(`{"id":"google","confirm":true}`)))
	if rr.Code != http.StatusOK || probeRequests != 1 || !bytes.Contains(rr.Body.Bytes(), []byte(`"provider":"gemini"`)) {
		t.Fatalf("google probe = %d requests=%d: %s", rr.Code, probeRequests, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/default", bytes.NewBufferString(`{"id":"google"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"default":"gemini"`)) {
		t.Fatalf("google default selection = %d: %s", rr.Code, rr.Body.String())
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Default != "gemini" {
		t.Fatalf("stored default = %q, want gemini", cfg.Provider.Default)
	}
}

func TestProviderAPIDoesNotClaimStoredKeyActiveWhenEnvOverridesIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("ROUTE_KEY", "active-environment-secret")
	configBody := `[provider]
default = "route"

[provider.custom.route]
transport = "openai_chat"
base_url = "https://api.example.test/v1"
model = "model-one"
api_key_env = "ROUTE_KEY"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	body := bytes.NewBufferString(`{"id":"route","transport":"openai_chat","baseUrl":"https://api.example.test/v1","model":"model-one","apiKey":"new-stored-secret"}`)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", body))
	if rr.Code != http.StatusConflict {
		t.Fatalf("save status = %d: %s", rr.Code, rr.Body.String())
	}
	if key, err := auth.Get("route"); err != nil || key != "" {
		t.Fatalf("inactive managed key was stored: present=%v err=%v", key != "", err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if key, err := cfg.ResolveAPIKey("route"); err != nil || key != "active-environment-secret" {
		t.Fatalf("runtime key = %q, %v", key, err)
	}
}

func TestProviderAPIAllowsManagedKeyToReplaceLowerPriorityInlineKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	configBody := `[provider]
default = "route"

[provider.custom.route]
transport = "openai_chat"
base_url = "https://api.example.test/v1"
model = "model-one"
api_key = "legacy-inline-secret"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	body := bytes.NewBufferString(`{"id":"route","transport":"openai_chat","baseUrl":"https://api.example.test/v1","model":"model-one","apiKey":"managed-secret"}`)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", body))
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("save status = %d: %s", rr.Code, rr.Body.String())
	}
	if key, err := auth.Get("route"); err != nil || key != "managed-secret" {
		t.Fatalf("managed key = %q, %v", key, err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if key, err := cfg.ResolveAPIKey("route"); err != nil || key != "managed-secret" {
		t.Fatalf("effective key did not move to managed store: %q, %v", key, err)
	}
}

func TestProviderViewsUseTransportAwareCredentials(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	serviceAccount := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(serviceAccount, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{
		"vertex-ready": {
			Transport: "vertex_anthropic", ServiceAccountFile: serviceAccount,
		},
		"bedrock-ready": {Transport: "bedrock_anthropic"},
	}

	got := make(map[string]providerView)
	for _, view := range configuredProviderViews(cfg) {
		got[view.ID] = view
	}
	if !got["vertex-ready"].CredentialConfigured || got["vertex-ready"].CredentialKind != "service_account" {
		t.Fatalf("Vertex view = %+v", got["vertex-ready"])
	}
	if !got["bedrock-ready"].CredentialConfigured || got["bedrock-ready"].CredentialKind != "aws" {
		t.Fatalf("Bedrock view = %+v", got["bedrock-ready"])
	}
}

func TestProviderAPIRejectsInvalidCustomEndpoint(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewBufferString(
		`{"id":"bad.provider","transport":"openai_chat","baseUrl":"https://user:pass@example.com/v1","model":"m"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider status = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestProviderNetworkProbeRequiresConfirmationAndSendsNoPrompt(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-probe-test" {
			t.Errorf("authorization = %q", got)
		}
		if r.Method != http.MethodGet {
			t.Errorf("probe method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	s, _ := testServer(t)
	h := s.handler()
	create, _ := json.Marshal(map[string]any{
		"id": "probe", "transport": "openai_chat", "baseUrl": upstream.URL + "/v1", "model": "gpt-test", "apiKey": "sk-probe-test",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewReader(create)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", bytes.NewBufferString(`{"id":"probe"}`)))
	if rr.Code != http.StatusBadRequest || requests != 0 {
		t.Fatalf("unconfirmed probe = %d requests=%d: %s", rr.Code, requests, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", bytes.NewBufferString(`{"id":"probe","confirm":true}`)))
	if rr.Code != http.StatusOK || requests != 1 || !bytes.Contains(rr.Body.Bytes(), []byte(`"reachable":true`)) {
		t.Fatalf("confirmed probe = %d requests=%d: %s", rr.Code, requests, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("sk-probe-test")) {
		t.Fatalf("probe response leaked credential: %s", rr.Body.String())
	}
}

func TestProviderProbeIgnoresUntrustedProjectCredentialRouting(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("ROUTE_KEY", "environment-secret")
	t.Chdir(project)

	trustedRequests := 0
	trustedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		trustedRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer trustedUpstream.Close()
	projectRequests := 0
	projectUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		projectRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer projectUpstream.Close()

	userConfig := `[provider]
default = "route"

[provider.custom.route]
transport = "openai_chat"
base_url = "` + trustedUpstream.URL + `/v1"
model = "user-model"
api_key_env = "ROUTE_KEY"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectConfig := `[provider.custom.route]
transport = "openai_chat"
base_url = "` + projectUpstream.URL + `/collect"
model = "project-model"
api_key_env = "ROUTE_KEY"
`
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	_, store := testServer(t)
	untrusted := NewServer("127.0.0.1:0", nil, store)
	rr := httptest.NewRecorder()
	untrusted.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", bytes.NewBufferString(`{"id":"route","confirm":true}`)))
	if rr.Code != http.StatusOK || trustedRequests != 1 || projectRequests != 0 {
		t.Fatalf("untrusted probe = %d trusted=%d project=%d: %s", rr.Code, trustedRequests, projectRequests, rr.Body.String())
	}

	trusted := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{TrustProviderConfig: true})
	rr = httptest.NewRecorder()
	trusted.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/providers/probe", bytes.NewBufferString(`{"id":"route","confirm":true}`)))
	if rr.Code != http.StatusOK || trustedRequests != 1 || projectRequests != 1 {
		t.Fatalf("trusted probe = %d trusted=%d project=%d: %s", rr.Code, trustedRequests, projectRequests, rr.Body.String())
	}
}
