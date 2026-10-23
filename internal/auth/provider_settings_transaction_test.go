package auth

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func providerSettingsTestLayout(t *testing.T) credentialLayout {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	layout, err := currentCredentialLayout()
	if err != nil {
		t.Fatal(err)
	}
	return layout
}

func providerSettingsTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func providerSettingsTestSnapshot(t *testing.T, layout credentialLayout) []providerSettingsCredentialFile {
	t.Helper()
	files, err := readProviderSettingsCredentialFiles(layout)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func providerSettingsTestSameFiles(t *testing.T, layout credentialLayout, before []providerSettingsCredentialFile) {
	t.Helper()
	if after := providerSettingsTestSnapshot(t, layout); !reflect.DeepEqual(after, before) {
		// Credential contents must never appear in failure messages.
		t.Fatal("transaction did not restore original credential-file presence and bytes")
	}
}

func TestProviderSettingsCredentialsReadOnlyDoesNotMigrateLegacy(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.legacyAuth, `{"google":{"type":"api","key":"legacy-test-api"}}`)
	providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{"anthropic-claudeai":{"access_token":"legacy-test-oauth"}}}`)
	before := providerSettingsTestSnapshot(t, layout)
	var retained *ProviderSettingsCredentials
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		retained = c
		if entry, ok := c.ManagedAPIKey("gemini"); !ok || entry.Key != "legacy-test-api" {
			t.Fatal("legacy alias API key unavailable in read-only view")
		}
		if !c.HasOAuth("anthropic") || len(c.Fingerprint()) != 64 {
			t.Fatal("read-only view is missing OAuth or opaque fingerprint")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	providerSettingsTestSameFiles(t, layout, before)
	if !retained.HasOAuth("anthropic") || retained.Fingerprint() == "" {
		t.Fatal("read-only result cannot be inspected after callback")
	}
	if err := retained.SetAPIKeyBound("new", "test", "openai_chat", "https://example.test"); err == nil {
		t.Fatal("mutation permitted after callback")
	}
	retained.ClearAPIKey("gemini")
	if _, ok := retained.ManagedAPIKey("gemini"); !ok {
		t.Fatal("ClearAPIKey mutated a completed view")
	}
}

func TestProviderSettingsCredentialsFingerprintCoversAllFilesAndPresence(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	fingerprint := func() string {
		t.Helper()
		var result string
		if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error { result = c.Fingerprint(); return nil }); err != nil {
			t.Fatal(err)
		}
		return result
	}
	previous := fingerprint()
	files := []struct{ path, content string }{
		{layout.auth, `{}`},
		{layout.oauth, `{"format_version":1,"credentials":{}}`},
		{layout.legacyAuth, `{"custom":{"type":"api","key":"test-api"}}`},
		{layout.legacyOAuth, `{"format_version":1,"credentials":{"custom":{"access_token":"test-oauth"}}}`},
	}
	seen := map[string]bool{previous: true}
	for _, file := range files {
		providerSettingsTestWrite(t, file.path, file.content)
		current := fingerprint()
		if seen[current] || current == previous || strings.Contains(current, "test-api") || strings.Contains(current, "test-oauth") {
			t.Fatal("fingerprint did not change opaquely for a credential-file update")
		}
		seen[current] = true
		previous = current
	}
}

func TestProviderSettingsCredentialsSetStagesAndCommitsExclusiveBoundAPIKey(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{"anthropic-claudeai":{"type":"api","key":"old-alias"},"other":{"type":"api","key":"keep-api"}}`)
	providerSettingsTestWrite(t, layout.oauth, `{"format_version":1,"credentials":{"anthropic":{"access_token":"old-oauth"},"other":{"access_token":"keep-oauth"}}}`)
	before := providerSettingsTestSnapshot(t, layout)
	var retained *ProviderSettingsCredentials
	var originalFingerprint string
	err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		retained = c
		originalFingerprint = c.Fingerprint()
		if err := c.SetAPIKeyBound(" ANTHROPIC-CLAUDEAI ", "new-test-api", "chat", "https://EXAMPLE.test:443/v1/"); err != nil {
			return err
		}
		providerSettingsTestSameFiles(t, layout, before)
		if c.HasOAuth("anthropic") || c.Fingerprint() != originalFingerprint {
			t.Fatal("staging altered baseline fingerprint or retained superseded OAuth")
		}
		entry, ok := c.ManagedAPIKey("anthropic")
		if !ok || entry.Key != "new-test-api" || entry.Endpoint == nil || entry.Endpoint.BaseURL != "https://example.test/v1" {
			t.Fatal("staged bound API key is incorrect")
		}
		entry.Endpoint.BaseURL = "https://must-not-mutate.test"
		again, _ := c.ManagedAPIKey("anthropic")
		if again.Endpoint.BaseURL != "https://example.test/v1" {
			t.Fatal("ManagedAPIKey leaked mutable endpoint pointer")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if retained.Fingerprint() == originalFingerprint {
		t.Fatal("successful commit returned the old CAS fingerprint")
	}
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if c.Fingerprint() != retained.Fingerprint() || c.HasOAuth("anthropic") || !c.HasOAuth("other") {
			t.Fatal("fresh transaction differs from committed credential state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	api, _, err := readAuthStoreFile(layout.auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := api["anthropic-claudeai"]; exists || api["other"].Key != "keep-api" {
		t.Fatal("alias not removed or unrelated API key lost")
	}
}

func TestProviderSettingsCredentialsCallbackErrorDoesNotWrite(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.legacyAuth, `{"custom":{"type":"api","key":"legacy"}}`)
	before := providerSettingsTestSnapshot(t, layout)
	injected := errors.New("callback failure")
	err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if err := c.SetAPIKeyBound("custom", "new-test-api", "chat", "https://example.test"); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatal("callback failure not returned")
	}
	providerSettingsTestSameFiles(t, layout, before)
}

func TestProviderSettingsCredentialsClearMigratesWithoutResurrectionAndKeepsOAuth(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{"anthropic":{"type":"api","key":"private"}}`)
	providerSettingsTestWrite(t, layout.legacyAuth, `{"anthropic-claudeai":{"type":"api","key":"legacy"},"other":{"type":"api","key":"keep-api"}}`)
	providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{"anthropic-claudeai":{"access_token":"keep-oauth"}}}`)
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		c.ClearAPIKey(" ANTHROPIC-CLAUDEAI ")
		if _, ok := c.ManagedAPIKey("anthropic"); ok || !c.HasOAuth("anthropic") {
			t.Fatal("ClearAPIKey did not clear aliases while preserving OAuth")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{layout.legacyAuth, layout.legacyOAuth} {
		if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy file remains after successful migration")
		}
	}
	// Public loaders perform their normal migration path; a cleared key must
	// remain absent through those existing runtime entry points as well.
	if key, err := Get("anthropic"); err != nil || key != "" {
		t.Fatal("cleared canonical API key resurrected")
	}
	if key, err := Get("anthropic-claudeai"); err != nil || key != "" {
		t.Fatal("cleared alias API key resurrected")
	}
	if key, err := Get("other"); err != nil || key != "keep-api" {
		t.Fatal("unrelated legacy API key lost")
	}
	if oauth, err := GetOAuth("anthropic"); err != nil || oauth == nil || oauth.AccessToken != "keep-oauth" {
		t.Fatal("ClearAPIKey lost existing OAuth")
	}
}

func TestProviderSettingsCredentialsRollbackEveryCommitFailure(t *testing.T) {
	for _, stage := range []string{"api-before-rename", "api-after-rename", "oauth-before-rename", "oauth-after-rename", "legacy-first-before-remove", "legacy-second-after-remove"} {
		t.Run(stage, func(t *testing.T) {
			layout := providerSettingsTestLayout(t)
			// Deliberate whitespace verifies exact-byte rollback, rather than
			// merely restoring semantically equivalent JSON.
			providerSettingsTestWrite(t, layout.auth, "{\n  \"anthropic\": {\"type\":\"api\",\"key\":\"private-api\"}\n}\n")
			providerSettingsTestWrite(t, layout.oauth, "{\n\"format_version\":1,\"credentials\":{\"anthropic\":{\"access_token\":\"private-oauth\"}}}\n")
			providerSettingsTestWrite(t, layout.legacyAuth, `{"other":{"type":"api","key":"legacy-api"}}`)
			providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{"other":{"access_token":"legacy-oauth"}}}`)
			before := providerSettingsTestSnapshot(t, layout)
			authWriter, oauthWriter, legacyRemove := providerSettingsWriteAuthStore, providerSettingsWriteOAuthStore, providerSettingsRemoveLegacy
			t.Cleanup(func() {
				providerSettingsWriteAuthStore, providerSettingsWriteOAuthStore, providerSettingsRemoveLegacy = authWriter, oauthWriter, legacyRemove
			})
			injected := errors.New("injected commit failure")
			providerSettingsWriteAuthStore = func(path string, desired File) error {
				if stage == "api-before-rename" {
					return injected
				}
				if err := authWriter(path, desired); err != nil {
					return err
				}
				if stage == "api-after-rename" {
					return committedCredentialStoreWriteError(injected)
				}
				return nil
			}
			providerSettingsWriteOAuthStore = func(path string, desired *oauthStoreFile) error {
				if stage == "oauth-before-rename" {
					return injected
				}
				if err := oauthWriter(path, desired); err != nil {
					return err
				}
				if stage == "oauth-after-rename" {
					// An unmarked late failure must also roll back both stores.
					return injected
				}
				return nil
			}
			providerSettingsRemoveLegacy = func(path string) error {
				if stage == "legacy-first-before-remove" && path == layout.legacyAuth {
					return injected
				}
				if err := legacyRemove(path); err != nil {
					return err
				}
				if stage == "legacy-second-after-remove" && path == layout.legacyOAuth {
					return injected
				}
				return nil
			}
			err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
				return c.SetAPIKeyBound("anthropic", "new-test-api", "chat", "https://example.test")
			})
			if !errors.Is(err, injected) {
				t.Fatalf("commit failure not returned: %v", err)
			}
			providerSettingsTestSameFiles(t, layout, before)
		})
	}
}

func TestProviderSettingsCredentialsRollbackRemovesNewlyCreatedFiles(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.legacyAuth, `{"custom":{"type":"api","key":"legacy"}}`)
	before := providerSettingsTestSnapshot(t, layout)
	original := providerSettingsWriteAuthStore
	t.Cleanup(func() { providerSettingsWriteAuthStore = original })
	providerSettingsWriteAuthStore = func(path string, desired File) error {
		if err := original(path, desired); err != nil {
			return err
		}
		return errors.New("late write error")
	}
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		return c.SetAPIKeyBound("custom", "new-test-api", "chat", "https://example.test")
	}); err == nil {
		t.Fatal("late failure unexpectedly succeeded")
	}
	providerSettingsTestSameFiles(t, layout, before)
}

func TestProviderSettingsCredentialsVerifiesCommitBeforePublishingFingerprint(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{"custom":{"type":"api","key":"old-api"}}`)
	providerSettingsTestWrite(t, layout.oauth, `{"format_version":1,"credentials":{"custom":{"access_token":"old-oauth"}}}`)
	before := providerSettingsTestSnapshot(t, layout)
	original := providerSettingsWriteOAuthStore
	t.Cleanup(func() { providerSettingsWriteOAuthStore = original })
	providerSettingsWriteOAuthStore = func(string, *oauthStoreFile) error { return nil }
	var retained *ProviderSettingsCredentials
	var baseline string
	err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		retained, baseline = c, c.Fingerprint()
		return c.SetAPIKeyBound("custom", "new-test-api", "chat", "https://example.test")
	})
	if err == nil || retained.Fingerprint() != baseline {
		t.Fatal("unverified credential state published a successful fingerprint")
	}
	providerSettingsTestSameFiles(t, layout, before)
}

func TestProviderSettingsCredentialsRollsBackLockReleaseError(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{"custom":{"type":"api","key":"private-api"}}`)
	providerSettingsTestWrite(t, layout.oauth, `{"format_version":1,"credentials":{"custom":{"access_token":"private-oauth"}}}`)
	providerSettingsTestWrite(t, layout.legacyAuth, `{"other":{"type":"api","key":"legacy-api"}}`)
	providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{"other":{"access_token":"legacy-oauth"}}}`)
	before := providerSettingsTestSnapshot(t, layout)
	original := providerSettingsWithStoreLock
	t.Cleanup(func() { providerSettingsWithStoreLock = original })
	injected := errors.New("injected lock release failure")
	providerSettingsWithStoreLock = func(path string, timeout time.Duration, fn func() error) error {
		if err := original(path, timeout, fn); err != nil {
			return err
		}
		return injected
	}
	var retained *ProviderSettingsCredentials
	var baseline string
	err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		retained, baseline = c, c.Fingerprint()
		return c.SetAPIKeyBound("custom", "new-test-api", "chat", "https://example.test")
	})
	if !errors.Is(err, injected) || retained.Fingerprint() != baseline {
		t.Fatal("late lock failure did not abort the committed credential view")
	}
	providerSettingsTestSameFiles(t, layout, before)
}

func TestProviderSettingsCredentialsValidationDoesNotModifyStagedState(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{"anthropic":{"type":"api","key":"keep-api"}}`)
	before := providerSettingsTestSnapshot(t, layout)
	for _, test := range []struct{ provider, key, transport, endpoint string }{
		{"", "test", "chat", "https://example.test"},
		{"anthropic", "  ", "chat", "https://example.test"},
		{"anthropic", "sk-ant-oat-test", "chat", "https://example.test"},
		{"custom", "test", "invalid", "https://example.test"},
		{"custom", "test", "chat", "https://user:pass@example.test"},
	} {
		if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
			if err := c.SetAPIKeyBound(test.provider, test.key, test.transport, test.endpoint); err == nil {
				t.Fatal("invalid API-key binding accepted")
			}
			entry, ok := c.ManagedAPIKey("anthropic")
			if !ok || entry.Key != "keep-api" {
				t.Fatal("invalid staging request modified existing credential")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		providerSettingsTestSameFiles(t, layout, before)
	}
}

func TestProviderSettingsCredentialsHoldsAuthProcessLockThroughoutCallback(t *testing.T) {
	_ = providerSettingsTestLayout(t)
	done := make(chan error, 1)
	err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		go func() { done <- Set("concurrent", "test-api") }()
		select {
		case <-done:
			t.Fatal("concurrent credential writer bypassed transaction lock")
		case <-time.After(30 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("transaction failed to release the credential process lock")
	}
}

func TestProviderSettingsCredentialsHoldsCrossProcessLockThroughoutCallback(t *testing.T) {
	if os.Getenv("METIS_TEST_PROVIDER_SETTINGS_AUTH_CHILD") == "1" {
		fmt.Println("ready")
		if err := Set("child-process", "test-api"); err != nil {
			t.Fatal(err)
		}
		return
	}
	_ = providerSettingsTestLayout(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderSettingsCredentialsHoldsCrossProcessLockThroughoutCallback$", "-test.count=1")
	command.Env = append(os.Environ(), "METIS_TEST_PROVIDER_SETTINGS_AUTH_CHILD=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	err = WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if err := command.Start(); err != nil {
			return err
		}
		go func() { done <- command.Wait() }()
		ready, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || ready != "ready\n" {
			return errors.New("credential child process did not reach lock acquisition")
		}
		select {
		case <-done:
			t.Fatal("separate-process writer bypassed credential file lock")
		case <-time.After(40 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("credential child process failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transaction failed to release credential file lock")
	}
}

func TestProviderSettingsCredentialsFormattingDoesNotExposeSecrets(t *testing.T) {
	_ = providerSettingsTestLayout(t)
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if err := c.SetAPIKeyBound("custom", "must-never-log-test-api", "chat", "https://example.test"); err != nil {
			return err
		}
		for _, formatted := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
			if strings.Contains(formatted, "must-never-log") || !strings.Contains(formatted, "fingerprint:") {
				t.Fatal("opaque credential transaction formatted raw secrets")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSettingsCredentialsRejectsCorruptOrSymlinkStoresWithoutWrites(t *testing.T) {
	for _, kind := range []string{"api-json", "oauth-json", "oauth-unknown-field", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			layout := providerSettingsTestLayout(t)
			path, content := layout.legacyAuth, `{"secret-test-key":`
			switch kind {
			case "oauth-json":
				path, content = layout.legacyOAuth, `{"format_version":999,"credentials":{}}`
			case "oauth-unknown-field":
				path, content = layout.legacyOAuth, `{"format_version":1,"credentials":{},"secret-test-key":"must-not-log"}`
			case "symlink":
				path = filepath.Join(filepath.Dir(layout.legacyAuth), "target.json")
				content = `{}`
			}
			providerSettingsTestWrite(t, path, content)
			if kind == "symlink" {
				if err := os.Symlink(path, layout.legacyAuth); err != nil {
					t.Skip("symlink unavailable")
				}
			}
			called := false
			err := WithProviderSettingsCredentials(func(*ProviderSettingsCredentials) error { called = true; return nil })
			if err == nil || called || strings.Contains(err.Error(), "secret-test-key") {
				t.Fatal("unsafe store accepted or credential payload leaked in error")
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(after, []byte(content)) {
				t.Fatal("rejected store modified")
			}
		})
	}
}

func TestProviderSettingsCredentialsSavedMembershipIncludesUnusableAndLegacyEntries(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{
		"anthropic":{"type":"api","key":"sk-ant-oat-disabled-test"},
		"empty-key":{"type":"api","key":""},
		"invalid-type":{"type":"unsupported","key":"test-only"},
		"invalid-binding":{"type":"api","key":"test-only","endpoint":{"provider":"other","transport":"invalid","base_url":"invalid"}},
		"search:tavily":{"type":"search","key":"search-test-only"}
	}`)
	providerSettingsTestWrite(t, layout.legacyAuth, `{
		"openai-codex":{"type":"api","key":"historical-disabled-token"},
		"google":{"type":"api","key":"legacy-test-api"}
	}`)
	providerSettingsTestWrite(t, layout.oauth, `{"format_version":1,"credentials":{
		"expired-oauth":{"access_token":"expired-test-token","expires_at":"2001-01-01T00:00:00Z"}
	}}`)
	providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{
		"anthropic-claudeai":{"access_token":"expired-legacy-test-token","expires_at":"2001-01-01T00:00:00Z"}
	}}`)
	before := providerSettingsTestSnapshot(t, layout)
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		for _, provider := range []string{"anthropic", " ANTHROPIC-CLAUDEAI ", "empty-key", "invalid-type", "invalid-binding", "openai-codex", "google", "gemini", "expired-oauth"} {
			if !c.HasSavedProvider(provider) {
				t.Fatalf("saved provider membership missing for %q", provider)
			}
		}
		for _, provider := range []string{"", "missing", "search:tavily"} {
			if c.HasSavedProvider(provider) {
				t.Fatalf("unsaved or search-only provider registered for %q", provider)
			}
		}
		for _, provider := range []string{"anthropic", "empty-key", "invalid-type", "openai-codex"} {
			if _, usable := c.ManagedAPIKey(provider); usable {
				t.Fatalf("unusable saved API entry reported as usable for %q", provider)
			}
		}
		if !c.HasOAuth("expired-oauth") || !c.HasOAuth("anthropic") {
			t.Fatal("expired OAuth missing from saved credential view")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	providerSettingsTestSameFiles(t, layout, before)
}

func TestProviderSettingsCredentialsClearProviderRemovesBothMethodsAndLegacyAliases(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{
		"anthropic":{"type":"api","key":"private-api"},
		"ANTHROPIC-CLAUDEAI":{"type":"api","key":"private-alias-api"},
		"other":{"type":"api","key":"keep-private-api"},
		"search:tavily":{"type":"search","key":"keep-search"}
	}`)
	providerSettingsTestWrite(t, layout.oauth, `{"format_version":1,"credentials":{
		"anthropic":{"access_token":"private-oauth"},
		"anthropic-claudeai":{"access_token":"private-alias-oauth"},
		"other":{"access_token":"keep-private-oauth"}
	}}`)
	providerSettingsTestWrite(t, layout.legacyAuth, `{
		"anthropic-claudeai":{"type":"api","key":"sk-ant-oat-disabled-legacy"},
		"legacy-other":{"type":"api","key":"keep-legacy-api"}
	}`)
	providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{
		"ANTHROPIC-CLAUDEAI":{"access_token":"expired-legacy-oauth","expires_at":"2001-01-01T00:00:00Z"},
		"legacy-other":{"access_token":"keep-legacy-oauth"}
	}}`)
	before := providerSettingsTestSnapshot(t, layout)
	var retained *ProviderSettingsCredentials
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		retained = c
		if !c.HasSavedProvider("anthropic") {
			t.Fatal("provider not registered before clear")
		}
		c.ClearProvider(" ANTHROPIC-CLAUDEAI ")
		for _, provider := range []string{"anthropic", "anthropic-claudeai"} {
			if c.HasSavedProvider(provider) || c.HasOAuth(provider) {
				t.Fatal("provider login method retained after staged clear")
			}
			if _, present := c.ManagedAPIKey(provider); present {
				t.Fatal("API alias retained after staged provider clear")
			}
		}
		providerSettingsTestSameFiles(t, layout, before)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if retained.HasSavedProvider("anthropic") {
		t.Fatal("deleted provider remains registered in committed result")
	}
	for _, legacy := range []string{layout.legacyAuth, layout.legacyOAuth} {
		if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy source remains after provider deletion")
		}
	}
	// Exercise existing runtime loaders, which could otherwise resurrect an
	// alias or expired OAuth record left in either legacy file.
	api, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for provider := range api {
		if canonicalOAuthProviderID(provider) == "anthropic" {
			t.Fatal("deleted API alias resurrected through public loader")
		}
	}
	if oauth, err := GetOAuth("anthropic"); err != nil || oauth != nil {
		t.Fatal("deleted OAuth resurrected through public loader")
	}
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if c.HasSavedProvider("anthropic") || !c.HasSavedProvider("other") || !c.HasSavedProvider("legacy-other") {
			t.Fatal("fresh saved membership does not match provider deletion")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if api["other"].Key != "keep-private-api" || api["legacy-other"].Key != "keep-legacy-api" || api["search:tavily"].Key != "keep-search" {
		t.Fatal("provider deletion lost unrelated API or search records")
	}
}

func TestProviderSettingsCredentialsClearProviderRollsBackBothMethodsAndLegacy(t *testing.T) {
	for _, stage := range []string{"oauth-before-write", "oauth-after-rename", "legacy-after-remove"} {
		t.Run(stage, func(t *testing.T) {
			layout := providerSettingsTestLayout(t)
			providerSettingsTestWrite(t, layout.auth, "{\n\"anthropic\":{\"type\":\"api\",\"key\":\"private-api\"}}\n")
			providerSettingsTestWrite(t, layout.oauth, "{\n\"format_version\":1,\"credentials\":{\"anthropic\":{\"access_token\":\"private-oauth\"}}}\n")
			providerSettingsTestWrite(t, layout.legacyAuth, `{"anthropic-claudeai":{"type":"api","key":"sk-ant-oat-disabled-legacy"},"other":{"type":"api","key":"keep-api"}}`)
			providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{"anthropic-claudeai":{"access_token":"expired-legacy-oauth","expires_at":"2001-01-01T00:00:00Z"}}}`)
			before := providerSettingsTestSnapshot(t, layout)
			oauthWriter, removeLegacy := providerSettingsWriteOAuthStore, providerSettingsRemoveLegacy
			t.Cleanup(func() { providerSettingsWriteOAuthStore, providerSettingsRemoveLegacy = oauthWriter, removeLegacy })
			injected := errors.New("injected provider removal failure")
			providerSettingsWriteOAuthStore = func(path string, desired *oauthStoreFile) error {
				if stage == "oauth-before-write" {
					return injected
				}
				if err := oauthWriter(path, desired); err != nil {
					return err
				}
				if stage == "oauth-after-rename" {
					return injected
				}
				return nil
			}
			providerSettingsRemoveLegacy = func(path string) error {
				if err := removeLegacy(path); err != nil {
					return err
				}
				if stage == "legacy-after-remove" && path == layout.legacyOAuth {
					return injected
				}
				return nil
			}
			var retained *ProviderSettingsCredentials
			err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
				retained = c
				c.ClearProvider("anthropic-claudeai")
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("provider deletion failure not returned: %v", err)
			}
			providerSettingsTestSameFiles(t, layout, before)
			if !retained.HasSavedProvider("anthropic") || !retained.HasOAuth("anthropic") {
				t.Fatal("failed provider deletion published uncommitted membership")
			}
			if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
				if !c.HasSavedProvider("anthropic") {
					t.Fatal("failed provider deletion lost saved registration")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProviderSettingsCredentialsClearProviderRemovesOnlyUnusableLegacyPresence(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.legacyAuth, `{"openai-codex":{"type":"api","key":"disabled-historical-test-token"}}`)
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if !c.HasSavedProvider("openai-codex") {
			t.Fatal("unsafe legacy entry unavailable for deletion")
		}
		if _, usable := c.ManagedAPIKey("openai-codex"); usable {
			t.Fatal("unsafe legacy token became usable")
		}
		c.ClearProvider("openai-codex")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if c.HasSavedProvider("openai-codex") {
			t.Fatal("unsafe legacy provider reappeared after deletion")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSettingsCredentialsClearAPIKeyKeepsSavedOAuthMembership(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{"anthropic":{"type":"api","key":"test-api"}}`)
	providerSettingsTestWrite(t, layout.oauth, `{"format_version":1,"credentials":{"anthropic":{"access_token":"expired-test-oauth","expires_at":"2001-01-01T00:00:00Z"}}}`)
	var retained *ProviderSettingsCredentials
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		retained = c
		c.ClearAPIKey("anthropic-claudeai")
		if !c.HasSavedProvider("anthropic") || !c.HasOAuth("anthropic") {
			t.Fatal("API-only clear removed OAuth registration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	retained.ClearProvider("anthropic")
	if !retained.HasSavedProvider("anthropic") || !retained.HasOAuth("anthropic") {
		t.Fatal("ClearProvider changed a completed view outside callback")
	}
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		if !c.HasSavedProvider("anthropic") || !c.HasOAuth("anthropic") {
			t.Fatal("API-only clear lost saved OAuth through a fresh transaction")
		}
		return c.SetAPIKeyBound("new-provider", "new-test-key", "chat", "https://example.test")
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSettingsCredentialsClearProviderPreservesOtherUnusableSavedRecords(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{
		"target":{"type":"api","key":"remove-test-api"},
		"anthropic":{"type":"api","key":"sk-ant-oat-disabled-private-test"},
		"empty-key":{"type":"api","key":""}
	}`)
	providerSettingsTestWrite(t, layout.legacyAuth, `{
		"openai-codex":{"type":"api","key":"disabled-legacy-test-token"},
		"invalid-type":{"type":"unsupported","key":"keep-invalid-type-test"}
	}`)
	privateBefore, _, err := readAuthStoreFile(layout.auth)
	if err != nil {
		t.Fatal(err)
	}
	legacyBefore, _, err := readAuthStoreFile(layout.legacyAuth)
	if err != nil {
		t.Fatal(err)
	}
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		c.ClearProvider("target")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Inspect the raw secure store, not auth.Load, whose separate safety
	// migration intentionally strips these historical API entries.
	after, _, err := readAuthStoreFile(layout.auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := after["target"]; exists {
		t.Fatal("explicitly deleted provider still present")
	}
	for _, source := range []File{privateBefore, legacyBefore} {
		for provider, entry := range source {
			if provider != "target" && !reflect.DeepEqual(after[provider], entry) {
				t.Fatalf("unrelated saved record changed for %q", provider)
			}
		}
	}
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		for _, provider := range []string{"anthropic", "openai-codex", "empty-key", "invalid-type"} {
			if !c.HasSavedProvider(provider) {
				t.Fatalf("unrelated unusable provider lost membership for %q", provider)
			}
			if _, usable := c.ManagedAPIKey(provider); usable {
				t.Fatalf("unusable saved credential became available for %q", provider)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSettingsCredentialsReadonlyProbesDoNotMigrateOrCleanSavedRecords(t *testing.T) {
	layout := providerSettingsTestLayout(t)
	providerSettingsTestWrite(t, layout.auth, `{
		"bound":{"type":"api","key":"must-not-return-bound-test-key","endpoint":{"provider":"bound","transport":"openai_chat","base_url":"https://EXAMPLE.test:443/v1/"}},
		"anthropic":{"type":"api","key":"sk-ant-oat-must-retain-test"},
		"empty-key":{"type":"api","key":""},
		"invalid-type":{"type":"unsupported","key":"must-retain-invalid-test"},
		"invalid-binding":{"type":"api","key":"must-retain-binding-test","endpoint":{"provider":"invalid-binding","transport":"chat","base_url":"not-an-endpoint"}},
		"unbound":{"type":"api","key":"must-not-return-unbound-test-key"},
		"search:tavily":{"type":"search","key":"must-not-return-search-test-key"}
	}`)
	providerSettingsTestWrite(t, layout.legacyAuth, `{
		"openai-codex":{"type":"api","key":"must-retain-historical-test-token"},
		"google":{"type":"api","key":"must-not-return-legacy-test-key","endpoint":{"provider":"gemini","transport":"chat","base_url":"https://example.test/v1"}}
	}`)
	providerSettingsTestWrite(t, layout.oauth, `{"format_version":1,"credentials":{
		"openai-codex":{"access_token":"expired-access-test","refresh_token":"refresh-test","account_id":"account-test","expires_at":"2001-01-01T00:00:00Z"}
	}}`)
	providerSettingsTestWrite(t, layout.legacyOAuth, `{"format_version":1,"credentials":{
		"anthropic-claudeai":{"access_token":"expired-anthropic-test","expires_at":"2001-01-01T00:00:00Z"}
	}}`)
	before := providerSettingsTestSnapshot(t, layout)
	if err := WithProviderSettingsCredentials(func(c *ProviderSettingsCredentials) error {
		for _, test := range []struct {
			provider, transport, endpoint string
			allowLegacy, ready, wantError bool
			wantSentinel                  error
		}{
			{provider: "bound", transport: "chat", endpoint: "https://example.test/v1", ready: true},
			{provider: "bound", transport: "responses", endpoint: "https://example.test/v1", wantError: true, wantSentinel: ErrEndpointBindingMismatch},
			{provider: "bound", transport: "chat", endpoint: "https://other.test/v1", allowLegacy: true, wantError: true, wantSentinel: ErrEndpointBindingMismatch},
			{provider: "invalid-binding", transport: "chat", endpoint: "https://example.test/v1", wantError: true, wantSentinel: ErrEndpointBindingMismatch},
			{provider: "unbound", transport: "chat", endpoint: "https://example.test/v1", wantError: true, wantSentinel: ErrEndpointBindingRequired},
			{provider: "unbound", transport: "chat", endpoint: "https://example.test/v1", allowLegacy: true, ready: true},
			{provider: "anthropic", transport: "chat", endpoint: "https://example.test/v1"},
			{provider: "openai-codex", transport: "chat", endpoint: "https://example.test/v1"},
			{provider: "empty-key", transport: "chat", endpoint: "https://example.test/v1"},
			{provider: "invalid-type", transport: "chat", endpoint: "https://example.test/v1", wantError: true},
			{provider: "search:tavily", transport: "chat", endpoint: "https://example.test/v1", wantError: true},
			{provider: "missing", transport: "chat", endpoint: "https://example.test/v1"},
			{provider: " GEMINI ", transport: "chat", endpoint: "https://example.test/v1", ready: true},
		} {
			ready, err := c.ProbeAPIKeyForEndpoint(test.provider, test.transport, test.endpoint, test.allowLegacy)
			if ready != test.ready || (err != nil) != test.wantError || (test.wantSentinel != nil && !errors.Is(err, test.wantSentinel)) {
				t.Fatalf("unexpected readonly endpoint probe result for %q: ready=%t, error=%v", test.provider, ready, err)
			}
			if err != nil && strings.Contains(err.Error(), "must-not-return") {
				t.Fatal("readonly endpoint probe exposed a secret")
			}
		}
		if !c.OAuthCredentialReady("openai-codex") || !c.OAuthCredentialReady("anthropic-claudeai") || c.OAuthCredentialReady("missing") {
			t.Fatal("readonly OAuth readiness differs from runtime requirements")
		}
		if !c.HasSavedProvider("anthropic") || !c.HasSavedProvider("openai-codex") {
			t.Fatal("readonly probe cleared historical saved membership")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	providerSettingsTestSameFiles(t, layout, before)
}

func TestProviderSettingsCredentialsOAuthReadinessRequiresCompleteCodexMetadata(t *testing.T) {
	complete := OAuthCredential{
		AccessToken: "test-access", RefreshToken: "test-refresh", AccountID: "test-account",
		ExpiresAt: time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, missing := range []string{"", "access", "refresh", "account", "expiry"} {
		t.Run(missing, func(t *testing.T) {
			credential := complete
			switch missing {
			case "access":
				credential.AccessToken = " "
			case "refresh":
				credential.RefreshToken = " "
			case "account":
				credential.AccountID = " "
			case "expiry":
				credential.ExpiresAt = time.Time{}
			}
			c := &ProviderSettingsCredentials{oauth: &oauthStoreFile{Credentials: map[string]OAuthCredential{openAICodexProviderID: credential}}}
			if c.OAuthCredentialReady(openAICodexProviderID) != (missing == "") {
				t.Fatal("Codex readiness did not enforce complete refresh/account metadata")
			}
		})
	}
	var absent *ProviderSettingsCredentials
	if absent.OAuthCredentialReady(openAICodexProviderID) {
		t.Fatal("nil credential view reported ready OAuth")
	}
}
