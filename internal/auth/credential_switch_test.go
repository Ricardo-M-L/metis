package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestActivateAPIKeyRollsBackWhenOAuthRemovalWriteFails(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := PutOAuth("anthropic", OAuthCredential{AccessToken: "old-oauth"}); err != nil {
		t.Fatal(err)
	}
	if err := Set("unrelated", "keep-api"); err != nil {
		t.Fatal(err)
	}

	original := credentialSwitchWriteOAuthStore
	t.Cleanup(func() { credentialSwitchWriteOAuthStore = original })
	injected := errors.New("injected OAuth store failure")
	credentialSwitchWriteOAuthStore = func(string, *oauthStoreFile) error { return injected }

	err := ActivateAPIKey(" ANTHROPIC ", "new-api")
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("ActivateAPIKey error = %v, want injected failure", err)
	}
	apiKey, err := Get("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		t.Fatal("failed API-key switch left the new API credential installed")
	}
	oauth, err := GetOAuth("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if oauth == nil || oauth.AccessToken != "old-oauth" {
		t.Fatalf("OAuth credential after rollback = %+v, want old credential", oauth)
	}
	if unrelated, err := Get("unrelated"); err != nil || unrelated != "keep-api" {
		t.Fatalf("unrelated API credential after rollback = %q, %v", unrelated, err)
	}
}

func TestActivateAPIKeyRejectsBlankWithoutChangingExistingOAuth(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := PutOAuth("anthropic", OAuthCredential{AccessToken: "existing-oauth"}); err != nil {
		t.Fatal(err)
	}

	err := ActivateAPIKey("anthropic", " \t ")
	if err == nil || !strings.Contains(err.Error(), "key required") {
		t.Fatalf("ActivateAPIKey blank error = %v, want key-required error", err)
	}
	if key, getErr := Get("anthropic"); getErr != nil || key != "" {
		t.Fatalf("blank API key was stored: present=%v err=%v", key != "", getErr)
	}
	credential, getErr := GetOAuth("anthropic")
	if getErr != nil || credential == nil || credential.AccessToken != "existing-oauth" {
		t.Fatalf("existing OAuth changed after blank API key: %+v err=%v", credential, getErr)
	}
}

func TestActivateOAuthContextCancelWhileWaitingDoesNotPersist(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	oauthStoreProcessSem <- struct{}{}
	released := false
	t.Cleanup(func() {
		if !released {
			<-oauthStoreProcessSem
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ActivateOAuthContext(ctx, "openai-codex", OAuthCredential{AccessToken: "must-not-persist"})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-oauthStoreProcessSem
	released = true

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ActivateOAuthContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ActivateOAuthContext did not stop after cancellation")
	}
	credential, err := GetOAuth("openai-codex")
	if err != nil {
		t.Fatal(err)
	}
	if credential != nil {
		t.Fatal("cancelled lock wait persisted an OAuth credential")
	}
}

func TestActivateOAuthRollsBackWhenAPIKeyRemovalWriteFails(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := Set("anthropic", "old-api"); err != nil {
		t.Fatal(err)
	}
	if err := PutOAuth("unrelated", OAuthCredential{AccessToken: "keep-oauth"}); err != nil {
		t.Fatal(err)
	}

	original := credentialSwitchWriteAuthStore
	t.Cleanup(func() { credentialSwitchWriteAuthStore = original })
	injected := errors.New("injected API store failure")
	credentialSwitchWriteAuthStore = func(string, File) error { return injected }

	err := ActivateOAuth(" AnThRoPiC ", OAuthCredential{AccessToken: "new-oauth"})
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("ActivateOAuth error = %v, want injected failure", err)
	}
	apiKey, err := Get("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "old-api" {
		t.Fatalf("API credential after rollback = %q, want old credential", apiKey)
	}
	oauth, err := GetOAuth("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if oauth != nil {
		t.Fatal("failed OAuth switch left the new OAuth credential installed")
	}
	unrelated, err := GetOAuth("unrelated")
	if err != nil || unrelated == nil || unrelated.AccessToken != "keep-oauth" {
		t.Fatalf("unrelated OAuth credential after rollback = %+v, %v", unrelated, err)
	}
}

func TestRemoveProviderCredentialsRollsBackWhenOAuthWriteFails(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := Set("anthropic", "old-api"); err != nil {
		t.Fatal(err)
	}
	if err := PutOAuth("anthropic", OAuthCredential{AccessToken: "old-oauth"}); err != nil {
		t.Fatal(err)
	}

	original := credentialSwitchWriteOAuthStore
	t.Cleanup(func() { credentialSwitchWriteOAuthStore = original })
	injected := errors.New("injected OAuth removal failure")
	credentialSwitchWriteOAuthStore = func(string, *oauthStoreFile) error { return injected }

	err := RemoveProviderCredentials(" AnThRoPiC ")
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("RemoveProviderCredentials error = %v, want injected failure", err)
	}
	apiKey, getErr := Get("anthropic")
	if getErr != nil || apiKey != "old-api" {
		t.Fatalf("API credential after rollback = %q, %v; want old-api", apiKey, getErr)
	}
	oauth, getErr := GetOAuth("anthropic")
	if getErr != nil || oauth == nil || oauth.AccessToken != "old-oauth" {
		t.Fatalf("OAuth credential after failed removal = %+v, %v", oauth, getErr)
	}
}

func TestRemoveProviderCredentialsDeletesCanonicalAndDeprecatedAlias(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := Set("anthropic-claudeai", "legacy-api-key"); err != nil {
		t.Fatal(err)
	}
	if err := PutOAuth("anthropic", OAuthCredential{AccessToken: "canonical-oauth"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProviderCredentials("anthropic-claudeai"); err != nil {
		t.Fatal(err)
	}
	if key, err := Get("anthropic-claudeai"); err != nil || key != "" {
		t.Fatalf("deprecated API-key alias remains: present=%v err=%v", key != "", err)
	}
	if credential, err := GetOAuth("anthropic"); err != nil || credential != nil {
		t.Fatalf("canonical OAuth remains: present=%v err=%v", credential != nil, err)
	}
}

func TestCredentialActivationCollapsesDeprecatedAlias(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := Set("anthropic-claudeai", "legacy-api-key"); err != nil {
		t.Fatal(err)
	}
	if err := ActivateOAuth("anthropic", OAuthCredential{AccessToken: "new-oauth"}); err != nil {
		t.Fatal(err)
	}
	if key, err := Get("anthropic-claudeai"); err != nil || key != "" {
		t.Fatalf("legacy alias survived OAuth activation: present=%v err=%v", key != "", err)
	}
	if err := ActivateAPIKey("anthropic-claudeai", "new-api-key"); err != nil {
		t.Fatal(err)
	}
	if key, err := Get("anthropic"); err != nil || key != "new-api-key" {
		t.Fatalf("canonical API key = %q err=%v", key, err)
	}
	if credential, err := GetOAuth("anthropic"); err != nil || credential != nil {
		t.Fatalf("OAuth survived API-key activation: present=%v err=%v", credential != nil, err)
	}
}

func TestActivateAPIKeyDoesNotRollbackAfterCommittedOAuthWriteError(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := PutOAuth("anthropic", OAuthCredential{AccessToken: "old-oauth"}); err != nil {
		t.Fatal(err)
	}
	original := credentialSwitchWriteOAuthStore
	t.Cleanup(func() { credentialSwitchWriteOAuthStore = original })
	injected := errors.New("post-commit OAuth durability error")
	credentialSwitchWriteOAuthStore = func(path string, store *oauthStoreFile) error {
		if err := original(path, store); err != nil {
			return err
		}
		return injected
	}

	err := ActivateAPIKey("anthropic", "new-api")
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("ActivateAPIKey error = %v, want durability error", err)
	}
	if key, getErr := Get("anthropic"); getErr != nil || key != "new-api" {
		t.Fatalf("new API credential was rolled back: key=%q err=%v", key, getErr)
	}
	if credential, getErr := GetOAuth("anthropic"); getErr != nil || credential != nil {
		t.Fatalf("superseded OAuth remains after committed removal: %+v err=%v", credential, getErr)
	}
}

func TestActivateOAuthCompletesSwitchAfterCommittedFirstWriteError(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := Set("anthropic", "old-api"); err != nil {
		t.Fatal(err)
	}
	original := credentialSwitchWriteOAuthStore
	t.Cleanup(func() { credentialSwitchWriteOAuthStore = original })
	injected := errors.New("post-commit OAuth durability error")
	credentialSwitchWriteOAuthStore = func(path string, store *oauthStoreFile) error {
		if err := original(path, store); err != nil {
			return err
		}
		return injected
	}

	err := ActivateOAuth("anthropic", OAuthCredential{AccessToken: "new-oauth"})
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("ActivateOAuth error = %v, want durability error", err)
	}
	if key, getErr := Get("anthropic"); getErr != nil || key != "" {
		t.Fatalf("superseded API key remains: key=%q err=%v", key, getErr)
	}
	if credential, getErr := GetOAuth("anthropic"); getErr != nil || credential == nil || credential.AccessToken != "new-oauth" {
		t.Fatalf("new OAuth credential missing after committed write: %+v err=%v", credential, getErr)
	}
}

func TestRemoveProviderCredentialsKeepsRemovalAfterCommittedWriteError(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := Set("anthropic", "old-api"); err != nil {
		t.Fatal(err)
	}
	if err := PutOAuth("anthropic", OAuthCredential{AccessToken: "old-oauth"}); err != nil {
		t.Fatal(err)
	}
	original := credentialSwitchWriteOAuthStore
	t.Cleanup(func() { credentialSwitchWriteOAuthStore = original })
	injected := errors.New("post-commit OAuth durability error")
	credentialSwitchWriteOAuthStore = func(path string, store *oauthStoreFile) error {
		if err := original(path, store); err != nil {
			return err
		}
		return injected
	}

	err := RemoveProviderCredentials("anthropic")
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("RemoveProviderCredentials error = %v, want durability error", err)
	}
	if key, getErr := Get("anthropic"); getErr != nil || key != "" {
		t.Fatalf("API key restored after committed removal: key=%q err=%v", key, getErr)
	}
	if credential, getErr := GetOAuth("anthropic"); getErr != nil || credential != nil {
		t.Fatalf("OAuth restored after committed removal: %+v err=%v", credential, getErr)
	}
}
