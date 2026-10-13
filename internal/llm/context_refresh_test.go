package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

const refreshProfileModel = "stale-profile-wire-model"
const refreshProfileBody = `{"models":[{"slug":"stale-profile-wire-model","context_window":272000,"max_context_window":872000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}]}`

func newRefreshCatalog(t *testing.T, model string, limit catalog.Limit) *catalog.Client {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "cache", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(catalog.Catalog{"original": {Models: map[string]catalog.Model{model: {Limit: limit}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return catalog.NewClient(home)
}

func newRefreshProfileResolver(t *testing.T, cat *catalog.Client) *ContextResolver {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(refreshProfileBody))
	}))
	t.Cleanup(server.Close)
	client, err := modelinfo.New("")
	if err != nil {
		t.Fatal(err)
	}
	binding := modelinfo.Binding{ProviderID: "private-profile-route", Transport: "anthropic_messages", BaseURL: server.URL}
	if _, err := client.Refresh(context.Background(), binding, "fixture-key"); err != nil {
		t.Fatal(err)
	}
	r, err := NewContextResolver(ContextResolverOptions{Client: client, Binding: binding, Catalog: cat, CatalogProvider: "private-profile-route", Model: refreshProfileModel})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func refreshCatalogBody(limit catalog.Limit) []byte {
	body, _ := json.Marshal(catalog.Catalog{"original": {Models: map[string]catalog.Model{refreshProfileModel: {Limit: limit}}}})
	return body
}

func TestProfileRefreshUpdatesPreviouslyFoundStaleCatalogLimits(t *testing.T) {
	var requests atomic.Int32
	want := catalog.Limit{Context: 1050000, Input: 922000, Output: 128000}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(refreshCatalogBody(want))
	}))
	defer server.Close()
	cat := newRefreshCatalog(t, refreshProfileModel, catalog.Limit{Context: 400000, Input: 300000, Output: 32000})
	cat.URL, cat.HTTP = server.URL, server.Client()
	r := newRefreshProfileResolver(t, cat)
	if before := r.Resolve(0, func() int { return 200000 }); before.Window != 400000 || requests.Load() != 0 {
		t.Fatalf("cached status triggered HTTP or lost old metadata: %+v requests=%d", before, requests.Load())
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.Resolve(0, func() int { return 200000 })
	if requests.Load() != 1 || got.Window != want.Context || got.MaxInputTokens != want.Input || got.MaxOutputTokens != want.Output || cat.Stat().Stale {
		t.Fatalf("known stale record never refreshed: %+v requests=%d stale=%v", got, requests.Load(), cat.Stat().Stale)
	}
}

func TestProfileRefreshMissingOfflineCatalogDoesNotStorm(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cat := newRefreshCatalog(t, "different-model", catalog.Limit{Context: 400000})
	cat.URL, cat.HTTP = server.URL, server.Client()
	r := newRefreshProfileResolver(t, cat)
	for i := 0; i < 4; i++ {
		if err := r.Refresh(context.Background()); err != nil {
			t.Fatalf("catalog outage blocked generation: %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("failed discovery retried on every query: requests=%d, want1", requests.Load())
	}
	if got := r.Resolve(0, func() int { return 200000 }); got.Window != 272000 || !cat.Stat().Stale {
		t.Fatalf("outage lost cached/profile metadata: %+v", got)
	}
}

func waitContextCatalogRefresh(t *testing.T, r *ContextResolver) {
	t.Helper()
	if r == nil {
		return
	}
	r.catalogMu.Lock()
	flight := r.catalogFlight
	r.catalogMu.Unlock()
	if flight != nil {
		select {
		case <-flight.done:
		case <-time.After(6 * time.Second):
			t.Error("bounded catalog refresh did not clean up its flight")
		}
	}
}

func TestProfileRefreshOfflineStaleSnapshotRetriesAfterCooldown(t *testing.T) {
	var requests atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	want := catalog.Limit{Context: 1050000, Input: 922000, Output: 128000}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(refreshCatalogBody(want))
	}))
	defer server.Close()
	cat := newRefreshCatalog(t, refreshProfileModel, catalog.Limit{Context: 400000, Input: 300000, Output: 32000})
	cat.URL, cat.HTTP = server.URL, server.Client()
	r := newRefreshProfileResolver(t, cat)
	var clock atomic.Int64
	base := time.Now()
	clock.Store(base.UnixNano())
	r.catalogNow = func() time.Time { return time.Unix(0, clock.Load()) }
	for i := 0; i < 5; i++ {
		if err := r.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.Resolve(0, func() int { return 200000 }); got.Window != 400000 || got.MaxInputTokens != 300000 || requests.Load() != 1 || !cat.Stat().Stale {
		t.Fatalf("nil-error offline fallback was treated as fresh or dropped: %+v requests=%d stale=%v", got, requests.Load(), cat.Stat().Stale)
	}
	clock.Store(base.Add(29 * time.Second).UnixNano())
	if err := r.Refresh(context.Background()); err != nil || requests.Load() != 1 {
		t.Fatalf("failure cooldown retried early: err=%v requests=%d", err, requests.Load())
	}
	fail.Store(false)
	clock.Store(base.Add(31 * time.Second).UnixNano())
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.Resolve(0, func() int { return 200000 }); got.Window != want.Context || requests.Load() != 2 || cat.Stat().Stale {
		t.Fatalf("catalog stayed stale after cooldown and recovery: %+v requests=%d stale=%v", got, requests.Load(), cat.Stat().Stale)
	}
	waitContextCatalogRefresh(t, r)
}

func TestProfileRefreshCancellationOnlyStopsCallerAndSharesFlight(t *testing.T) {
	var requests atomic.Int32
	var upstreamCancelled atomic.Bool
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var r *ContextResolver
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			_, _ = w.Write(refreshCatalogBody(catalog.Limit{Context: 1050000, Input: 922000, Output: 128000}))
		case <-req.Context().Done():
			upstreamCancelled.Store(true)
		}
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		waitContextCatalogRefresh(t, r)
		server.Close()
	}()
	cat := newRefreshCatalog(t, refreshProfileModel, catalog.Limit{Context: 400000})
	cat.URL, cat.HTTP = server.URL, server.Client()
	r = newRefreshProfileResolver(t, cat)
	ctx, cancel := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	go func() { owner <- r.Refresh(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("stale matched catalog did not start refresh")
	}
	cancel()
	select {
	case err := <-owner:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation waited for shared HTTP")
	}
	if upstreamCancelled.Load() {
		t.Fatal("caller cancellation cancelled the shared upstream request")
	}
	resolved := make(chan ContextCapabilities, 1)
	go func() { resolved <- r.Resolve(0, func() int { return 200000 }) }()
	select {
	case got := <-resolved:
		if got.Window != 400000 {
			t.Fatalf("cancelled request replaced old metadata prematurely: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Resolve waited for catalog refresh lock")
	}
	const waiters = 12
	errors := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() { errors <- r.Refresh(context.Background()) }()
	}
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < waiters; i++ {
		select {
		case err := <-errors:
			if err != nil {
				t.Fatalf("shared waiter inherited another caller's cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("shared catalog waiter did not finish")
		}
	}
	if got := r.Resolve(0, func() int { return 200000 }); got.Window != 1050000 || requests.Load() != 1 || upstreamCancelled.Load() {
		t.Fatalf("cancelled flight poisoned next request or repeated HTTP: %+v requests=%d cancelled=%v", got, requests.Load(), upstreamCancelled.Load())
	}
}

func TestProfileRefreshSharedTimeoutIsBoundedAndPreservesOldLimits(t *testing.T) {
	var requests atomic.Int32
	requestDone := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-r.Context().Done()
		requestDone <- struct{}{}
	}))
	defer server.Close()
	cat := newRefreshCatalog(t, refreshProfileModel, catalog.Limit{Context: 400000})
	cat.URL, cat.HTTP = server.URL, server.Client()
	r := newRefreshProfileResolver(t, cat)
	r.catalogRefreshTimeout = 50 * time.Millisecond
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("bounded catalog timeout blocked generation: %v", err)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("bounded request did not close upstream HTTP")
	}
	for i := 0; i < 4; i++ {
		if err := r.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.Resolve(0, func() int { return 200000 }); got.Window != 400000 || requests.Load() != 1 {
		t.Fatalf("timeout lost old limits or retried every query: %+v requests=%d", got, requests.Load())
	}
	waitContextCatalogRefresh(t, r)
}

func TestResolverConstructionCapturesDiskCatalogWithoutNetworkWarmup(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "status must not request network", http.StatusInternalServerError)
	}))
	defer server.Close()
	cat := newRefreshCatalog(t, refreshProfileModel, catalog.Limit{Context: 400000, Input: 300000, Output: 32000})
	t.Setenv("METIS_HOME", filepath.Dir(filepath.Dir(cat.CachePath)))
	t.Setenv("METIS_MODELS_URL", server.URL)
	t.Setenv("METIS_CATALOG_DISABLE", "")
	r := newRefreshProfileResolver(t, nil)
	for i := 0; i < 30; i++ {
		got := r.Resolve(0, func() int { return 200000 })
		if got.Window != 400000 || got.MaxInputTokens != 300000 || requests.Load() != 0 || r.catalog == nil || !r.catalog.Stat().Stale {
			t.Fatalf("constructor/status refreshed network or lost captured disk age: %+v requests=%d", got, requests.Load())
		}
	}
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	disabled := newRefreshProfileResolver(t, nil)
	if disabled.catalog != nil {
		t.Fatal("disabled catalog was instantiated")
	}
	if got := disabled.Resolve(0, func() int { return 200000 }); got.Window != 272000 || requests.Load() != 0 {
		t.Fatalf("disabled catalog fell through to network warmup: %+v requests=%d", got, requests.Load())
	}
}
