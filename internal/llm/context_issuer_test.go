package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func issuerCatalog(model string) *catalog.Client {
	return catalog.NewStaticClient(catalog.Catalog{
		"moonshotai": {ID: "moonshotai", API: "https://api.moonshot.ai/v1", Doc: "https://platform.moonshot.ai/docs/api/chat", Models: map[string]catalog.Model{
			model: {Limit: catalog.Limit{Context: 1048576, Output: 1048576}},
		}},
		"moonshotai-cn": {ID: "moonshotai-cn", API: "https://api.moonshot.cn/v1", Doc: "https://platform.moonshot.cn/docs/api/chat", Models: map[string]catalog.Model{
			model: {Limit: catalog.Limit{Context: 1048576, Output: 1048576}},
		}},
		"tinfoil": {Models: map[string]catalog.Model{model: {Limit: catalog.Limit{Context: 262144, Input: 220000, Output: 131072}}}},
	})
}

const kimiIssuerProfile = `{"models":[{"slug":"kimi-k3","owned_by":"kimi","context_window":272000,"max_context_window":272000,"supported_in_api":true,"minimal_client_version":"0.124.0","supports_experimental_context":false}]}`

func TestKimiClientProfileUsesIssuerCatalogAcrossRouteConflict(t *testing.T) {
	r := profileResolver(t, issuerCatalog("kimi-k3"), "friendly-gateway", "kimi-k3", kimiIssuerProfile, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 1048576 || got.MaxWindow != 1048576 || got.MaxInputTokens != 0 || got.MaxOutputTokens != 1048576 || got.Source != "catalog_issuer_profile" || got.ProfileDefaultWindow != 272000 || got.ProfileMaxWindow != 272000 {
		t.Fatalf("issuer capacity was lost to unrelated reseller limits: %+v", got)
	}
	configured := r.Resolve(800000, func() int { return 200000 })
	if configured.Window != 800000 || configured.Source != "configured" || configured.MaxWindow != 1048576 {
		t.Fatalf("explicit operating budget lost precedence: %+v", configured)
	}
}

func TestKimiIssuerFromSameEndpointDisplayAliasDoesNotImportOtherWireLimits(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Query().Get("client_version") == "pi" {
			w.Write([]byte(`{"models":[{"slug":"kimi-k3","context_window":272000,"max_context_window":272000,"supported_in_api":true,"minimal_client_version":"0.124.0","supports_experimental_context":false}]}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"claude-fable-5-dd-3k-imik","display_name":"kimi-k3","owned_by":"kimi","max_input_tokens":200000,"max_tokens":64000}]}`))
	}))
	defer server.Close()
	client, err := modelinfo.New("")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewContextResolver(ContextResolverOptions{Client: client, Catalog: issuerCatalog("kimi-k3"), Binding: modelinfo.Binding{ProviderID: "friendly-gateway", Transport: "anthropic_messages", BaseURL: server.URL}, CatalogProvider: "friendly-gateway", Model: "kimi-k3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.Resolve(0, func() int { return 200000 })
	if requests.Load() != 2 || got.Window != 1048576 || got.MaxInputTokens != 0 || got.MaxOutputTokens != 1048576 || got.Source != "catalog_issuer_profile" {
		t.Fatalf("issuer enrichment lost or other wire alias limits borrowed: requests=%d context=%+v", requests.Load(), got)
	}
}

func TestIssuerCatalogNeverOverridesOrdinaryEndpointCapacity(t *testing.T) {
	listing := `{"data":[{"id":"kimi-k3","owned_by":"kimi","context_window":200000,"max_input_tokens":160000,"max_tokens":64000}]}`
	r := profileResolver(t, issuerCatalog("kimi-k3"), "friendly-gateway", "kimi-k3", listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 200000 || got.MaxInputTokens != 160000 || got.MaxOutputTokens != 64000 || got.Source != "endpoint_pi" || got.ProfileDefaultWindow != 0 {
		t.Fatalf("actual endpoint capacity enlarged by issuer catalog: %+v", got)
	}
}

func TestIssuerCatalogPreservesIndependentProfileInputAndOutputCaps(t *testing.T) {
	listing := `{"models":[{"slug":"kimi-k3","owned_by":"kimi","context_window":272000,"max_context_window":272000,"max_input_tokens":200000,"max_tokens":64000,"supported_in_api":true,"minimal_client_version":"0.124.0","supports_experimental_context":false}]}`
	r := profileResolver(t, issuerCatalog("kimi-k3"), "friendly-gateway", "kimi-k3", listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 1048576 || got.MaxInputTokens != 200000 || got.MaxOutputTokens != 64000 || got.Source != "catalog_issuer_profile" {
		t.Fatalf("issuer profile discarded independent endpoint limits: %+v", got)
	}
}

func TestIssuerCatalogKeepsExplicitAndKnownRouteBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, route, source string
		strict              bool
		window              int
	}{
		{"known_reseller", "tinfoil", "catalog_profile", false, 262144},
		{"explicit_miss", "misspelled-route", "endpoint_pi", true, 272000},
		{"known_route_model_miss", "moonshotai", "endpoint_pi", false, 272000},
	} {
		t.Run(test.name, func(t *testing.T) {
			cat := issuerCatalog("kimi-k3")
			if test.name == "known_route_model_miss" {
				cat = issuerCatalog("different-model")
			}
			r := profileResolver(t, cat, test.route, "kimi-k3", kimiIssuerProfile, test.strict)
			got := r.Resolve(0, func() int { return 200000 })
			if got.Window != test.window || got.Source != test.source {
				t.Fatalf("issuer crossed actual route boundary: %+v", got)
			}
		})
	}
}

func zhipuIssuerCatalog(model string, limit catalog.Limit) *catalog.Client {
	providers := catalog.Catalog{
		"reseller": {Models: map[string]catalog.Model{model: {Limit: catalog.Limit{Context: 272000}}}},
	}
	for _, id := range []string{"zhipuai", "zai", "zai-coding-plan"} {
		api := "https://api.z.ai/api/paas/v4"
		if id == "zhipuai" {
			api = "https://open.bigmodel.cn/api/paas/v4"
		} else if id == "zai-coding-plan" {
			api = "https://api.z.ai/api/coding/paas/v4"
		}
		providers[id] = catalog.Provider{ID: id, API: api, Doc: "https://docs.z.ai/guides/llm/glm-5.3", Models: map[string]catalog.Model{
			model: {ID: model, Limit: limit},
		}}
	}
	return catalog.NewStaticClient(providers)
}

func zhipuIssuerProfile(model string) string {
	return fmt.Sprintf(`{"models":[{"slug":%q,"owned_by":"zhipu","context_window":272000,"max_context_window":272000,"supported_in_api":true,"minimal_client_version":"0.124.0","supports_experimental_context":false}]}`, model)
}

func TestZhipuClientProfileUsesExactIssuerCapacityAcrossResellerConflict(t *testing.T) {
	for _, test := range []struct {
		model  string
		window int
		output int
	}{
		{"glm-5.3", 1_000_000, 131_072},
		{"glm-4.5", 131_072, 98_304},
	} {
		t.Run(test.model, func(t *testing.T) {
			cat := zhipuIssuerCatalog(test.model, catalog.Limit{Context: test.window, Output: test.output})
			r := profileResolver(t, cat, "easycliproxyapi", test.model, zhipuIssuerProfile(test.model), false)
			got := r.Resolve(0, func() int { return 200000 })
			if got.Window != test.window || got.MaxWindow != test.window || got.ModelWindow != test.window || got.MaxOutputTokens != test.output || got.MaxInputTokens != 0 || got.Source != "catalog_issuer_profile" || got.ProfileDefaultWindow != 272000 || got.ProfileMaxWindow != 272000 {
				t.Fatalf("client profile replaced independently evidenced issuer capacity: %+v", got)
			}
			configured := r.Resolve(test.window+1, func() int { return 200000 })
			if configured.Window != test.window || configured.RequestedWindow != test.window+1 || configured.Source != "configured_clamped" {
				t.Fatalf("explicit budget exceeded evidenced issuer capacity: %+v", configured)
			}
		})
	}
}

func TestZhipuIssuerDoesNotOverrideServedEndpointCapacity(t *testing.T) {
	listing := `{"data":[{"id":"glm-5.3","owned_by":"zhipu","context_window":128000,"max_input_tokens":96000,"max_tokens":32000}]}`
	r := profileResolver(t, zhipuIssuerCatalog("glm-5.3", catalog.Limit{Context: 1_000_000, Output: 131_072}), "easycliproxyapi", "glm-5.3", listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 128000 || got.MaxWindow != 128000 || got.MaxInputTokens != 96000 || got.MaxOutputTokens != 32000 || got.Source != "endpoint_pi" || got.ProfileDefaultWindow != 0 {
		t.Fatalf("known served endpoint capacity was enlarged by issuer directory: %+v", got)
	}
}

func TestZhipuIssuerResolverKeepsKnownAndStrictRouteAuthority(t *testing.T) {
	for _, test := range []struct {
		name, route, source string
		strict              bool
		window              int
	}{
		{"known reseller", "reseller", "catalog_profile", false, 272000},
		{"explicit missing mapping", "explicit-miss", "endpoint_pi", true, 272000},
		{"known original missing model", "zhipuai", "endpoint_pi", false, 272000},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := "glm-5.3"
			if test.name == "known original missing model" {
				model = "glm-5.3-high"
			}
			r := profileResolver(t, zhipuIssuerCatalog("glm-5.3", catalog.Limit{Context: 1_000_000}), test.route, model, zhipuIssuerProfile(model), test.strict)
			got := r.Resolve(0, func() int { return 200000 })
			if got.Window != test.window || got.Source != test.source {
				t.Fatalf("issuer metadata crossed exact route or model boundary: %+v", got)
			}
		})
	}
}
