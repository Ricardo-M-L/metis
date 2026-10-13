package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

type codexContextTransport func(*http.Request) (*http.Response, error)

func (f codexContextTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func codexContextReply(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func fixtureCodexCatalog() *catalog.Client {
	return catalog.NewStaticClient(catalog.Catalog{
		"openai": {Models: map[string]catalog.Model{
			"gpt-6-astra": {Limit: catalog.Limit{Context: 1_050_000, Input: 922_000, Output: 128_000}},
			"gpt-6.1-sol": {Limit: catalog.Limit{Context: 1_050_000, Output: 128_000}},
		}},
		// Another route's similarly named variant is not a subscription spec.
		"unrelated-proxy": {Models: map[string]catalog.Model{"gpt-6-new-model": {Limit: catalog.Limit{Context: 8_000_000}}}},
	})
}

func TestCodexContextUsesOnlyOAuthSubscriptionDiscovery(t *testing.T) {
	var genericRequests, subscriptionRequests, authCalls atomic.Int64
	genericClient, _ := modelinfo.New("")
	generic, err := llm.NewContextResolver(llm.ContextResolverOptions{Client: genericClient, Model: "gpt-6-astra", APIKey: "public-api-key",
		Binding: modelinfo.Binding{ProviderID: "public-openai", Transport: "openai_responses", BaseURL: "http://127.0.0.1:1/v1"}, Catalog: catalog.NewStaticClient(catalog.Catalog{})})
	if err != nil {
		t.Fatal(err)
	}
	p := NewCodexResponses("gpt-6-astra", 4096, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
		authCalls.Add(1)
		return ResponsesOAuthCredential{AccessToken: "oauth-fixture", AccountID: "account-fixture"}, nil
	})
	p.ContextResolver = generic
	p.APIKey = "must-not-use-this-key"
	p.CodexContextResolver = NewCodexContextResolver(CodexContextResolverOptions{TokenSource: p.OAuthTokenSource, Catalog: fixtureCodexCatalog()})
	p.CodexContextResolver.http.Transport = codexContextTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" {
			genericRequests.Add(1)
			t.Errorf("subscription auth escaped origin: %s", r.URL)
		}
		subscriptionRequests.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != codexModelsURL || r.Header.Get("Authorization") != "Bearer oauth-fixture" || r.Header.Get("chatgpt-account-id") != "account-fixture" {
			t.Errorf("wrong OAuth discovery contract: method=%s url=%s", r.Method, r.URL)
		}
		return codexContextReply(r, 200, `{"models":[{"slug":"gpt-6-astra","context_window":272000,"max_context_window":872000,"max_tokens":128000}]}`), nil
	})
	for i := 0; i < 5; i++ {
		_ = p.ContextCapabilities()
		_ = p.MaxContextTokens()
	}
	if subscriptionRequests.Load() != 0 || authCalls.Load() != 0 {
		t.Fatal("construction/status reads made an authenticated request")
	}
	if err := p.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := p.ContextCapabilities()
	if got.Window != 1_050_000 || got.ModelWindow != 1_050_000 || got.ModelSource != "codex_model_catalog" || got.ProfileDefaultWindow != 272000 || got.ProfileMaxWindow != 872000 || got.DefaultWindow != 0 || got.MaxOutputTokens != 128000 || got.MaxInputTokens != 922000 || got.MaxWindow != 1_050_000 || got.Source != "codex_model_catalog" {
		t.Fatalf("model capacity mixed with recommendation: %+v", got)
	}
	for i := 0; i < 5; i++ {
		_ = p.ContextCapabilities()
		_ = p.MaxContextTokens()
	}
	if err := p.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if subscriptionRequests.Load() != 1 || genericRequests.Load() != 0 {
		t.Fatalf("wrong request count subscription=%d public=%d", subscriptionRequests.Load(), genericRequests.Load())
	}
}

func TestCodexContextDynamicModelsUnknownAndProfilePolicy(t *testing.T) {
	profiles := []string{`{"slug":"gpt-6-astra","context_window":272000,"max_context_window":872000}`, `{"slug":"gpt-6.1-sol","context_window":500000,"max_context_window":872000}`}
	for i := 0; i < 7; i++ {
		profiles = append(profiles, fmt.Sprintf(`{"slug":"future-model-%d","context_window":%d,"max_context_window":872000}`, i, 250000+i))
	}
	profiles = append(profiles, `{"slug":"gpt-6-new-model"}`)
	c := NewCodexContextResolver(CodexContextResolverOptions{TokenSource: func(context.Context) (ResponsesOAuthCredential, error) {
		return ResponsesOAuthCredential{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
	}, Catalog: fixtureCodexCatalog()})
	c.http.Transport = codexContextTransport(func(r *http.Request) (*http.Response, error) {
		return codexContextReply(r, 200, `{"models":[`+strings.Join(profiles, ",")+`]}`), nil
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.snapshot.Models) != 10 {
		t.Fatalf("new IDs were dropped: %d", len(c.snapshot.Models))
	}
	got := c.Resolve("gpt-6.1-sol", 0, func() int { return 200000 })
	if got.Window != 1_050_000 || got.ProfileDefaultWindow != 500000 || got.ProfileMaxWindow != 872000 {
		t.Fatalf("new exact catalog model/default lost: %+v", got)
	}
	got = c.Resolve("future-model-4", 0, func() int { return 200000 })
	if got.Window != 250004 || got.ModelWindow != 0 || got.MaxWindow != 0 || got.ProfileMaxWindow != 872000 {
		t.Fatalf("unknown capacity was invented: %+v", got)
	}
	got = c.Resolve("gpt-6-new-model", 0, func() int { return 200000 })
	if got.Window != 200000 || got.ModelWindow != 0 || got.MaxWindow != 0 {
		t.Fatalf("prefix/unrelated route supplied capacity: %+v", got)
	}
	c.autoMode = "profile_default"
	got = c.Resolve("gpt-6.1-sol", 0, func() int { return 200000 })
	if got.Window != 500000 || got.ModelWindow != 1_050_000 || got.Source != "codex_subscription_models" {
		t.Fatalf("profile policy did not preserve independent model spec: %+v", got)
	}
	got = c.Resolve("gpt-6.1-sol", 2_000_000, func() int { return 200000 })
	if got.Window != 1_050_000 || got.RequestedWindow != 2_000_000 || got.Source != "configured_clamped" {
		t.Fatalf("manual budget exceeded real model ceiling: %+v", got)
	}
}

func TestCodexContextCacheAccountScopeAndCredentialRotation(t *testing.T) {
	cache := t.TempDir()
	credential := ResponsesOAuthCredential{AccessToken: "private-token-A", AccountID: "private-account-A"}
	var requests atomic.Int64
	c := NewCodexContextResolver(CodexContextResolverOptions{TokenSource: func(context.Context) (ResponsesOAuthCredential, error) { return credential, nil }, CacheDir: cache, AccountScope: credential.AccountID})
	c.http.Transport = codexContextTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.Header.Get("chatgpt-account-id") == "private-account-B" {
			return codexContextReply(r, 401, "bad token private-token-B"), nil
		}
		return codexContextReply(r, 200, `{"models":[{"slug":"wire-A","context_window":350000,"max_context_window":872000}]}`), nil
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(cache, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("cache file count=%d error=%v", len(files), err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), credential.AccountID) || strings.Contains(string(data), credential.AccessToken) || strings.Contains(files[0], credential.AccountID) {
		t.Fatal("account/secret escaped hashed cache")
	}
	credential.AccessToken = "rotated-token-A"
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("credential rotation incorrectly reused authorization freshness")
	}
	files, _ = filepath.Glob(filepath.Join(cache, "*.json"))
	if len(files) != 1 {
		t.Fatal("rotation changed account metadata identity")
	}
	cold := NewCodexContextResolver(CodexContextResolverOptions{CacheDir: cache, AccountScope: credential.AccountID})
	if got := cold.Resolve("wire-A", 0, func() int { return 200000 }); got.Window != 350000 || requests.Load() != 2 {
		t.Fatalf("offline cold read failed or made HTTP: %+v requests=%d", got, requests.Load())
	}
	credential = ResponsesOAuthCredential{AccessToken: "private-token-B", AccountID: "private-account-B"}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Resolve("wire-A", 0, func() int { return 200000 }); got.Window != 200000 || got.ProfileDefaultWindow != 0 {
		t.Fatalf("old account leaked after failed new-account refresh: %+v", got)
	}
	credential = ResponsesOAuthCredential{AccessToken: "rotated-token-A", AccountID: "private-account-A"}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Resolve("wire-A", 0, func() int { return 200000 }); got.Window != 350000 {
		t.Fatalf("returning account lost valid cache: %+v", got)
	}
}

func TestCodexContextInvalidResponsesPreserveLastValidSnapshot(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
	}{
		{"HTTP", "secret must not escape", 500},
		{"malformed", "<html>bad</html>", 200},
		{"negative", `{"models":[{"slug":"wire","context_window":-1}]}`, 200},
		{"duplicate", `{"models":[{"slug":"wire","context_window":1},{"slug":"wire","context_window":2}]}`, 200},
		{"reversed", `{"models":[{"slug":"wire","context_window":500000,"max_context_window":200000}]}`, 200},
		{"string-limit", `{"models":[{"slug":"wire","context_window":"500000"}]}`, 200},
		{"oversize", strings.Repeat(" ", codexContextBodyLimit+1), 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCodexContextResolver(CodexContextResolverOptions{TokenSource: func(context.Context) (ResponsesOAuthCredential, error) {
				return ResponsesOAuthCredential{AccessToken: "private-key", AccountID: "account"}, nil
			}})
			c.ttl = 0
			valid := true
			c.http.Transport = codexContextTransport(func(r *http.Request) (*http.Response, error) {
				if valid {
					return codexContextReply(r, 200, `{"models":[{"slug":"wire","context_window":350000}]}`), nil
				}
				return codexContextReply(r, tt.status, tt.body), nil
			})
			if err := c.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			valid = false
			if err := c.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := c.Resolve("wire", 0, func() int { return 200000 }); got.Window != 350000 {
				t.Fatalf("invalid discovery replaced validated metadata: %+v", got)
			}
		})
	}
}

func TestCodexContextRedirectsNeverForwardSubscriptionCredentials(t *testing.T) {
	var calls atomic.Int64
	c := NewCodexContextResolver(CodexContextResolverOptions{TokenSource: func(context.Context) (ResponsesOAuthCredential, error) {
		return ResponsesOAuthCredential{AccessToken: "private-key", AccountID: "private-account"}, nil
	}})
	c.http.CheckRedirect = nil // A caller cannot accidentally relax discovery.
	c.http.Transport = codexContextTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "chatgpt.com" {
			t.Errorf("redirect leaked auth to %s", r.URL)
		}
		response := codexContextReply(r, 302, "")
		response.Header.Set("Location", "https://unrelated.example/models")
		return response, nil
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("followed %d requests", calls.Load())
	}
}

func TestCodexContextConcurrentRefreshAndStatusRead(t *testing.T) {
	var calls atomic.Int64
	c := NewCodexContextResolver(CodexContextResolverOptions{TokenSource: func(context.Context) (ResponsesOAuthCredential, error) {
		return ResponsesOAuthCredential{AccessToken: "private-key", AccountID: "account"}, nil
	}})
	c.http.Transport = codexContextTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return codexContextReply(r, 200, `{"models":[{"slug":"wire","context_window":350000}]}`), nil
	})
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_ = c.Refresh(context.Background())
			_ = c.Resolve("wire", 0, func() int { return 200000 })
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("parallel callers duplicated fresh discovery %d times", calls.Load())
	}
}

func TestCodexContextBudgetUpdatesOnlyAtRequestBoundary(t *testing.T) {
	var calls, configured atomic.Int64
	configured.Store(64000)
	p := NewCodexResponses("gpt-6-astra", 4096, time.Second, 0, nil)
	p.ContextWindow = 32000
	p.ContextBudgetPolicy = llm.NewContextBudgetPolicy(llm.ContextBudgetPolicyOptions{LegacyWindow: 32000, InitialWindow: 64000, InitialSource: llm.ContextBudgetPerModel,
		Lookup: func(context.Context) (int, bool, error) { calls.Add(1); return int(configured.Load()), true, nil }})
	p.CodexContextResolver = NewCodexContextResolver(CodexContextResolverOptions{Catalog: fixtureCodexCatalog()})
	if got := p.ContextCapabilities(); got.Window != 64000 || calls.Load() != 0 {
		t.Fatalf("status lost policy or did lookup: %+v calls=%d", got, calls.Load())
	}
	configured.Store(1_000_000)
	if got := p.ContextCapabilities(); got.Window != 64000 || calls.Load() != 0 {
		t.Fatalf("status bypassed policy snapshot: %+v", got)
	}
	if err := p.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.ContextCapabilities(); got.Window != 1_000_000 || got.BudgetSource != llm.ContextBudgetPerModel || p.ContextWindow != 32000 || calls.Load() != 1 {
		t.Fatalf("same task provider did not bind next-request budget: %+v calls=%d", got, calls.Load())
	}
}

func TestCodexContextOutputBudgetStopsDirectGenerationWithoutPOST(t *testing.T) {
	capture := &responsesAuthCapture{}
	server := newResponsesAuthServer(t, capture)
	var authCalls atomic.Int64
	p := NewCodexResponses("gpt-output-fixture", 4096, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
		authCalls.Add(1)
		return ResponsesOAuthCredential{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
	})
	p.BaseURL = server.URL + "/backend-api/codex"
	p.CodexContextResolver = NewCodexContextResolver(CodexContextResolverOptions{Catalog: catalog.NewStaticClient(catalog.Catalog{
		"openai": {Models: map[string]catalog.Model{"gpt-output-fixture": {Limit: catalog.Limit{Context: 1_050_000, Output: 1024}}}},
	})})
	if _, err := p.Stream(context.Background(), provider.Request{}); !errors.Is(err, llm.ErrContextOutputBudget) {
		t.Fatalf("invalid Stream default output: %v", err)
	}
	if _, err := p.Complete(context.Background(), provider.Request{MaxTokens: 2048}); !errors.Is(err, llm.ErrContextOutputBudget) {
		t.Fatalf("invalid Complete explicit output: %v", err)
	}
	if len(capture.paths) != 0 || authCalls.Load() != 0 {
		t.Fatal("invalid output budget reached POST/auth callback")
	}
	if _, err := p.Complete(context.Background(), provider.Request{MaxTokens: 1024}); err != nil {
		t.Fatal(err)
	}
	stream, err := p.Stream(context.Background(), provider.Request{MaxTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if len(capture.paths) != 2 {
		t.Fatalf("valid requests did not execute: %d", len(capture.paths))
	}
	for _, body := range capture.bodies {
		if strings.Contains(body, "max_output_tokens") {
			t.Fatal("Codex budget validation added unsupported wire output field")
		}
	}
}

func TestCodexContextBindingPolicyErrorsCannotBecomeDiscoveryFallback(t *testing.T) {
	c := NewCodexContextResolver(CodexContextResolverOptions{TokenSource: func(context.Context) (ResponsesOAuthCredential, error) {
		return ResponsesOAuthCredential{}, fmt.Errorf("%w: private-account private-token", llm.ErrContextBudgetPolicy)
	}})
	err := c.Refresh(context.Background())
	if !errors.Is(err, llm.ErrContextBudgetPolicy) {
		t.Fatalf("account policy error became unavailable discovery: %v", err)
	}
	if strings.Contains(err.Error(), "private-account") || strings.Contains(err.Error(), "private-token") {
		t.Fatal("token-source binding error leaked private identity")
	}
}

func TestCodexContextWaitingRefreshRemainsCancellable(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := NewCodexContextResolver(CodexContextResolverOptions{TokenSource: func(context.Context) (ResponsesOAuthCredential, error) {
		return ResponsesOAuthCredential{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
	}})
	c.http.Transport = codexContextTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-release
		return codexContextReply(r, 200, `{"models":[{"slug":"wire","context_window":350000}]}`), nil
	})
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
		t.Fatal("cancelled waiter blocked behind another model listing")
	}
	if got := c.Resolve("wire", 0, func() int { return 200000 }); got.Window != 200000 {
		t.Fatalf("in-flight status read changed cached metadata: %+v", got)
	}
}
