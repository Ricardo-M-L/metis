package runtime

// Coverage for the custom-provider dispatch path added when wiring
// [provider.custom.<id>] profiles to BuildProvider. Pure config-side
// shape verification — no network calls, no model invocations.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/cloud"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

func boolPtr(v bool) *bool { return &v }

func newCfgWithCustom(name string, raw config.ProviderRaw) *config.Config {
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{name: raw}
	return cfg
}

func writeRuntimeTestServiceAccount(t *testing.T) string {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("x509.MarshalPKCS8PrivateKey: %v", err)
	}
	data, err := json.Marshal(cloud.ServiceAccountKey{
		Type:        "service_account",
		ClientEmail: "metis-test@example.iam.gserviceaccount.com",
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		TokenURI:    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write service account: %v", err)
	}
	return path
}

func TestBuildProvider_Custom_AnthropicTransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("minimax-anthropic", config.ProviderRaw{
		Transport:   "anthropic_messages",
		APIKeyEnv:   "FAKE_KEY",
		BaseURL:     "https://api.example.com/anthropic",
		Model:       "MiniMax-M2.7",
		MaxTokens:   8192,
		TimeoutSecs: 60,
	})

	pb, err := BuildProvider(cfg, "minimax-anthropic", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if pb == nil || pb.Provider == nil {
		t.Fatal("nil provider")
	}
	if pb.Provider.Name() != "anthropic" {
		t.Errorf("transport routing: provider.Name() = %q, want anthropic", pb.Provider.Name())
	}
	if pb.Model != "MiniMax-M2.7" {
		t.Errorf("model: got %q, want MiniMax-M2.7", pb.Model)
	}
}

func TestBuildProvider_Custom_OpenAITransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("deepseek", config.ProviderRaw{
		Transport: "openai_chat",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://api.deepseek.com/v1",
		Model:     "deepseek-chat",
	})

	pb, err := BuildProvider(cfg, "deepseek", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if pb.Provider.Name() != "openai" {
		t.Errorf("provider.Name() = %q, want openai", pb.Provider.Name())
	}
	if pb.MaxOutputTokens != transport.DefaultMaxOutputTokens {
		t.Errorf("effective default max_tokens = %d, want %d", pb.MaxOutputTokens, transport.DefaultMaxOutputTokens)
	}
}

func TestBuildProvider_CustomPropagatesCatalogProviderIdentity(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	tests := []struct {
		name             string
		transport        string
		catalogProvider  string
		want             string
		assertProviderID func(t *testing.T, provider pubprov.Provider) string
	}{
		{
			name: "explicit OpenAI-compatible catalog provider", transport: "openai_chat",
			catalogProvider: "zhipuai", want: "zhipuai",
			assertProviderID: func(t *testing.T, provider pubprov.Provider) string {
				t.Helper()
				p, ok := provider.(*openai.OpenAI)
				if !ok {
					t.Fatalf("provider type = %T, want *openai.OpenAI", provider)
				}
				return p.CatalogProvider
			},
		},
		{
			name: "explicit Anthropic-compatible catalog provider", transport: "anthropic_messages",
			catalogProvider: "minimax", want: "minimax",
			assertProviderID: func(t *testing.T, provider pubprov.Provider) string {
				t.Helper()
				p, ok := provider.(*anthropic.Anthropic)
				if !ok {
					t.Fatalf("provider type = %T, want *anthropic.Anthropic", provider)
				}
				return p.CatalogProvider
			},
		},
		{
			name: "profile id is the default catalog provider", transport: "openai_chat",
			want: "profile-id",
			assertProviderID: func(t *testing.T, provider pubprov.Provider) string {
				t.Helper()
				p, ok := provider.(*openai.OpenAI)
				if !ok {
					t.Fatalf("provider type = %T, want *openai.OpenAI", provider)
				}
				return p.CatalogProvider
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newCfgWithCustom("profile-id", config.ProviderRaw{
				Transport: tt.transport, APIKeyEnv: "FAKE_KEY",
				BaseURL: "https://api.example.invalid/v1", Model: "shared-model",
				CatalogProvider: tt.catalogProvider,
			})
			pb, err := BuildProvider(cfg, "profile-id", "")
			if err != nil {
				t.Fatal(err)
			}
			if got := tt.assertProviderID(t, pb.Provider); got != tt.want {
				t.Fatalf("catalog provider = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildProviderRejectsOutputBudgetAtOrAboveWindow(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("bad-window", config.ProviderRaw{
		Transport: "openai_chat", APIKeyEnv: "FAKE_KEY", Model: "small-model",
		ContextWindow: 4_096, MaxTokens: 4_096,
	})
	_, err := BuildProvider(cfg, "bad-window", "")
	if err == nil || !strings.Contains(err.Error(), "must be smaller than context_window") {
		t.Fatalf("invalid max_tokens/window should fail clearly, got %v", err)
	}
}

func TestBuildProvider_CustomVisionOverride(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	tests := []struct {
		name     string
		model    string
		override bool
	}{
		{name: "force unknown model on", model: "vendor-new-multimodal", override: true},
		{name: "force known vision model off", model: "gpt-4o", override: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newCfgWithCustom("custom-provider", config.ProviderRaw{
				Transport:      "openai_chat",
				APIKeyEnv:      "FAKE_KEY",
				BaseURL:        "https://api.example.invalid/v1",
				Model:          tt.model,
				SupportsVision: boolPtr(tt.override),
			})
			pb, err := BuildProvider(cfg, "custom-provider", "")
			if err != nil {
				t.Fatalf("BuildProvider: %v", err)
			}
			if got := pubprov.ProviderSupportsVision(pb.Provider); got != tt.override {
				t.Fatalf("ProviderSupportsVision(%q) = %v, want override %v", tt.model, got, tt.override)
			}
		})
	}
}

func TestBuildProvider_CustomVisionOverridePreservesOptionalHistoryPolicy(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	tests := []struct {
		name         string
		transport    string
		wantPolicy   bool
		wantThinking bool
	}{
		{
			name:         "chat provider does not gain history policy",
			transport:    "openai_chat",
			wantPolicy:   false,
			wantThinking: true,
		},
		{
			name:         "responses provider retains history policy",
			transport:    "openai_responses",
			wantPolicy:   true,
			wantThinking: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newCfgWithCustom("custom-provider", config.ProviderRaw{
				Transport:      tt.transport,
				APIKeyEnv:      "FAKE_KEY",
				BaseURL:        "https://api.example.invalid/v1",
				Model:          "vendor-private-model",
				SupportsVision: boolPtr(true),
			})
			pb, err := BuildProvider(cfg, "custom-provider", "")
			if err != nil {
				t.Fatalf("BuildProvider: %v", err)
			}
			policy, gotPolicy := pb.Provider.(pubprov.ContextHistoryPolicy)
			if gotPolicy != tt.wantPolicy {
				t.Fatalf("ContextHistoryPolicy implemented = %v, want %v", gotPolicy, tt.wantPolicy)
			}
			if gotPolicy {
				gotThinking := policy.ContextIncludesAssistantBlock(pubprov.ContentBlock{Type: "thinking"})
				if gotThinking != tt.wantThinking {
					t.Fatalf("thinking replay policy = %v, want %v", gotThinking, tt.wantThinking)
				}
			}
		})
	}
}

func TestBuildProvider_SenseNovaFlashLiteIsVisionCapable(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("sensenova", config.ProviderRaw{
		Transport: "openai_chat",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://token.sensenova.cn/v1",
		Model:     "sensenova-6.8-flash-lite",
	})
	pb, err := BuildProvider(cfg, "sensenova", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if got := pubprov.ProviderVisionCapability(pb.Provider); got != pubprov.VisionSupported {
		t.Fatalf("SenseNova capability = %v, want VisionSupported", got)
	}
}

func TestBuildProvider_CustomGeminiRejectsPositiveVisionOverride(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("custom-provider", config.ProviderRaw{
		Transport:      "gemini_native",
		APIKeyEnv:      "FAKE_KEY",
		BaseURL:        "https://generativelanguage.googleapis.com",
		Model:          "gemini-2.5-pro",
		SupportsVision: boolPtr(true),
	})
	_, err := BuildProvider(cfg, "custom-provider", "")
	if err == nil || !strings.Contains(err.Error(), "supports_vision=true") {
		t.Fatalf("positive Gemini override should fail clearly until its encoder supports images, got %v", err)
	}
}

func TestBuildProvider_CustomTransportReportsEffectiveDefaultModel(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	tests := []struct {
		name      string
		transport string
		wantModel string
	}{
		{name: "openai", transport: "openai_chat", wantModel: "gpt-4o-mini"},
		{name: "anthropic", transport: "anthropic_messages", wantModel: "claude-opus-4-7"},
		{name: "gemini", transport: "gemini_native", wantModel: "gemini-2.5-pro"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newCfgWithCustom("custom-provider", config.ProviderRaw{
				Transport: tt.transport,
				APIKeyEnv: "FAKE_KEY",
				BaseURL:   "https://api.example.invalid",
			})
			pb, err := BuildProvider(cfg, "custom-provider", "")
			if err != nil {
				t.Fatalf("BuildProvider: %v", err)
			}
			if pb.Model != tt.wantModel || pb.Provider.ModelID() != tt.wantModel {
				t.Fatalf("effective models = build %q provider %q, want %q", pb.Model, pb.Provider.ModelID(), tt.wantModel)
			}
		})
	}
}

func TestBuildProvider_Custom_GeminiTransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "AIza-test")
	cfg := newCfgWithCustom("vertex-shim", config.ProviderRaw{
		Transport: "gemini_native",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta",
		Model:     "gemini-2.0-flash",
	})

	pb, err := BuildProvider(cfg, "vertex-shim", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if pb.Provider.Name() != "gemini" {
		t.Errorf("provider.Name() = %q, want gemini", pb.Provider.Name())
	}
}

func TestBuildProvider_Custom_VertexUsesServiceAccountWithoutAPIKey(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := newCfgWithCustom("vertex-claude", config.ProviderRaw{
		Transport:          "vertex_anthropic",
		ServiceAccountFile: writeRuntimeTestServiceAccount(t),
		Project:            "metis-test-project",
		Region:             "us-central1",
		Model:              "claude-sonnet-4-6",
	})

	pb, err := BuildProvider(cfg, "vertex-claude", "")
	if err != nil {
		t.Fatalf("BuildProvider rejected service-account auth: %v", err)
	}
	if pb.Provider.Name() != "vertex" || pb.Model != "claude-sonnet-4-6" {
		t.Fatalf("unexpected Vertex build: provider=%q model=%q", pb.Provider.Name(), pb.Model)
	}
}

func TestBuildProvider_Custom_BedrockUsesStandardAWSEnvironment(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_METIS_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "metis-test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "metis-test-session")
	cfg := newCfgWithCustom("bedrock-claude", config.ProviderRaw{
		Transport: "bedrock_anthropic",
		Region:    "us-east-1",
		Model:     "us.anthropic.claude-sonnet-4-6-v1:0",
	})

	pb, err := BuildProvider(cfg, "bedrock-claude", "")
	if err != nil {
		t.Fatalf("BuildProvider rejected standard AWS credentials: %v", err)
	}
	if pb.Provider.Name() != "bedrock" || pb.Model != "us.anthropic.claude-sonnet-4-6-v1:0" {
		t.Fatalf("unexpected Bedrock build: provider=%q model=%q", pb.Provider.Name(), pb.Model)
	}
}

func TestBuildProvider_Custom_BedrockRequiresSecretHalf(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_METIS_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	cfg := newCfgWithCustom("bedrock-claude", config.ProviderRaw{
		Transport: "bedrock_anthropic",
		Model:     "us.anthropic.claude-sonnet-4-6-v1:0",
	})

	_, err := BuildProvider(cfg, "bedrock-claude", "")
	if err == nil || !strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("missing AWS secret should fail clearly, got %v", err)
	}
}

func TestBuildProvider_Custom_DefaultsToAnthropicWhenTransportEmpty(t *testing.T) {
	// Empty transport is the legacy/permissive default — most users
	// pointing at "an Anthropic-compatible gateway" don't think to set
	// transport, and that's the historically common case.
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("legacy", config.ProviderRaw{
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://example.com/anthropic",
		Model:     "claude-3.5-sonnet",
	})

	pb, err := BuildProvider(cfg, "legacy", "")
	if err != nil {
		t.Fatalf("BuildProvider with empty transport: %v", err)
	}
	if pb.Provider.Name() != "anthropic" {
		t.Errorf("empty transport should default to anthropic; got %q", pb.Provider.Name())
	}
}

func TestBuildProvider_Custom_UnknownTransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("weird", config.ProviderRaw{
		Transport: "websocket-rpc-v3",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://example.com",
		Model:     "x",
	})

	_, err := BuildProvider(cfg, "weird", "")
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
}

func TestBuildProvider_Custom_ModelOverride(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("ds", config.ProviderRaw{
		Transport: "openai_chat",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://api.deepseek.com/v1",
		Model:     "deepseek-chat",
	})

	pb, err := BuildProvider(cfg, "ds", "deepseek-reasoner")
	if err != nil {
		t.Fatalf("BuildProvider with override: %v", err)
	}
	if pb.Model != "deepseek-reasoner" {
		t.Errorf("override: got %q, want deepseek-reasoner", pb.Model)
	}
}

func TestBuildProvider_UnknownProfileGivesHelpfulError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{
		"deepseek": {Transport: "openai_chat"},
		"minimax":  {Transport: "anthropic_messages"},
	}
	_, err := BuildProvider(cfg, "no-such-profile", "")
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
	msg := err.Error()
	for _, want := range []string{"no-such-profile", "deepseek", "minimax", "anthropic", "openai", "gemini"} {
		if !contains(msg, want) {
			t.Errorf("error message %q should mention %q so the user can tab-complete", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
