package runtime

import (
	"bytes"
	"errors"
	"strings"
	"testing"

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

func TestIsKnownProvider(t *testing.T) {
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{
		"groq": {Transport: "openai_chat"},
	}
	cases := map[string]bool{
		"anthropic": true,
		"openai":    true,
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
