package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func TestOpenAIEndpointCapabilitiesDefaultMaximumAndRefresh(t *testing.T) {
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	for _, transport := range []string{"openai_chat", "openai_responses"} {
		t.Run(transport, func(t *testing.T) {
			var requests, defaultWindow atomic.Int64
			defaultWindow.Store(272000)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/models" || r.URL.Query().Get("client_version") != "pi" {
					t.Errorf("discovery URL = %s", r.URL)
				}
				fmt.Fprintf(w, `{"models":[{"slug":"provider-unknown-fixture","context_window":%d,"max_context_window":872000,"max_tokens":128000}]}`, defaultWindow.Load())
			}))
			defer srv.Close()
			client, err := modelinfo.New("", modelinfo.WithTTL(0))
			if err != nil {
				t.Fatal(err)
			}
			resolver, err := llm.NewContextResolver(llm.ContextResolverOptions{
				Client: client, Model: "provider-unknown-fixture", APIKey: "test-secret",
				Binding: modelinfo.Binding{ProviderID: "custom-route", Transport: transport, BaseURL: srv.URL + "/v1"},
			})
			if err != nil {
				t.Fatal(err)
			}
			var p llm.ContextCapabilitiesProvider
			var setWindow func(int)
			fallback := 128000
			if transport == "openai_chat" {
				chat := New("test-secret", srv.URL+"/v1", "provider-unknown-fixture", 4096, time.Second, 0)
				chat.ContextResolver = resolver
				p, setWindow = chat, func(window int) { chat.ContextWindow = window }
			} else {
				responses := NewResponses("test-secret", srv.URL+"/v1", "provider-unknown-fixture", 4096, time.Second, 0)
				responses.ContextResolver = resolver
				p, setWindow = responses, func(window int) { responses.ContextWindow = window }
				fallback = 200000
			}
			maxContext := p.(interface{ MaxContextTokens() int })
			if got := p.ContextCapabilities(); got.Window != fallback || got.Source != "fallback" || requests.Load() != 0 {
				t.Fatalf("cold read performed discovery: %+v requests=%d", got, requests.Load())
			}
			if err := p.RefreshContextCapabilities(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := p.ContextCapabilities()
			if got.Window != 272000 || got.DefaultWindow != 272000 || got.MaxWindow != 872000 || got.MaxOutputTokens != 128000 || got.Source != modelinfo.SourceEndpointPI {
				t.Fatalf("endpoint default/max/output lost or mixed: %+v", got)
			}
			if maxContext.MaxContextTokens() != 272000 || requests.Load() != 1 {
				t.Fatalf("hot path performed discovery or used extended max: window=%d requests=%d", maxContext.MaxContextTokens(), requests.Load())
			}
			setWindow(64000)
			if got := p.ContextCapabilities(); got.Window != 64000 || got.Source != "configured" || got.DefaultWindow != 272000 || got.MaxWindow != 872000 {
				t.Fatalf("override lost default/max metadata: %+v", got)
			}
			setWindow(0)
			defaultWindow.Store(544000)
			if err := p.RefreshContextCapabilities(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := p.ContextCapabilities(); got.Window != 544000 || maxContext.MaxContextTokens() != 544000 {
				t.Fatalf("refreshed provider remained bound to old capacity: %+v", got)
			}
		})
	}
}

func TestCodexBypassesAccidentallyAttachedGenericResolver(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6-astra","context_window":1050000}]}`))
	}))
	defer srv.Close()
	client, err := modelinfo.New("")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := llm.NewContextResolver(llm.ContextResolverOptions{Client: client, Model: "gpt-6-astra",
		Binding: modelinfo.Binding{ProviderID: "public-openai", Transport: "openai_responses", BaseURL: srv.URL + "/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	p := NewCodexResponses("gpt-6-astra", 4096, time.Second, 0, nil)
	p.ContextResolver = resolver
	if err := p.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.ContextCapabilities(); got.Window != 272000 || got.Source != "subscription_model" || requests.Load() != 0 {
		t.Fatalf("Codex crossed public/subscription capability boundary: %+v requests=%d", got, requests.Load())
	}
}

func TestOpenAIContextCapabilitiesConfiguredAndFallbackSources(t *testing.T) {
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	tests := []struct {
		name     string
		provider llm.ContextCapabilitiesProvider
		window   int
		source   string
	}{
		{"chat configured", &OpenAI{Model: "context-capabilities-unknown-fixture", ContextWindow: 64000}, 64000, "configured"},
		{"chat fallback", &OpenAI{Model: "context-capabilities-unknown-fixture"}, 128000, "fallback"},
		{"chat suffix", &OpenAI{Model: "context-capabilities-unknown-fixture-32k"}, 32000, "fallback"},
		{"responses configured", &Responses{Model: "context-capabilities-unknown-fixture", ContextWindow: 64000}, 64000, "configured"},
		{"responses fallback", &Responses{Model: "context-capabilities-unknown-fixture"}, 200000, "fallback"},
		{"codex known subscription", &Responses{Model: "gpt-6-astra", ProviderName: "openai-codex"}, 272000, "subscription_model"},
		{"codex unknown subscription", &Responses{Model: "context-capabilities-unknown-fixture", ProviderName: "openai-codex"}, 200000, "fallback"},
		{"codex configured", &Responses{Model: "gpt-6-astra", ProviderName: "openai-codex", ContextWindow: 64000}, 64000, "configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.provider.ContextCapabilities()
			window := tt.provider.(interface{ MaxContextTokens() int }).MaxContextTokens()
			if got.Window != tt.window || got.Source != tt.source || window != got.Window {
				t.Fatalf("capabilities = %+v, want window=%d source=%q", got, tt.window, tt.source)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := tt.provider.RefreshContextCapabilities(ctx); err != nil {
				t.Fatalf("no resolver refresh must be a no-op: %v", err)
			}
		})
	}
}

func TestOpenAIContextCapabilitiesColdCache(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(home, ".metis", "cache", "models.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	const fixture = `{
"known": {"models": {
"shared-model": {"limit": {"context": 272000}},
"unanimous-model": {"limit": {"context": 1050000}},
"gpt-6-astra": {"limit": {"context": 1050000}}
}},
"larger": {"models": {
"shared-model": {"limit": {"context": 872000}},
"unanimous-model": {"limit": {"context": 1050000}},
"other-model": {"limit": {"context": 65536}}
}}}`
	if err := os.WriteFile(cache, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestOpenAIContextCapabilitiesColdCacheHelper$", "-test.v")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "METIS_HOME=") && !strings.HasPrefix(value, "METIS_CATALOG_DISABLE=") && !strings.HasPrefix(value, "METIS_MODELS_URL=") && !strings.HasPrefix(value, "METIS_CONTEXT_CAPABILITIES_HELPER=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "METIS_MODELS_URL=http://127.0.0.1:1", "METIS_CONTEXT_CAPABILITIES_HELPER=openai")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cold-cache provider checks failed: %v\n%s", err, output)
	}
}

func TestOpenAIContextCapabilitiesColdCacheHelper(t *testing.T) {
	if os.Getenv("METIS_CONTEXT_CAPABILITIES_HELPER") != "openai" {
		t.Skip("subprocess helper")
	}
	tests := []struct {
		name, route, model, source string
		strict                     bool
		window                     int
	}{
		{"exact known route cap", "known", "shared-model", "catalog_route", false, 272000},
		{"friendly route exact unanimous model", "easycliproxyapi", "unanimous-model", "catalog_model", false, 1_050_000},
		{"friendly route conflicting windows", "easycliproxyapi", "shared-model", "fallback", false, 128000},
		{"explicit catalog typo", "unknown", "unanimous-model", "fallback", true, 128000},
		{"known route missing model", "known", "other-model", "fallback", false, 128000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &OpenAI{Model: tt.model, CatalogProvider: tt.route, CatalogProviderStrict: tt.strict}
			got := p.ContextCapabilities()
			if got.Window != tt.window || got.Source != tt.source || p.MaxContextTokens() != got.Window {
				t.Fatalf("chat capabilities=%+v, want window=%d source=%s", got, tt.window, tt.source)
			}
			r := &Responses{Model: tt.model, CatalogProvider: tt.route, CatalogProviderStrict: tt.strict}
			wantWindow := tt.window
			if tt.source == "fallback" {
				wantWindow = 200000
			}
			got = r.ContextCapabilities()
			if got.Window != wantWindow || got.Source != tt.source || r.MaxContextTokens() != got.Window {
				t.Fatalf("responses capabilities=%+v, want window=%d source=%s", got, wantWindow, tt.source)
			}
		})
	}
	codex := &Responses{Model: "gpt-6-astra", ProviderName: "openai-codex", CatalogProvider: "known"}
	if got := codex.ContextCapabilities(); got.Window != 272000 || got.Source != "subscription_model" {
		t.Fatalf("Codex borrowed public API capacity: %+v", got)
	}
}
