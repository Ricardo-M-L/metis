package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestCredentialStoresUsePrivateCanonicalDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.Chmod(home, 0o777); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, CredentialDirectoryName)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Set("openai", "api-key"); err != nil {
		t.Fatal(err)
	}
	if err := PutOAuth("anthropic", OAuthCredential{AccessToken: "oauth-token"}); err != nil {
		t.Fatal(err)
	}
	canonicalDir := CredentialDirectory()
	if Path() != filepath.Join(canonicalDir, authFileName) || OAuthPath() != filepath.Join(canonicalDir, oauthFileName) {
		t.Fatalf("canonical paths = %q, %q", Path(), OAuthPath())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(canonicalDir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("pre-existing custom credential directory mode = %#o, want 0700", got)
		}
	}
}

func TestRemoveProviderCredentialsNormalizesProviderID(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := ActivateAPIKey("Anthropic", "test-secret"); err != nil {
		t.Fatalf("ActivateAPIKey: %v", err)
	}
	if err := RemoveProviderCredentials(" ANTHROPIC "); err != nil {
		t.Fatalf("RemoveProviderCredentials: %v", err)
	}
	credentials, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := credentials["anthropic"]; ok {
		t.Fatal("mixed-case logout left the normalized anthropic credential behind")
	}
}

func TestCredentialStoresMigrateAndMergeLegacyFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	legacyAuth := filepath.Join(home, authFileName)
	legacyOAuth := filepath.Join(home, oauthFileName)
	if err := os.WriteFile(legacyAuth, []byte(`{"legacy":{"type":"api","key":"legacy-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyOAuth, []byte(`{"format_version":1,"credentials":{"legacy":{"access_token":"legacy-oauth"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Get("legacy"); err != nil || got != "legacy-key" {
		t.Fatalf("migrated API key = %q, %v", got, err)
	}
	if got, err := GetOAuth("legacy"); err != nil || got == nil || got.AccessToken != "legacy-oauth" {
		t.Fatalf("migrated OAuth = %+v, %v", got, err)
	}
	for _, legacy := range []string{legacyAuth, legacyOAuth} {
		if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy credential copy remains at %q: %v", legacy, err)
		}
	}

	// If an older binary writes another root-level store later, canonical
	// entries win conflicts while previously unseen providers are preserved.
	if err := os.WriteFile(legacyAuth, []byte(`{"legacy":{"type":"api","key":"stale"},"added":{"type":"api","key":"new"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Get("legacy"); err != nil || got != "legacy-key" {
		t.Fatalf("legacy overwrite won over canonical: %q, %v", got, err)
	}
	if got, err := Get("added"); err != nil || got != "new" {
		t.Fatalf("legacy-only entry was not merged: %q, %v", got, err)
	}
}

func TestLegacyOAuthTokensAreNeverMigratedAsAPIKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	legacyAuth := filepath.Join(home, authFileName)
	body := `{
  "anthropic":{"type":"api","key":"sk-ant-oat01-legacy"},
  "anthropic-claudeai":{"type":"api","key":"sk-ant-oat01-alias"},
  "openai-codex":{"type":"api","key":"legacy-jwt"},
  "github":{"type":"api","key":"gho_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "openai":{"type":"api","key":"keep-platform-key"}
}`
	if err := os.WriteFile(legacyAuth, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := Get("openai")
	if err != nil || key != "keep-platform-key" {
		t.Fatalf("preserved API key = %q, %v", key, err)
	}
	for _, provider := range []string{"anthropic", "anthropic-claudeai", "openai-codex", "github"} {
		if key, err := Get(provider); err != nil || key != "" {
			t.Fatalf("misclassified %s token remained an API key: %q, %v", provider, key, err)
		}
	}
	if _, err := os.Stat(legacyAuth); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy root credential file remains: %v", err)
	}
	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secretFragment := range []string{"sk-ant-oat", "legacy-jwt", "gho_"} {
		if strings.Contains(string(raw), secretFragment) {
			t.Fatalf("canonical API-key store retained misclassified token fragment %q", secretFragment)
		}
	}
}

func TestCanonicalOAuthTokensAreRemovedFromAPIKeyStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.MkdirAll(CredentialDirectory(), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{
  "anthropic":{"type":"api","key":"sk-ant-oat01-canonical"},
  "anthropic-claudeai":{"type":"api","key":"sk-ant-oat01-canonical-alias"},
  "openai-codex":{"type":"api","key":"canonical-jwt"},
  "github":{"type":"api","key":"gho_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
  "openai":{"type":"api","key":"keep-canonical-platform-key"}
}`
	if err := os.WriteFile(Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := Get("openai")
	if err != nil || key != "keep-canonical-platform-key" {
		t.Fatalf("preserved canonical API key = %q, %v", key, err)
	}
	for _, provider := range []string{"anthropic", "anthropic-claudeai", "openai-codex", "github"} {
		if key, err := Get(provider); err != nil || key != "" {
			t.Fatalf("misclassified canonical %s token remained an API key: %q, %v", provider, key, err)
		}
	}
	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secretFragment := range []string{"sk-ant-oat", "canonical-jwt", "gho_"} {
		if strings.Contains(string(raw), secretFragment) {
			t.Fatalf("rewritten canonical API-key store retained token fragment %q", secretFragment)
		}
	}
}

func TestSetRejectsOAuthTokenShapesInAPIKeyStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for provider, token := range map[string]string{
		"anthropic":    "sk-ant-oat01-legacy",
		"openai-codex": "not-an-api-key",
		"github":       "gho_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if err := Set(provider, token); err == nil || !strings.Contains(err.Error(), "OAuth access token") {
			t.Fatalf("Set(%s) error = %v, want OAuth/API-key separation error", provider, err)
		}
	}
}

func TestConcurrentAPIKeyWritesPreserveProviders(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Set(fmt.Sprintf("provider-%02d", i), fmt.Sprintf("key-%02d", i))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	providers, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != count {
		t.Fatalf("concurrent API-key providers = %d, want %d: %v", len(providers), count, providers)
	}
}

func TestConcurrentCredentialMethodActivationLeavesExactlyOneMethod(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const provider = "anthropic"
	for round := 0; round < 40; round++ {
		if err := ActivateAPIKey(provider, "seed"); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- ActivateAPIKey(provider, "api")
		}()
		go func() {
			<-start
			errs <- ActivateOAuth(provider, OAuthCredential{AccessToken: "oauth"})
		}()
		close(start)
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		apiKey, err := Get(provider)
		if err != nil {
			t.Fatal(err)
		}
		oauth, err := GetOAuth(provider)
		if err != nil {
			t.Fatal(err)
		}
		if (apiKey != "") == (oauth != nil) {
			t.Fatalf("round %d left ambiguous methods: api=%t oauth=%t", round, apiKey != "", oauth != nil)
		}
	}
}

func TestCredentialOperationsDoNotFallBackToWorkingDirectory(t *testing.T) {
	t.Setenv("METIS_HOME", "")
	original := userHomeDir
	userHomeDir = func() (string, error) { return "", errors.New("unavailable") }
	t.Cleanup(func() { userHomeDir = original })
	if Path() != "" || OAuthPath() != "" || CredentialDirectory() != "" {
		t.Fatal("unresolved home returned a relative credential path")
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "resolve user home") {
		t.Fatalf("Load error = %v, want home-resolution error", err)
	}
	if _, err := GetOAuth("anthropic"); err == nil || !strings.Contains(err.Error(), "resolve user home") {
		t.Fatalf("GetOAuth error = %v, want home-resolution error", err)
	}
}

func TestCredentialHomePinsSymlinkTargetForProcessLifetime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	if err := os.MkdirAll(first, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "current")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("METIS_HOME", link)
	resolvedFirst, err := filepath.EvalSymlinks(first)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedFirst, CredentialDirectoryName, authFileName)
	if got := Path(); got != want {
		t.Fatalf("initial canonical credential path = %q, want %q", got, want)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if got := Path(); got != want {
		t.Fatalf("retargeted METIS_HOME changed frozen credential path to %q; want %q", got, want)
	}
	if err := Set("openai", "pinned"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("credential was not written to pinned target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, CredentialDirectoryName, authFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential followed retargeted symlink: %v", err)
	}
}

func TestCredentialWriteRejectsReplacedMetisHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory replacement semantics differ on Windows")
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", home)
	if err := Set("openai", "first"); err != nil {
		t.Fatal(err)
	}
	oldHome := filepath.Join(parent, "old-home")
	if err := os.Rename(home, oldHome); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, CredentialDirectoryName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Set("openai", "must-not-be-written"); err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("write after METIS_HOME replacement error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, CredentialDirectoryName, authFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential reached replacement METIS_HOME: %v", err)
	}
}

func TestCredentialWriteRejectsReplacedPrivateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory replacement semantics differ on Windows")
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", home)
	if err := Set("openai", "first"); err != nil {
		t.Fatal(err)
	}
	privateDir := CredentialDirectory()
	if err := os.Rename(privateDir, filepath.Join(home, "old-credentials")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Set("openai", "must-not-be-written"); err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("write after credential directory replacement error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateDir, authFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential reached replacement private directory: %v", err)
	}
}

func TestCredentialWriteRejectsSymlinkInsertedIntoMissingHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "missing-home")
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", home)
	if Path() == "" {
		t.Fatal("missing METIS_HOME did not resolve a prospective credential path")
	}
	if err := os.Symlink(target, home); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := Set("openai", "must-not-be-written"); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("write through inserted METIS_HOME symlink error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, CredentialDirectoryName, authFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential followed inserted METIS_HOME symlink: %v", err)
	}
}
