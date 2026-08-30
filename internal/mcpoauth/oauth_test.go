package mcpoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"resource": base + "/mcp", "authorization_servers": []string{base},
		})
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

func TestTokenStorePutForcesPrivatePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(home, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "mcp-oauth.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := NewTokenStore().Put("srv", &auth.Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// Windows permission bits do not model ACL confidentiality. Chmod is
		// still called by the implementation; strict ACL storage is a separate
		// platform concern.
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token store permissions = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing METIS_HOME permissions = %04o, want preserved 0755", got)
	}
	lockInfo, err := os.Stat(filepath.Join(home, tokenStoreLockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if got := lockInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("token store lock permissions = %04o, want 0600", got)
	}
}

func TestTokenStoreCreatesMissingHomePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits do not model Windows ACLs")
	}
	home := filepath.Join(t.TempDir(), "new-metis-home")
	t.Setenv("METIS_HOME", home)
	if err := NewTokenStore().Put("srv", &auth.Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("new METIS_HOME permissions = %04o, want 0700", got)
	}
}

func TestTokenStorePutRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	target := filepath.Join(home, "outside.json")
	if err := os.WriteFile(target, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "mcp-oauth.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := NewTokenStore().Put("srv", &auth.Token{AccessToken: "secret"})
	if err == nil {
		t.Fatal("Put through a symlink unexpectedly succeeded")
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "do-not-touch" {
		t.Fatalf("symlink target was modified: %q", got)
	}
}

func TestTokenStoreGetDoesNotFollowSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	target := filepath.Join(home, "outside.json")
	if err := os.WriteFile(target, []byte(`{"srv":{"access_token":"outside-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "mcp-oauth.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if tok, ok := NewTokenStore().Get("srv"); ok || tok != nil {
		t.Fatalf("Get followed a symlink: token=%+v ok=%v", tok, ok)
	}
}

func TestTokenStoreConcurrentPutPreservesDistinctServers(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const count = 64
	stores := make([]*TokenStore, count)
	for i := range stores {
		stores[i] = NewTokenStore()
	}

	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(i int, store *TokenStore) {
			defer wg.Done()
			<-start
			errs <- store.Put(
				fmt.Sprintf("server-%02d", i),
				&auth.Token{AccessToken: fmt.Sprintf("token-%02d", i)},
			)
		}(i, store)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put: %v", err)
		}
	}

	store := NewTokenStore()
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("server-%02d", i)
		tok, ok := store.Get(key)
		if !ok || tok.AccessToken != fmt.Sprintf("token-%02d", i) {
			t.Fatalf("entry %q lost after concurrent Put: token=%+v ok=%v", key, tok, ok)
		}
	}
}

func TestTokenStorePutIsAtomicForReaders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	store := NewTokenStore()
	if err := store.Put("initial", &auth.Token{AccessToken: "ready"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "mcp-oauth.json")

	stop := make(chan struct{})
	done := make(chan struct{})
	readerErr := make(chan error, 1)
	var stopOnce sync.Once
	stopReader := func() {
		stopOnce.Do(func() { close(stop) })
		<-done
	}
	defer stopReader()
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				readerErr <- err
				return
			}
			var entries map[string]*auth.Token
			if err := json.Unmarshal(b, &entries); err != nil {
				readerErr <- fmt.Errorf("reader observed partial token store: %w", err)
				return
			}
		}
	}()

	largeToken := strings.Repeat("x", 1<<20)
	for i := 0; i < 20; i++ {
		if err := store.Put(fmt.Sprintf("server-%02d", i), &auth.Token{AccessToken: largeToken}); err != nil {
			t.Fatal(err)
		}
	}
	stopReader()
	select {
	case err := <-readerErr:
		t.Fatal(err)
	default:
	}
}

func TestEnsureToken_CachedValid(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s := NewTokenStore()
	serverURL := "http://127.0.0.1:1/never"
	if err := s.PutEntry("srv", boundEntry(serverURL, &auth.Token{
		AccessToken: "still-good", ExpiresAt: time.Now().Add(time.Hour),
	})); err != nil {
		t.Fatal(err)
	}
	// serverURL is bogus on purpose — a valid cached token must NOT hit
	// the network.
	got, err := s.EnsureToken(context.Background(), "srv", serverURL, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "still-good" {
		t.Errorf("expected cached token, got %q", got)
	}
}

func TestLoginForcesFreshOAuthWithUnexpiredCredential(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := mockAS(t)
	store := NewTokenStore()
	serverURL := srv.URL + "/mcp"
	if err := store.PutEntry("srv", boundEntry(serverURL, &auth.Token{
		AccessToken: "still-valid-but-revoked", ExpiresAt: time.Now().Add(time.Hour),
	})); err != nil {
		t.Fatal(err)
	}
	prior := oauthLoginWithProvider
	loginCalls := 0
	oauthLoginWithProvider = func(_ context.Context, _ auth.OAuthProvider, opts auth.OAuthOptions) (*auth.Token, error) {
		loginCalls++
		if !opts.SkipPersist {
			t.Fatal("forced MCP OAuth login did not isolate credential persistence")
		}
		return &auth.Token{AccessToken: "fresh-login", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	t.Cleanup(func() { oauthLoginWithProvider = prior })

	got, err := store.Login(context.Background(), "srv", serverURL)
	if err != nil {
		t.Fatalf("force login: %v", err)
	}
	if got != "fresh-login" || loginCalls != 1 {
		t.Fatalf("force login = %q, browser mock calls=%d", got, loginCalls)
	}
	stored, ok := store.Get("srv")
	if !ok || stored.AccessToken != "fresh-login" {
		t.Fatalf("fresh credential not persisted: token=%+v present=%v", stored, ok)
	}
}

func TestEnsureToken_NonInteractiveMissingTokenPointsToSlashLogin(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := mockAS(t)

	_, err := NewTokenStore().EnsureToken(context.Background(), "srv", srv.URL+"/mcp", false)
	if err == nil {
		t.Fatal("EnsureToken without a cached credential unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "`/mcp login srv`") {
		t.Fatalf("EnsureToken error does not point to the explicit slash command: %q", err)
	}
	if strings.Contains(err.Error(), "metis mcp login") {
		t.Fatalf("EnsureToken error points to the nonexistent CLI command: %q", err)
	}
}

func TestEnsureToken_InteractivePersistsOnlyInMCPStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := mockAS(t)
	prior := oauthLoginWithProvider
	oauthLoginWithProvider = func(_ context.Context, _ auth.OAuthProvider, opts auth.OAuthOptions) (*auth.Token, error) {
		if !opts.SkipPersist {
			t.Fatal("MCP OAuth login did not request return-token-only behavior")
		}
		return &auth.Token{
			AccessToken: "mcp-access", RefreshToken: "mcp-refresh",
			ExpiresAt: time.Now().Add(time.Hour),
		}, nil
	}
	t.Cleanup(func() { oauthLoginWithProvider = prior })

	store := NewTokenStore()
	got, err := store.EnsureToken(context.Background(), "srv", srv.URL+"/mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "mcp-access" {
		t.Fatalf("EnsureToken = %q, want mcp-access", got)
	}
	stored, ok := store.Get("srv")
	if !ok || stored.AccessToken != "mcp-access" || stored.RefreshToken != "mcp-refresh" {
		t.Fatalf("MCP token store = %+v, present=%v", stored, ok)
	}
	entry, err := store.GetEntry("srv")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ClientID != "dyn-client-123" || entry.Issuer != srv.URL+"/" ||
		entry.ServerURL != srv.URL+"/mcp" || entry.ResourceURL != srv.URL+"/mcp" {
		t.Fatalf("dynamic registration/binding was not persisted: %+v", entry)
	}
	if _, err := os.Stat(auth.Path()); !os.IsNotExist(err) {
		t.Fatalf("MCP OAuth duplicated its token into auth.json: stat error = %v", err)
	}
}

func TestEnsureToken_RefreshesExpired(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := mockAS(t)
	s := NewTokenStore()
	// Expired but refreshable.
	entry := boundEntry(srv.URL+"/mcp", &auth.Token{
		AccessToken:  "old",
		RefreshToken: "my-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})
	entry.Issuer = srv.URL
	entry.AuthURL = srv.URL + "/authorize"
	entry.TokenURL = srv.URL + "/token"
	entry.ClientID = "dyn-client-123"
	if err := s.PutEntry("srv", entry); err != nil {
		t.Fatal(err)
	}
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
