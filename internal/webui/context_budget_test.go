package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

type contextBudgetPreviewProvider struct {
	llm.Provider
	capabilities llm.ContextCapabilities
}

func (p *contextBudgetPreviewProvider) ContextCapabilities() llm.ContextCapabilities {
	return p.capabilities
}

func (*contextBudgetPreviewProvider) RefreshContextCapabilities(context.Context) error { return nil }

func contextBudgetHTTPFixture(t *testing.T) (*Server, string, []byte) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	t.Setenv("METIS_CONTEXT_BUDGET_TEST_KEY", "fixture-only-key")
	t.Chdir(t.TempDir())
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected generation or metadata path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"model-a","context_window":64000,"max_context_window":96000,"max_output_tokens":32000},{"id":"model-b","context_window":64000,"max_context_window":96000,"max_output_tokens":32000}]}`)
	}))
	t.Cleanup(metadata.Close)
	original := []byte(fmt.Sprintf("[provider]\ndefault = \"budget-test\"\n[provider.custom.budget-test]\ntransport = \"openai_chat\"\nbase_url = %q\nmodel = \"model-a\"\napi_key_env = \"METIS_CONTEXT_BUDGET_TEST_KEY\"\nmax_tokens = 4096\n", metadata.URL+"/v1"))
	path := filepath.Join(config.Home(), "config.toml")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	return NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{}), path, original
}

func callContextBudgetHTTP(t *testing.T, s *Server, method, model, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	path := "/api/providers/budget-test/context-budget?model=" + model
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	var result map[string]any
	if json.Unmarshal(rr.Body.Bytes(), &result) != nil {
		t.Fatalf("invalid response: %s", rr.Body.String())
	}
	return rr, result
}

func TestProviderContextBudgetHTTPBindsOnlyExactModelAndPreservesConfig(t *testing.T) {
	s, path, original := contextBudgetHTTPFixture(t)
	rr, a := callContextBudgetHTTP(t, s, http.MethodGet, "model-a", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rr.Code, rr.Body.String())
	}
	key := a["routeKey"].(string)
	if a["capabilities"].(map[string]any)["window"] != float64(64000) {
		t.Fatalf("default: %+v", a)
	}
	request := fmt.Sprintf(`{"model":"model-a","window":40000,"expectedRouteKey":%q}`, key)
	rr, a = callContextBudgetHTTP(t, s, http.MethodPost, "model-a", request)
	if rr.Code != http.StatusOK || a["requestedWindow"] != float64(40000) || a["applied"] != false || a["appliesOn"] != "next_task" {
		t.Fatalf("save = %d %+v", rr.Code, a)
	}
	if a["capabilities"].(map[string]any)["window"] != float64(40000) {
		t.Fatalf("saved budget not previewed: %+v", a)
	}
	_, b := callContextBudgetHTTP(t, s, http.MethodGet, "model-b", "")
	if b["requestedWindow"] != float64(0) || b["capabilities"].(map[string]any)["window"] != float64(64000) || b["routeKey"] == key {
		t.Fatalf("model budget leaked: %+v", b)
	}
	updated, err := os.ReadFile(path)
	if err != nil || string(updated) != string(original) {
		t.Fatal("budget setting changed provider config or selected model")
	}
	request = fmt.Sprintf(`{"model":"model-a","window":120000,"expectedRouteKey":%q}`, key)
	_, a = callContextBudgetHTTP(t, s, http.MethodPost, "model-a", request)
	caps := a["capabilities"].(map[string]any)
	if a["requestedWindow"] != float64(120000) || caps["window"] != float64(96000) || caps["requestedWindow"] != float64(120000) || caps["source"] != "configured_clamped" {
		t.Fatalf("manual ceiling not visible: %+v", a)
	}
	request = fmt.Sprintf(`{"model":"model-a","window":0,"expectedRouteKey":%q}`, key)
	_, a = callContextBudgetHTTP(t, s, http.MethodPost, "model-a", request)
	if a["configured"] != true || a["capabilities"].(map[string]any)["window"] != float64(64000) {
		t.Fatalf("explicit automatic not restored: %+v", a)
	}
}

func TestProviderContextBudgetHTTPRejectsStaleRouteAndInjectedEndpoint(t *testing.T) {
	s, path, original := contextBudgetHTTPFixture(t)
	_, first := callContextBudgetHTTP(t, s, http.MethodGet, "model-a", "")
	key := first["routeKey"].(string)
	changed := strings.Replace(string(original), "/v1\"", "/v2\"", 1)
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	rr, _ := callContextBudgetHTTP(t, s, http.MethodPost, "model-a", fmt.Sprintf(`{"model":"model-a","window":40000,"expectedRouteKey":%q}`, key))
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale route = %d %s", rr.Code, rr.Body.String())
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	route, err := rtpkg.ProviderContextBudgetRoute(cfg, "budget-test")
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := config.LookupContextBudget(route, "model-a")
	if err != nil || found {
		t.Fatal("stale save wrote a budget to changed endpoint")
	}
	for _, body := range []string{
		fmt.Sprintf(`{"model":"model-a","window":40000,"expectedRouteKey":%q,"baseUrl":"https://evil.invalid"}`, key),
		fmt.Sprintf(`{"model":"model-a","window":40000,"expectedRouteKey":%q,"apiKey":"injected"}`, key),
		fmt.Sprintf(`{"model":"model-a","window":-1,"expectedRouteKey":%q}`, key),
		fmt.Sprintf(`{"model":"model-a","window":1.5,"expectedRouteKey":%q}`, key),
	} {
		rr, _ = callContextBudgetHTTP(t, s, http.MethodPost, "model-a", body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("bad request = %d %s", rr.Code, rr.Body.String())
		}
	}
}

func TestProviderContextBudgetHTTPRejectsPreviewFromChangedRoute(t *testing.T) {
	s, _, _ := contextBudgetHTTPFixture(t)
	s.buildProvider = func(provider, model string) (*rtpkg.ProviderBuild, error) {
		return &rtpkg.ProviderBuild{Provider: &contextBudgetPreviewProvider{capabilities: llm.ContextCapabilities{
			ProviderID: provider, Model: model, RouteKey: "new-route-read-by-provider-builder", Window: 1000000,
		}}}, nil
	}
	rr, data := callContextBudgetHTTP(t, s, http.MethodGet, "model-a", "")
	if rr.Code != http.StatusConflict || data["capabilities"] != nil {
		t.Fatalf("metadata from changed connection displayed under original route: %d %+v", rr.Code, data)
	}
}
