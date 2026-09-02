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
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Chdir(project)
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("OPENAI_API_KEY", "")
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
		{name: "unknown provider", provider: "other", baseURL: "https://api.openai.com/v1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.Provider.Anthropic.BaseURL = ""
			cfg.Provider.OpenAI.BaseURL = ""
			switch tt.provider {
			case "anthropic":
				cfg.Provider.Anthropic.BaseURL = tt.baseURL
			case "openai":
				cfg.Provider.OpenAI.BaseURL = tt.baseURL
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

func TestAuthWizardKeyInputIsPlaintextAndEditableByDefault(t *testing.T) {
	m := newAuthModel()
	if got := m.keyInput.EchoMode; got != textinput.EchoNormal {
		t.Fatalf("key input EchoMode = %v, want EchoNormal", got)
	}

	m.step = stepEnterKey
	m.keyInput.SetValue("sk-test-visible")
	m.keyInput.CursorEnd()
	if got := ansi.Strip(m.keyInput.View()); !strings.Contains(got, "sk-test-visible") {
		t.Fatalf("plaintext key is not present in rendered input: %q", got)
	}

	m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = driveAuthWizard(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got, want := m.keyInput.Value(), "sk-test-visiblx"; got != want {
		t.Fatalf("edited key = %q, want %q", got, want)
	}

	view := ansi.Strip(m.View().Content)
	if strings.Contains(view, "Input is masked") {
		t.Fatalf("wizard still claims the plaintext input is masked:\n%s", view)
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
