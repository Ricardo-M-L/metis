package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

func cfgWithAnthropicKey(key string) *config.Config {
	cfg := &config.Config{}
	cfg.Provider.Anthropic.APIKey = key
	cfg.Provider.Default = "anthropic"
	return cfg
}

func TestEnsureAPIKey_HappyPath(t *testing.T) {
	cfg := cfgWithAnthropicKey("sk-x")
	got, gotProv, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{})
	if err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	if gotProv != "anthropic" {
		t.Errorf("provider = %q, want anthropic", gotProv)
	}
	if got != cfg {
		t.Errorf("config should be returned as-is when key already exists")
	}
}

func TestEnsureAPIKey_VertexServiceAccountSkipsAPIKeyWizard(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := newCfgWithCustom("vertex-claude", config.ProviderRaw{
		Transport:          "vertex_anthropic",
		ServiceAccountFile: writeRuntimeTestServiceAccount(t),
		Project:            "metis-test-project",
		Model:              "claude-sonnet-4-6",
	})
	wizardRan := false

	got, gotProv, err := EnsureAPIKey(cfg, "vertex-claude", AuthGateOptions{
		IsTTY: func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			wizardRan = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("EnsureAPIKey rejected Vertex service-account auth: %v", err)
	}
	if wizardRan {
		t.Error("API-key wizard ran for a configured Vertex service account")
	}
	if got != cfg || gotProv != "vertex-claude" {
		t.Fatalf("unexpected auth-gate result: cfg=%p want=%p provider=%q", got, cfg, gotProv)
	}
}

func TestEnsureAPIKey_BedrockAWSCredentialsSkipAPIKeyWizard(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_METIS_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "metis-test-secret")
	cfg := newCfgWithCustom("bedrock-claude", config.ProviderRaw{
		Transport: "bedrock_anthropic",
		Model:     "us.anthropic.claude-sonnet-4-6-v1:0",
	})
	wizardRan := false

	got, gotProv, err := EnsureAPIKey(cfg, "bedrock-claude", AuthGateOptions{
		IsTTY: func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			wizardRan = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("EnsureAPIKey rejected Bedrock AWS auth: %v", err)
	}
	if wizardRan {
		t.Error("API-key wizard ran for configured Bedrock AWS credentials")
	}
	if got != cfg || gotProv != "bedrock-claude" {
		t.Fatalf("unexpected auth-gate result: cfg=%p want=%p provider=%q", got, cfg, gotProv)
	}
}

func TestEnsureAPIKey_BedrockMissingSecretHasCloudSpecificError(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_METIS_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	cfg := newCfgWithCustom("bedrock-claude", config.ProviderRaw{
		Transport: "bedrock_anthropic",
		Model:     "us.anthropic.claude-sonnet-4-6-v1:0",
	})

	_, _, err := EnsureAPIKey(cfg, "bedrock-claude", AuthGateOptions{})
	if err == nil || !strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("missing Bedrock secret should return AWS-specific guidance, got %v", err)
	}
}

func TestEnsureAPIKey_NoTTY_NoWizardRuns(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir()) // isolate from user's real auth.json
	cfg := &config.Config{}
	cfg.Provider.Anthropic.APIKeyEnv = "METIS_TEST_NO_KEY_HERE"
	cfg.Provider.Default = "anthropic"

	wizardRan := false
	_, _, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{
		IsTTY: func() bool { return false },
		RunWizard: func() (*WizardResult, error) {
			wizardRan = true
			return nil, nil
		},
	})
	if wizardRan {
		t.Error("wizard should NOT run when stderr isn't a tty")
	}
	if err == nil {
		t.Error("missing key + no wizard should error")
	}
}

func TestEnsureAPIKey_NoWizardFlag_SkipsEvenOnTTY(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir()) // isolate from user's real auth.json
	cfg := &config.Config{}
	cfg.Provider.Anthropic.APIKeyEnv = "METIS_TEST_NO_KEY_HERE"
	cfg.Provider.Default = "anthropic"

	wizardRan := false
	_, _, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{
		NoWizard: true,
		IsTTY:    func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			wizardRan = true
			return nil, nil
		},
	})
	if wizardRan {
		t.Error("--no-auth-wizard should suppress wizard launch")
	}
	if err == nil {
		t.Error("missing key + suppressed wizard should error")
	}
}

func TestEnsureAPIKey_WizardCancelledMessage(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir()) // isolate from user's real auth.json
	cfg := &config.Config{}
	cfg.Provider.Anthropic.APIKeyEnv = "METIS_TEST_NO_KEY_HERE"
	cfg.Provider.Default = "anthropic"

	var stderr bytes.Buffer
	_, _, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{
		IsTTY: func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			return nil, ErrWizardCancelled
		},
		Stderr: &stderr,
	})
	if err == nil {
		t.Fatal("cancelled wizard should error")
	}
	if !strings.Contains(err.Error(), "auth setup cancelled") {
		t.Errorf("error should mention cancellation; got %v", err)
	}
}

func TestEnsureAPIKey_WizardErrorPropagates(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Provider.Anthropic.APIKeyEnv = "METIS_TEST_NO_KEY_HERE"

	want := errors.New("network exploded")
	_, _, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{
		IsTTY: func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			return nil, want
		},
	})
	if !errors.Is(err, want) {
		t.Errorf("non-cancellation error should propagate; got %v, want %v", err, want)
	}
}

func TestEnsureAPIKey_WizardCanSelectNewCustomProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	cfg := &config.Config{}
	cfg.Provider.Default = "anthropic"
	cfg.Provider.Anthropic.APIKeyEnv = "METIS_TEST_NO_KEY_HERE"

	got, gotProv, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{
		IsTTY: func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			contents := `[provider]
default = "sensenova"

[provider.custom.sensenova]
transport = "openai_chat"
base_url = "https://token.sensenova.cn/v1"
model = "sensenova-6.8-flash-lite"
`
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600); err != nil {
				return nil, err
			}
			if err := auth.Set("sensenova", "sk-test-only"); err != nil {
				return nil, err
			}
			return &WizardResult{Provider: "sensenova", Key: "sk-test-only"}, nil
		},
	})
	if err != nil {
		t.Fatalf("EnsureAPIKey rejected custom provider created by wizard: %v", err)
	}
	if gotProv != "sensenova" {
		t.Fatalf("provider = %q, want sensenova", gotProv)
	}
	raw, ok := got.Provider.Custom["sensenova"]
	if !ok || raw.Model != "sensenova-6.8-flash-lite" {
		t.Fatalf("reloaded custom provider = %#v, present=%v", raw, ok)
	}
}

func TestEnsureAPIKey_WizardConfigReloadErrorPropagates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	cfg := &config.Config{}
	cfg.Provider.Default = "anthropic"
	cfg.Provider.Anthropic.APIKeyEnv = "METIS_TEST_NO_KEY_HERE"

	_, _, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{
		IsTTY: func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider\n"), 0o600); err != nil {
				return nil, err
			}
			return &WizardResult{Provider: "sensenova", Key: "sk-test-only"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "reload config after auth setup") {
		t.Fatalf("malformed wizard config should return a reload error, got %v", err)
	}
}

func TestEnsureAPIKey_WizardUnknownProviderFailsClearly(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Provider.Default = "anthropic"
	cfg.Provider.Anthropic.APIKeyEnv = "METIS_TEST_NO_KEY_HERE"

	_, _, err := EnsureAPIKey(cfg, "anthropic", AuthGateOptions{
		IsTTY: func() bool { return true },
		RunWizard: func() (*WizardResult, error) {
			return &WizardResult{Provider: "missing-profile", Key: "sk-test-only"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), `wizard selected unknown provider "missing-profile"`) {
		t.Fatalf("unknown wizard provider should fail clearly, got %v", err)
	}
}

func TestIsKnownProvider(t *testing.T) {
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{
		"groq": {Transport: "openai_chat"},
	}
	cases := map[string]bool{
		"anthropic": true,
		"openai":    true,
		"gemini":    true,
		"google":    true,
		"groq":      true,
		"unknown":   false,
		"":          false,
	}
	for id, want := range cases {
		if got := IsKnownProvider(cfg, id); got != want {
			t.Errorf("IsKnownProvider(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestIsKnownProvider_NilCfgSafe(t *testing.T) {
	if IsKnownProvider(nil, "anthropic") != true {
		t.Error("anthropic should still be recognized with nil cfg")
	}
	if IsKnownProvider(nil, "groq") != false {
		t.Error("custom id with nil cfg should be false")
	}
}
