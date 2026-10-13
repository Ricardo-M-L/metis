package anthropic

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

func TestAnthropicEndpointCapabilitiesRefreshAtExplicitBoundary(t *testing.T) {
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	var requests atomic.Int64
	var defaultWindow atomic.Int64
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
		Binding: modelinfo.Binding{ProviderID: "custom-route", Transport: "anthropic_messages", BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := New("test-secret", srv.URL, "provider-unknown-fixture", 4096, time.Second, "")
	p.ContextResolver = resolver
	if got := p.ContextCapabilities(); got.Window != 200000 || got.Source != "fallback" || requests.Load() != 0 {
		t.Fatalf("reading cold capabilities unexpectedly performed discovery: %+v requests=%d", got, requests.Load())
	}
	if err := p.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := p.ContextCapabilities()
	if got.Window != 272000 || got.DefaultWindow != 272000 || got.MaxWindow != 872000 || got.MaxOutputTokens != 128000 || got.Source != modelinfo.SourceEndpointPI {
		t.Fatalf("endpoint default/max/output lost or mixed: %+v", got)
	}
	if p.MaxContextTokens() != 272000 || requests.Load() != 1 {
		t.Fatalf("hot path performed discovery or used extended max: window=%d requests=%d", p.MaxContextTokens(), requests.Load())
	}
	p.ContextWindow = 64000
	if got := p.ContextCapabilities(); got.Window != 64000 || got.Source != "configured" || got.DefaultWindow != 272000 || got.MaxWindow != 872000 {
		t.Fatalf("override lost default/max metadata: %+v", got)
	}
	p.ContextWindow = 0
	defaultWindow.Store(544000)
	if err := p.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.ContextCapabilities(); got.Window != 544000 || p.MaxContextTokens() != 544000 {
		t.Fatalf("refreshed provider remained bound to old capacity: %+v", got)
	}
}

func TestAnthropicContextCapabilitiesConfiguredAndFallbackSources(t *testing.T) {
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	tests := []struct {
		provider *Anthropic
		window   int
		source   string
	}{
		{&Anthropic{Model: "context-capabilities-unknown-fixture", ContextWindow: 64000}, 64000, "configured"},
		{&Anthropic{Model: "context-capabilities-unknown-fixture"}, 200000, "fallback"},
		{&Anthropic{Model: "context-capabilities-unknown-fixture-32k"}, 32000, "fallback"},
	}
	for _, tt := range tests {
		got := tt.provider.ContextCapabilities()
		if got.Window != tt.window || got.Source != tt.source || tt.provider.MaxContextTokens() != got.Window {
			t.Fatalf("capabilities = %+v, want window=%d source=%q", got, tt.window, tt.source)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := tt.provider.RefreshContextCapabilities(ctx); err != nil {
			t.Fatalf("no resolver refresh must be a no-op: %v", err)
		}
	}
}

// A separate process ensures catalog.Default starts cold, unaffected by the
// fallback tests' process-wide catalog-disable singleton.
func TestAnthropicContextCapabilitiesColdCache(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(home, ".metis", "cache", "models.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	const fixture = `{
"known": {"models": {
"shared-model": {"limit": {"context": 272000}},
"unanimous-model": {"limit": {"context": 1050000}}
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
	cmd := exec.Command(binary, "-test.run=^TestAnthropicContextCapabilitiesColdCacheHelper$", "-test.v")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "METIS_HOME=") && !strings.HasPrefix(value, "METIS_CATALOG_DISABLE=") && !strings.HasPrefix(value, "METIS_MODELS_URL=") && !strings.HasPrefix(value, "METIS_CONTEXT_CAPABILITIES_HELPER=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "METIS_MODELS_URL=http://127.0.0.1:1", "METIS_CONTEXT_CAPABILITIES_HELPER=anthropic")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cold-cache provider checks failed: %v\n%s", err, output)
	}
}

func TestAnthropicContextCapabilitiesColdCacheHelper(t *testing.T) {
	if os.Getenv("METIS_CONTEXT_CAPABILITIES_HELPER") != "anthropic" {
		t.Skip("subprocess helper")
	}
	tests := []struct {
		name, route, model, source string
		strict                     bool
		window                     int
	}{
		{"exact known route cap", "known", "shared-model", "catalog_route", false, 272000},
		{"friendly route exact unanimous model", "easycliproxyapi", "unanimous-model", "catalog_model", false, 1_050_000},
		{"friendly route conflicting windows", "easycliproxyapi", "shared-model", "fallback", false, 200000},
		{"explicit catalog typo", "unknown", "unanimous-model", "fallback", true, 200000},
		{"known route missing model", "known", "other-model", "fallback", false, 200000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Anthropic{Model: tt.model, CatalogProvider: tt.route, CatalogProviderStrict: tt.strict}
			got := p.ContextCapabilities()
			if got.Window != tt.window || got.Source != tt.source || p.MaxContextTokens() != got.Window {
				t.Fatalf("capabilities=%+v, want window=%d source=%s", got, tt.window, tt.source)
			}
		})
	}
}
