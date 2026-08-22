package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

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
	configData, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configData, []byte(secret)) {
		t.Fatal("provider secret leaked into config.toml")
	}
	authInfo, err := os.Stat(filepath.Join(home, "auth.json"))
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
