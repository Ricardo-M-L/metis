package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestGeminiContextSameRouteExactModelAndIndependentLimits(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/gateway/v1beta/models/gemini-fixture-pro" || r.URL.RawQuery != "" || r.Header.Get("x-goog-api-key") != "fixture-key" || r.Header.Get("Authorization") != "" {
			t.Errorf("wrong discovery contract: %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `{"name":"models/gemini-fixture-pro","inputTokenLimit":1048576,"outputTokenLimit":65536}`)
	}))
	defer server.Close()
	g := New("fixture-key", server.URL+"/gateway", "gemini-fixture-pro", 32000, time.Second, 0)
	for i := 0; i < 3; i++ {
		_ = g.ContextCapabilities()
		_ = g.MaxContextTokens()
	}
	if requests.Load() != 0 {
		t.Fatal("construction/status made HTTP")
	}
	if err := g.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := g.ContextCapabilities()
	if got.Window != 1048576 || got.MaxInputTokens != 1048576 || got.MaxOutputTokens != 65536 || got.WindowKind != llm.ContextWindowInput || got.MaxWindow != 0 || got.ModelWindow != 0 || got.DefaultWindow != 0 || got.Source != "gemini_model_input" {
		t.Fatalf("independent input/output limits became a total: %+v", got)
	}
	if g.MaxContextTokens() != got.Window {
		t.Fatal("generation sizing and snapshot diverged")
	}
	if err := g.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("fresh cache made %d HTTP requests", requests.Load())
	}
	g.ContextWindow, g.ContextBudgetSource = 2_000_000, llm.ContextBudgetPerModel
	got = g.ContextCapabilities()
	if got.Window != 1048576 || got.RequestedWindow != 2_000_000 || got.Source != "configured_clamped" || got.BudgetSource != llm.ContextBudgetPerModel {
		t.Fatalf("manual budget escaped served input ceiling: %+v", got)
	}
}

func TestGeminiContextCacheRetainsRouteAndExactWireSelection(t *testing.T) {
	cache := t.TempDir()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/gemini-first") {
			fmt.Fprint(w, `{"name":"models/gemini-first","inputTokenLimit":900000,"outputTokenLimit":64000}`)
		} else {
			fmt.Fprint(w, `{"name":"models/gemini-second","inputTokenLimit":32000,"outputTokenLimit":8192}`)
		}
	}))
	defer server.Close()
	first := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL, Model: "gemini-first", APIKey: "private-api-key", CacheDir: cache})
	if err := first.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(cache, "*.json"))
	if len(files) != 1 {
		t.Fatal("metadata not cached")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-api-key") || strings.Contains(string(data), server.URL) {
		t.Fatal("key or origin leaked into cache")
	}
	cold := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL, Model: "gemini-first", APIKey: "rotated-private-api-key", CacheDir: cache})
	if got := cold.resolve(llm.ContextCapabilities{Window: 1000000}); got.Window != 900000 || calls.Load() != 1 {
		t.Fatalf("cold cache missed or performed HTTP: %+v calls=%d", got, calls.Load())
	}
	second := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL, Model: "gemini-second", APIKey: "private-api-key", CacheDir: cache})
	if got := second.resolve(llm.ContextCapabilities{Window: 200000}); got.Window != 200000 || got.MaxInputTokens != 0 {
		t.Fatalf("models inherited each other's cache: %+v", got)
	}
	if err := second.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := second.resolve(llm.ContextCapabilities{Window: 200000}); got.Window != 32000 {
		t.Fatalf("selected model limit missed: %+v", got)
	}
	differentOrigin := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL + "/other", Model: "gemini-first", APIKey: "private-api-key", CacheDir: cache})
	if got := differentOrigin.resolve(llm.ContextCapabilities{Window: 200000}); got.Window != 200000 || got.MaxInputTokens != 0 {
		t.Fatalf("origins shared cache: %+v", got)
	}
}

func TestGeminiContextInvalidMetadataKeepsLastValidSnapshot(t *testing.T) {
	for _, bad := range []string{`<html>bad</html>`, `{"name":"models/another-model","inputTokenLimit":99999999}`, `{"name":"models/gemini-first","inputTokenLimit":-1}`, `{"name":"models/gemini-first","inputTokenLimit":"1000000"}`, `{"name":"models/gemini-first","inputTokenLimit":1000000,"outputTokenLimit":-1}`, strings.Repeat(" ", geminiContextBodyLimit+1)} {
		t.Run(fmt.Sprintf("bad-%d", len(bad)), func(t *testing.T) {
			var body atomic.Value
			body.Store(`{"name":"models/gemini-first","inputTokenLimit":900000}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body.Load().(string)) }))
			defer server.Close()
			c := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL, Model: "gemini-first", APIKey: "fixture-key"})
			c.ttl = 0
			if err := c.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			body.Store(bad)
			if err := c.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := c.resolve(llm.ContextCapabilities{Window: 200000})
			if got.Window != 900000 || got.MaxOutputTokens != 0 {
				t.Fatalf("bad schema replaced valid metadata or invented output limit: %+v", got)
			}
		})
	}
}

func TestGeminiContextOutputBudgetRejectsInvalidDirectRequests(t *testing.T) {
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"name":"models/gemini-first","inputTokenLimit":900000,"outputTokenLimit":1024}`)
			return
		}
		posts.Add(1)
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n")
		} else {
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
		}
	}))
	defer server.Close()
	g := New("fixture-key", server.URL, "gemini-first", 4096, time.Second, 0)
	if err := g.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Complete(context.Background(), provider.Request{}); !errors.Is(err, ErrContextOutputBudget) {
		t.Fatalf("invalid default Complete budget: %v", err)
	}
	if _, err := g.Stream(context.Background(), provider.Request{MaxTokens: 2048}); !errors.Is(err, ErrContextOutputBudget) {
		t.Fatalf("invalid explicit Stream budget: %v", err)
	}
	if posts.Load() != 0 {
		t.Fatal("invalid output budgets reached model generation")
	}
	if _, err := g.Complete(context.Background(), provider.Request{MaxTokens: 1024}); err != nil {
		t.Fatal(err)
	}
	stream, err := g.Stream(context.Background(), provider.Request{MaxTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if posts.Load() != 2 {
		t.Fatalf("valid output budgets did not execute: %d", posts.Load())
	}
}

func TestGeminiContextBudgetUpdatesOnlyAtRequestBoundary(t *testing.T) {
	var lookupCalls, configured atomic.Int64
	configured.Store(64000)
	g := New("", "https://generativelanguage.googleapis.com", "gemini-first", 4096, time.Second, 0)
	g.ContextWindow = 32000
	g.ContextBudgetPolicy = llm.NewContextBudgetPolicy(llm.ContextBudgetPolicyOptions{LegacyWindow: 32000, InitialWindow: 64000, InitialSource: llm.ContextBudgetPerModel,
		Lookup: func(context.Context) (int, bool, error) { lookupCalls.Add(1); return int(configured.Load()), true, nil }})
	if got := g.ContextCapabilities(); got.Window != 64000 || lookupCalls.Load() != 0 {
		t.Fatalf("status did disk lookup or lost policy: %+v calls=%d", got, lookupCalls.Load())
	}
	configured.Store(128000)
	if got := g.ContextCapabilities(); got.Window != 64000 || lookupCalls.Load() != 0 {
		t.Fatalf("status bypassed request policy snapshot: %+v", got)
	}
	if err := g.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := g.ContextCapabilities(); got.Window != 128000 || got.BudgetSource != llm.ContextBudgetPerModel || g.ContextWindow != 32000 || lookupCalls.Load() != 1 {
		t.Fatalf("next request did not bind updated budget: %+v legacy=%d calls=%d", got, g.ContextWindow, lookupCalls.Load())
	}
}

func TestGeminiContextUnknownModel404KeepsConservativeBudget(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model metadata unavailable"}`)
	}))
	defer server.Close()
	g := New("fixture-key", server.URL, "gemini-future-fixture", 4096, time.Second, 0)
	if err := g.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := g.ContextCapabilities()
	if got.Window != 64000 || got.ModelWindow != 0 || got.MaxWindow != 0 || got.MaxInputTokens != 0 || got.MaxOutputTokens != 0 || got.Source != "estimated" {
		t.Fatalf("unknown model received an asserted million-token window: %+v", got)
	}
	g.ContextWindow = 2_000_000
	got = g.ContextCapabilities()
	if got.Window != 2_000_000 || got.RequestedWindow != 2_000_000 || got.Source != "configured" || got.ModelWindow != 0 || got.MaxWindow != 0 {
		t.Fatalf("unknown server limits were invented to reject explicit budget: %+v", got)
	}
	if err := g.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("failed discovery ignored cooldown: %d requests", calls.Load())
	}
}

func TestGeminiContextWaitingRefreshRemainsCancellable(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"name":"models/gemini-first","inputTokenLimit":900000}`)
	}))
	defer server.Close()
	c := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL, Model: "gemini-first", APIKey: "fixture-key"})
	first := make(chan error, 1)
	defer func() {
		close(release)
		if err := <-first; err != nil {
			t.Errorf("first discovery cleanup: %v", err)
		}
	}()
	go func() { first <- c.Refresh(context.Background()) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { second <- c.Refresh(ctx) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancelled waiter result=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter blocked behind another native model listing")
	}
	if got := c.resolve(llm.ContextCapabilities{Window: 64000}); got.Window != 64000 {
		t.Fatalf("status was coupled to in-flight HTTP: %+v", got)
	}
}

func TestGeminiContextRedirectCannotForwardAPIKey(t *testing.T) {
	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		if r.Header.Get("x-goog-api-key") != "" {
			t.Error("key leaked through metadata redirect")
		}
		w.WriteHeader(200)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	c := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: source.URL, Model: "gemini-first", APIKey: "private-key"})
	c.http.CheckRedirect = nil
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("metadata followed redirect %d times", targetRequests.Load())
	}
}

func TestGeminiContextCancellationAndInvalidWireAliases(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"name":"models/gemini-first","inputTokenLimit":900000}`)
	}))
	defer server.Close()
	c := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL, Model: "gemini-first", APIKey: "fixture-key"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Refresh(ctx); err != context.Canceled {
		t.Fatalf("cancelled discovery error=%v", err)
	}
	for _, alias := range []string{"models/gemini-first", "gemini-first?key=other", "gemini%2ffirst", "../gemini-first", "gemini-first:generateContent"} {
		c := NewContextDiscovery(ContextDiscoveryOptions{BaseURL: server.URL, Model: alias, APIKey: "fixture-key"})
		if err := c.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := c.resolve(llm.ContextCapabilities{Window: 200000}); got.Window != 200000 || got.ModelWindow != 0 {
			t.Fatalf("alias received metadata for another wire route: %+v", got)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid aliases/cancellation made %d requests", requests.Load())
	}
}
