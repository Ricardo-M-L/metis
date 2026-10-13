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

func TestCatalogFallbackRefreshesStaleIDOnlyAndAbsentMetadata(t *testing.T) {
	for _, withMetadata := range []bool{true, false} {
		t.Run(map[bool]string{true: "ID_only", false: "no_discovery_client"}[withMetadata], func(t *testing.T) {
			var fetches atomic.Int32
			catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				fetches.Add(1)
				w.Write(refreshCatalogBody(catalog.Limit{Context: 600000, Input: 500000, Output: 64000}))
			}))
			defer catalogServer.Close()
			cat := newRefreshCatalog(t, refreshProfileModel, catalog.Limit{Context: 400000})
			cat.URL, cat.HTTP = catalogServer.URL, catalogServer.Client()
			modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte(`{"data":[{"id":"stale-profile-wire-model"}]}`))
			}))
			defer modelServer.Close()
			var client *modelinfo.Client
			if withMetadata {
				client, _ = modelinfo.New("")
			}
			r, err := NewContextResolver(ContextResolverOptions{Client: client, Catalog: cat, Model: refreshProfileModel, CatalogProvider: "private-profile-route", Binding: modelinfo.Binding{ProviderID: "private-profile-route", Transport: "anthropic_messages", BaseURL: modelServer.URL}})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := r.Resolve(0, func() int { return 200000 })
			if fetches.Load() != 1 || got.Window != 600000 || got.MaxInputTokens != 500000 || got.MaxOutputTokens != 64000 {
				t.Fatalf("catalog-only fallback kept stale capacity: fetches=%d capabilities=%+v", fetches.Load(), got)
			}
		})
	}
}
