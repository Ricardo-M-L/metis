package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthStoreHelperProcess(t *testing.T) {
	if os.Getenv("METIS_LLM_OAUTH_HELPER") != "1" {
		return
	}
	ready, start := os.Getenv("METIS_LLM_OAUTH_READY"), os.Getenv("METIS_LLM_OAUTH_START")
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(start); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper start timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	provider := os.Getenv("METIS_LLM_OAUTH_PROVIDER")
	switch os.Getenv("METIS_LLM_OAUTH_MODE") {
	case "put":
		if err := PutOAuth(provider, OAuthCredential{AccessToken: "token-" + provider}); err != nil {
			t.Fatal(err)
		}
	case "resolve":
		KnownProviders[provider] = OAuthProvider{Name: provider, ClientID: "client", TokenURL: os.Getenv("METIS_LLM_OAUTH_TOKEN_URL")}
		credential, err := ResolveOAuthCredential(context.Background(), provider)
		if err != nil {
			t.Fatal(err)
		}
		if credential == nil || credential.AccessToken != "cross-process-fresh" {
			t.Fatalf("unexpected resolved credential")
		}
	case "resolve-failure":
		KnownProviders[provider] = OAuthProvider{Name: provider, ClientID: "client", TokenURL: os.Getenv("METIS_LLM_OAUTH_TOKEN_URL")}
		if _, err := ResolveOAuthCredential(context.Background(), provider); err == nil {
			t.Fatal("refresh failure helper unexpectedly succeeded")
		}
	default:
		t.Fatal("unknown helper mode")
	}
}

func oauthStoreHelper(home, mode, provider, ready, start, tokenURL string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestOAuthStoreHelperProcess$")
	cmd.Env = append(os.Environ(),
		"METIS_HOME="+home,
		"METIS_LLM_OAUTH_HELPER=1",
		"METIS_LLM_OAUTH_MODE="+mode,
		"METIS_LLM_OAUTH_PROVIDER="+provider,
		"METIS_LLM_OAUTH_READY="+ready,
		"METIS_LLM_OAUTH_START="+start,
		"METIS_LLM_OAUTH_TOKEN_URL="+tokenURL,
	)
	return cmd
}

func waitForOAuthHelperFiles(t *testing.T, paths []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				ready = false
				break
			}
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("helpers did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOAuthStoreRoundTripAndPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	want := OAuthCredential{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().UTC().Truncate(time.Second).Add(time.Hour),
		TokenType: "Bearer", Scope: "openid profile", AccountID: "account-1",
	}
	if err := PutOAuth("openai-codex", want); err != nil {
		t.Fatal(err)
	}
	if OAuthPath() != filepath.Join(CredentialDirectory(), "llm-oauth.json") {
		t.Fatalf("OAuthPath = %q", OAuthPath())
	}
	info, err := os.Stat(OAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("credential store permissions = %#o, want 0600", got)
		}
	}
	got, err := GetOAuth("openai-codex")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	got.AccessToken = "mutated"
	again, _ := GetOAuth("openai-codex")
	if again.AccessToken != "access" {
		t.Fatal("GetOAuth returned mutable store-owned data")
	}
	providers, err := ListOAuth()
	if err != nil || len(providers) != 1 || providers[0] != "openai-codex" {
		t.Fatalf("ListOAuth = %v, %v", providers, err)
	}
	if ok, err := HasOAuth("openai-codex"); err != nil || !ok {
		t.Fatalf("HasOAuth = %v, %v", ok, err)
	}
	if err := RemoveOAuth("openai-codex"); err != nil {
		t.Fatal(err)
	}
	if got, err := GetOAuth("openai-codex"); err != nil || got != nil {
		t.Fatalf("removed credential = %+v, %v", got, err)
	}
}

func TestOAuthStoreSecuresDefaultHomeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows home discovery uses USERPROFILE; ACL coverage is in oauth_store_security_windows_test.go")
	}
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("METIS_HOME", "")
	metisHome := filepath.Join(userHome, ".metis")
	if err := os.Mkdir(metisHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := GetOAuth("anthropic"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(metisHome, CredentialDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("credential directory permissions = %#o, want 0700", got)
	}
}

func TestOAuthStoreRejectsOversizedCredentialWithoutCorruptingExistingStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := PutOAuth("existing", OAuthCredential{AccessToken: "preserved"}); err != nil {
		t.Fatal(err)
	}
	err := PutOAuth("oversized", OAuthCredential{AccessToken: strings.Repeat("x", (512<<10)+1)})
	if err == nil || !strings.Contains(err.Error(), "invalid credential") {
		t.Fatalf("oversized credential accepted: %v", err)
	}
	credential, readErr := GetOAuth("existing")
	if readErr != nil || credential == nil || credential.AccessToken != "preserved" {
		t.Fatalf("rejected write corrupted store: %+v, %v", credential, readErr)
	}
}

func TestOAuthStoreCanonicalizesDeprecatedAnthropicAlias(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	credential := OAuthCredential{AccessToken: "access"}
	if err := PutOAuth("anthropic-claudeai", credential); err != nil {
		t.Fatal(err)
	}
	if got, err := GetOAuth("anthropic"); err != nil || got == nil || got.AccessToken != "access" {
		t.Fatalf("canonical credential = %+v, %v", got, err)
	}
	providers, err := ListOAuth()
	if err != nil || len(providers) != 1 || providers[0] != "anthropic" {
		t.Fatalf("alias created duplicate provider: %v, %v", providers, err)
	}
}

func TestOAuthStoreCanonicalizesAliasesAlreadyPresentOnDisk(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(OAuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `{
  "format_version": 1,
  "credentials": {
    "anthropic-claudeai": {"access_token": "legacy-alias"},
    "anthropic": {"access_token": "canonical-wins"},
    "google": {"access_token": "legacy-google"}
  }
}`
	if err := os.WriteFile(OAuthPath(), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	anthropic, err := GetOAuth("anthropic")
	if err != nil || anthropic == nil || anthropic.AccessToken != "canonical-wins" {
		t.Fatalf("canonical Anthropic credential = %+v, %v", anthropic, err)
	}
	gemini, err := GetOAuth("gemini")
	if err != nil || gemini == nil || gemini.AccessToken != "legacy-google" {
		t.Fatalf("canonical Gemini credential = %+v, %v", gemini, err)
	}
	providers, err := ListOAuth()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(providers, ","), "anthropic,gemini"; got != want {
		t.Fatalf("providers = %q, want %q", got, want)
	}
}

func TestOAuthStoreRejectsSymlinkAndUnsupportedVersion(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("METIS_HOME", home)
		if err := os.MkdirAll(filepath.Dir(OAuthPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "outside")
		if err := os.WriteFile(target, []byte(`{"format_version":1,"credentials":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, OAuthPath()); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := GetOAuth("anthropic"); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink store accepted: %v", err)
		}
		content, _ := os.ReadFile(target)
		if !strings.Contains(string(content), `"format_version":1`) {
			t.Fatal("symlink target was modified")
		}
	})

	t.Run("symlink-lock", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("METIS_HOME", home)
		if err := os.MkdirAll(filepath.Dir(OAuthPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "outside-lock")
		if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(filepath.Dir(OAuthPath()), ".llm-oauth.lock")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := GetOAuth("anthropic"); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink lock accepted: %v", err)
		}
		content, _ := os.ReadFile(target)
		if string(content) != "unchanged" {
			t.Fatal("symlink lock target was modified")
		}
	})

	t.Run("future-version", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("METIS_HOME", home)
		if err := os.MkdirAll(filepath.Dir(OAuthPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(OAuthPath(), []byte(`{"format_version":999,"credentials":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ListOAuth(); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("future schema accepted: %v", err)
		}
	})
}

func TestOAuthStoreConcurrentWritesPreserveProviders(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const count = 24
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- PutOAuth(fmt.Sprintf("provider-%02d", i), OAuthCredential{AccessToken: fmt.Sprintf("token-%02d", i)})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	providers, err := ListOAuth()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != count {
		t.Fatalf("concurrent providers = %d, want %d: %v", len(providers), count, providers)
	}
}

func TestOAuthStoreConcurrentProcessesPreserveProviders(t *testing.T) {
	home := t.TempDir()
	start := filepath.Join(home, "start")
	const count = 10
	commands := make([]*exec.Cmd, count)
	outputs := make([]bytes.Buffer, count)
	ready := make([]string, count)
	for i := 0; i < count; i++ {
		provider := "process-" + strconv.Itoa(i)
		ready[i] = filepath.Join(home, provider+".ready")
		commands[i] = oauthStoreHelper(home, "put", provider, ready[i], start, "")
		commands[i].Stdout, commands[i].Stderr = &outputs[i], &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	waitForOAuthHelperFiles(t, ready)
	if err := os.WriteFile(start, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	t.Setenv("METIS_HOME", home)
	providers, err := ListOAuth()
	if err != nil || len(providers) != count {
		t.Fatalf("cross-process credentials = %v, %v", providers, err)
	}
}

func TestResolveOAuthCredentialRefreshSingleFlightAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	const provider = "process-refresh"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(75 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "cross-process-fresh", "expires_in": 3600})
	}))
	t.Cleanup(server.Close)
	if err := PutOAuth(provider, OAuthCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	start := filepath.Join(home, "start")
	const count = 6
	commands := make([]*exec.Cmd, count)
	outputs := make([]bytes.Buffer, count)
	ready := make([]string, count)
	for i := 0; i < count; i++ {
		ready[i] = filepath.Join(home, "refresh-"+strconv.Itoa(i)+".ready")
		commands[i] = oauthStoreHelper(home, "resolve", provider, ready[i], start, server.URL)
		commands[i].Stdout, commands[i].Stderr = &outputs[i], &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	waitForOAuthHelperFiles(t, ready)
	if err := os.WriteFile(start, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("refresh helper %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cross-process refresh requests = %d, want 1", got)
	}
}

func TestResolveOAuthCredentialRefreshFailureSingleFlightAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	const provider = "process-refresh-failure"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(75 * time.Millisecond)
		http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	if err := PutOAuth(provider, OAuthCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	start := filepath.Join(home, "start")
	const count = 6
	commands := make([]*exec.Cmd, count)
	outputs := make([]bytes.Buffer, count)
	ready := make([]string, count)
	for i := 0; i < count; i++ {
		ready[i] = filepath.Join(home, "failure-"+strconv.Itoa(i)+".ready")
		commands[i] = oauthStoreHelper(home, "resolve-failure", provider, ready[i], start, server.URL)
		commands[i].Stdout, commands[i].Stderr = &outputs[i], &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	waitForOAuthHelperFiles(t, ready)
	if err := os.WriteFile(start, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("failure helper %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cross-process failed refresh requests = %d, want 1", got)
	}
}

func TestResolveOAuthCredentialRefreshSingleFlightAndPreservesOldOnFailure(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const providerID = "single-flight-test"
	previous, existed := KnownProviders[providerID]
	defer func() {
		if existed {
			KnownProviders[providerID] = previous
		} else {
			delete(KnownProviders, providerID)
		}
	}()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.Form.Get("refresh_token") != "old-refresh" {
			t.Error("refresh token missing from request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "expires_in": 3600})
	}))
	t.Cleanup(server.Close)
	KnownProviders[providerID] = OAuthProvider{Name: providerID, TokenURL: server.URL, ClientID: "public-client"}
	old := OAuthCredential{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Minute)}
	if err := PutOAuth(providerID, old); err != nil {
		t.Fatal(err)
	}

	const callers = 20
	results := make(chan *OAuthCredential, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credential, err := ResolveOAuthCredential(context.Background(), providerID)
			results <- credential
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result == nil || result.AccessToken != "fresh-access" || result.RefreshToken != "old-refresh" {
			t.Fatalf("resolved credential = %+v", result)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}

	// A failed refresh must not erase or partially update the last credential.
	server.Close()
	stored := OAuthCredential{AccessToken: "still-old", RefreshToken: "still-refresh", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := PutOAuth(providerID, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveOAuthCredential(context.Background(), providerID); err == nil {
		t.Fatal("failed refresh unexpectedly succeeded")
	}
	after, err := GetOAuth(providerID)
	if err != nil || after == nil || after.AccessToken != stored.AccessToken || after.RefreshToken != stored.RefreshToken || !after.ExpiresAt.Equal(stored.ExpiresAt) {
		t.Fatalf("failed refresh mutated store: %+v, %v", after, err)
	}
}

func TestResolveOAuthCredentialKnownProviderRefreshInheritsRefreshToken(t *testing.T) {
	for _, test := range []struct {
		provider    string
		accessToken func(*testing.T) string
	}{
		{provider: "anthropic", accessToken: func(*testing.T) string { return "fresh-anthropic-access" }},
		{provider: openAICodexProviderID, accessToken: func(t *testing.T) string { return codexTestAccessToken(t, "account-refresh") }},
	} {
		t.Run(test.provider, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			accessToken := test.accessToken(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": accessToken,
					"expires_in":   3600,
				})
			}))
			t.Cleanup(server.Close)

			previous := KnownProviders[test.provider]
			KnownProviders[test.provider] = OAuthProvider{
				Name: test.provider, TokenURL: server.URL, ClientID: "public-client",
			}
			t.Cleanup(func() { KnownProviders[test.provider] = previous })

			const oldRefresh = "existing-refresh-token"
			if err := PutOAuth(test.provider, OAuthCredential{
				AccessToken: "expired-access", RefreshToken: oldRefresh,
				ExpiresAt: time.Now().Add(-time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			credential, err := ResolveOAuthCredential(context.Background(), test.provider)
			if err != nil {
				t.Fatal(err)
			}
			if credential == nil || credential.AccessToken != accessToken || credential.RefreshToken != oldRefresh {
				t.Fatalf("refreshed credential = %+v", credential)
			}
		})
	}
}

func TestResolveOAuthCredentialRefreshDoesNotResurrectRemovedCredential(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const providerID = "refresh-remove-cas"
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "refreshed", "expires_in": 3600})
	}))
	t.Cleanup(server.Close)
	previous, existed := KnownProviders[providerID]
	KnownProviders[providerID] = OAuthProvider{Name: providerID, TokenURL: server.URL, ClientID: "client"}
	t.Cleanup(func() {
		if existed {
			KnownProviders[providerID] = previous
		} else {
			delete(KnownProviders, providerID)
		}
	})
	if err := PutOAuth(providerID, OAuthCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		credential *OAuthCredential
		err        error
	}
	done := make(chan result, 1)
	go func() {
		credential, err := ResolveOAuthCredential(context.Background(), providerID)
		done <- result{credential: credential, err: err}
	}()
	<-entered
	if err := RemoveOAuth(providerID); err != nil {
		t.Fatal(err)
	}
	close(release)
	got := <-done
	if got.err != nil || got.credential != nil {
		t.Fatalf("resolve after concurrent removal = %+v, %v", got.credential, got.err)
	}
	stored, err := GetOAuth(providerID)
	if err != nil || stored != nil {
		t.Fatalf("removed credential was resurrected: %+v, %v", stored, err)
	}
}

func TestResolveOAuthCredentialRefreshDoesNotOverwriteNewLogin(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const providerID = "refresh-put-cas"
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "stale-refresh-result", "expires_in": 3600})
	}))
	t.Cleanup(server.Close)
	previous, existed := KnownProviders[providerID]
	KnownProviders[providerID] = OAuthProvider{Name: providerID, TokenURL: server.URL, ClientID: "client"}
	t.Cleanup(func() {
		if existed {
			KnownProviders[providerID] = previous
		} else {
			delete(KnownProviders, providerID)
		}
	})
	if err := PutOAuth(providerID, OAuthCredential{AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		credential *OAuthCredential
		err        error
	}
	done := make(chan result, 1)
	go func() {
		credential, err := ResolveOAuthCredential(context.Background(), providerID)
		done <- result{credential: credential, err: err}
	}()
	<-entered
	newLogin := OAuthCredential{AccessToken: "new-login", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}
	if err := PutOAuth(providerID, newLogin); err != nil {
		t.Fatal(err)
	}
	close(release)
	got := <-done
	if got.err != nil || got.credential == nil || got.credential.AccessToken != newLogin.AccessToken {
		t.Fatalf("resolve did not adopt concurrent login: %+v, %v", got.credential, got.err)
	}
	stored, err := GetOAuth(providerID)
	if err != nil || stored == nil || stored.AccessToken != newLogin.AccessToken || stored.RefreshToken != newLogin.RefreshToken {
		t.Fatalf("new login was overwritten: %+v, %v", stored, err)
	}
}

func TestResolveOAuthCredentialRefreshFailureSingleFlightInProcess(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const providerID = "failure-single-flight-test"
	previous, existed := KnownProviders[providerID]
	defer func() {
		if existed {
			KnownProviders[providerID] = previous
		} else {
			delete(KnownProviders, providerID)
		}
	}()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	KnownProviders[providerID] = OAuthProvider{Name: providerID, TokenURL: server.URL, ClientID: "client"}
	if err := PutOAuth(providerID, OAuthCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ResolveOAuthCredential(context.Background(), providerID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("failed refresh unexpectedly succeeded")
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("failed refresh requests = %d, want 1", got)
	}
	if credential, err := GetOAuth(providerID); err != nil || credential == nil || credential.AccessToken != "old" {
		t.Fatalf("refresh failure blocked or changed store read: %+v, %v", credential, err)
	}
	if err := PutOAuth("unrelated-provider", OAuthCredential{AccessToken: "unrelated"}); err != nil {
		t.Fatalf("provider-scoped failure blocked unrelated write: %v", err)
	}
	if credential, err := GetOAuth("unrelated-provider"); err != nil || credential == nil {
		t.Fatalf("provider-scoped failure blocked unrelated read: %+v, %v", credential, err)
	}
}

func TestResolveOAuthCredentialRejectsInvalidRefreshWithoutMutatingStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const provider = "invalid-refresh-token"
	previous, existed := KnownProviders[provider]
	defer func() {
		if existed {
			KnownProviders[provider] = previous
		} else {
			delete(KnownProviders, provider)
		}
	}()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "invalid\r\nheader", "refresh_token": "rotated", "expires_in": 3600,
		})
	}))
	t.Cleanup(server.Close)
	KnownProviders[provider] = OAuthProvider{Name: provider, TokenURL: server.URL, ClientID: "client"}
	original := OAuthCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := PutOAuth(provider, original); err != nil {
		t.Fatal(err)
	}
	const callers = 12
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ResolveOAuthCredential(context.Background(), provider)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("invalid refreshed credential accepted")
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("invalid refresh responses = %d, want 1", got)
	}
	after, err := GetOAuth(provider)
	if err != nil || after == nil || after.AccessToken != original.AccessToken || after.RefreshToken != original.RefreshToken || !after.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatalf("invalid refresh mutated old credential: %+v, %v", after, err)
	}
}

func TestResolveOAuthCredentialCancellationDoesNotSetFailureCooldown(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const provider = "canceled-refresh"
	previous, existed := KnownProviders[provider]
	defer func() {
		if existed {
			KnownProviders[provider] = previous
		} else {
			delete(KnownProviders, provider)
		}
	}()
	started := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		if attempt == 1 {
			close(started)
			select {
			case <-r.Context().Done():
			case <-releaseFirst:
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 3600})
	}))
	t.Cleanup(server.Close)
	KnownProviders[provider] = OAuthProvider{Name: provider, TokenURL: server.URL, ClientID: "client"}
	if err := PutOAuth(provider, OAuthCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ResolveOAuthCredential(ctx, provider)
		done <- err
	}()
	<-started
	cancel()
	// Some HTTP transports do not propagate the client cancellation to the
	// server request until the handler returns. Release the handler as well so
	// this test cannot deadlock on that transport detail.
	close(releaseFirst)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh = %v", err)
	}
	credential, err := ResolveOAuthCredential(context.Background(), provider)
	if err != nil || credential == nil || credential.AccessToken != "fresh" {
		t.Fatalf("post-cancel refresh was suppressed: %+v, %v", credential, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests after cancellation = %d, want 2", got)
	}
}

func TestOpenAICodexAccountIDExtraction(t *testing.T) {
	makeJWT := func(claims map[string]any) string {
		payload, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	}
	for _, claims := range []map[string]any{
		{openAIAuthClaim: map[string]any{"chatgpt_account_id": "acct-nested"}},
		{openAIAuthClaim + ".chatgpt_account_id": "acct-flat"},
	} {
		if got := openAICodexAccountID(makeJWT(claims)); !strings.HasPrefix(got, "acct-") {
			t.Fatalf("account id = %q for claims %v", got, claims)
		}
	}
	secret := makeJWT(map[string]any{"unrelated": "value"})
	_, err := credentialFromToken(openAICodexProviderID, &Token{AccessToken: secret, RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)})
	if err == nil {
		t.Fatal("missing account id accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("JWT leaked through account-id error")
	}
}

func TestCredentialFromTokenRequiresRefreshableSubscriptionCredential(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai-codex"} {
		_, err := credentialFromToken(provider, &Token{AccessToken: "access"})
		if err == nil || !strings.Contains(err.Error(), "incomplete refreshable credential") {
			t.Fatalf("%s incomplete credential accepted: %v", provider, err)
		}
	}
}

type failingRandomReader struct{ secret string }

func (r failingRandomReader) Read([]byte) (int, error) { return 0, errors.New(r.secret) }

func TestOAuthLoginHandlesRandomFailureWithoutLeakingCause(t *testing.T) {
	previous := oauthRandomReader
	oauthRandomReader = failingRandomReader{secret: "entropy-source-private-detail"}
	t.Cleanup(func() { oauthRandomReader = previous })
	_, err := OAuthLoginOptsContext(context.Background(), "anthropic", OAuthOptions{})
	if err == nil || !strings.Contains(err.Error(), "secure random generation failed") {
		t.Fatalf("random failure = %v", err)
	}
	if strings.Contains(err.Error(), "entropy-source-private-detail") {
		t.Fatal("random-source internals leaked")
	}
}

func TestLoginOAuthCredentialUsesRichStoreOnly(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const providerID = "test-login-rich"
	previous, existed := KnownProviders[providerID]
	defer func() {
		if existed {
			KnownProviders[providerID] = previous
		} else {
			delete(KnownProviders, providerID)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	KnownProviders[providerID] = OAuthProvider{
		Name: providerID, AuthURL: "https://issuer.example.test/authorize", TokenURL: server.URL,
		ManualRedirectURL: "https://issuer.example.test/manual", UsePKCE: true,
	}
	credential, err := LoginOAuthCredential(context.Background(), providerID, OAuthOptions{
		Manual: true, PasteCode: func(string) (string, error) { return "authorization-code", nil },
		AuthURLHandler: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "access" || credential.RefreshToken != "refresh" {
		t.Fatalf("rich credential = %+v", credential)
	}
	if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OAuth login wrote legacy auth.json: %v", err)
	}
}
