package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

func putRuntimeOAuth(t *testing.T, providerID, token, accountID string) {
	t.Helper()
	if err := auth.PutOAuth(providerID, auth.OAuthCredential{
		AccessToken:  token,
		RefreshToken: "refresh-" + providerID,
		AccountID:    accountID,
		ExpiresAt:    time.Now().Add(2 * time.Hour),
		TokenType:    "Bearer",
	}); err != nil {
		t.Fatalf("PutOAuth(%s): %v", providerID, err)
	}
}

func anthropicOAuthConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Provider.Anthropic.BaseURL = "https://api.anthropic.com"
	cfg.Provider.Anthropic.Model = "claude-test"
	cfg.Provider.Anthropic.MaxTokens = 1024
	cfg.Provider.Anthropic.ContextWindow = 200000
	cfg.Provider.Anthropic.TimeoutSecs = 5
	return cfg
}

func codexOAuthConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Provider.OpenAICodex.Model = "gpt-5.5"
	cfg.Provider.OpenAICodex.MaxTokens = 16000
	cfg.Provider.OpenAICodex.ContextWindow = 200000
	cfg.Provider.OpenAICodex.TimeoutSecs = 5
	return cfg
}

func TestBuildProviderAnthropicAPIKeyWinsOverOAuth(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	putRuntimeOAuth(t, "anthropic", "oauth-secret", "")
	cfg := anthropicOAuthConfig()
	cfg.Provider.Anthropic.APIKey = "api-secret"

	built, err := BuildProvider(cfg, "anthropic", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	p, ok := built.Provider.(*anthropic.Anthropic)
	if !ok {
		t.Fatalf("provider type = %T", built.Provider)
	}
	if p.APIKey != "api-secret" || p.OAuthTokenSource != nil {
		t.Fatalf("API-key precedence not preserved: APIKey=%q OAuthSource=%v", p.APIKey, p.OAuthTokenSource != nil)
	}
}

func TestBuildProviderAnthropicOAuthOnlyUsesDynamicResolver(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	putRuntimeOAuth(t, "anthropic", "oauth-first", "")
	cfg := anthropicOAuthConfig()

	built, err := BuildProvider(cfg, "anthropic", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	p, ok := built.Provider.(*anthropic.Anthropic)
	if !ok || p.OAuthTokenSource == nil || p.APIKey != "" {
		t.Fatalf("OAuth provider = %#v (%T)", p, built.Provider)
	}
	got, err := p.OAuthTokenSource(context.Background())
	if err != nil || got != "oauth-first" {
		t.Fatalf("first resolve = %q, %v", got, err)
	}
	putRuntimeOAuth(t, "anthropic", "oauth-refreshed", "")
	got, err = p.OAuthTokenSource(context.Background())
	if err != nil || got != "oauth-refreshed" {
		t.Fatalf("dynamic resolve after store update = %q, %v", got, err)
	}
}

func TestBuildProviderAnthropicCredentialStoreFailureDoesNotFallBackToOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	putRuntimeOAuth(t, "anthropic", "oauth-must-not-be-used", "")
	credentialDir := filepath.Join(home, ".credentials")
	if err := os.WriteFile(filepath.Join(credentialDir, "auth.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := BuildProvider(anthropicOAuthConfig(), "anthropic", "")
	if err == nil || !strings.Contains(err.Error(), "parse credential store") {
		t.Fatalf("BuildProvider error = %v, want API-key store failure", err)
	}
	if ProviderHasCredentials(anthropicOAuthConfig(), "anthropic") {
		t.Fatal("credential preflight hid a corrupt API-key store behind OAuth")
	}
}

func TestBuildProviderAnthropicOAuthRefusesCustomOrigin(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	putRuntimeOAuth(t, "anthropic", "oauth-secret", "")
	cfg := anthropicOAuthConfig()
	cfg.Provider.Anthropic.BaseURL = "https://gateway.example.com"
	_, err := BuildProvider(cfg, "anthropic", "")
	if err == nil || !strings.Contains(err.Error(), "refusing to send Anthropic OAuth") {
		t.Fatalf("custom-origin error = %v", err)
	}
}

func TestBuildProviderAnthropicOAuthRequiresHTTPS(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	putRuntimeOAuth(t, "anthropic", "oauth-secret", "")
	cfg := anthropicOAuthConfig()
	cfg.Provider.Anthropic.BaseURL = "http://api.anthropic.com"
	_, err := BuildProvider(cfg, "anthropic", "")
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("plaintext-origin error = %v", err)
	}
}

func TestBuildProviderOpenAICodex(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	putRuntimeOAuth(t, "openai-codex", "codex-token", "account-123")
	cfg := codexOAuthConfig()

	built, err := BuildProvider(cfg, "openai-codex", "gpt-override")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	p, ok := built.Provider.(*openai.Responses)
	if !ok {
		t.Fatalf("provider type = %T", built.Provider)
	}
	if built.Model != "gpt-override" || p.Model != "gpt-override" {
		t.Fatalf("model override: build=%q provider=%q", built.Model, p.Model)
	}
	if p.Name() != "openai-codex" || p.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("Codex identity/base: name=%q base=%q", p.Name(), p.BaseURL)
	}
	if p.ContextWindow != 200000 || p.OAuthTokenSource == nil {
		t.Fatalf("Codex config not applied: context=%d source=%v", p.ContextWindow, p.OAuthTokenSource != nil)
	}
	credential, err := p.OAuthTokenSource(context.Background())
	if err != nil {
		t.Fatalf("resolve credential: %v", err)
	}
	if credential.AccessToken != "codex-token" || credential.AccountID != "account-123" {
		t.Fatalf("resolved credential = %#v", credential)
	}
}

func TestOpenAICodexCredentialPreflightAndKnownProvider(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := codexOAuthConfig()
	if ProviderHasCredentials(cfg, "openai-codex") {
		t.Fatal("missing Codex login reported as configured")
	}
	_, err := BuildProvider(cfg, "openai-codex", "")
	if err == nil || !strings.Contains(err.Error(), "metis login openai-codex") {
		t.Fatalf("missing login error = %v", err)
	}
	putRuntimeOAuth(t, "openai-codex", "codex-token", "account-123")
	if !ProviderHasCredentials(cfg, "openai-codex") {
		t.Fatal("stored Codex OAuth login was not detected")
	}
	if !IsKnownProvider(cfg, "openai-codex") || !IsKnownProvider(nil, "openai-codex") {
		t.Fatal("openai-codex must be a built-in known provider")
	}
}

func TestOpenAICodexExpiredUnrefreshableCredentialIsNotReady(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{
		AccessToken: "expired-access", AccountID: "account-123", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := codexOAuthConfig()
	if ProviderHasCredentials(cfg, "openai-codex") {
		t.Fatal("expired unrefreshable credential reported ready")
	}
	if _, err := BuildProviderWithoutPreconnect(cfg, "openai-codex", ""); err == nil || !strings.Contains(err.Error(), "metis login openai-codex") {
		t.Fatalf("BuildProviderWithoutPreconnect error = %v, want re-login remediation", err)
	}
}
