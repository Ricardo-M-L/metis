package mcpoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func TestDiscoverUsesPathQualifiedWellKnownURLs(t *testing.T) {
	var base string
	var protectedRequests, issuerRequests int
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base = server.URL

	resourceURL := base + "/tenant/api/mcp?region=us"
	issuerURL := base + "/issuer/tenant"
	mux.HandleFunc("/.well-known/oauth-protected-resource/tenant/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		protectedRequests++
		if got := r.URL.RawQuery; got != "region=us" {
			t.Errorf("protected-resource metadata query = %q, want region=us", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              resourceURL,
			"authorization_servers": []string{issuerURL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/issuer/tenant", func(w http.ResponseWriter, _ *http.Request) {
		issuerRequests++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 issuerURL,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	})

	discovered, err := discoverOAuth(context.Background(), resourceURL, nil)
	if err != nil {
		t.Fatalf("discoverOAuth: %v", err)
	}
	if protectedRequests != 1 || issuerRequests != 1 {
		t.Fatalf("metadata requests = protected:%d issuer:%d, want 1 each", protectedRequests, issuerRequests)
	}
	if got := discovered.ResourceURL; got != resourceURL {
		t.Fatalf("resource URL = %q, want %q", got, resourceURL)
	}
	if got := discovered.Provider.ResourceURL; got != resourceURL {
		t.Fatalf("provider resource URL = %q, want %q", got, resourceURL)
	}
}

func TestWellKnownURLPreservesEscapedPathAndResourceQuery(t *testing.T) {
	got, err := oauthWellKnownURL(
		"https://resource.example.test/tenant%2Falpha/?b=2&a=1",
		"oauth-protected-resource",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://resource.example.test/.well-known/oauth-protected-resource/tenant%2Falpha?a=1&b=2"
	if got != want {
		t.Fatalf("well-known URL = %q, want %q", got, want)
	}

	issuer, err := oauthWellKnownURL(
		"https://issuer.example.test/tenant/",
		"oauth-authorization-server",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://issuer.example.test/.well-known/oauth-authorization-server/tenant"; issuer != want {
		t.Fatalf("issuer well-known URL = %q, want %q", issuer, want)
	}
}

func TestCanonicalOAuthURLRejectsFragments(t *testing.T) {
	for _, raw := range []string{
		"https://resource.example.test/mcp#fragment",
		"https://resource.example.test/mcp#",
	} {
		if got, err := canonicalOAuthURL(raw); err == nil || got != "" {
			t.Fatalf("canonicalOAuthURL(%q) = %q, %v; want rejection", raw, got, err)
		}
	}
}

func TestDiscoverRejectsProtectedResourceIdentifierMismatch(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base = server.URL

	mux.HandleFunc("/.well-known/oauth-protected-resource/tenant/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              base + "/another-tenant/mcp",
			"authorization_servers": []string{base},
		})
	})

	_, err := discoverOAuth(context.Background(), base+"/tenant/mcp", nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "resource") ||
		!strings.Contains(strings.ToLower(err.Error()), "mismatch") {
		t.Fatalf("mismatched protected resource metadata accepted: %v", err)
	}
}

func TestDiscoverRejectsMissingProtectedResourceIdentifier(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_servers": []string{server.URL},
		})
	})

	_, err := discoverOAuth(context.Background(), server.URL+"/mcp", nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "resource") ||
		!strings.Contains(strings.ToLower(err.Error()), "omitted") {
		t.Fatalf("missing protected resource identifier accepted: %v", err)
	}
}

func TestDiscoverRejectsOversizedProtectedResourceMetadata(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base = server.URL

	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, base+"/mcp", base)
		_, _ = fmt.Fprint(w, strings.Repeat(" ", 2<<20))
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
		})
	})

	_, err := discoverOAuth(context.Background(), base+"/mcp", nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "large") {
		t.Fatalf("oversized protected-resource metadata accepted: %v", err)
	}
}

func TestRegisterClientRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"client_id":"client"}`)
		_, _ = fmt.Fprint(w, strings.Repeat(" ", 2<<20))
	}))
	t.Cleanup(server.Close)

	if _, err := registerClient(context.Background(), server.URL, nil); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "large") {
		t.Fatalf("oversized registration response accepted: %v", err)
	}
}

func TestStoredResourceIndicatorIsSentOnRefresh(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	resourceSeen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		resourceSeen <- r.Form.Get("resource")
		_, _ = fmt.Fprint(w, `{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	resourceURL := server.URL + "/tenant/mcp"
	entry := boundEntry(resourceURL, &auth.Token{
		AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour),
	})
	entry.Issuer = server.URL
	entry.AuthURL = server.URL + "/authorize"
	entry.TokenURL = server.URL
	if err := NewTokenStore().PutEntry("server", entry); err != nil {
		t.Fatal(err)
	}

	if _, err := NewTokenStore().EnsureToken(context.Background(), "server", resourceURL, false); err != nil {
		t.Fatal(err)
	}
	if got := <-resourceSeen; got != resourceURL {
		t.Fatalf("refresh resource = %q, want %q", got, resourceURL)
	}
}
