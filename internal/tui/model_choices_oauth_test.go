package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

func putModelChoiceCodexOAuth(t *testing.T) {
	t.Helper()
	if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{
		AccessToken:  "test-access",
		RefreshToken: "test-refresh",
		AccountID:    "test-account",
		ExpiresAt:    time.Now().Add(time.Hour),
		TokenType:    "Bearer",
	}); err != nil {
		t.Fatal(err)
	}
}

func isolatedModelChoiceConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(name, "")
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestOpenAICodexIsAvailableToCLIModelAndProviderPickers(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	provider, model, ok := configuredProviderModel(cfg, "OPENAI-CODEX")
	if !ok || provider != "openai-codex" || model != cfg.Provider.OpenAICodex.Model {
		t.Fatalf("configuredProviderModel = (%q, %q, %v)", provider, model, ok)
	}

	found := false
	for _, choice := range configuredModelChoices(cfg) {
		if choice.Provider != "openai-codex" {
			continue
		}
		found = true
		if choice.ID != cfg.Provider.OpenAICodex.Model {
			t.Fatalf("Codex model = %q, want %q", choice.ID, cfg.Provider.OpenAICodex.Model)
		}
		if got := modelChoiceVisionCapability(cfg, choice); got != pubprovider.VisionSupported {
			t.Fatalf("Codex vision capability = %v, want supported", got)
		}
	}
	if !found {
		t.Fatal("configured model choices omitted openai-codex")
	}
}

func TestSplitConfiguredProviderModelAcceptsOpenAICodex(t *testing.T) {
	cfg := &config.Config{}
	provider, model := splitConfiguredProviderModel(cfg, "openai-codex/gpt-5.5")
	if provider != "openai-codex" || model != "gpt-5.5" {
		t.Fatalf("split provider/model = %q/%q", provider, model)
	}
}

func TestCodexOAuthUnlocksEntireCatalogAndHidesUnconfiguredProviders(t *testing.T) {
	cfg := isolatedModelChoiceConfig(t)
	putModelChoiceCodexOAuth(t)

	choices := (&Model{cfg: cfg}).modelPickerChoices(false)
	got := make([]string, 0, len(choices))
	for _, choice := range choices {
		if choice.Provider != "openai-codex" {
			t.Fatalf("unconfigured provider appeared in picker: %+v", choice)
		}
		got = append(got, choice.ID)
	}
	want := make([]string, 0, len(openai.CodexModels()))
	for _, model := range openai.CodexModels() {
		want = append(want, model.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex picker models = %v, want %v", got, want)
	}
}

func TestOpenAIAPIKeyDoesNotUnlockCodexOAuthModels(t *testing.T) {
	cfg := isolatedModelChoiceConfig(t)
	cfg.Provider.OpenAI.APIKey = "test-platform-key"

	choices := (&Model{cfg: cfg}).modelPickerChoices(false)
	if len(choices) == 0 {
		t.Fatal("OpenAI API-key models were hidden")
	}
	for _, choice := range choices {
		if choice.Provider == "openai-codex" {
			t.Fatalf("OpenAI API key unlocked Codex OAuth model: %+v", choice)
		}
		if choice.Provider != "openai" {
			t.Fatalf("unexpected provider with only OpenAI API key: %+v", choice)
		}
	}
}

func TestCodexOAuthAndOpenAIAPIKeyExposeSeparateCatalogs(t *testing.T) {
	cfg := isolatedModelChoiceConfig(t)
	putModelChoiceCodexOAuth(t)
	cfg.Provider.OpenAI.APIKey = "test-platform-key"

	var codexIDs, platformIDs []string
	for _, choice := range (&Model{cfg: cfg}).modelPickerChoices(false) {
		switch choice.Provider {
		case "openai-codex":
			codexIDs = append(codexIDs, choice.ID)
		case "openai":
			platformIDs = append(platformIDs, choice.ID)
		default:
			t.Fatalf("unconfigured provider appeared in combined picker: %+v", choice)
		}
	}
	wantCodex := make([]string, 0, len(openai.CodexModels()))
	for _, model := range openai.CodexModels() {
		wantCodex = append(wantCodex, model.ID)
	}
	if !reflect.DeepEqual(codexIDs, wantCodex) {
		t.Fatalf("Codex models = %v, want %v", codexIDs, wantCodex)
	}
	if want := []string{"gpt-4o", "gpt-4o-mini"}; !reflect.DeepEqual(platformIDs, want) {
		t.Fatalf("OpenAI Platform models = %v, want %v", platformIDs, want)
	}
}

func TestIncompleteCodexOAuthDoesNotUnlockCatalog(t *testing.T) {
	cfg := isolatedModelChoiceConfig(t)
	if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{
		AccessToken: "incomplete-test-access",
	}); err != nil {
		t.Fatal(err)
	}

	if choices := (&Model{cfg: cfg}).modelPickerChoices(false); len(choices) != 0 {
		t.Fatalf("incomplete Codex OAuth unlocked models: %+v", choices)
	}
}
