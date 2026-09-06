package mcpoauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func TestCanonicalOAuthURLRejectsNonLoopbackPlainHTTP(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/oauth",
		"http://192.0.2.10/oauth",
		"http://10.0.0.2/oauth",
	} {
		if got, err := canonicalOAuthURL(raw); err == nil || got != "" {
			t.Fatalf("canonicalOAuthURL(%q) = (%q, %v), want fail-closed", raw, got, err)
		}
	}
	for _, raw := range []string{
		"http://localhost:7700/callback",
		"http://127.0.0.1:7700/callback",
		"http://[::1]:7700/callback",
		"https://example.com/oauth",
	} {
		if _, err := canonicalOAuthURL(raw); err != nil {
			t.Fatalf("canonicalOAuthURL(%q): %v", raw, err)
		}
	}
}

func TestDiscoverExplicitIssuerFailureDoesNotFallBackToResourceOrigin(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "explicit issuer unavailable", http.StatusServiceUnavailable)
	}))
	defer issuer.Close()

	var origin string
	mux := http.NewServeMux()
	resource := httptest.NewServer(mux)
	defer resource.Close()
	origin = resource.URL
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource": resource.URL + "/mcp", "authorization_servers": []string{issuer.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": origin, "authorization_endpoint": origin + "/authorize", "token_endpoint": origin + "/token",
		})
	})

	if _, err := Discover(context.Background(), resource.URL+"/mcp", nil); err == nil ||
		!strings.Contains(err.Error(), "explicit") {
		t.Fatalf("explicit issuer failure fell back to resource origin: %v", err)
	}
}

func TestDiscoverRejectsAuthorizationServerIssuerMismatch(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource": base + "/mcp", "authorization_servers": []string{base},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 "https://different-issuer.example.test",
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	})

	if _, err := Discover(context.Background(), base+"/mcp", nil); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "issuer") {
		t.Fatalf("issuer mismatch was accepted: %v", err)
	}
}

func TestEnsureTokenConcurrentRefreshUsesSingleProcessLeaseAndRereads(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	var requests atomic.Int32
	firstRequest := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		once.Do(func() { close(firstRequest) })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "single-fresh-access", "refresh_token": "rotated", "expires_in": 3600,
		})
	})

	entry := boundEntry(server.URL+"/mcp", &auth.Token{
		AccessToken: "expired", RefreshToken: "refresh-once", ExpiresAt: time.Now().Add(-time.Hour),
	})
	entry.Issuer = server.URL
	entry.AuthURL = server.URL + "/authorize"
	entry.TokenURL = server.URL + "/token"
	if err := NewTokenStore().PutEntry("srv", entry); err != nil {
		t.Fatal(err)
	}

	const callers = 16
	start := make(chan struct{})
	results := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := NewTokenStore().EnsureToken(context.Background(), "srv", server.URL+"/mcp", false)
			results <- got
			errs <- err
		}()
	}
	close(start)
	select {
	case <-firstRequest:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh endpoint was not called")
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureToken: %v", err)
		}
	}
	for got := range results {
		if got != "single-fresh-access" {
			t.Fatalf("concurrent EnsureToken = %q", got)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("refresh endpoint requests = %d, want exactly one", got)
	}
}

func TestRefreshDoesNotOverwriteConcurrentNewLogin(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(refreshStarted)
		<-releaseRefresh
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "stale-refresh-access", "refresh_token": "stale-rotated-refresh", "expires_in": 3600,
		})
	}))
	defer server.Close()

	store := NewTokenStore()
	expired := boundEntry(server.URL+"/mcp", &auth.Token{
		AccessToken: "expired", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour),
	})
	expired.Issuer = server.URL
	expired.AuthURL = server.URL + "/authorize"
	expired.TokenURL = server.URL
	if err := store.PutEntry("srv", expired); err != nil {
		t.Fatal(err)
	}

	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		token, err := store.EnsureToken(context.Background(), "srv", server.URL+"/mcp", false)
		done <- result{token: token, err: err}
	}()
	select {
	case <-refreshStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh endpoint was not called")
	}

	newLogin := boundEntry(server.URL+"/mcp", &auth.Token{
		AccessToken: "new-login-access", RefreshToken: "new-login-refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	newLogin.Issuer = server.URL
	newLogin.AuthURL = server.URL + "/authorize"
	newLogin.TokenURL = server.URL
	if err := store.PutEntry("srv", newLogin); err != nil {
		t.Fatal(err)
	}
	close(releaseRefresh)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.token != "new-login-access" {
			t.Fatalf("EnsureToken returned %q, want concurrent login token", got.token)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EnsureToken did not complete")
	}
	stored, err := store.GetEntry("srv")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Token == nil || stored.Token.AccessToken != "new-login-access" || stored.Token.RefreshToken != "new-login-refresh" {
		t.Fatalf("stale refresh overwrote concurrent login: %+v", stored)
	}
}
