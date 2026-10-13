package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func TestContextResolverColdDiscoveryUsesDefaultNotMaximum(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("client_version") != "pi" {
			t.Errorf("incorrect discovery route: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect discovery authentication")
		}
		w.Write([]byte(`{"models":[{"slug":"first-seen-private-model","context_window":272000,"max_context_window":872000,"max_tokens":128000}]}`))
	}))
	defer server.Close()
	client, err := modelinfo.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContextResolver(ContextResolverOptions{Client: client, Binding: modelinfo.Binding{ProviderID: "private-route", Transport: "anthropic_messages", BaseURL: server.URL, AuthKind: modelinfo.AuthBearer}, Model: "first-seen-private-model", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolver.Resolve(0, func() int { return 200000 }); got.Window != 200000 || got.Source != "fallback" {
		t.Fatalf("unexpected cold fallback: %+v", got)
	}
	if requests.Load() != 0 {
		t.Fatal("status resolution made a network request")
	}
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := ContextCapabilities{Window: 272000, WindowKind: ContextWindowTotal,
		DefaultWindow: 272000, MaxWindow: 872000, MaxOutputTokens: 128000,
		Source: "endpoint_pi", LimitSource: "endpoint_pi", BudgetSource: ContextBudgetAutomatic}
	if got := resolver.Resolve(0, func() int { return 200000 }); got != want {
		t.Fatalf("capabilities = %+v; want %+v", got, want)
	}
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("fresh cache was refetched")
	}
	configured := resolver.Resolve(800000, func() int { return 200000 })
	if configured.Window != 800000 || configured.DefaultWindow != 272000 || configured.MaxWindow != 872000 || configured.Source != "configured" {
		t.Fatalf("explicit configuration lost provenance: %+v", configured)
	}
}

func TestContextResolverUnavailableListingPreservesCapabilityAndCancellation(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"data":[{"id":"fresh-private","context_window":400000}]}`))
	}))
	defer server.Close()
	client, _ := modelinfo.New("", modelinfo.WithTTL(0))
	resolver, err := NewContextResolver(ContextResolverOptions{Client: client, Binding: modelinfo.Binding{ProviderID: "route", Transport: "openai_chat", BaseURL: server.URL}, Model: "fresh-private"})
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatalf("unsupported/unavailable discovery disabled generation: %v", err)
	}
	if got := resolver.Resolve(0, func() int { return 200000 }); got.Window != 400000 || got.Source != "endpoint_pi" {
		t.Fatalf("valid capability lost on failed refresh: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := resolver.Refresh(ctx); err != context.Canceled {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestContextResolverMaximumAloneDoesNotExpandDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"max-only-private","max_context_window":1000000,"max_input_tokens":160000,"max_tokens":4000}]}`))
	}))
	defer server.Close()
	client, _ := modelinfo.New("")
	resolver, _ := NewContextResolver(ContextResolverOptions{Client: client, Binding: modelinfo.Binding{ProviderID: "route", Transport: "openai_chat", BaseURL: server.URL}, Model: "max-only-private"})
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := resolver.Resolve(0, func() int { return 200000 })
	if got.Window != 200000 || got.DefaultWindow != 0 || got.MaxWindow != 1000000 || got.MaxInputTokens != 160000 || got.Source != "fallback" {
		t.Fatalf("maximum became implicit default: %+v", got)
	}
}
