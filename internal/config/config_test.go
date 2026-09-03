package config

import (
	"errors"
	"os"
	"os/exec"
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
	if cfg.LoopDetection.Global != 0 {
		t.Errorf("LoopDetection.Global default = %d, want 0 (disabled — 2026-05-15 refactor C; rely on signature-window + progress detector)", cfg.LoopDetection.Global)
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
		t.Errorf("AutoCompactThreshold default = %v, want 0.85 (unified heavy compaction trigger)", cfg.Session.AutoCompactThreshold)
	}
	if cfg.Session.AutoCompactMinimumTokens != 50_000 {
		t.Errorf("AutoCompactMinimumTokens default = %d, want 50000 (DeepSeek-TUI-style absolute floor — prevents Compact churn on small/fresh sessions)", cfg.Session.AutoCompactMinimumTokens)
	}
	if cfg.UI.ThinkingDisplay != "auto" {
		t.Errorf("UI.ThinkingDisplay default = %q, want auto", cfg.UI.ThinkingDisplay)
	}
}

func TestProviderRawSupportsVisionIsTriState(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want *bool
	}{
		{name: "unset", toml: `model = "unknown"`, want: nil},
		{name: "enabled", toml: "model = \"unknown\"\nsupports_vision = true", want: boolPtrConfig(true)},
		{name: "disabled", toml: "model = \"gpt-4o\"\nsupports_vision = false", want: boolPtrConfig(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw ProviderRaw
			if _, err := toml.Decode(tt.toml, &raw); err != nil {
				t.Fatalf("toml.Decode: %v", err)
			}
			if tt.want == nil {
				if raw.SupportsVision != nil {
					t.Fatalf("SupportsVision = %v, want nil auto-detection", *raw.SupportsVision)
				}
				return
			}
			if raw.SupportsVision == nil || *raw.SupportsVision != *tt.want {
				t.Fatalf("SupportsVision = %v, want %v", raw.SupportsVision, *tt.want)
			}
		})
	}
}

func TestProviderRawCatalogProviderParses(t *testing.T) {
	var parsed struct {
		Provider struct {
			Custom map[string]ProviderRaw `toml:"custom"`
		} `toml:"provider"`
	}
	input := `[provider.custom.bigmodel]
transport = "openai_chat"
model = "glm-5.3"
catalog_provider = "zhipuai"
`
	if _, err := toml.Decode(input, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Provider.Custom["bigmodel"].CatalogProvider; got != "zhipuai" {
		t.Fatalf("CatalogProvider = %q, want zhipuai", got)
	}
}

func boolPtrConfig(v bool) *bool { return &v }

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

func TestLoad_RejectsDeprecatedSandboxTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[bash.sandbox]
mode = "permissions"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "[tools.bash.sandbox]") {
		t.Fatalf("Load() error = %v, want migration hint for [tools.bash.sandbox]", err)
	}
}

func TestLoad_ParsesCanonicalSandboxTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[tools.bash.sandbox]
mode = "permissions"
network = "block"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Tools.Bash.Sandbox.Mode; got != "permissions" {
		t.Fatalf("sandbox mode = %q, want permissions", got)
	}
	if got := cfg.Tools.Bash.Sandbox.Network; got != "block" {
		t.Fatalf("sandbox network = %q, want block", got)
	}
}

func TestLoad_RejectsUnknownSandboxMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[tools.bash.sandbox]
mode = "premissions"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "invalid tools.bash.sandbox.mode") {
		t.Fatalf("Load() error = %v, want strict sandbox mode error", err)
	}
}

func TestLoad_RejectsUnknownSandboxNetwork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[tools.bash.sandbox]
mode = "permissions"
network = "blokc"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "invalid tools.bash.sandbox.network") {
		t.Fatalf("Load() error = %v, want strict sandbox network error", err)
	}
}

func TestLoad_AcceptsCanonicalSandboxNetworkValues(t *testing.T) {
	for _, network := range []string{"", "allow", "block"} {
		t.Run("network="+network, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			body := "[tools.bash.sandbox]\nmode = \"permissions\"\n"
			if network != "" {
				body += "network = \"" + network + "\"\n"
			}
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, _, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if got := cfg.Tools.Bash.Sandbox.Network; got != network {
				t.Fatalf("sandbox network = %q, want %q", got, network)
			}
		})
	}
}

func TestLoadHooksForWorkspaceExcludesUntrustedProjectHooks(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[hooks.session_start]]
type = "command"
command = "user-hook"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(`
[[hooks.pre_tool_use]]
type = "command"
command = "project-hook"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	untrusted, err := LoadHooksForWorkspace(false)
	if err != nil {
		t.Fatalf("LoadHooksForWorkspace(false): %v", err)
	}
	if len(untrusted.SessionStart) != 1 || untrusted.SessionStart[0].Command != "user-hook" {
		t.Fatalf("user hook missing from untrusted workspace: %+v", untrusted)
	}
	if len(untrusted.PreToolUse) != 0 {
		t.Fatalf("untrusted project hook was loaded: %+v", untrusted.PreToolUse)
	}

	trusted, err := LoadHooksForWorkspace(true)
	if err != nil {
		t.Fatalf("LoadHooksForWorkspace(true): %v", err)
	}
	if len(trusted.SessionStart) != 1 || len(trusted.PreToolUse) != 1 || trusted.PreToolUse[0].Command != "project-hook" {
		t.Fatalf("trusted workspace hooks were not merged: %+v", trusted)
	}
}

func TestLoadPermissionModeForWorkspaceRejectsOnlyUntrustedProjectFullAccess(t *testing.T) {
	tests := []struct {
		name           string
		userMode       string
		projectMode    string
		projectTrusted bool
		want           string
	}{
		{
			name:     "untrusted project fullAccess keeps user default",
			userMode: "default", projectMode: "fullAccess", want: "default",
		},
		{
			name:     "untrusted project full alias keeps trusted user fullAccess",
			userMode: "fullAccess", projectMode: "full", want: "fullAccess",
		},
		{
			name:     "untrusted project may choose a safer mode",
			userMode: "fullAccess", projectMode: "plan", want: "plan",
		},
		{
			name:     "trusted project may choose fullAccess",
			userMode: "default", projectMode: "fullAccess", projectTrusted: true, want: "fullAccess",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Chdir(project)
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[permission]\nmode = \""+tt.userMode+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[permission]\nmode = \""+tt.projectMode+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := LoadPermissionModeForWorkspace(tt.projectTrusted)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("permission mode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadPermissionModeForWorkspaceRejectsUntrustedProjectLocalFullAccess(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[permission]\nmode = \"dontAsk\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.local.toml"), []byte("[permission]\nmode = \"fullAccess\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadPermissionModeForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "dontAsk" {
		t.Fatalf("permission mode = %q, want trusted user mode dontAsk", got)
	}
}

func TestSessionPermissionStateTrustedForWorkspaceTracksSessionDirSource(t *testing.T) {
	tests := []struct {
		name           string
		userSessionDir bool
		projectConfig  string
		projectTrusted bool
		want           bool
	}{
		{name: "default session store is trusted", want: true},
		{name: "user session store is trusted", userSessionDir: true, want: true},
		{name: "untrusted project without session override keeps trusted store", projectConfig: "[permission]\nmode = \"plan\"\n", want: true},
		{name: "untrusted project session override is untrusted", projectConfig: "[session]\ndir = \"project-sessions\"\n", want: false},
		{name: "trusted project session override is trusted", projectConfig: "[session]\ndir = \"project-sessions\"\n", projectTrusted: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Chdir(project)
			if tt.userSessionDir {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[session]\ndir = \"user-sessions\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.projectConfig != "" {
				if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(tt.projectConfig), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got, err := SessionPermissionStateTrustedForWorkspace(tt.projectTrusted)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("session permission trust = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadHooksForWorkspaceProjectLocalOverrideRequiresTrust(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[hooks.pre_tool_use]]
command = "user-policy"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.local.toml"), []byte(`
[[hooks.pre_tool_use]]
command = "local-project-policy"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	untrusted, err := LoadHooksForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(untrusted.PreToolUse) != 1 || untrusted.PreToolUse[0].Command != "user-policy" {
		t.Fatalf("untrusted local overlay changed user hook: %+v", untrusted.PreToolUse)
	}
	trusted, err := LoadHooksForWorkspace(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(trusted.PreToolUse) != 1 || trusted.PreToolUse[0].Command != "local-project-policy" {
		t.Fatalf("trusted local overlay did not win: %+v", trusted.PreToolUse)
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
	} else if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("missing credential error = %v, want ErrMissingAPIKey", err)
	}
}

// TestLoad_RewritesLegacyDelphiPaths is the regression for the
// 2026-05-09 bug: users who ran `delphi config init` before the
// 2026-04-29 rename had `~/.local/share/delphi/sessions` hardcoded
// in their config.toml. migrateLegacyHome() moves the data but the
// toml still pointed at the old location, so metis kept reading and
// writing to the delphi-named XDG path forever. Load() now rewrites
// the in-memory config back to the canonical ~/.metis path.
func TestLoad_RewritesLegacyDelphiPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	metisHome := filepath.Join(home, ".metis")
	t.Setenv("METIS_HOME", metisHome)
	if err := os.MkdirAll(metisHome, 0o755); err != nil {
		t.Fatal(err)
	}

	delphiSessions := filepath.Join(home, ".local", "share", "delphi", "sessions")
	delphiSkills := filepath.Join(home, ".local", "share", "delphi", "skills")
	tomlContent := "[session]\ndir = \"" + delphiSessions + "\"\nskill_dir = \"" + delphiSkills + "\"\n"
	if err := os.WriteFile(filepath.Join(metisHome, "config.toml"), []byte(tomlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantSessions := filepath.Join(metisHome, "sessions")
	wantSkills := filepath.Join(metisHome, "skills")
	if cfg.Session.Dir != wantSessions {
		t.Errorf("Session.Dir: want %q, got %q", wantSessions, cfg.Session.Dir)
	}
	if cfg.Session.SkillDir != wantSkills {
		t.Errorf("Session.SkillDir: want %q, got %q", wantSkills, cfg.Session.SkillDir)
	}
}

// TestLoad_RewritesLegacyMetisXDGPaths covers the original (pre-bug)
// case: users with `~/.local/share/metis/sessions` from a metis-era
// XDG install also get migrated.
func TestLoad_RewritesLegacyMetisXDGPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	metisHome := filepath.Join(home, ".metis")
	t.Setenv("METIS_HOME", metisHome)
	if err := os.MkdirAll(metisHome, 0o755); err != nil {
		t.Fatal(err)
	}

	xdgSessions := filepath.Join(home, ".local", "share", "metis", "sessions")
	xdgSkills := filepath.Join(home, ".local", "share", "metis", "skills")
	tomlContent := "[session]\ndir = \"" + xdgSessions + "\"\nskill_dir = \"" + xdgSkills + "\"\n"
	if err := os.WriteFile(filepath.Join(metisHome, "config.toml"), []byte(tomlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.Dir != filepath.Join(metisHome, "sessions") {
		t.Errorf("Session.Dir: %q", cfg.Session.Dir)
	}
}

// TestLoad_KeepsCustomSessionDir — users who deliberately pointed
// Session.Dir at a non-default path keep it. The legacy rewrite must
// only fire on EXACT match against a known legacy default.
func TestLoad_KeepsCustomSessionDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	metisHome := filepath.Join(home, ".metis")
	t.Setenv("METIS_HOME", metisHome)
	if err := os.MkdirAll(metisHome, 0o755); err != nil {
		t.Fatal(err)
	}

	customDir := filepath.Join(home, "my-custom-sessions")
	tomlContent := "[session]\ndir = \"" + customDir + "\"\n"
	if err := os.WriteFile(filepath.Join(metisHome, "config.toml"), []byte(tomlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.Dir != customDir {
		t.Errorf("custom Session.Dir should not be rewritten; got %q", cfg.Session.Dir)
	}
}

// TestShellDefault_PrefersBashOverLoginShell — zsh's NOMATCH aborts
// whole commands with "no matches found" when a glob matches nothing;
// bash passes the unmatched pattern through literally. The default must
// resolve to bash even when $SHELL points at zsh (user report
// 2026-08-15, aligned with DeepSeek Harness hardcoding /bin/bash).
func TestShellDefault_PrefersBashOverLoginShell(t *testing.T) {
	if bash, err := exec.LookPath("bash"); err == nil && bash != "" {
		t.Setenv("SHELL", "/bin/zsh")
		if got := shellDefault(); got != bash {
			t.Fatalf("shellDefault() = %q, want bash %q even with SHELL=/bin/zsh", got, bash)
		}
	}
}

// TestShellDefault_FallsBackToLoginShell — systems without bash must
// still get a usable default (e.g. Alpine/FreeBSD with sh/zsh only).
func TestShellDefault_FallsBackToLoginShell(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: LookPath("bash") fails
	t.Setenv("SHELL", "/bin/zsh")
	if got := shellDefault(); got != "/bin/zsh" {
		t.Fatalf("shellDefault() = %q, want SHELL fallback /bin/zsh", got)
	}
}
