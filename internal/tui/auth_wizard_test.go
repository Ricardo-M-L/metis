package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

func driveAuthWizard(t *testing.T, m authModel, msg tea.Msg) authModel {
	t.Helper()
	updated, _ := m.Update(msg)
	next, ok := updated.(authModel)
	if !ok {
		t.Fatalf("auth wizard Update returned %T, want authModel", updated)
	}
	return next
}

func TestPersistAuthWizardResultRejectsProjectOverrideBeforeSavingKey(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	if err := os.Mkdir(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectConfig := `[provider.custom.sensenova]
transport = "openai_chat"
base_url = "https://attacker.invalid/v1"
model = "different-model"
`
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := auth.Set("sensenova", "sk-existing-must-remain"); err != nil {
		t.Fatal(err)
	}

	err := persistAuthWizardResult(AuthResult{
		Provider: "sensenova",
		Key:      "sk-must-not-be-saved",
		Custom: &CustomProviderResult{
			Transport: "openai_chat",
			BaseURL:   "https://token.sensenova.cn/v1",
			Model:     "sensenova-6.8-flash-lite",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "different endpoint") {
		t.Fatalf("project override should be rejected, got %v", err)
	}
	key, getErr := auth.Get("sensenova")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if key != "sk-existing-must-remain" {
		t.Fatal("existing credential was changed despite a conflicting project override")
	}
	if _, statErr := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("user config was written before project override rejection: %v", statErr)
	}
}

func TestPersistAuthWizardResultRejectsBuiltInEndpointOverrideBeforeSavingKey(t *testing.T) {
	tests := []struct {
		provider string
		table    string
	}{
		{provider: "anthropic", table: "provider.anthropic"},
		{provider: "openai", table: "provider.openai"},
		{provider: "gemini", table: "provider.gemini"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Chdir(project)
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("GEMINI_API_KEY", "")
			if err := os.Mkdir(filepath.Join(project, ".metis"), 0o700); err != nil {
				t.Fatal(err)
			}
			projectConfig := "[" + tt.table + "]\nbase_url = \"https://attacker.invalid/v1\"\n"
			if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(projectConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := auth.Set(tt.provider, "sk-existing-must-remain"); err != nil {
				t.Fatal(err)
			}

			err := persistAuthWizardResult(AuthResult{Provider: tt.provider, Key: "sk-must-not-be-saved"})
			if err == nil || !strings.Contains(err.Error(), "non-official base_url") {
				t.Fatalf("built-in endpoint override should be rejected, got %v", err)
			}
			key, getErr := auth.Get(tt.provider)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if key != "sk-existing-must-remain" {
				t.Fatal("existing credential was changed despite a conflicting built-in endpoint override")
			}
			if _, statErr := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(statErr) {
				t.Fatalf("user config was written before endpoint override rejection: %v", statErr)
			}
		})
	}
}

func TestPersistAuthWizardResultRejectsHigherPriorityCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	t.Setenv("SENSENOVA_API_KEY", "sk-env-must-not-move")
	configBody := `[provider]
default = "sensenova"

[provider.custom.sensenova]
transport = "openai_chat"
base_url = "https://token.sensenova.cn/v1"
model = "old-model"
api_key_env = "SENSENOVA_API_KEY"
`
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	err := persistAuthWizardResult(AuthResult{
		Provider: "sensenova",
		Key:      "sk-wizard-must-not-be-saved",
		Custom: &CustomProviderResult{
			Transport: "openai_chat",
			BaseURL:   "https://token.sensenova.cn/v1",
			Model:     "new-model",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "active api_key_env") {
		t.Fatalf("active higher-priority credential should be rejected, got %v", err)
	}
	if key, getErr := auth.Get("sensenova"); getErr != nil || key != "" {
		t.Fatalf("wizard credential was unexpectedly stored: key_present=%v err=%v", key != "", getErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != configBody {
		t.Fatal("config changed despite higher-priority credential rejection")
	}
}

func TestBuiltInProviderHigherPriorityCredentialIncludesGoogleAPIKey(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "legacy-gemini-key")
	if !builtInProviderHasHigherPriorityCredential(&config.Config{}, "gemini") {
		t.Fatal("GOOGLE_API_KEY must block a wizard-managed Gemini key from becoming inert")
	}
}

func TestPersistAuthWizardResultMigratesInlineAPIKeyToManagedStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Chdir(t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[provider.openai]
base_url = "https://api.openai.com/v1"
api_key = "legacy-inline-secret"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistAuthWizardResult(AuthResult{
		Provider: "openai", Method: AuthMethodAPIKey, Key: "managed-secret",
	}); err != nil {
		t.Fatalf("migrate inline API key: %v", err)
	}
	if key, err := auth.Get("openai"); err != nil || key != "managed-secret" {
		t.Fatalf("managed key = %q, %v", key, err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if key, err := cfg.ResolveAPIKey("openai"); err != nil || key != "managed-secret" {
		t.Fatalf("effective key did not move to managed store: %q, %v", key, err)
	}
}

func TestLoginProviderListExcludesCloudCredentialShapes(t *testing.T) {
	providers := loginProviders([]ConfiguredLoginProvider{
		{ID: "http-route", Transport: "openai_responses"},
		{ID: "vertex-route", Transport: "vertex_anthropic"},
		{ID: "bedrock-route", Transport: "bedrock_anthropic"},
	})
	if _, ok := findAuthProvider(providers, "http-route"); !ok {
		t.Fatal("API-key custom provider missing from login picker")
	}
	for _, id := range []string{"vertex-route", "bedrock-route"} {
		if _, ok := findAuthProvider(providers, id); ok {
			t.Fatalf("cloud-auth provider %q must not appear as a single-key login", id)
		}
	}
}

func TestPersistAuthWizardResultMakesAPIKeyTheOnlyCredentialMethod(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Chdir(t.TempDir())
	if err := auth.PutOAuth("anthropic", auth.OAuthCredential{AccessToken: "old-oauth"}); err != nil {
		t.Fatal(err)
	}
	if err := persistAuthWizardResult(AuthResult{Provider: "anthropic", Key: "new-api-key"}); err != nil {
		t.Fatal(err)
	}
	if key, err := auth.Get("anthropic"); err != nil || key != "new-api-key" {
		t.Fatalf("stored API key = %q err=%v", key, err)
	}
	if credential, err := auth.GetOAuth("anthropic"); err != nil || credential != nil {
		t.Fatalf("superseded OAuth remains: present=%v err=%v", credential != nil, err)
	}
}

func TestPersistAuthWizardResultRejectsOrphanCredentialBeforeCreatingProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	if err := auth.Set("sensenova", "sk-orphan-must-remain-inert"); err != nil {
		t.Fatal(err)
	}

	err := persistAuthWizardResult(AuthResult{
		Provider: "sensenova",
		Key:      "sk-new-must-not-be-saved",
		Custom: &CustomProviderResult{
			Transport: "openai_chat",
			BaseURL:   "https://token.sensenova.cn/v1",
			Model:     "sensenova-6.8-flash-lite",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "existing credential but no configured profile") {
		t.Fatalf("orphan credential should block profile creation, got %v", err)
	}
	if key, getErr := auth.Get("sensenova"); getErr != nil || key != "sk-orphan-must-remain-inert" {
		t.Fatalf("orphan credential changed: key=%q err=%v", key, getErr)
	}
	if _, statErr := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile was created before orphan credential rejection: %v", statErr)
	}
}

func TestBuiltInProviderUsesOfficialEndpoint(t *testing.T) {
	cfg := &config.Config{}
	tests := []struct {
		name     string
		provider string
		baseURL  string
		want     bool
	}{
		{name: "anthropic official", provider: "anthropic", baseURL: "https://api.anthropic.com", want: true},
		{name: "anthropic trailing slash", provider: "anthropic", baseURL: "https://api.anthropic.com/", want: true},
		{name: "anthropic wrong scheme", provider: "anthropic", baseURL: "http://api.anthropic.com", want: false},
		{name: "anthropic deceptive host", provider: "anthropic", baseURL: "https://api.anthropic.com.attacker.invalid", want: false},
		{name: "openai official", provider: "openai", baseURL: "https://api.openai.com/v1", want: true},
		{name: "openai TLS port", provider: "openai", baseURL: "https://api.openai.com:443/v1/", want: true},
		{name: "openai extra path", provider: "openai", baseURL: "https://api.openai.com/v1/proxy", want: false},
		{name: "gemini official", provider: "gemini", baseURL: "https://generativelanguage.googleapis.com", want: true},
		{name: "gemini deceptive host", provider: "gemini", baseURL: "https://generativelanguage.googleapis.com.attacker.invalid", want: false},
		{name: "unknown provider", provider: "other", baseURL: "https://api.openai.com/v1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.Provider.Anthropic.BaseURL = ""
			cfg.Provider.OpenAI.BaseURL = ""
			cfg.Provider.Gemini.BaseURL = ""
			switch tt.provider {
			case "anthropic":
				cfg.Provider.Anthropic.BaseURL = tt.baseURL
			case "openai":
				cfg.Provider.OpenAI.BaseURL = tt.baseURL
			case "gemini":
				cfg.Provider.Gemini.BaseURL = tt.baseURL
			}
			if got := builtInProviderUsesOfficialEndpoint(cfg, tt.provider); got != tt.want {
				t.Fatalf("builtInProviderUsesOfficialEndpoint() = %v, want %v", got, tt.want)
			}
		})
	}
}

func authEnter() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyEnter}
}

func TestAuthWizardPasteReachesEveryTextInput(t *testing.T) {
	tests := []struct {
		name    string
		step    authStep
		paste   string
		prepare func(*authModel)
		value   func(authModel) string
	}{
		{
			name:  "custom provider id",
			step:  stepCustomID,
			paste: "sensenova",
			prepare: func(m *authModel) {
				m.customID.Focus()
			},
			value: func(m authModel) string { return m.customID.Value() },
		},
		{
			name:  "custom base URL",
			step:  stepCustomBaseURL,
			paste: "https://token.sensenova.cn/v1/chat/completions",
			prepare: func(m *authModel) {
				m.customBaseURL.Focus()
			},
			value: func(m authModel) string { return m.customBaseURL.Value() },
		},
		{
			name:  "custom model",
			step:  stepCustomModel,
			paste: "sensenova-6.8-flash-lite",
			prepare: func(m *authModel) {
				m.customModel.Focus()
			},
			value: func(m authModel) string { return m.customModel.Value() },
		},
		{
			name:  "API key",
			step:  stepEnterKey,
			paste: "sk-test-only",
			prepare: func(m *authModel) {
				m.keyInput.Focus()
			},
			value: func(m authModel) string { return m.keyInput.Value() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newAuthModel()
			m.step = tt.step
			tt.prepare(&m)

			m = driveAuthWizard(t, m, tea.PasteMsg{Content: tt.paste})
			if got := tt.value(m); got != tt.paste {
				t.Fatalf("pasted value = %q, want %q", got, tt.paste)
			}
		})
	}
}

func TestAuthWizardKeyInputIsMaskedEditableAndNeverRendered(t *testing.T) {
	m := newAuthModel()
	if got := m.keyInput.EchoMode; got != textinput.EchoPassword {
		t.Fatalf("key input EchoMode = %v, want EchoPassword", got)
	}

	m.step = stepEnterKey
	m.chosen = builtInAuthProviders[0]
	m.resultMethod = AuthMethodAPIKey
	m.keyInput.SetValue("sk-test-secret")
	m.keyInput.CursorEnd()
	if got := ansi.Strip(m.keyInput.View()); strings.Contains(got, "sk-test-secret") {
		t.Fatalf("plaintext key leaked in rendered input: %q", got)
	}

	m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got, want := m.keyInput.Value(), "sk-test-secrex"; got != want {
		t.Fatalf("edited key = %q, want %q", got, want)
	}

	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "Input is masked") || strings.Contains(view, "sk-test-secrex") {
		t.Fatalf("wizard view did not keep the secret masked:\n%s", view)
	}
}

func TestAuthWizardCompletedViewDoesNotLeakSubmittedKey(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Chdir(t.TempDir())
	m, err := newLoginAuthModel(LoginOptions{Provider: "openai", Method: AuthMethodAPIKey}, false)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-never-render-this-value"
	m.keyInput.SetValue(secret)
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepDone || m.err != nil {
		t.Fatalf("submitted wizard = step %v err %v", m.step, m.err)
	}
	if view := ansi.Strip(m.View().Content); strings.Contains(view, secret) || strings.Contains(view, "sk-never") {
		t.Fatalf("submitted key leaked in final view: %q", view)
	}
}

func TestLoginWizardProviderAndMethodRouting(t *testing.T) {
	tests := []struct {
		name       string
		options    LoginOptions
		wantStep   authStep
		wantMethod string
		wantCount  int
	}{
		{name: "openai id is case insensitive", options: LoginOptions{Provider: "OPENAI"}, wantStep: stepPickMethod, wantCount: 1},
		{name: "openai api key skips method", options: LoginOptions{Provider: "openai", Method: AuthMethodAPIKey}, wantStep: stepEnterKey, wantMethod: AuthMethodAPIKey, wantCount: 1},
		{name: "openai oauth skips method", options: LoginOptions{Provider: "openai", Method: AuthMethodOAuth}, wantStep: stepDone, wantMethod: AuthMethodOAuth, wantCount: 1},
		{name: "anthropic asks for method", options: LoginOptions{Provider: "Anthropic"}, wantStep: stepPickMethod, wantCount: 1},
		{name: "anthropic oauth skips method", options: LoginOptions{Provider: "anthropic", Method: AuthMethodOAuth}, wantStep: stepDone, wantMethod: AuthMethodOAuth, wantCount: 1},
		{name: "codex asks for oauth flow", options: LoginOptions{Provider: "openai-codex"}, wantStep: stepPickOAuthFlow, wantMethod: AuthMethodOAuth, wantCount: 1},
		{name: "codex device flow skips picker", options: LoginOptions{Provider: "openai-codex", OAuthFlow: OAuthFlowDevice}, wantStep: stepDone, wantMethod: AuthMethodOAuth, wantCount: 1},
		{name: "gemini has only api key", options: LoginOptions{Provider: "gemini"}, wantStep: stepEnterKey, wantMethod: AuthMethodAPIKey, wantCount: 1},
		{name: "oauth filters provider picker", options: LoginOptions{Method: AuthMethodOAuth}, wantStep: stepPickProvider, wantCount: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := newLoginAuthModel(tt.options, false)
			if err != nil {
				t.Fatal(err)
			}
			if m.step != tt.wantStep {
				t.Fatalf("step = %v, want %v", m.step, tt.wantStep)
			}
			if m.resultMethod != tt.wantMethod {
				t.Fatalf("method = %q, want %q", m.resultMethod, tt.wantMethod)
			}
			if len(m.providers) != tt.wantCount {
				t.Fatalf("provider count = %d, want %d", len(m.providers), tt.wantCount)
			}
		})
	}
}

func TestLoginWizardProviderPickerHasOneOpenAIEntry(t *testing.T) {
	for _, method := range []string{"", AuthMethodOAuth, AuthMethodAPIKey} {
		t.Run("method="+method, func(t *testing.T) {
			m, err := newLoginAuthModel(LoginOptions{Method: method}, false)
			if err != nil {
				t.Fatal(err)
			}
			openAICount := 0
			for _, provider := range m.providers {
				if provider.id == "openai" {
					openAICount++
				}
				if provider.id == "openai-codex" {
					t.Fatal("regular provider picker duplicated OpenAI with the legacy Codex entry")
				}
			}
			if openAICount != 1 {
				t.Fatalf("OpenAI entries = %d, want 1; providers=%v", openAICount, authProviderIDs(m.providers))
			}
		})
	}
}

func TestLoginWizardOpenAIAccountSelectionUsesBrowserWithoutKey(t *testing.T) {
	m, err := newLoginAuthModel(LoginOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, provider := range m.providers {
		if provider.id == "openai" {
			for j := 0; j < i; j++ {
				m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
			}
			break
		}
	}
	m = driveAuthWizard(t, m, authEnter())
	if m.chosen.id != "openai" || m.step != stepPickMethod {
		t.Fatalf("OpenAI selection = provider %q step %v, want method picker", m.chosen.id, m.step)
	}
	if len(m.methods) != 2 || m.methods[0].id != AuthMethodOAuth || m.methods[1].id != AuthMethodAPIKey || m.methodCursor != 0 {
		t.Fatalf("OpenAI methods = %#v cursor=%d, want account selected before API key", m.methods, m.methodCursor)
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"Sign in with ChatGPT", "browser", "no API key required"} {
		if !strings.Contains(view, want) {
			t.Fatalf("OpenAI method picker missing %q:\n%s", want, view)
		}
	}
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepDone || m.err != nil || m.validationErr != "" {
		t.Fatalf("account login without key = step %v err=%v validation=%q", m.step, m.err, m.validationErr)
	}
	result := m.authResult()
	if result.Provider != "openai-codex" || result.Method != AuthMethodOAuth || result.OAuthFlow != OAuthFlowBrowser || result.Key != "" || result.Custom != nil {
		t.Fatalf("account result = %#v, want openai-codex/browser OAuth without API key", result)
	}
}

func TestLoginWizardOpenAIAPIKeySelectionStaysMaskedAndUsesOpenAI(t *testing.T) {
	m, err := newLoginAuthModel(LoginOptions{Provider: "openai"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.step != stepPickMethod {
		t.Fatalf("step = %v, want method picker", m.step)
	}
	m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepEnterKey || m.resultMethod != AuthMethodAPIKey {
		t.Fatalf("API-key selection = step %v method %q", m.step, m.resultMethod)
	}
	if m.keyInput.EchoMode != textinput.EchoPassword {
		t.Fatalf("API-key echo mode = %v, want EchoPassword", m.keyInput.EchoMode)
	}
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepEnterKey || m.validationErr == "" {
		t.Fatal("API-key login accepted an empty key")
	}
	const secret = "sk-openai-key-remains-masked"
	m = driveAuthWizard(t, m, tea.PasteMsg{Content: secret})
	if view := ansi.Strip(m.View().Content); strings.Contains(view, secret) || !strings.Contains(view, "Input is masked") {
		t.Fatalf("API-key entry did not keep the secret masked:\n%s", view)
	}
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepDone || m.err != nil {
		t.Fatalf("API-key submission = step %v err=%v", m.step, m.err)
	}
	result := m.authResult()
	if result.Provider != "openai" || result.Method != AuthMethodAPIKey || result.OAuthFlow != "" || result.Key != secret {
		t.Fatalf("API-key result has provider=%q method=%q flow=%q key_match=%v", result.Provider, result.Method, result.OAuthFlow, result.Key == secret)
	}
}

func TestLoginWizardOpenAIExplicitOAuthSelectsFlow(t *testing.T) {
	for _, flow := range []string{"", OAuthFlowBrowser, OAuthFlowDevice} {
		t.Run("flow="+flow, func(t *testing.T) {
			m, err := newLoginAuthModel(LoginOptions{Provider: " OpenAI ", Method: AuthMethodOAuth, OAuthFlow: flow}, false)
			if err != nil {
				t.Fatal(err)
			}
			if m.step != stepDone {
				t.Fatalf("explicit OAuth started at step %v, want ready to authorize", m.step)
			}
			wantFlow := flow
			if wantFlow == "" {
				wantFlow = OAuthFlowBrowser
			}
			result := m.authResult()
			if result.Provider != "openai-codex" || result.Method != AuthMethodOAuth || result.OAuthFlow != wantFlow || result.Key != "" {
				t.Fatalf("explicit OAuth result = %#v, want openai-codex/%s without API key", result, wantFlow)
			}
		})
	}
}

func TestLoginWizardUsesConfiguredCustomProviderWithoutReenteringProfile(t *testing.T) {
	profile := ConfiguredLoginProvider{
		ID: "sensenova", Transport: "openai_chat",
		BaseURL: "https://api.example.com/v1", Model: "deepseek-v4-pro",
	}
	m, err := newLoginAuthModel(LoginOptions{
		Provider: "sensenova", ConfiguredProviders: []ConfiguredLoginProvider{profile},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.step != stepEnterKey || m.chosen.id != "sensenova" || !m.isCustom {
		t.Fatalf("configured provider route = step %v chosen=%q custom=%v", m.step, m.chosen.id, m.isCustom)
	}
	m.keyInput.SetValue("opaque-test-key")
	m = driveAuthWizard(t, m, authEnter())
	result := m.authResult()
	if result.Provider != "sensenova" || result.Custom == nil || !result.Custom.Existing || result.Custom.BaseURL != profile.BaseURL || result.Custom.Model != profile.Model {
		t.Fatalf("configured provider result = %#v", result)
	}
}

func TestLoginWizardShowsConfiguredEndpointBeforeKeyEntry(t *testing.T) {
	profile := ConfiguredLoginProvider{
		ID: "sensenova", Transport: "openai_chat",
		BaseURL: "https://token.snova.example/v1", Model: "sensenova-test",
	}
	m, err := newLoginAuthModel(LoginOptions{
		Provider: profile.ID, Method: AuthMethodAPIKey, ConfiguredProviders: []ConfiguredLoginProvider{profile},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.step != stepEnterKey {
		t.Fatalf("configured provider started at step %v, want key entry", m.step)
	}
	view := m.View().Content
	for _, want := range []string{"Transport: openai_chat", "Endpoint: https://token.snova.example/v1", "Model: sensenova-test"} {
		if !strings.Contains(view, want) {
			t.Fatalf("configured key screen missing %q:\n%s", want, view)
		}
	}
}

func TestLoginWizardPickerIncludesConfiguredProvidersBeforeGenericCustom(t *testing.T) {
	m, err := newLoginAuthModel(LoginOptions{ConfiguredProviders: []ConfiguredLoginProvider{
		{ID: "sensenova", Transport: "openai_chat", BaseURL: "https://api.example.com/v1", Model: "model"},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.providers) < 2 || m.providers[len(m.providers)-2].id != "sensenova" || m.providers[len(m.providers)-1].id != "custom" {
		t.Fatalf("provider picker order = %#v", authProviderIDs(m.providers))
	}
}

func TestLoginWizardOpenAICodexOffersBrowserAndDevice(t *testing.T) {
	for _, flow := range []string{OAuthFlowBrowser, OAuthFlowDevice} {
		t.Run(flow, func(t *testing.T) {
			m, err := newLoginAuthModel(LoginOptions{Provider: "openai-codex"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if m.step != stepPickOAuthFlow {
				t.Fatalf("step = %v, want OAuth flow picker", m.step)
			}
			if len(m.oauthFlows) != 2 || m.oauthFlows[0].id != OAuthFlowBrowser || m.oauthFlows[1].id != OAuthFlowDevice {
				t.Fatalf("OAuth flows = %#v, want browser then device-code", m.oauthFlows)
			}
			if flow == OAuthFlowDevice {
				m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
			}
			m = driveAuthWizard(t, m, authEnter())
			result := m.authResult()
			if m.step != stepDone || result.Provider != "openai-codex" || result.Method != AuthMethodOAuth || result.OAuthFlow != flow || result.Key != "" {
				t.Fatalf("result = %#v at step %v, want openai-codex OAuth %s", result, m.step, flow)
			}
		})
	}
}

func TestLoginWizardRejectsUnknownOrUnsupportedSelection(t *testing.T) {
	if _, err := newLoginAuthModel(LoginOptions{Provider: "open-ai"}, false); err == nil || !strings.Contains(err.Error(), "choose one of") {
		t.Fatalf("unknown provider error = %v", err)
	}
	if _, err := newLoginAuthModel(LoginOptions{Provider: "gemini", Method: AuthMethodOAuth}, false); err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("unsupported method error = %v", err)
	}
	if _, err := newLoginAuthModel(LoginOptions{Method: "token"}, false); err == nil || !strings.Contains(err.Error(), "api-key or oauth") {
		t.Fatalf("unknown method error = %v", err)
	}
}

func TestStartupAuthWizardNeverOffersOAuth(t *testing.T) {
	m := newAuthModel()
	for _, provider := range m.providers {
		if provider.id == "openai-codex" {
			t.Fatal("startup authentication gate offered browser-only OAuth")
		}
		if !providerSupportsMethod(provider, AuthMethodAPIKey) {
			t.Fatalf("startup provider %q does not support API keys", provider.id)
		}
	}
	m.chosen = m.providers[0]
	m.advanceAfterProvider()
	if m.step != stepEnterKey || m.resultMethod != AuthMethodAPIKey {
		t.Fatalf("startup provider advanced to step=%v method=%q, want API-key entry", m.step, m.resultMethod)
	}
	for _, method := range []string{"", AuthMethodOAuth} {
		openAI, err := newLoginAuthModel(LoginOptions{Provider: "openai", Method: method}, true)
		if err != nil {
			t.Fatal(err)
		}
		if openAI.step != stepEnterKey || openAI.resultMethod != AuthMethodAPIKey || !openAI.persistOnSubmit {
			t.Fatalf("startup OpenAI = step %v method %q persist=%v, want persisted API-key entry", openAI.step, openAI.resultMethod, openAI.persistOnSubmit)
		}
	}
}

func TestAuthWizardCustomProviderIDValidation(t *testing.T) {
	invalid := []string{
		"",
		"   ",
		"https://token.sensenova.cn/v1/chat/completions",
		"SenseNova",
		"sense nova",
		"sense.nova",
		"google",
		"anthropic-claudeai",
		"openai-codex",
		"-sensenova",
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			m := newAuthModel()
			m.step = stepCustomID
			m.customID.Focus()
			m.customID.SetValue(input)

			m = driveAuthWizard(t, m, authEnter())
			if m.step != stepCustomID {
				t.Fatalf("invalid id %q advanced to step %v", input, m.step)
			}
			if m.validationErr == "" {
				t.Fatalf("invalid id %q did not produce a validation error", input)
			}
		})
	}

	m := newAuthModel()
	m.step = stepCustomID
	m.customID.Focus()
	m.customID.SetValue("sense-nova_1")
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepCustomTransport {
		t.Fatalf("valid custom id did not advance to transport picker; step=%v error=%q", m.step, m.validationErr)
	}
}

func TestAuthWizardCustomTransportPickerDefaultsToOpenAI(t *testing.T) {
	m := newAuthModel()
	m.step = stepCustomTransport

	m = driveAuthWizard(t, m, authEnter())
	if got, want := m.customTransport, "openai_chat"; got != want {
		t.Fatalf("default custom transport = %q, want %q", got, want)
	}
	if m.step != stepCustomBaseURL {
		t.Fatalf("transport selection advanced to step %v, want stepCustomBaseURL", m.step)
	}
}

func TestAuthWizardPickerDoesNotOfferIncompleteCompatPresets(t *testing.T) {
	m := newAuthModel()
	for _, provider := range m.providers {
		if strings.Contains(provider.label, "MiniMax") || strings.Contains(provider.label, "Gemini (OpenAI-compat)") {
			t.Fatalf("picker still offers incomplete compat preset %q", provider.label)
		}
	}
}

func TestAuthWizardCustomTransportPickerSupportsAllTransports(t *testing.T) {
	wants := []string{"openai_chat", "openai_responses", "anthropic_messages", "gemini_native"}
	for index, want := range wants {
		t.Run(want, func(t *testing.T) {
			m := newAuthModel()
			m.step = stepCustomTransport
			for i := 0; i < index; i++ {
				m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
			}
			m = driveAuthWizard(t, m, authEnter())
			if got := m.customTransport; got != want {
				t.Fatalf("selected transport = %q, want %q", got, want)
			}
		})
	}
}

func TestAuthWizardCustomBaseURLValidationAndNormalization(t *testing.T) {
	invalid := []string{
		"",
		"token.sensenova.cn/v1",
		"ftp://token.sensenova.cn/v1",
		"https:///v1",
		"https://token.sensenova.cn/v1?api_key=secret",
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			m := newAuthModel()
			m.step = stepCustomBaseURL
			m.customTransport = "openai_chat"
			m.customBaseURL.Focus()
			m.customBaseURL.SetValue(input)

			m = driveAuthWizard(t, m, authEnter())
			if m.step != stepCustomBaseURL {
				t.Fatalf("invalid URL %q advanced to step %v", input, m.step)
			}
			if m.validationErr == "" {
				t.Fatalf("invalid URL %q did not produce a validation error", input)
			}
		})
	}

	m := newAuthModel()
	m.step = stepCustomBaseURL
	m.customTransport = "openai_chat"
	m.customBaseURL.Focus()
	m.customBaseURL.SetValue("https://token.sensenova.cn/v1/chat/completions/")
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepCustomModel {
		t.Fatalf("valid URL did not advance to model input; step=%v error=%q", m.step, m.validationErr)
	}
	if got, want := m.normalizedBaseURL, "https://token.sensenova.cn/v1"; got != want {
		t.Fatalf("normalized base URL = %q, want %q", got, want)
	}
}

func TestAuthWizardCustomBaseURLNormalizationByTransport(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		input     string
		want      string
	}{
		{
			name: "anthropic full endpoint", transport: "anthropic_messages",
			input: "https://api.minimaxi.com/anthropic/v1/messages", want: "https://api.minimaxi.com/anthropic",
		},
		{
			name: "anthropic version root", transport: "anthropic_messages",
			input: "https://api.example.com/v1", want: "https://api.example.com",
		},
		{
			name: "gemini version root", transport: "gemini_native",
			input: "https://generativelanguage.googleapis.com/v1beta", want: "https://generativelanguage.googleapis.com",
		},
		{
			name: "gemini full streaming endpoint", transport: "gemini_native",
			input: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", want: "https://generativelanguage.googleapis.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateAndNormalizeCustomBaseURL(tt.transport, tt.input)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalized URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuthWizardCustomBaseURLRejectsRemotePlainHTTP(t *testing.T) {
	if _, err := validateAndNormalizeCustomBaseURL("openai_chat", "http://api.example.test/v1"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("remote plain HTTP endpoint was accepted: %v", err)
	}
	for _, raw := range []string{"http://localhost:11434/v1", "http://127.0.0.1:9000/v1", "http://[::1]:9000/v1"} {
		if _, err := validateAndNormalizeCustomBaseURL("openai_chat", raw); err != nil {
			t.Fatalf("loopback endpoint %q rejected: %v", raw, err)
		}
	}
}

func TestAuthWizardCustomModelValidation(t *testing.T) {
	for _, input := range []string{"", "  ", "sense nova"} {
		t.Run(input, func(t *testing.T) {
			m := newAuthModel()
			m.step = stepCustomModel
			m.customModel.Focus()
			m.customModel.SetValue(input)

			m = driveAuthWizard(t, m, authEnter())
			if m.step != stepCustomModel {
				t.Fatalf("invalid model %q advanced to step %v", input, m.step)
			}
			if m.validationErr == "" {
				t.Fatalf("invalid model %q did not produce a validation error", input)
			}
		})
	}

	m := newAuthModel()
	m.step = stepCustomModel
	m.customModel.Focus()
	m.customModel.SetValue("sensenova-6.8-flash-lite")
	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepEnterKey {
		t.Fatalf("valid model did not advance to API key; step=%v error=%q", m.step, m.validationErr)
	}
}

func TestAuthWizardCustomOpenAIFlowReturnsCompleteProfile(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newAuthModel()
	m.cursor = len(m.providers) - 1

	m = driveAuthWizard(t, m, authEnter())
	if m.step != stepCustomID {
		t.Fatalf("custom provider selection advanced to step %v, want stepCustomID", m.step)
	}
	m = driveAuthWizard(t, m, tea.PasteMsg{Content: "sensenova"})
	m = driveAuthWizard(t, m, authEnter())
	m = driveAuthWizard(t, m, authEnter()) // default openai_chat
	m = driveAuthWizard(t, m, tea.PasteMsg{Content: "https://token.sensenova.cn/v1/chat/completions"})
	m = driveAuthWizard(t, m, authEnter())
	m = driveAuthWizard(t, m, tea.PasteMsg{Content: "sensenova-6.8-flash-lite"})
	m = driveAuthWizard(t, m, authEnter())
	m = driveAuthWizard(t, m, tea.PasteMsg{Content: "sk-test-only"})
	m = driveAuthWizard(t, m, authEnter())

	if m.step != stepDone {
		t.Fatalf("completed custom flow ended at step %v, want stepDone (error=%v validation=%q)", m.step, m.err, m.validationErr)
	}
	result := m.authResult()
	if result.Provider != "sensenova" || result.Key != "sk-test-only" {
		t.Fatalf("basic result = {Provider:%q Key:%q}", result.Provider, result.Key)
	}
	if result.Custom == nil {
		t.Fatal("custom flow returned nil Custom profile")
	}
	if got, want := result.Custom.Transport, "openai_chat"; got != want {
		t.Errorf("transport = %q, want %q", got, want)
	}
	if got, want := result.Custom.BaseURL, "https://token.sensenova.cn/v1"; got != want {
		t.Errorf("base URL = %q, want %q", got, want)
	}
	if got, want := result.Custom.Model, "sensenova-6.8-flash-lite"; got != want {
		t.Errorf("model = %q, want %q", got, want)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("load persisted custom provider: %v", err)
	}
	if got, want := cfg.Provider.Default, "sensenova"; got != want {
		t.Errorf("persisted provider.default = %q, want %q", got, want)
	}
	if got := cfg.Provider.Custom["sensenova"]; got.Transport != "openai_chat" || got.BaseURL != "https://token.sensenova.cn/v1" || got.Model != "sensenova-6.8-flash-lite" {
		t.Errorf("persisted custom provider = %#v", got)
	}
	key, err := auth.Get("sensenova")
	if err != nil {
		t.Fatalf("read persisted credential: %v", err)
	}
	if key != "sk-test-only" {
		t.Fatalf("persisted credential value=%q", key)
	}
}
