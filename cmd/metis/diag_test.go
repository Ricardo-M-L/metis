package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestProviderKeyAndModelCanonicalizesGoogleAlias(t *testing.T) {
	cfg := &config.Config{}
	cfg.Provider.Gemini.APIKeyEnv = "GEMINI_TEST_KEY"
	cfg.Provider.Gemini.Model = "gemini-test-model"

	gotEnv, gotModel := providerKeyAndModel(cfg, " GoOgLe ")
	if gotEnv != "GEMINI_TEST_KEY" || gotModel != "gemini-test-model" {
		t.Fatalf("google alias resolved to (%q, %q), want Gemini settings", gotEnv, gotModel)
	}
}

func TestDiagOpenAICodexReportsOAuthWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	writeDiagConfig(t, home)
	const accessToken = "oauth-access-must-not-appear"
	const accountID = "account-must-not-appear"
	if err := auth.PutOAuth("openai-codex", auth.OAuthCredential{
		AccessToken:  accessToken,
		RefreshToken: "refresh-must-not-appear",
		ExpiresAt:    time.Now().Add(time.Hour),
		AccountID:    accountID,
	}); err != nil {
		t.Fatal(err)
	}

	output, err := captureDiagOutput(t, context.Background())
	if err != nil {
		t.Fatalf("cmdDiag() error = %v\n%s", err, output)
	}
	for _, want := range []string{"provider.default", "openai-codex", "model", "gpt-diag-codex", "OAuth credential", "configured"} {
		if !strings.Contains(output, want) {
			t.Fatalf("diag output missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{"api_key_env", accessToken, accountID, "refresh-must-not-appear"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("diag output contains forbidden %q:\n%s", forbidden, output)
		}
	}
}

func TestDiagOpenAICodexMissingOAuthShowsLoginRemediation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	writeDiagConfig(t, home)

	output, err := captureDiagOutput(t, context.Background())
	if err == nil {
		t.Fatalf("cmdDiag() unexpectedly succeeded:\n%s", output)
	}
	for _, want := range []string{"model", "gpt-diag-codex", "OAuth credential", "missing", "metis login openai-codex"} {
		if !strings.Contains(output, want) {
			t.Fatalf("diag output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "api_key_env") {
		t.Fatalf("OAuth-only provider was reported as api_key_env:\n%s", output)
	}
}

func TestDiagOpenAICodexIncompleteOAuthIsMissing(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential auth.OAuthCredential
	}{
		{
			name: "missing refresh token",
			credential: auth.OAuthCredential{
				AccessToken: "access-must-not-appear", ExpiresAt: time.Now().Add(time.Hour), AccountID: "account-must-not-appear",
			},
		},
		{
			name: "missing expiry",
			credential: auth.OAuthCredential{
				AccessToken: "access-must-not-appear", RefreshToken: "refresh-must-not-appear", AccountID: "account-must-not-appear",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			writeDiagConfig(t, home)
			if err := auth.PutOAuth("openai-codex", test.credential); err != nil {
				t.Fatal(err)
			}

			output, err := captureDiagOutput(t, context.Background())
			if err == nil || !strings.Contains(output, "OAuth credential") || !strings.Contains(output, "missing") || !strings.Contains(output, "metis login openai-codex") {
				t.Fatalf("incomplete credential diagnostic = err %v:\n%s", err, output)
			}
			for _, secret := range []string{test.credential.AccessToken, test.credential.RefreshToken, test.credential.AccountID} {
				if secret != "" && strings.Contains(output, secret) {
					t.Fatalf("diag output leaked credential field:\n%s", output)
				}
			}
		})
	}
}

func TestDiagAnthropicCredentialMatchesRuntimeRules(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		apiKey    string
		wantError bool
		want      []string
		forbidden []string
	}{
		{
			name: "official OAuth", baseURL: "https://api.anthropic.com",
			want:      []string{"provider.default", "anthropic", "OAuth credential", "configured"},
			forbidden: []string{"ANTHROPIC_API_KEY", "oauth-access-must-not-appear"},
		},
		{
			name: "custom gateway rejects OAuth", baseURL: "https://gateway.example/v1", wantError: true,
			want:      []string{"OAuth credential", "cannot be used", "non-Anthropic base_url"},
			forbidden: []string{"oauth-access-must-not-appear"},
		},
		{
			name: "API key wins on custom gateway", baseURL: "https://gateway.example/v1", apiKey: "api-key-must-not-appear",
			want:      []string{"API credential", "configured"},
			forbidden: []string{"api-key-must-not-appear", "oauth-access-must-not-appear"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			contents := "[provider]\ndefault = \"anthropic\"\n\n[provider.anthropic]\nmodel = \"claude-diag\"\nbase_url = \"" + test.baseURL + "\"\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := auth.PutOAuth("anthropic", auth.OAuthCredential{AccessToken: "oauth-access-must-not-appear"}); err != nil {
				t.Fatal(err)
			}
			if test.apiKey != "" {
				if err := auth.ActivateAPIKeyBound("anthropic", test.apiKey, "anthropic_messages", test.baseURL); err != nil {
					t.Fatal(err)
				}
			}
			output, err := captureDiagOutput(t, context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("cmdDiag() error = %v, wantError=%t\n%s", err, test.wantError, output)
			}
			for _, want := range test.want {
				if !strings.Contains(output, want) {
					t.Fatalf("diag output missing %q:\n%s", want, output)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(output, forbidden) {
					t.Fatalf("diag output contains secret/incorrect remediation %q:\n%s", forbidden, output)
				}
			}
		})
	}
}

func TestDiagCloudProvidersUseTransportSpecificCredentialGuidance(t *testing.T) {
	for _, test := range []struct {
		name      string
		provider  string
		profile   string
		want      string
		forbidden string
	}{
		{
			name: "vertex", provider: "vertex-route", want: "Vertex service account",
			forbidden: "metis login vertex-route",
			profile: `[provider.custom.vertex-route]
transport = "vertex_anthropic"
model = "claude-test"
project = "test-project"
region = "us-east5"
service_account_file = "/missing/service-account.json"
`,
		},
		{
			name: "bedrock", provider: "bedrock-route", want: "AWS credentials",
			forbidden: "metis login bedrock-route",
			profile: `[provider.custom.bedrock-route]
transport = "bedrock_anthropic"
model = "anthropic.claude-test"
region = "us-east-1"
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Chdir(t.TempDir())
			body := "[provider]\ndefault = \"" + test.provider + "\"\n\n" + test.profile
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := captureDiagOutput(t, context.Background())
			if err == nil {
				t.Fatalf("cloud diagnostic unexpectedly succeeded:\n%s", output)
			}
			if !strings.Contains(output, test.want) || strings.Contains(output, test.forbidden) {
				t.Fatalf("cloud diagnostic guidance mismatch:\n%s", output)
			}
		})
	}
}

func TestDiagWebSearchBackendsNeverPrintCredentialBytes(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	t.Setenv("SERPER_API_KEY", "")
	const secret = "UNIQUESEARCHSECRET0123456789"
	t.Setenv("TAVILY_API_KEY", secret)

	d := newDiag()
	d.section("websearch backends")
	d.reportWebSearchBackends()
	for name, output := range map[string]string{"text": d.text(), "json": d.json()} {
		if strings.Contains(output, secret) || strings.Contains(output, secret[:6]) {
			t.Fatalf("%s diagnostic leaked credential bytes: %s", name, output)
		}
		if !strings.Contains(output, "TAVILY_API_KEY set via env") || !strings.Contains(output, fmt.Sprintf("%d chars", len(secret))) {
			t.Fatalf("%s diagnostic lost safe source metadata: %s", name, output)
		}
	}
}

func writeDiagConfig(t *testing.T, home string) {
	t.Helper()
	contents := []byte("[provider]\ndefault = \"openai-codex\"\n\n[provider.openai-codex]\nmodel = \"gpt-diag-codex\"\n")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func captureDiagOutput(t *testing.T, ctx context.Context) (string, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "diag-output-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	t.Cleanup(func() { os.Stdout = original })

	diagErr := cmdDiag(ctx, nil)
	os.Stdout = original
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(file.Name())
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return string(output), diagErr
}
