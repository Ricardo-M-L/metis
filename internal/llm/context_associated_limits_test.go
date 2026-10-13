package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func associatedContextResolver(t *testing.T, plain string) *ContextResolver {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("client_version") == "pi" {
			w.Write([]byte(kimiIssuerProfile))
		} else {
			w.Write([]byte(plain))
		}
	}))
	t.Cleanup(server.Close)
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
	return r
}

func TestAssociatedCapacityWindowConstrainsIssuerCapacity(t *testing.T) {
	r := associatedContextResolver(t, `{"data":[{"id":"wire-alias","display_name":"kimi-k3","owned_by":"kimi","context_window":200000,"max_input_tokens":180000,"max_tokens":64000}]}`)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 200000 || got.MaxWindow != 200000 || got.MaxInputTokens != 0 || got.MaxOutputTokens != 200000 || got.Source != "endpoint_associated_capacity" || got.ProfileDefaultWindow != 272000 || got.ProfileMaxWindow != 272000 {
		t.Fatalf("served total limit lost or unrelated bare fields trusted: %+v", got)
	}
	configured := r.Resolve(120000, func() int { return 200000 })
	if configured.Window != 120000 || configured.Source != "configured" || configured.MaxWindow != 200000 {
		t.Fatalf("valid explicit operating budget lost: %+v", configured)
	}
	oversized := r.Resolve(800000, func() int { return 200000 })
	if oversized.Window != 200000 || oversized.MaxWindow != 200000 || oversized.Source != "endpoint_associated_capacity" {
		t.Fatalf("configured budget bypassed a proven served ceiling: %+v", oversized)
	}
}

func TestAssociatedDeclaredIndependentCapsConstrainCatalog(t *testing.T) {
	r := associatedContextResolver(t, `{"data":[{"id":"wire-alias","display_name":"kimi-k3","owned_by":"kimi","limit_evidence":"served_capacity","max_input_tokens":80000,"max_tokens":4096}]}`)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 1048576 || got.MaxWindow != 1048576 || got.MaxInputTokens != 80000 || got.MaxOutputTokens != 4096 || got.Source != "catalog_issuer_profile" {
		t.Fatalf("declared independent wire limits not applied: %+v", got)
	}
}

func TestAssociatedSameIDOrdinaryCapacityConstrainsProfile(t *testing.T) {
	r := associatedContextResolver(t, `{"data":[{"id":"kimi-k3","owned_by":"kimi","context_window":64000,"limit_evidence":"served_capacity","max_input_tokens":48000,"max_tokens":16000}]}`)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 64000 || got.MaxWindow != 64000 || got.MaxInputTokens != 48000 || got.MaxOutputTokens != 16000 || got.Source != "endpoint_associated_capacity" || got.ProfileDefaultWindow != 272000 {
		t.Fatalf("same-ID served capacity lost behind profile metadata: %+v", got)
	}
}

func TestAssociatedMaximumKeepsValidExplicitExpandedBudget(t *testing.T) {
	r := associatedContextResolver(t, `{"data":[{"id":"wire-alias","display_name":"kimi-k3","owned_by":"kimi","context_window":200000,"max_context_window":1000000}]}`)
	got := r.Resolve(800000, func() int { return 200000 })
	if got.Window != 800000 || got.MaxWindow != 1000000 || got.Source != "configured" || got.ProfileMaxWindow != 272000 {
		t.Fatalf("explicit budget within declared maximum was discarded: %+v", got)
	}
}

func TestAssociatedBarePeerFieldsRemainUnknown(t *testing.T) {
	r := associatedContextResolver(t, `{"data":[{"id":"wire-alias","display_name":"kimi-k3","owned_by":"kimi","max_input_tokens":200000,"max_tokens":64000}]}`)
	got := r.Resolve(0, func() int { return 200000 })
	if got.Window != 1048576 || got.MaxInputTokens != 0 || got.MaxOutputTokens != 1048576 || got.Source != "catalog_issuer_profile" {
		t.Fatalf("bare compatibility fields were promoted to served limits: %+v", got)
	}
}
