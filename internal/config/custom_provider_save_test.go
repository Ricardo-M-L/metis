package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestSaveUserCustomProvider_CreatesMinimalPrivateConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	err := SaveUserCustomProvider(CustomProviderSpec{
		ID:        "sensenova",
		Transport: "openai_chat",
		BaseURL:   "https://token.sensenova.cn/v1",
		Model:     "sensenova-6.8-flash-lite",
	})
	if err != nil {
		t.Fatalf("SaveUserCustomProvider(): %v", err)
	}

	path := filepath.Join(home, "config.toml")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("config.toml mode = %#o, want 0600", got)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Provider.Default != "sensenova" {
		t.Fatalf("provider.default = %q, want sensenova", cfg.Provider.Default)
	}
	got, ok := cfg.Provider.Custom["sensenova"]
	if !ok {
		t.Fatal("custom provider sensenova was not persisted")
	}
	if got.Transport != "openai_chat" || got.BaseURL != "https://token.sensenova.cn/v1" || got.Model != "sensenova-6.8-flash-lite" {
		t.Fatalf("custom provider = %+v", got)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, mergedDefault := range []string{"[permission]", "[session]", "api_key ="} {
		if strings.Contains(text, mergedDefault) {
			t.Errorf("minimal user config unexpectedly contains %q:\n%s", mergedDefault, text)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config.toml.") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestSaveUserCustomProvider_UpdatesSimpleTablesAndPreservesOtherText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := `# user's heading
[experimental]
future_option = "keep me"

[provider] # selected provider
default = "anthropic" # keep this comment
unknown_provider_option = 42

[provider.custom.sensenova] # existing profile
transport = "anthropic_messages" # wire format
base_url = "https://old.invalid" # endpoint
model = "old-model" # model name
api_key = "existing-inline-secret-must-stay-verbatim"
mystery = "keep this too"
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	err := SaveUserCustomProvider(CustomProviderSpec{
		ID:        "sensenova",
		Transport: "openai_chat",
		BaseURL:   "https://token.sensenova.cn/v1",
		Model:     "sensenova-6.8-flash-lite",
	})
	if err != nil {
		t.Fatalf("SaveUserCustomProvider(): %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, preserved := range []string{
		"# user's heading",
		`future_option = "keep me"`,
		"# keep this comment",
		`unknown_provider_option = 42`,
		"# existing profile",
		"# wire format",
		"# endpoint",
		"# model name",
		`api_key = "existing-inline-secret-must-stay-verbatim"`,
		`mystery = "keep this too"`,
	} {
		if !strings.Contains(text, preserved) {
			t.Errorf("updated config lost %q:\n%s", preserved, text)
		}
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v\n%s", err, text)
	}
	got := cfg.Provider.Custom["sensenova"]
	if cfg.Provider.Default != "sensenova" || got.Transport != "openai_chat" || got.BaseURL != "https://token.sensenova.cn/v1" || got.Model != "sensenova-6.8-flash-lite" {
		t.Fatalf("persisted values not updated: default=%q provider=%+v", cfg.Provider.Default, got)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode after update = %v, stat err = %v; want 0600", st.Mode().Perm(), err)
	}
}

func TestSaveUserCustomProvider_InsertsParentBeforeExistingChildTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := `[provider.custom.other]
transport = "openai_chat"
base_url = "https://example.invalid/v1"
model = "other-model"
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	err := SaveUserCustomProvider(CustomProviderSpec{
		ID:        "sensenova",
		Transport: "openai_chat",
		BaseURL:   "https://token.sensenova.cn/v1",
		Model:     "sensenova-6.8-flash-lite",
	})
	if err != nil {
		t.Fatalf("SaveUserCustomProvider(): %v", err)
	}
	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Provider.Default != "sensenova" {
		t.Fatalf("provider.default = %q", cfg.Provider.Default)
	}
	if _, ok := cfg.Provider.Custom["other"]; !ok {
		t.Fatal("existing child table was lost")
	}
	if _, ok := cfg.Provider.Custom["sensenova"]; !ok {
		t.Fatal("new child table missing")
	}
}

func TestSaveUserCustomProvider_RejectsUnsafeExistingTableWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "quoted table name",
			body: `[provider.custom."sensenova"]
transport = "openai_chat"
base_url = "https://old.invalid/v1"
model = "old"
`,
		},
		{
			name: "inline custom table",
			body: `[provider]
default = "anthropic"
custom = { sensenova = { transport = "openai_chat", base_url = "https://old.invalid/v1", model = "old" } }
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			path := filepath.Join(home, "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}

			err := SaveUserCustomProvider(CustomProviderSpec{
				ID:        "sensenova",
				Transport: "openai_chat",
				BaseURL:   "https://new.invalid/v1",
				Model:     "new-model",
			})
			if err == nil || !strings.Contains(err.Error(), "cannot safely update") {
				t.Fatalf("error = %v, want explicit safe-update error", err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tt.body {
				t.Fatalf("config mutated on error:\n%s", got)
			}
		})
	}
}

func TestSaveUserCustomProvider_RejectsInvalidSpecWithoutMutation(t *testing.T) {
	tests := []CustomProviderSpec{
		{ID: "", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "bad.id", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "BadID", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "-bad", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "openai", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "google", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "custom", Transport: "", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "custom", Transport: "openai_chat", BaseURL: "", Model: "m"},
		{ID: "custom", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: ""},
		{ID: "custom", Transport: "openai_chat", BaseURL: "https://example.invalid/v1\napi_key = \"leak\"", Model: "m"},
		{ID: "custom-provider", Transport: "unknown", BaseURL: "https://example.invalid/v1", Model: "m"},
		{ID: "custom-provider", Transport: "openai_chat", BaseURL: "token.example.invalid/v1", Model: "m"},
		{ID: "custom-provider", Transport: "openai_chat", BaseURL: "https://user:pass@example.invalid/v1", Model: "m"},
		{ID: "custom-provider", Transport: "openai_chat", BaseURL: "https://example.invalid/v1?key=secret", Model: "m"},
		{ID: "custom-provider", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "bad model"},
	}
	for i, spec := range tests {
		t.Run(spec.ID+string(rune('a'+i)), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			path := filepath.Join(home, "config.toml")
			const original = "# untouched\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := SaveUserCustomProvider(spec); err == nil {
				t.Fatal("SaveUserCustomProvider() succeeded for invalid spec")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Fatalf("invalid input mutated config: %q", got)
			}
		})
	}
}

func TestSaveUserProviderDefault_UpdatesOnlyUserDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := `# preserve me
[ui]
theme = "dark"

[provider]
default = "anthropic" # selected

[provider.openai]
api_key = "legacy-secret-stays-verbatim"
model = "gpt-existing"
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SaveUserProviderDefault("openai"); err != nil {
		t.Fatalf("SaveUserProviderDefault(): %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, preserved := range []string{
		"# preserve me",
		`theme = "dark"`,
		"# selected",
		`api_key = "legacy-secret-stays-verbatim"`,
		`model = "gpt-existing"`,
	} {
		if !strings.Contains(text, preserved) {
			t.Errorf("updated config lost %q:\n%s", preserved, text)
		}
	}
	var probe userConfigProbe
	if _, err := toml.Decode(text, &probe); err != nil {
		t.Fatalf("decode updated config: %v", err)
	}
	if probe.Provider.Default != "openai" {
		t.Fatalf("provider.default = %q, want openai", probe.Provider.Default)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode after update = %v, stat err = %v; want 0600", st.Mode().Perm(), err)
	}
}

func TestSaveUserProviderDefault_CreatesParentBeforeChild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("[provider.openai]\nmodel = \"gpt-existing\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SaveUserProviderDefault("openai"); err != nil {
		t.Fatalf("SaveUserProviderDefault(): %v", err)
	}
	var probe userConfigProbe
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toml.Decode(string(b), &probe); err != nil {
		t.Fatalf("decode updated config: %v\n%s", err, b)
	}
	if probe.Provider.Default != "openai" {
		t.Fatalf("provider.default = %q, want openai", probe.Provider.Default)
	}
}

func TestSaveUserProviderDefault_RejectsUnsafeOrInvalidInput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	const original = "provider.default = \"anthropic\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"openai", "", "bad.id", "bad\nvalue"} {
		if err := SaveUserProviderDefault(id); err == nil {
			t.Fatalf("SaveUserProviderDefault(%q) succeeded", id)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != original {
			t.Fatalf("config mutated on error: %q", got)
		}
	}
}

func TestSaveUserProviderDefault_RejectsSymlinkWithoutReplacingTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	target := filepath.Join(t.TempDir(), "managed-config.toml")
	const original = "[provider]\ndefault = \"anthropic\"\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserProviderDefault("openai"); err == nil || !strings.Contains(err.Error(), "symlinked") {
		t.Fatalf("symlinked config should be rejected, got %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("symlink target changed: %q", got)
	}
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink was replaced: stat=%v err=%v", st.Mode(), err)
	}
}

func TestDeleteUserCustomProviderPreservesUnrelatedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := SaveUserCustomProvider(CustomProviderSpec{
		ID: "local-gateway", Transport: "openai_chat", BaseURL: "http://127.0.0.1:9000/v1", Model: "local-model",
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[ui]\ntheme = \"nord\"\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	if err := DeleteUserCustomProvider("local-gateway"); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("deleting current default should fail, got %v", err)
	}
	if err := SaveUserProviderDefault("openai"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteUserCustomProvider("local-gateway"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "provider.custom.local-gateway") || !strings.Contains(string(b), "theme = \"nord\"") {
		t.Fatalf("unexpected config after delete:\n%s", b)
	}
	var probe userConfigProbe
	if _, err := toml.Decode(string(b), &probe); err != nil {
		t.Fatalf("deleted config is invalid: %v", err)
	}
	if probe.Provider.Default != "openai" {
		t.Fatalf("provider.default = %q", probe.Provider.Default)
	}
}

func TestProviderOverrideSourcesDetectProjectLayers(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("METIS_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(`
[provider]
default = "project-gateway"

[provider.custom.project-gateway]
transport = "openai_chat"
base_url = "https://example.com/v1"
model = "project-model"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if source, err := ProviderDefaultOverrideSource(); err != nil || source == "" {
		t.Fatalf("default override source = %q, %v", source, err)
	}
	if source, err := CustomProviderOverrideSource("project-gateway"); err != nil || source == "" {
		t.Fatalf("custom override source = %q, %v", source, err)
	}
	if source, err := CustomProviderOverrideSource("other"); err != nil || source != "" {
		t.Fatalf("unrelated custom source = %q, %v", source, err)
	}
}
