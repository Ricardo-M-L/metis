package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func endpointBudgetResolver(t *testing.T, body, budgetSource string, cat *catalog.Client) *ContextResolver {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client, err := modelinfo.New("")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContextResolver(ContextResolverOptions{Client: client, Catalog: cat,
		Binding: modelinfo.Binding{ProviderID: "budget-route", Transport: "openai_chat", BaseURL: server.URL},
		Model:   "exact-model", CatalogProvider: "budget-route", BudgetSource: budgetSource})
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return resolver
}

func TestContextBudgetCannotExceedKnownServingMaximum(t *testing.T) {
	r := endpointBudgetResolver(t, `{"data":[{"id":"exact-model","context_window":128000,"max_context_window":256000,"max_input_tokens":220000,"max_tokens":32000}]}`,
		ContextBudgetPerModel, catalog.NewStaticClient(catalog.Catalog{}))
	got := r.Resolve(1000000, func() int { return 200000 })
	if got.Window != 256000 || got.RequestedWindow != 1000000 || got.MaxWindow != 256000 || got.Source != "configured_clamped" || got.LimitSource != "endpoint_pi" || got.BudgetSource != ContextBudgetPerModel {
		t.Fatalf("oversized manual request bypassed serving ceiling or hid adjustment: %+v", got)
	}
	if got.DefaultWindow != 128000 || got.ModelWindow != 0 || got.ModelSource != "" || got.MaxInputTokens != 220000 || got.MaxOutputTokens != 32000 {
		t.Fatalf("clamping destroyed independent discovery fields: %+v", got)
	}
	valid := r.Resolve(180000, func() int { return 200000 })
	if valid.Window != 180000 || valid.Source != "configured" || valid.RequestedWindow != 180000 {
		t.Fatalf("valid manual budget was not honored: %+v", valid)
	}
}

func TestContextBudgetProfileRecommendationsAndFullModelStaySeparate(t *testing.T) {
	cat := catalog.NewStaticClient(catalog.Catalog{"openai": {Models: map[string]catalog.Model{
		"exact-model": {Limit: catalog.Limit{Context: 1050000, Input: 922000, Output: 128000}},
	}}})
	r := endpointBudgetResolver(t, `{"models":[{"slug":"exact-model","context_window":500000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`,
		ContextBudgetAutomatic, cat)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 1050000 || got.ModelWindow != 1050000 || got.ModelMaxInputTokens != 922000 || got.ModelMaxOutputTokens != 128000 || got.ModelSource != "catalog_model" {
		t.Fatalf("precise full-model evidence was reduced to a recommendation: %+v", got)
	}
	if got.DefaultWindow != 0 || got.ProfileDefaultWindow != 500000 || got.ProfileMaxWindow != 872000 || got.BudgetSource != ContextBudgetAutomatic {
		t.Fatalf("recommendations masqueraded as endpoint defaults or limits: %+v", got)
	}
	configured := r.Resolve(800000, func() int { return 200000 })
	if configured.Window != 800000 || configured.RequestedWindow != 800000 || configured.Source != "configured" || configured.DefaultWindow != 0 || configured.ProfileDefaultWindow != 500000 {
		t.Fatalf("client recommendation became a physical ceiling for a valid expanded budget: %+v", configured)
	}
}

func TestContextBudgetUnknownProfileDoesNotManufactureHardMaximum(t *testing.T) {
	r := endpointBudgetResolver(t, `{"models":[{"slug":"exact-model","context_window":272000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`,
		ContextBudgetPerModel, catalog.NewStaticClient(catalog.Catalog{}))
	got := r.Resolve(1000000, func() int { return 200000 })
	if got.Window != 1000000 || got.MaxWindow != 0 || got.ModelWindow != 0 || got.ModelSource != "" || got.ProfileMaxWindow != 872000 || got.Source != "configured" {
		t.Fatalf("unknown model received a manufactured physical ceiling: %+v", got)
	}
}

func TestContextBudgetInputOnlyDoesNotInventTotalOrReserveOutputTwice(t *testing.T) {
	r := endpointBudgetResolver(t, `{"data":[{"id":"exact-model","max_input_tokens":160000,"max_tokens":64000}]}`,
		ContextBudgetAutomatic, catalog.NewStaticClient(catalog.Catalog{}))
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 160000 || got.WindowKind != ContextWindowInput || got.MaxInputTokens != 160000 || got.MaxOutputTokens != 64000 || got.MaxWindow != 0 || got.ModelWindow != 0 {
		t.Fatalf("independent input/output limits became a fake total window: %+v", got)
	}
	oversized := r.Resolve(240000, func() int { return 200000 })
	if oversized.Window != 160000 || oversized.RequestedWindow != 240000 || oversized.Source != "configured_clamped" {
		t.Fatalf("input-only manual budget bypassed a real input cap: %+v", oversized)
	}
}

func TestContextBudgetServingLimitDoesNotOverwriteIndependentModelCapacity(t *testing.T) {
	cat := catalog.NewStaticClient(catalog.Catalog{"openai": {Models: map[string]catalog.Model{
		"exact-model": {Limit: catalog.Limit{Context: 1000000, Input: 900000, Output: 100000}},
	}}})
	r := endpointBudgetResolver(t, `{"data":[{"id":"exact-model","context_window":128000,"max_input_tokens":96000,"max_tokens":32000}]}`,
		ContextBudgetAutomatic, cat)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 128000 || got.MaxWindow != 128000 || got.ModelWindow != 1000000 || got.ModelMaxInputTokens != 900000 || got.ModelMaxOutputTokens != 100000 || got.ModelSource != "catalog_model" || got.LimitSource != "endpoint_pi" {
		t.Fatalf("serving restriction destroyed model specification or model spec bypassed route: %+v", got)
	}
	manual := r.Resolve(800000, func() int { return 200000 })
	if manual.Window != 128000 || manual.RequestedWindow != 800000 || manual.MaxWindow != 128000 || manual.DefaultWindow != 128000 || manual.ModelWindow != 1000000 || manual.Source != "configured_clamped" || manual.LimitSource != "endpoint_pi" {
		t.Fatalf("manual budget bypassed a capacity-only serving record or hid model/default provenance: %+v", manual)
	}
}

func TestContextBudgetSourceAnnotationPreservesClamp(t *testing.T) {
	got := ApplyContextBudgetSource(ContextCapabilities{Window: 128000, RequestedWindow: 1000000,
		Source: "configured_clamped", LimitSource: "endpoint_models"}, ContextBudgetPerModel)
	if got.Source != "configured_clamped" || got.RequestedWindow != 1000000 || got.Window != 128000 || got.LimitSource != "endpoint_models" || got.BudgetSource != ContextBudgetPerModel {
		t.Fatalf("configuration origin erased clamp or discovery provenance: %+v", got)
	}
}

func TestContextBudgetModelSwitchDoesNotCarryTotalCapacityIntoInputOnlyModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"total-model","context_window":1000000,"max_tokens":100000},{"id":"input-model","max_input_tokens":160000,"max_tokens":64000}]}`))
	}))
	defer server.Close()
	client, err := modelinfo.New("")
	if err != nil {
		t.Fatal(err)
	}
	options := ContextResolverOptions{Client: client, Catalog: catalog.NewStaticClient(catalog.Catalog{}),
		Binding: modelinfo.Binding{ProviderID: "switch-route", Transport: "openai_chat", BaseURL: server.URL}, Model: "total-model"}
	total, err := NewContextResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := total.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	options.Model = "input-model"
	input, err := NewContextResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	if got := total.Resolve(0, func() int { return 200000 }); got.Window != 1000000 || got.WindowKind != ContextWindowTotal || got.ModelWindow != 0 || got.MaxWindow != 1000000 {
		t.Fatalf("total-model binding lost: %+v", got)
	}
	got := input.Resolve(0, func() int { return 200000 })
	if got.Window != 160000 || got.WindowKind != ContextWindowInput || got.ModelWindow != 0 || got.MaxWindow != 0 || got.MaxOutputTokens != 64000 {
		t.Fatalf("new model inherited prior total-model limits: %+v", got)
	}
}

func TestContextBudgetDoesNotCopyCatalogCapacityFromDisplayAlias(t *testing.T) {
	cat := catalog.NewStaticClient(catalog.Catalog{"openai": {Models: map[string]catalog.Model{
		"physical-model": {Limit: catalog.Limit{Context: 1000000, Input: 900000, Output: 100000}},
	}}})
	r := endpointBudgetResolver(t, `{"data":[{"id":"exact-model","display_name":"physical-model","owned_by":"openai"}]}`,
		ContextBudgetAutomatic, cat)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 200000 || got.ModelWindow != 0 || got.MaxWindow != 0 || got.ModelSource != "" || got.Source != "fallback" {
		t.Fatalf("display alias silently borrowed another model's capability: %+v", got)
	}
}

func TestContextBudgetAutomaticFullModelUsesGenuineExpandedServingCapacity(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		body                  string
		wantInput, wantOutput int
	}{
		{"missing serving IO cannot erase known model constraints", `{"data":[{"id":"exact-model","context_window":400000,"max_context_window":600000}]}`, 300000, 128000},
		{"stricter serving IO constrains full model", `{"data":[{"id":"exact-model","context_window":400000,"max_context_window":600000,"max_input_tokens":160000,"max_tokens":64000}]}`, 160000, 64000},
		{"larger serving IO cannot enlarge known model constraints", `{"data":[{"id":"exact-model","context_window":400000,"max_context_window":600000,"max_input_tokens":900000,"max_tokens":256000}]}`, 300000, 128000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := catalog.NewStaticClient(catalog.Catalog{"openai": {Models: map[string]catalog.Model{
				"exact-model": {Limit: catalog.Limit{Context: 1000000, Input: 300000, Output: 128000}},
			}}})
			r := endpointBudgetResolver(t, tc.body, ContextBudgetAutomatic, cat)
			got := r.Resolve(0, func() int { return 200000 })
			if got.Window != 600000 || got.DefaultWindow != 400000 || got.MaxWindow != 600000 || got.ModelWindow != 1000000 || got.ModelSource != "catalog_model" || got.Source != "endpoint_pi" || got.LimitSource != "endpoint_pi" || got.BudgetSource != ContextBudgetAutomatic {
				t.Fatalf("automatic full budget ignored expansion or lost independent capacity provenance: %+v", got)
			}
			if got.MaxInputTokens != tc.wantInput || got.MaxOutputTokens != tc.wantOutput || got.ModelMaxInputTokens != 300000 || got.ModelMaxOutputTokens != 128000 {
				t.Fatalf("serving metadata erased model IO limits or changed model specification: %+v", got)
			}
		})
	}
}

func TestContextBudgetRouteMaximumWithoutModelSpecificationKeepsConservativeDefault(t *testing.T) {
	r := endpointBudgetResolver(t, `{"data":[{"id":"exact-model","context_window":400000,"max_context_window":600000}]}`,
		ContextBudgetAutomatic, catalog.NewStaticClient(catalog.Catalog{}))
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 400000 || got.DefaultWindow != 400000 || got.MaxWindow != 600000 || got.ModelWindow != 0 || got.ModelMaxInputTokens != 0 || got.ModelMaxOutputTokens != 0 || got.ModelSource != "" {
		t.Fatalf("serving maximum became an independently known full-model specification: %+v", got)
	}
	manual := r.Resolve(800000, func() int { return 200000 })
	if manual.Window != 600000 || manual.RequestedWindow != 800000 || manual.ModelWindow != 0 || manual.Source != "configured_clamped" {
		t.Fatalf("a manually expanded unknown model bypassed a genuine route ceiling: %+v", manual)
	}
}

func TestContextBudgetKnownModelPhysicalCeilingCannotBeEnlargedByServingMetadata(t *testing.T) {
	cat := catalog.NewStaticClient(catalog.Catalog{"openai": {Models: map[string]catalog.Model{
		"exact-model": {Limit: catalog.Limit{Context: 100000, Input: 80000, Output: 20000}},
	}}})
	r := endpointBudgetResolver(t, `{"data":[{"id":"exact-model","context_window":400000,"max_context_window":600000,"max_input_tokens":300000,"max_tokens":128000}]}`,
		ContextBudgetPerModel, cat)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 100000 || got.DefaultWindow != 400000 || got.MaxWindow != 100000 || got.ModelWindow != 100000 || got.MaxInputTokens != 80000 || got.MaxOutputTokens != 20000 || got.Source != "catalog_model" || got.ModelSource != "catalog_model" {
		t.Fatalf("route metadata enlarged an independently known model ceiling: %+v", got)
	}
	manual := r.Resolve(120000, func() int { return 200000 })
	if manual.Window != 100000 || manual.RequestedWindow != 120000 || manual.Source != "configured_clamped" || manual.LimitSource != "catalog_model" {
		t.Fatalf("manual budget bypassed known model physical ceiling: %+v", manual)
	}
}
