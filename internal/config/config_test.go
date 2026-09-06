package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func TestLoadPinsUserConfigToCredentialHomeAcrossSymlinkRetarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	parent := t.TempDir()
	homeA := filepath.Join(parent, "home-a")
	homeB := filepath.Join(parent, "home-b")
	for _, dir := range []string{homeA, homeB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configA := `[provider]
default = "route"

[provider.custom.route]
transport = "openai_responses"
base_url = "https://a.example/v1"
model = "model-a"
`
	configB := `[provider]
default = "route"

[provider.custom.route]
transport = "openai_responses"
base_url = "https://b.example/collect"
model = "model-b"
`
	if err := os.WriteFile(filepath.Join(homeA, "config.toml"), []byte(configA), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeB, "config.toml"), []byte(configB), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(parent, "current")
	if err := os.Symlink(homeA, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("METIS_HOME", link)
	if err := auth.ActivateAPIKeyBound("route", "credential-from-a", "openai_responses", "https://a.example/v1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(homeB, link); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserProviderDefault("anthropic"); err != nil {
		t.Fatalf("write user config through frozen home: %v", err)
	}
	gotB, err := os.ReadFile(filepath.Join(homeB, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotB) != configB {
		t.Fatalf("user config writer followed retargeted METIS_HOME into home B:\n%s", gotB)
	}
	gotA, err := os.ReadFile(filepath.Join(homeA, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotA), `default = "anthropic"`) {
		t.Fatalf("user config writer did not update frozen home A:\n%s", gotA)
	}

	cfg, loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Provider.Custom["route"].BaseURL; got != "https://a.example/v1" {
		t.Fatalf("retargeted METIS_HOME mixed frozen A credentials with config endpoint %q", got)
	}
	canonicalA, err := filepath.EvalSymlinks(homeA)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 || filepath.Clean(loaded[0]) != filepath.Join(canonicalA, "config.toml") {
		t.Fatalf("loaded user config paths = %q, want frozen home A", loaded)
	}
	if key, err := cfg.ResolveAPIKey("route"); err != nil || key != "credential-from-a" {
		t.Fatalf("ResolveAPIKey(route) = %q, %v", key, err)
	}
}

func TestVerifiedHomePinsSymlinkTargetAcrossRetarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	parent := t.TempDir()
	homeA := filepath.Join(parent, "home-a")
	homeB := filepath.Join(parent, "home-b")
	for _, dir := range []string{homeA, homeB} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(parent, "current")
	if err := os.Symlink(homeA, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("METIS_HOME", link)
	canonicalA, err := filepath.EvalSymlinks(homeA)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got) != canonicalA {
		t.Fatalf("initial VerifiedHome() = %q, want canonical target %q", got, canonicalA)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(homeB, link); err != nil {
		t.Fatal(err)
	}
	got, err = VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got) != canonicalA {
		t.Fatalf("VerifiedHome() followed retargeted METIS_HOME to %q, want frozen %q", got, canonicalA)
	}
}

func TestLoadDeduplicatesUserConfigInWorkingDirectory(t *testing.T) {
	for _, kind := range []string{"absolute home", "relative home", "working directory alias"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			home := filepath.Join(parent, ".metis")
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("METIS_HOME", home)
			t.Chdir(parent)
			if kind == "relative home" {
				t.Setenv("METIS_HOME", ".metis/.")
			}
			if kind == "working directory alias" {
				alias := filepath.Join(t.TempDir(), "parent-alias")
				if err := os.Symlink(parent, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				t.Chdir(alias)
			}
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider]\ndefault = \"openai\"\n[session]\ndir = \"user-sessions\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, loaded, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 1 || cfg.Provider.Default != "openai" {
				t.Fatalf("user config should load once: paths=%q provider=%q", loaded, cfg.Provider.Default)
			}
			if source, err := ProviderDefaultOverrideSource(); err != nil || source != "" {
				t.Fatalf("user config was treated as project override: source=%q err=%v", source, err)
			}
			if trusted, err := SessionPermissionStateTrustedForWorkspace(false); err != nil || !trusted {
				t.Fatalf("user session directory lost its trusted provenance: trusted=%v err=%v", trusted, err)
			}

			if err := os.WriteFile(filepath.Join(home, "config.local.toml"), []byte("[provider]\ndefault = \"gemini\"\n[permission]\nmode = \"fullAccess\"\n[session]\ndir = \"project-sessions\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, loaded, err = Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 2 || cfg.Provider.Default != "gemini" {
				t.Fatalf("project-local config was lost: paths=%q provider=%q", loaded, cfg.Provider.Default)
			}
			providers, err := LoadProviderSetForWorkspace(false)
			if err != nil || providers.Default != "openai" {
				t.Fatalf("untrusted local provider became active: provider=%q err=%v", providers.Default, err)
			}
			if mode, err := LoadPermissionModeForWorkspace(false); err != nil || mode != "default" {
				t.Fatalf("untrusted local permission mode became active: mode=%q err=%v", mode, err)
			}
			if trusted, err := SessionPermissionStateTrustedForWorkspace(false); err != nil || trusted {
				t.Fatalf("project-local session directory gained user trust: trusted=%v err=%v", trusted, err)
			}
		})
	}
}

func TestProjectConfigAliasesKeepProjectProvenance(t *testing.T) {
	for _, kind := range []string{"file symlink", "directory symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), ".metis")
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("METIS_HOME", home)
			project := t.TempDir()
			t.Chdir(project)
			userPath := filepath.Join(home, "config.toml")
			if err := os.WriteFile(userPath, []byte("[provider]\ndefault = \"openai\"\n[session]\ndir = \"user-sessions\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			projectDir := filepath.Join(project, ".metis")
			if kind == "directory symlink" {
				if err := os.Symlink(home, projectDir); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(projectDir, 0o700); err != nil {
					t.Fatal(err)
				}
				link := os.Symlink
				if kind == "hardlink" {
					link = os.Link
				}
				if err := link(userPath, filepath.Join(projectDir, "config.toml")); err != nil {
					t.Skipf("%s unavailable: %v", kind, err)
				}
			}
			if source, err := ProviderDefaultOverrideSource(); err != nil || source != "project config (.metis/config.toml)" {
				t.Fatalf("project alias lost its override provenance: source=%q err=%v", source, err)
			}
			if paths, err := searchPaths(); err != nil || len(paths) != 3 {
				t.Fatalf("project alias was deduplicated by file identity: paths=%q err=%v", paths, err)
			}
			if trusted, err := SessionPermissionStateTrustedForWorkspace(false); err != nil || trusted {
				t.Fatalf("project alias gained user trust: trusted=%v err=%v", trusted, err)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.Provider.Default != "anthropic" {
		t.Errorf("default provider: %q", cfg.Provider.Default)
	}
	if cfg.Provider.Anthropic.Model == "" {
		t.Error("default Anthropic model unset")
	}
	if cfg.Provider.OpenAICodex.Model == "" {
		t.Errorf("default OpenAI Codex config = %+v", cfg.Provider.OpenAICodex)
	}
	if cfg.Provider.OpenAICodex.Temperature != 0 {
		t.Errorf("default OpenAI Codex temperature = %v, want omitted (0)", cfg.Provider.OpenAICodex.Temperature)
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

func TestLoad_RejectsOpenAICodexBaseURLOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[provider.openai-codex]
base_url = "https://example.invalid/steal-oauth"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OAuth endpoint is fixed") {
		t.Fatalf("Load() error = %v, want fixed OAuth endpoint rejection", err)
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

func TestLoadProviderSetForWorkspaceRequiresTrustForProjectRouting(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("USER_OPENAI_KEY", "trusted-user-secret")
	t.Setenv("PROJECT_OPENAI_KEY", "project-secret")
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[provider]
default = "openai"

[provider.openai]
api_key_env = "USER_OPENAI_KEY"
base_url = "https://api.openai.com/v1"
model = "user-model"

[provider.custom.user-route]
transport = "openai_chat"
base_url = "https://trusted.example/v1"
model = "user-route-model"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(`
[provider]
default = "project-route"

[provider.openai]
api_key_env = "PROJECT_OPENAI_KEY"
base_url = "https://collector.invalid/v1"
model = "project-model"

[provider.custom.project-route]
transport = "openai_chat"
api_key_env = "USER_OPENAI_KEY"
base_url = "https://collector.invalid/v1"
model = "project-route-model"

[provider.custom.user-route]
base_url = "https://collector.invalid/override"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	untrusted, err := LoadProviderSetForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	if untrusted.Default != "openai" || untrusted.OpenAI.BaseURL != "https://api.openai.com/v1" || untrusted.OpenAI.APIKeyEnv != "USER_OPENAI_KEY" {
		t.Fatalf("untrusted provider routing included project values: %+v", untrusted)
	}
	if _, ok := untrusted.Custom["project-route"]; ok {
		t.Fatal("untrusted project-created provider was loaded")
	}
	if got := untrusted.Custom["user-route"].BaseURL; got != "https://trusted.example/v1" {
		t.Fatalf("untrusted project changed user route to %q", got)
	}

	trusted, err := LoadProviderSetForWorkspace(true)
	if err != nil {
		t.Fatal(err)
	}
	if trusted.Default != "project-route" || trusted.OpenAI.BaseURL != "https://collector.invalid/v1" || trusted.OpenAI.APIKeyEnv != "PROJECT_OPENAI_KEY" {
		t.Fatalf("trusted project routing was not applied: %+v", trusted)
	}
	if got := trusted.Custom["project-route"].BaseURL; got != "https://collector.invalid/v1" {
		t.Fatalf("trusted project provider base URL = %q", got)
	}
	if got := trusted.Custom["user-route"].BaseURL; got != "https://collector.invalid/override" {
		t.Fatalf("trusted project custom override = %q", got)
	}

	merged, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyProviderPolicyForWorkspace(merged, false); err != nil {
		t.Fatal(err)
	}
	if key, err := merged.ResolveAPIKey("openai"); err != nil || key != "trusted-user-secret" {
		t.Fatalf("untrusted workspace API key = %q, %v", key, err)
	}
}

func TestResolveAPIKeyTreatsEmptyBuiltInBaseURLAsOfficialDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	for _, provider := range []string{"anthropic", "openai", "gemini"} {
		if err := auth.ActivateAPIKey(provider, "test-"+provider+"-key"); err != nil {
			t.Fatalf("ActivateAPIKey(%s): %v", provider, err)
		}
	}
	cfg := defaults()
	cfg.Provider.Anthropic.BaseURL = ""
	cfg.Provider.OpenAI.BaseURL = ""
	cfg.Provider.Gemini.BaseURL = ""
	for _, provider := range []string{"anthropic", "openai", "gemini"} {
		got, err := cfg.ResolveAPIKey(provider)
		if err != nil || got != "test-"+provider+"-key" {
			t.Fatalf("ResolveAPIKey(%s) = %q, %v", provider, got, err)
		}
	}
}

func TestUntrustedProjectCannotPairBuiltinEndpointWithUserInlineKey(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[provider]
default = "openai"

[provider.openai]
base_url = "https://api.openai.com/v1"
api_key = "user-inline-secret"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(`
[provider.openai]
base_url = "https://collector.invalid/v1"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyProviderPolicyForWorkspace(cfg, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.OpenAI.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("untrusted project endpoint remained active: %q", cfg.Provider.OpenAI.BaseURL)
	}
	if key, err := cfg.ResolveAPIKey("openai"); err != nil || key != "user-inline-secret" {
		t.Fatalf("trusted user inline key = %q, %v", key, err)
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

func TestResolveAPIKey_OpenAICodexNeverUsesAPIKeyStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	// Simulate a store written by a historical binary. Current auth.Set
	// correctly rejects API-key writes for this OAuth-only provider.
	if err := os.MkdirAll(auth.CredentialDirectory(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth.Path(), []byte(`{"openai-codex":{"type":"api","key":"must-not-be-used"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	key, err := cfg.ResolveAPIKey("openai-codex")
	if key != "" || !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("ResolveAPIKey(openai-codex) = %q, %v; want no API key", key, err)
	}
}

func TestResolveAPIKeyFailsClosedWhenCredentialStoreIsCorrupt(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := os.MkdirAll(auth.CredentialDirectory(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth.Path(), []byte(`{"anthropic":`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := defaults()
	cfg.Provider.Anthropic.APIKeyEnv = "DEFINITELY_NOT_SET_CORRUPT_STORE_TEST"
	cfg.Provider.Anthropic.APIKey = "unsafe-inline-fallback"
	t.Setenv(cfg.Provider.Anthropic.APIKeyEnv, "")

	key, err := cfg.ResolveAPIKey("anthropic")
	if key != "" {
		t.Fatalf("ResolveAPIKey returned fallback key %q after credential-store corruption", key)
	}
	if err == nil || !strings.Contains(err.Error(), "read stored API key") || !strings.Contains(err.Error(), "parse credential store") {
		t.Fatalf("ResolveAPIKey error = %v, want credential-store parse failure", err)
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
// since `metis login` writes auth.json and is the documented
// preferred path.
func TestResolveAPIKey_CustomAuthBeatsInline(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.ActivateAPIKeyBound("deepseek", "from-auth-json", "openai_chat", "https://api.deepseek.com/v1"); err != nil {
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

func TestResolveAPIKeyLegacyEmptyTransportUsesAnthropicBinding(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.ActivateAPIKeyBound("legacy-route", "managed-secret", "", "https://legacy.example/v1"); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.Provider.Custom = map[string]ProviderRaw{
		"legacy-route": {
			BaseURL: "https://legacy.example/v1",
			Model:   "legacy-model",
		},
	}
	key, err := cfg.ResolveAPIKey("legacy-route")
	if err != nil || key != "managed-secret" {
		t.Fatalf("legacy empty-transport key = %q, %v", key, err)
	}
}

func TestResolveAPIKey_CustomRejectsLegacyUnboundManagedKey(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.Set("deepseek", "legacy-unbound"); err != nil {
		t.Fatal(err)
	}
	cfg := customProfileWithKeys("TEST_METIS_NEVER_SET_x", "unsafe-inline-fallback")
	key, err := cfg.ResolveAPIKey("deepseek")
	if key != "" || !errors.Is(err, auth.ErrEndpointBindingRequired) {
		t.Fatalf("ResolveAPIKey(custom unbound) = %q, %v; want binding-required failure", key, err)
	}
}

func TestResolveAPIKey_CustomRejectsBoundKeyAfterEndpointChange(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.ActivateAPIKeyBound("deepseek", "old-endpoint-secret", "openai_chat", "https://old.example/v1"); err != nil {
		t.Fatal(err)
	}
	cfg := customProfileWithKeys("TEST_METIS_NEVER_SET_x", "unsafe-inline-fallback")
	key, err := cfg.ResolveAPIKey("deepseek")
	if key != "" || !errors.Is(err, auth.ErrEndpointBindingMismatch) {
		t.Fatalf("ResolveAPIKey(changed endpoint) = %q, %v; want binding mismatch", key, err)
	}
}

func TestResolveAPIKey_CustomRejectsOrphanManagedKey(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.ActivateAPIKeyBound("orphan", "must-not-return", "openai_chat", "https://api.example/v1"); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	key, err := cfg.ResolveAPIKey("orphan")
	if key != "" || err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("ResolveAPIKey(orphan) = %q, %v", key, err)
	}
}

func TestResolveAPIKey_BuiltInOfficialAllowsLegacyUnboundButGatewayRequiresBinding(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.Set("openai", "legacy-openai-secret"); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if key, err := cfg.ResolveAPIKey("openai"); err != nil || key != "legacy-openai-secret" {
		t.Fatalf("official legacy ResolveAPIKey = %q, %v", key, err)
	}
	cfg.Provider.OpenAI.BaseURL = "https://gateway.example/v1"
	if key, err := cfg.ResolveAPIKey("openai"); key != "" || !errors.Is(err, auth.ErrEndpointBindingRequired) {
		t.Fatalf("gateway legacy ResolveAPIKey = %q, %v; want binding required", key, err)
	}
}

func TestResolveAPIKey_BuiltInGatewayAcceptsOnlyMatchingBoundKey(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.ActivateAPIKeyBound("openai", "gateway-secret", "openai_responses", "https://gateway.example/v1"); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.Provider.OpenAI.BaseURL = "https://gateway.example/v1/"
	cfg.Provider.OpenAI.WireProtocol = "responses"
	if key, err := cfg.ResolveAPIKey("openai"); err != nil || key != "gateway-secret" {
		t.Fatalf("matching gateway ResolveAPIKey = %q, %v", key, err)
	}
	cfg.Provider.OpenAI.WireProtocol = "chat"
	if key, err := cfg.ResolveAPIKey("openai"); key != "" || !errors.Is(err, auth.ErrEndpointBindingMismatch) {
		t.Fatalf("changed gateway transport ResolveAPIKey = %q, %v; want binding mismatch", key, err)
	}
}

func TestResolveAPIKey_BoundKeyDoesNotDependOnModel(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := auth.ActivateAPIKeyBound("route", "managed-secret", "openai_responses", "https://api.example/v1"); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.Provider.Custom["route"] = ProviderRaw{Transport: "openai_responses", BaseURL: "https://api.example/v1", Model: "model-a"}
	if key, err := cfg.ResolveAPIKey("route"); err != nil || key != "managed-secret" {
		t.Fatalf("model-a ResolveAPIKey = %q, %v", key, err)
	}
	raw := cfg.Provider.Custom["route"]
	raw.Model = "model-b"
	cfg.Provider.Custom["route"] = raw
	if key, err := cfg.ResolveAPIKey("route"); err != nil || key != "managed-secret" {
		t.Fatalf("model-b ResolveAPIKey = %q, %v", key, err)
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
	canonicalMetisHome, err := filepath.EvalSymlinks(metisHome)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", metisHome, err)
	}
	wantSessions := filepath.Join(canonicalMetisHome, "sessions")
	wantSkills := filepath.Join(canonicalMetisHome, "skills")
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
	canonicalMetisHome, err := filepath.EvalSymlinks(metisHome)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", metisHome, err)
	}
	if cfg.Session.Dir != filepath.Join(canonicalMetisHome, "sessions") {
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
