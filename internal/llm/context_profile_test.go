package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func profileResolver(t *testing.T, cat *catalog.Client, route, model, body string, strict bool) *ContextResolver {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	t.Cleanup(server.Close)
	client, err := modelinfo.New("")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContextResolver(ContextResolverOptions{Client: client, Catalog: cat, Binding: modelinfo.Binding{ProviderID: "friendly-gateway", Transport: "anthropic_messages", BaseURL: server.URL}, CatalogProvider: route, StrictRoute: strict, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return resolver
}

func fullProfileCatalog(model string) *catalog.Client {
	return catalog.NewStaticClient(catalog.Catalog{"openai": {Models: map[string]catalog.Model{model: {Limit: catalog.Limit{Context: 1050000, Input: 922000, Output: 128000}}}}})
}

func TestClientProfileUsesFullCatalogCapacity(t *testing.T) {
	// These are Codex client recommendations, not a served-model capacity
	// measurement. No model-family or numeric special case is involved.
	const model = "brand-new-exact-model"
	const listing = `{"models":[{"slug":"brand-new-exact-model","context_window":272000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`
	r := profileResolver(t, fullProfileCatalog(model), "friendly-gateway", model, listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 1050000 || got.MaxWindow != 1050000 || got.MaxInputTokens != 922000 || got.MaxOutputTokens != 128000 || got.Source != "catalog_profile" || got.ProfileDefaultWindow != 272000 || got.ProfileMaxWindow != 872000 {
		t.Fatalf("client recommendation replaced full model capacity: %+v", got)
	}
	configured := r.Resolve(800000, func() int { return 200000 })
	if configured.Window != 800000 || configured.Source != "configured" || configured.MaxWindow != 1050000 || configured.MaxInputTokens != 922000 || configured.ProfileDefaultWindow != 272000 {
		t.Fatalf("explicit budget lost independent model/profile limits: %+v", configured)
	}
}

func TestOrdinaryEndpointCapacityStillWinsOverFullCatalog(t *testing.T) {
	const model = "brand-new-exact-model"
	const listing = `{"models":[{"slug":"brand-new-exact-model","context_window":128000,"max_context_window":128000,"max_input_tokens":96000,"max_tokens":32000}]}`
	r := profileResolver(t, fullProfileCatalog(model), "friendly-gateway", model, listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 128000 || got.MaxWindow != 128000 || got.MaxInputTokens != 96000 || got.MaxOutputTokens != 32000 || got.ProfileDefaultWindow != 0 || got.Source != "endpoint_pi" {
		t.Fatalf("actual endpoint limit borrowed larger public capability: %+v", got)
	}
}

func TestUnknownClientProfileDoesNotInventFullCapacity(t *testing.T) {
	const model = "no-capacity-in-catalog"
	const listing = `{"models":[{"slug":"no-capacity-in-catalog","context_window":272000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`
	r := profileResolver(t, fullProfileCatalog("another-model"), "friendly-gateway", model, listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 272000 || got.MaxWindow != 0 || got.MaxInputTokens != 0 || got.ProfileMaxWindow != 872000 || got.Source != "endpoint_pi" {
		t.Fatalf("unknown profile maximum was promoted to technical capability: %+v", got)
	}
}

func TestExplicitCatalogMissDoesNotBorrowClientProfileCapacity(t *testing.T) {
	const model = "brand-new-exact-model"
	const listing = `{"models":[{"slug":"brand-new-exact-model","context_window":272000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`
	r := profileResolver(t, fullProfileCatalog(model), "misspelled-explicit-route", model, listing, true)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 272000 || got.MaxWindow != 0 || got.Source != "endpoint_pi" {
		t.Fatalf("strict route miss silently borrowed public capability: %+v", got)
	}
}

func TestClientProfilePreservesIndependentEndpointTokenCeilings(t *testing.T) {
	const model = "brand-new-exact-model"
	const listing = `{"models":[{"slug":"brand-new-exact-model","context_window":272000,"max_context_window":872000,"max_input_tokens":100000,"max_tokens":64000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`
	r := profileResolver(t, fullProfileCatalog(model), "friendly-gateway", model, listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 1050000 || got.ProfileDefaultWindow != 272000 || got.MaxInputTokens != 100000 || got.MaxOutputTokens != 64000 {
		t.Fatalf("profile classification erased actual endpoint token ceilings: %+v", got)
	}
}

func TestClientProfileColdCatalogRefreshBeforeRequest(t *testing.T) {
	var fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Write([]byte(`{"openai":{"models":{"brand-new-exact-model":{"limit":{"context":1050000,"input":922000,"output":128000}}}}}`))
	}))
	defer server.Close()
	cat := catalog.NewClient(t.TempDir())
	cat.URL = server.URL
	const listing = `{"models":[{"slug":"brand-new-exact-model","context_window":272000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`
	r := profileResolver(t, cat, "friendly-gateway", "brand-new-exact-model", listing, false)
	got := r.Resolve(0, func() int { return 200000 })
	if fetches.Load() != 1 || got.Window != 1050000 || got.MaxInputTokens != 922000 || got.Source != "catalog_profile" {
		t.Fatalf("cold first request retained client recommendation before catalog refresh: %+v fetches=%d", got, fetches.Load())
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 1 {
		t.Fatal("known cached model capacity unnecessarily fetched again")
	}
}
