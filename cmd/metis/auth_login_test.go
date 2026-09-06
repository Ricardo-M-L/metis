package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	gort "runtime"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/tui"
)

func TestParseLoginArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantProvider string
		wantMethod   string
		wantManual   bool
		wantHelp     bool
		wantErr      string
	}{
		{name: "provider", args: []string{"Anthropic"}, wantProvider: "Anthropic"},
		{name: "oauth", args: []string{"anthropic", "--method", "OAUTH"}, wantProvider: "anthropic", wantMethod: tui.AuthMethodOAuth},
		{name: "manual implies oauth", args: []string{"openai-codex", "--manual"}, wantProvider: "openai-codex", wantMethod: tui.AuthMethodOAuth, wantManual: true},
		{name: "device code", args: []string{"--device-code"}, wantProvider: "openai-codex", wantMethod: tui.AuthMethodOAuth},
		{name: "openai device code", args: []string{"OpenAI", "--device-code"}, wantProvider: "openai-codex", wantMethod: tui.AuthMethodOAuth},
		{name: "openai browser", args: []string{"openai", "--method", "oauth"}, wantProvider: "openai", wantMethod: tui.AuthMethodOAuth},
		{name: "equals method", args: []string{"--method=api-key", "gemini"}, wantProvider: "gemini", wantMethod: tui.AuthMethodAPIKey},
		{name: "help", args: []string{"--help"}, wantHelp: true},
		{name: "bad method", args: []string{"--method", "token"}, wantErr: "api-key or oauth"},
		{name: "manual api key", args: []string{"--manual", "--method", "api-key"}, wantErr: "only valid"},
		{name: "device wrong provider", args: []string{"anthropic", "--device-code"}, wantErr: "only for openai-codex"},
		{name: "manual and device", args: []string{"openai-codex", "--manual", "--device-code"}, wantErr: "cannot be used together"},
		{name: "two providers", args: []string{"openai", "anthropic"}, wantErr: "only one provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, manual, help, err := parseLoginArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.Provider != tt.wantProvider || opts.Method != tt.wantMethod || manual != tt.wantManual || help != tt.wantHelp {
				t.Fatalf("parse = (%+v, manual=%v, help=%v), want provider=%q method=%q manual=%v help=%v", opts, manual, help, tt.wantProvider, tt.wantMethod, tt.wantManual, tt.wantHelp)
			}
		})
	}
}

func TestCanonicalLoginIsEarlySubcommand(t *testing.T) {
	idx, ok := findEarlySubcommand([]string{"--provider", "openai", "login", "anthropic"}, 16)
	if !ok || idx != 2 {
		t.Fatalf("findEarlySubcommand(login) = (%d, %v), want (2, true)", idx, ok)
	}
}

func TestLoginArgsWithLeadingGlobals(t *testing.T) {
	t.Run("provider becomes login provider", func(t *testing.T) {
		got, err := loginArgsWithLeadingGlobals([]string{"-p", "openai"}, []string{"--method", "api-key"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"openai", "--method", "api-key"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args = %q, want %q", got, want)
		}
	})

	t.Run("equals provider", func(t *testing.T) {
		got, err := loginArgsWithLeadingGlobals([]string{"--provider=anthropic"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "anthropic" {
			t.Fatalf("args = %q, want [anthropic]", got)
		}
	})

	t.Run("model cannot become manual", func(t *testing.T) {
		_, err := loginArgsWithLeadingGlobals([]string{"-m", "gpt-5"}, nil)
		if err == nil || !strings.Contains(err.Error(), "not applicable") {
			t.Fatalf("error = %v, want not-applicable error", err)
		}
	})
}

func TestLogoutArgsWithLeadingGlobals(t *testing.T) {
	got, err := logoutArgsWithLeadingGlobals([]string{"--provider", "openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "openai" {
		t.Fatalf("args = %q, want [openai]", got)
	}
}

func TestLogoutHelpIsNotTreatedAsProvider(t *testing.T) {
	if err := cmdAuthLogout([]string{"--help"}); err != nil {
		t.Fatalf("logout --help: %v", err)
	}
}

func TestLogoutGoogleAliasRemovesGeminiCredential(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.Set("gemini", "test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := cmdAuthLogout([]string{"google"}); err != nil {
		t.Fatal(err)
	}
	if key, err := auth.Get("gemini"); err != nil || key != "" {
		t.Fatalf("gemini credential remains after google alias logout: present=%v err=%v", key != "", err)
	}
}

func TestLegacyAuthLoginUsesSameNonTTYGuard(t *testing.T) {
	// go test does not attach stderr to a terminal. Both spellings should reach
	// the same canonical command after parsing rather than treating login as a
	// model prompt.
	canonicalErr := cmdAuthLogin(context.Background(), nil)
	legacyErr := cmdAuth(context.Background(), []string{"login"})
	bareLegacyErr := cmdAuth(context.Background(), nil)
	for name, err := range map[string]error{"canonical": canonicalErr, "legacy": legacyErr, "bare legacy": bareLegacyErr} {
		if err == nil || !strings.Contains(err.Error(), "requires an interactive terminal") {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}

func TestDeviceCodeLoginRunsWithoutTTY(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	original := openAICodexDeviceCodeLogin
	t.Cleanup(func() { openAICodexDeviceCodeLogin = original })
	called := false
	openAICodexDeviceCodeLogin = func(ctx context.Context, opts auth.OpenAICodexDeviceOptions) (*auth.OAuthCredential, error) {
		called = true
		if ctx == nil || opts.Notify == nil {
			t.Fatal("device-code login did not receive context/notification callback")
		}
		return &auth.OAuthCredential{
			AccessToken: "headless-access", RefreshToken: "headless-refresh",
			ExpiresAt: time.Now().Add(time.Hour), AccountID: "account-1",
		}, nil
	}
	if err := cmdAuthLogin(context.Background(), []string{"OpenAI", "--device-code"}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("headless device-code flow did not run")
	}
	credential, err := auth.GetOAuth("openai-codex")
	if err != nil || credential == nil || credential.AccessToken != "headless-access" {
		t.Fatalf("stored device credential = %+v, %v", credential, err)
	}
}

func TestAccountLoginFromUserHomeReachesAuthorization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	t.Chdir(home)
	if err := addTrustedDir(home); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveUserProviderDefault("anthropic"); err != nil {
		t.Fatal(err)
	}
	original := openAICodexDeviceCodeLogin
	t.Cleanup(func() { openAICodexDeviceCodeLogin = original })
	called := false
	openAICodexDeviceCodeLogin = func(context.Context, auth.OpenAICodexDeviceOptions) (*auth.OAuthCredential, error) {
		called = true
		return nil, context.Canceled
	}
	err := cmdAuthLogin(context.Background(), []string{"openai", "--device-code"})
	if !called || !errors.Is(err, context.Canceled) {
		t.Fatalf("login from home did not reach authorization: called=%v err=%v", called, err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Default != "anthropic" {
		t.Fatalf("cancelled account login changed default provider: %q", cfg.Provider.Default)
	}
}

func TestOAuthLoginPreflightsProjectDefaultOverrideBeforeCredentialWrite(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[provider]\ndefault = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	if err := addTrustedDir(project); err != nil {
		t.Fatal(err)
	}

	original := openAICodexDeviceCodeLogin
	t.Cleanup(func() { openAICodexDeviceCodeLogin = original })
	called := false
	openAICodexDeviceCodeLogin = func(context.Context, auth.OpenAICodexDeviceOptions) (*auth.OAuthCredential, error) {
		called = true
		return &auth.OAuthCredential{AccessToken: "must-not-persist"}, nil
	}
	err := cmdAuthLogin(context.Background(), []string{"openai-codex", "--device-code"})
	if err == nil || !strings.Contains(err.Error(), "controlled by") {
		t.Fatalf("login error = %v, want project override preflight", err)
	}
	if called {
		t.Fatal("OAuth acquisition started despite project default override")
	}
	if credential, getErr := auth.GetOAuth("openai-codex"); getErr != nil || credential != nil {
		t.Fatalf("OAuth credential persisted: %+v, %v", credential, getErr)
	}
	if _, statErr := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("user default mutated: %v", statErr)
	}
}

func TestAPIKeyLoginPreflightsProjectDefaultOverrideBeforeCredentialWrite(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[provider]\ndefault = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	if err := addTrustedDir(project); err != nil {
		t.Fatal(err)
	}

	err := completeAPIKeyLogin(tui.AuthResult{Provider: "anthropic", Method: tui.AuthMethodAPIKey, Key: "must-not-persist"})
	if err == nil || !strings.Contains(err.Error(), "controlled by") {
		t.Fatalf("login error = %v, want project override preflight", err)
	}
	if key, getErr := auth.Get("anthropic"); getErr != nil || key != "" {
		t.Fatalf("API credential persisted: present=%v, %v", key != "", getErr)
	}
	if _, statErr := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("user default mutated: %v", statErr)
	}
}

func TestAPIKeyLoginBindsCustomCredentialToEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())

	result := tui.AuthResult{
		Provider: "example-route",
		Method:   tui.AuthMethodAPIKey,
		Key:      "test-only-secret",
		Custom: &tui.CustomProviderResult{
			Transport: "openai_responses",
			BaseURL:   "https://api.example.test/v1",
			Model:     "model-one",
		},
	}
	if err := completeAPIKeyLogin(result); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if key, err := cfg.ResolveAPIKey(result.Provider); err != nil || key != result.Key {
		t.Fatalf("bound custom key = %q, %v", key, err)
	}
	cfg.Provider.Custom[result.Provider] = config.ProviderRaw{
		Transport: "openai_responses",
		BaseURL:   "https://other.example.test/v1",
		Model:     "model-one",
	}
	if key, err := cfg.ResolveAPIKey(result.Provider); key != "" || !errors.Is(err, auth.ErrEndpointBindingMismatch) {
		t.Fatalf("changed endpoint resolved key=%q err=%v", key, err)
	}
}

func TestLegacyGitHubOAuthRefusesUnusedCredential(t *testing.T) {
	err := cmdAuthOAuth(context.Background(), []string{"github"})
	if err == nil || !strings.Contains(err.Error(), "not connected to a METIS runtime") {
		t.Fatalf("github OAuth error = %v", err)
	}
}

func TestDeprecatedAnthropicOAuthAliasUsesRichLoginPath(t *testing.T) {
	err := cmdAuthOAuth(context.Background(), []string{"anthropic-claudeai"})
	// Unit tests have no TTY, so reaching the canonical command's TTY guard
	// proves the alias was normalized before wizard/provider validation.
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("alias routing error = %v, want canonical login TTY guard", err)
	}
}

func TestLegacyOpenAIOAuthUsesAccountLogin(t *testing.T) {
	err := cmdAuthOAuth(context.Background(), []string{"openai"})
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("OpenAI OAuth alias did not reach account login: %v", err)
	}
}

func TestCredentialMethodSwitchRemovesOnlySupersededStoredKind(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	putBoth := func() {
		t.Helper()
		if err := auth.Set("anthropic", "api-secret"); err != nil {
			t.Fatal(err)
		}
		if err := auth.PutOAuth("anthropic", auth.OAuthCredential{AccessToken: "oauth-secret"}); err != nil {
			t.Fatal(err)
		}
	}

	putBoth()
	if err := clearSupersededCredential("anthropic", tui.AuthMethodOAuth); err != nil {
		t.Fatal(err)
	}
	if key, err := auth.Get("anthropic"); err != nil || key != "" {
		t.Fatalf("API key remains after OAuth activation: present=%v err=%v", key != "", err)
	}
	if credential, err := auth.GetOAuth("anthropic"); err != nil || credential == nil {
		t.Fatalf("OAuth missing after OAuth activation: present=%v err=%v", credential != nil, err)
	}

	putBoth()
	if err := clearSupersededCredential("anthropic", tui.AuthMethodAPIKey); err != nil {
		t.Fatal(err)
	}
	if key, err := auth.Get("anthropic"); err != nil || key == "" {
		t.Fatalf("API key missing after API-key activation: present=%v err=%v", key != "", err)
	}
	if credential, err := auth.GetOAuth("anthropic"); err != nil || credential != nil {
		t.Fatalf("OAuth remains after API-key activation: present=%v err=%v", credential != nil, err)
	}
}

func TestOAuthSwitchRejectsUnremovableEnvironmentOverride(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "environment-secret")
	source, err := higherPriorityAPIKeySource("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if source != "ANTHROPIC_API_KEY" {
		t.Fatalf("override source = %q, want ANTHROPIC_API_KEY", source)
	}
}

func TestOAuthLoginSelectsProviderAfterCredentialSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	if err := config.SaveUserProviderDefault("anthropic"); err != nil {
		t.Fatal(err)
	}
	if err := selectLoggedInProvider("openai-codex"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Default != "openai-codex" {
		t.Fatalf("provider.default = %q, want openai-codex", cfg.Provider.Default)
	}
}

func TestConfiguredLoginProvidersLoadsCustomProfiles(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{
		ID: "sensenova", Transport: "openai_chat",
		BaseURL: "https://api.example.com/v1", Model: "deepseek-v4-pro",
	}); err != nil {
		t.Fatal(err)
	}
	providers, err := configuredLoginProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].ID != "sensenova" || providers[0].Model != "deepseek-v4-pro" {
		t.Fatalf("configured providers = %#v", providers)
	}
}

func TestConfiguredLoginProvidersExcludeUntrustedProjectProfiles(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	if err := config.SaveUserCustomProvider(config.CustomProviderSpec{
		ID: "user-route", Transport: "openai_chat",
		BaseURL: "https://user.example/v1", Model: "user-model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectConfig := `[provider.custom.project-route]
transport = "openai_chat"
base_url = "https://project.example/collect"
model = "project-model"
`
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	providers, err := configuredLoginProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].ID != "user-route" {
		t.Fatalf("untrusted login providers = %#v, want only user-route", providers)
	}

	if err := addTrustedDir(project); err != nil {
		t.Fatal(err)
	}
	providers, err = configuredLoginProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 || providers[0].ID != "project-route" || providers[1].ID != "user-route" {
		t.Fatalf("trusted login providers = %#v", providers)
	}
}

func TestConfiguredLoginProvidersExcludeCloudCredentialShapes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	configBody := `[provider.custom.http-route]
transport = "openai_chat"
base_url = "https://api.example.test/v1"
model = "model-one"

[provider.custom.vertex-route]
transport = "vertex_anthropic"
model = "claude-test"
service_account_file = "/tmp/service-account.json"
project = "test-project"
region = "us-east5"

[provider.custom.bedrock-route]
transport = "bedrock_anthropic"
model = "anthropic.claude-test"
region = "us-east-1"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	providers, err := configuredLoginProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].ID != "http-route" {
		t.Fatalf("login providers = %#v, want only http-route", providers)
	}
	if err := validateExplicitLoginProvider("vertex-route"); err == nil || !strings.Contains(err.Error(), "service-account") {
		t.Fatalf("Vertex login validation = %v", err)
	}
	if err := validateExplicitLoginProvider("bedrock-route"); err == nil || !strings.Contains(err.Error(), "AWS credentials") {
		t.Fatalf("Bedrock login validation = %v", err)
	}
}

func TestLoginKeepsConfigAndCredentialsOnFrozenMetisHome(t *testing.T) {
	if gort.GOOS == "windows" {
		t.Skip("symlink replacement requires additional Windows privileges")
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
	alias := filepath.Join(parent, "current")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", alias)

	// Activating the credential freezes the canonical state root at first.
	if err := auth.ActivateAPIKey("openai", "test-only-secret"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	if err := selectLoggedInProvider("openai"); err != nil {
		t.Fatalf("retargeted login should stay on the frozen root: %v", err)
	}

	firstConfig, err := os.ReadFile(filepath.Join(first, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstConfig), `default = "openai"`) {
		t.Fatalf("frozen-root config did not select openai: %s", firstConfig)
	}
	if _, err := os.Lstat(filepath.Join(second, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retargeted root unexpectedly received config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first, auth.CredentialDirectoryName, "auth.json")); err != nil {
		t.Fatalf("credential missing from frozen root: %v", err)
	}
}

func TestOAuthLoginRejectsAnthropicTokenWithCustomGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider.anthropic]\nbase_url = \"https://gateway.example/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := validateOAuthLoginTarget("anthropic")
	if err == nil || !strings.Contains(err.Error(), "non-Anthropic base_url") {
		t.Fatalf("custom gateway validation error = %v", err)
	}
}

func TestManualOAuthCodeReaderHonorsCancelledContextBeforeInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readManualOAuthCode(ctx, "https://example.invalid"); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("readManualOAuthCode error = %v, want context canceled", err)
	}
}

func TestAuthListLabelsTypesWithoutLeakingCredentials(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.Set("anthropic", "sk-ant-secret-prefix"); err != nil {
		t.Fatal(err)
	}
	if err := auth.PutOAuth("anthropic", auth.OAuthCredential{AccessToken: "oauth-secret-prefix"}); err != nil {
		t.Fatal(err)
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	t.Cleanup(func() { os.Stdout = original })
	if err := cmdAuthList(); err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = original
	body, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	output := string(body)
	if !strings.Contains(output, "anthropic (api-key, oauth)") {
		t.Fatalf("list output = %q", output)
	}
	for _, secret := range []string{"sk-ant", "oauth-secret"} {
		if strings.Contains(output, secret) {
			t.Fatalf("credential prefix %q leaked in output %q", secret, output)
		}
	}
}

func TestAuthLogoutRemovesAPIKeyAndOAuth(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.Set("anthropic", "api-secret"); err != nil {
		t.Fatal(err)
	}
	if err := auth.PutOAuth("anthropic", auth.OAuthCredential{AccessToken: "oauth-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdAuthLogout([]string{"anthropic"}); err != nil {
		t.Fatal(err)
	}
	if key, err := auth.Get("anthropic"); err != nil || key != "" {
		t.Fatalf("API key remains: present=%v err=%v", key != "", err)
	}
	if credential, err := auth.GetOAuth("anthropic"); err != nil || credential != nil {
		t.Fatalf("OAuth credential remains: present=%v err=%v", credential != nil, err)
	}
}

func TestAuthLogoutDeprecatedAnthropicAliasRemovesCanonicalCredential(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.PutOAuth("anthropic", auth.OAuthCredential{AccessToken: "oauth-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdAuthLogout([]string{"anthropic-claudeai"}); err != nil {
		t.Fatal(err)
	}
	if credential, err := auth.GetOAuth("anthropic"); err != nil || credential != nil {
		t.Fatalf("canonical OAuth remains after alias logout: present=%v err=%v", credential != nil, err)
	}
}
