package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func boundEntry(serverURL string, token *auth.Token) *TokenEntry {
	return &TokenEntry{
		ServerURL:   serverURL,
		ResourceURL: serverURL,
		Issuer:      "https://issuer.example.test",
		ClientID:    "client-1",
		AuthURL:     "https://issuer.example.test/authorize",
		TokenURL:    "https://issuer.example.test/token",
		Scopes:      []string{"mcp.read"},
		Token:       token,
	}
}

func TestEnsureTokenNonInteractiveMissingDoesNoDiscovery(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected network request", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := NewTokenStore().EnsureToken(context.Background(), "missing", srv.URL+"/mcp", false)
	if !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("EnsureToken error = %v, want ErrCredentialMissing", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("missing non-interactive credential made %d network request(s)", got)
	}
}

func TestEnsureTokenNeverUsesTokenBoundToDifferentURL(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	oldURL := "https://old.example.test/mcp"
	if err := NewTokenStore().PutEntry("srv", boundEntry(oldURL, &auth.Token{
		AccessToken: "old-secret",
		ExpiresAt:   time.Now().Add(time.Hour),
	})); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "old token must not be sent", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	_, err := NewTokenStore().EnsureToken(context.Background(), "srv", srv.URL+"/mcp", false)
	if !errors.Is(err, ErrCredentialReauthRequired) {
		t.Fatalf("EnsureToken error = %v, want ErrCredentialReauthRequired", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("URL mismatch made %d network request(s)", got)
	}
}

func TestLegacyUnboundTokenLoadsButFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	legacy := map[string]*auth.Token{
		"srv": {AccessToken: "legacy-secret", ExpiresAt: time.Now().Add(time.Hour)},
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "mcp-oauth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Get remains source-compatible for callers that inspect the old store.
	got, ok := NewTokenStore().Get("srv")
	if !ok || got.AccessToken != "legacy-secret" {
		t.Fatalf("legacy Get = %+v, %v", got, ok)
	}
	_, err = NewTokenStore().EnsureToken(
		context.Background(), "srv", "https://server.example.test/mcp", false,
	)
	if !errors.Is(err, ErrCredentialReauthRequired) {
		t.Fatalf("legacy EnsureToken error = %v, want ErrCredentialReauthRequired", err)
	}
}

func TestRefreshReusesPersistedRegistrationWithoutDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	var requests atomic.Int32
	var clientID atomic.Value
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		clientID.Store(r.Form.Get("client_id"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1000)
		http.Error(w, "discovery/registration must not run", http.StatusInternalServerError)
	})

	entry := boundEntry(srv.URL+"/mcp", &auth.Token{
		AccessToken:  "expired",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	entry.Issuer = srv.URL
	entry.AuthURL = srv.URL + "/authorize"
	entry.TokenURL = srv.URL + "/token"
	entry.ClientID = "persisted-client"
	if err := NewTokenStore().PutEntry("srv", entry); err != nil {
		t.Fatal(err)
	}

	got, err := NewTokenStore().EnsureToken(context.Background(), "srv", srv.URL+"/mcp", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fresh-access" {
		t.Fatalf("EnsureToken = %q", got)
	}
	if gotRequests := requests.Load(); gotRequests != 1 {
		t.Fatalf("refresh made unexpected discovery/registration requests: weighted count=%d", gotRequests)
	}
	if gotClient, _ := clientID.Load().(string); gotClient != "persisted-client" {
		t.Fatalf("refresh client_id = %q, want persisted-client", gotClient)
	}
	stored, err := NewTokenStore().GetEntry("srv")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClientID != "persisted-client" || stored.Issuer != srv.URL+"/" {
		t.Fatalf("registration metadata was not preserved: %+v", stored)
	}
}

func TestRefreshPropagatesPersistenceFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	storePath := filepath.Join(home, "mcp-oauth.json")
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		// Sabotage the store only after EnsureToken has loaded the refresh token.
		if err := os.Rename(storePath, storePath+".previous"); err != nil {
			t.Errorf("rename token store: %v", err)
		}
		if err := os.Mkdir(storePath, 0o700); err != nil {
			t.Errorf("replace token store with directory: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access",
			"expires_in":   3600,
		})
	})

	entry := boundEntry(srv.URL+"/mcp", &auth.Token{
		AccessToken: "expired", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Hour),
	})
	entry.Issuer = srv.URL
	entry.TokenURL = srv.URL + "/token"
	if err := NewTokenStore().PutEntry("srv", entry); err != nil {
		t.Fatal(err)
	}

	_, err := NewTokenStore().EnsureToken(context.Background(), "srv", srv.URL+"/mcp", false)
	if err == nil {
		t.Fatal("EnsureToken ignored refresh persistence failure")
	}
	if errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("persistence failure was misclassified as missing: %v", err)
	}
	if !strings.Contains(err.Error(), "persist") && !strings.Contains(err.Error(), "token store") {
		t.Fatalf("persistence failure is not diagnostic: %v", err)
	}
}
