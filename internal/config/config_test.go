package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.Provider.Default != "anthropic" {
		t.Errorf("default provider: %q", cfg.Provider.Default)
	}
	if cfg.Provider.Anthropic.Model == "" {
		t.Error("default Anthropic model unset")
	}
	if !cfg.LoopDetection.Enabled {
		t.Error("LoopDetection should default to enabled")
	}
	if cfg.LoopDetection.Global != 80 {
		t.Errorf("LoopDetection.Global default = %d, want 80", cfg.LoopDetection.Global)
	}
	if cfg.LoopDetection.SignatureWindow != 10 {
		t.Errorf("LoopDetection.SignatureWindow default = %d, want 10", cfg.LoopDetection.SignatureWindow)
	}
	if cfg.LoopDetection.SignatureMaxRepeats != 5 {
		t.Errorf("LoopDetection.SignatureMaxRepeats default = %d, want 5", cfg.LoopDetection.SignatureMaxRepeats)
	}
	if cfg.Tools.Bash.TimeoutSeconds != 120 {
		t.Errorf("Bash timeout default = %d, want 120", cfg.Tools.Bash.TimeoutSeconds)
	}
	if cfg.Session.AutoCompactThreshold != 0.85 {
		t.Errorf("AutoCompactThreshold default = %v, want 0.85", cfg.Session.AutoCompactThreshold)
	}
}

// Regression: the security audit's three required denylist patterns must
// be present out-of-the-box so a fresh install isn't immediately yelling.
func TestDefaults_BashDenylistCoversAuditFloor(t *testing.T) {
	cfg := defaults()
	deny := cfg.Tools.Bash.Denylist
	for _, required := range []string{"rm -rf /", "dd of=/dev", ":(){"} {
		found := false
		for _, d := range deny {
			if strings.Contains(d, required) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("default denylist missing %q (got %v)", required, deny)
		}
	}
}

func TestMCPServersParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	err := os.WriteFile(path, []byte(`
[[mcp.servers]]
name = "filesystem"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]

[[mcp.servers]]
name = "github"
command = "/usr/local/bin/mcp-github"
disabled = true
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	cfg := defaults()
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		t.Fatalf("toml.DecodeFile: %v", err)
	}

	if len(cfg.MCP.Servers) != 2 {
		t.Fatalf("expected 2 MCP servers, got %d", len(cfg.MCP.Servers))
	}
	s0 := cfg.MCP.Servers[0]
	if s0.Name != "filesystem" || s0.Command != "npx" || s0.Disabled {
		t.Errorf("server[0] = %+v", s0)
	}
	if len(s0.Args) != 3 || s0.Args[0] != "-y" {
		t.Errorf("server[0].Args = %v", s0.Args)
	}
	if s1 := cfg.MCP.Servers[1]; !s1.Disabled {
		t.Errorf("server[1].Disabled should be true: %+v", s1)
	}
}

// Note: the lazy-MCP-schema feature is no longer config-driven. It's
// controlled by the ENABLE_TOOL_SEARCH env var; the parse table is
// covered in internal/agent/lazy_tools_test.go::TestParseEnableToolSearch.

func TestLoopDetectionParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(`
[loop_detection]
enabled = false
warning = 5
critical = 12
global = 100
`), 0o644)

	cfg := defaults()
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.LoopDetection.Enabled {
		t.Error("Enabled should be false")
	}
	if cfg.LoopDetection.Critical != 12 {
		t.Errorf("Critical = %d", cfg.LoopDetection.Critical)
	}
}

func TestResolveAPIKey_FromEnv(t *testing.T) {
	const envName = "TEST_METIS_RESOLVE_KEY_x"
	cfg := defaults()
	cfg.Provider.Anthropic.APIKeyEnv = envName
	cfg.Provider.Anthropic.APIKey = ""

	os.Setenv(envName, "secret123")
	defer os.Unsetenv(envName)

	key, err := cfg.ResolveAPIKey("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if key != "secret123" {
		t.Errorf("key = %q, want secret123", key)
	}
}

func TestResolveAPIKey_FromFile(t *testing.T) {
	// Scope METIS_HOME so the dev-machine auth.json (which may have a
	// real minimax key the compat fallback would surface as "anthropic")
	// doesn't leak into this test.
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := defaults()
	cfg.Provider.Anthropic.APIKeyEnv = "DEFINITELY_NOT_SET_x"
	cfg.Provider.Anthropic.APIKey = "from-file"
	os.Unsetenv("DEFINITELY_NOT_SET_x")

	key, err := cfg.ResolveAPIKey("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if key != "from-file" {
		t.Errorf("key = %q, want from-file", key)
	}
}

func TestResolveAPIKey_Missing(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := defaults()
	cfg.Provider.Anthropic.APIKeyEnv = "ALSO_DEFINITELY_NOT_SET_x"
	cfg.Provider.Anthropic.APIKey = ""

	if _, err := cfg.ResolveAPIKey("anthropic"); err == nil {
		t.Error("expected error when no key is set anywhere")
	}
}

func TestResolveAPIKey_UnknownProvider(t *testing.T) {
	cfg := defaults()
	if _, err := cfg.ResolveAPIKey("definitely-not-a-provider"); err == nil {
		t.Error("expected error for unknown provider")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Custom-provider 4-tier resolution chain. Pre-2026-05-09 the default
// branch in ResolveAPIKey only honored env + auth.json — inline
// `api_key = "..."` in [provider.custom.<id>] was dead code. These
// tests pin all four reachable outcomes so a future "let's prune
// fallbacks" refactor can't silently re-break inline keys.
// ─────────────────────────────────────────────────────────────────────

// customProfileWithKeys returns a Config whose [provider.custom.deepseek]
// has the requested env / inline keys. auth.json placement is the
// caller's job (write the file under METIS_HOME).
func customProfileWithKeys(envName, inlineKey string) *Config {
	cfg := defaults()
	cfg.Provider.Custom = map[string]ProviderRaw{
		"deepseek": {
			Transport: "openai_chat",
			APIKeyEnv: envName,
			APIKey:    inlineKey,
			BaseURL:   "https://api.deepseek.com/v1",
			Model:     "deepseek-v4-pro",
		},
	}
	return cfg
}

func TestResolveAPIKey_CustomFromEnv(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const envName = "TEST_METIS_DEEPSEEK_KEY_x"
	t.Setenv(envName, "from-env")

	cfg := customProfileWithKeys(envName, "from-inline")
	key, err := cfg.ResolveAPIKey("deepseek")
	if err != nil {
		t.Fatal(err)
	}
	// Env beats inline — same precedence as built-in providers.
	if key != "from-env" {
		t.Errorf("env should win over inline; got %q", key)
	}
}

// TestResolveAPIKey_CustomFromInline — the regression test: with no
// env var set and no auth.json entry, the inline `api_key` field
// MUST be returned. This is the case the bug fix targets.
func TestResolveAPIKey_CustomFromInline(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const envName = "TEST_METIS_DEEPSEEK_KEY_NOT_SET_x"
	os.Unsetenv(envName)

	cfg := customProfileWithKeys(envName, "from-inline")
	key, err := cfg.ResolveAPIKey("deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if key != "from-inline" {
		t.Errorf("inline api_key should be returned when env+auth.json both miss; got %q", key)
	}
}

// TestResolveAPIKey_CustomAuthBeatsInline — auth.json sits between
// env and inline in the chain, so a profile with both an inline key
// AND an auth.json entry should return auth.json. Pins the order
// since `metis auth login` writes auth.json and is the documented
// preferred path.
func TestResolveAPIKey_CustomAuthBeatsInline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(home+"/auth.json",
		[]byte(`{"deepseek":{"type":"api","key":"from-auth-json"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := customProfileWithKeys("TEST_METIS_NEVER_SET_x", "from-inline")

	key, err := cfg.ResolveAPIKey("deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if key != "from-auth-json" {
		t.Errorf("auth.json should beat inline; got %q", key)
	}
}

// TestResolveAPIKey_CustomMissingEverywhere — three slots empty, the
// 4-tier chain returns the standard error. Without this we couldn't
// tell apart "fell through silently" from "no key configured."
func TestResolveAPIKey_CustomMissingEverywhere(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := customProfileWithKeys("TEST_METIS_NEVER_SET_x", "")
	if _, err := cfg.ResolveAPIKey("deepseek"); err == nil {
		t.Error("expected 'missing API key' error when all 3 slots empty")
	}
}
