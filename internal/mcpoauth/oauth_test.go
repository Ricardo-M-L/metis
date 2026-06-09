package mcpoauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

// mockAS stands up an OAuth authorization server + MCP protected-resource
// metadata + dynamic registration + a refresh token endpoint.
func mockAS(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{base}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"registration_endpoint":  base + "/register",
			"scopes_supported":       []string{"mcp.read", "mcp.write"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "dyn-client-123"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			http.Error(w, "expected refresh_token grant", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access-" + r.Form.Get("refresh_token"),
			"refresh_token": "rotated-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	return srv
}

func TestDiscover(t *testing.T) {
	srv := mockAS(t)
	p, err := Discover(context.Background(), srv.URL+"/mcp", []string{"http://127.0.0.1:7700/callback"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.AuthURL != srv.URL+"/authorize" || p.TokenURL != srv.URL+"/token" {
		t.Errorf("endpoints wrong: auth=%q token=%q", p.AuthURL, p.TokenURL)
	}
	if p.ClientID != "dyn-client-123" {
		t.Errorf("dynamic client_id not registered: %q", p.ClientID)
	}
	if !p.UsePKCE {
		t.Error("expected PKCE enabled")
	}
}

func TestTokenStore_RoundTrip(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s := NewTokenStore()
	if _, ok := s.Get("srv"); ok {
		t.Error("empty store should miss")
	}
	tok := &auth.Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.Put("srv", tok); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("srv")
	if !ok || got.AccessToken != "a" || got.RefreshToken != "r" {
		t.Errorf("round-trip failed: %+v ok=%v", got, ok)
	}
}

func TestEnsureToken_CachedValid(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s := NewTokenStore()
	_ = s.Put("srv", &auth.Token{AccessToken: "still-good", ExpiresAt: time.Now().Add(time.Hour)})
	// serverURL is bogus on purpose — a valid cached token must NOT hit
	// the network.
	got, err := s.EnsureToken(context.Background(), "srv", "http://127.0.0.1:1/never", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "still-good" {
		t.Errorf("expected cached token, got %q", got)
	}
}

func TestEnsureToken_RefreshesExpired(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := mockAS(t)
	s := NewTokenStore()
	// Expired but refreshable.
	_ = s.Put("srv", &auth.Token{
		AccessToken:  "old",
		RefreshToken: "my-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})
	got, err := s.EnsureToken(context.Background(), "srv", srv.URL+"/mcp", false)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if got != "fresh-access-my-refresh" {
		t.Errorf("expected refreshed token, got %q", got)
	}
	// The rotated refresh token is persisted.
	stored, _ := s.Get("srv")
	if stored.RefreshToken != "rotated-refresh" {
		t.Errorf("rotated refresh not persisted: %q", stored.RefreshToken)
	}
}
