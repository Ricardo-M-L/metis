package pluginmarket

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

func nativeValidationFixture(t *testing.T, manifest string) (string, string) {
	t.Helper()
	home, source := t.TempDir(), t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(source, "plugin.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("---\nname: probe\ndescription: validation fixture\n---\nOnly a fixture.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, source
}

func TestInstallRejectsUnsupportedNativeCapabilitiesBeforeCommit(t *testing.T) {
	cases := []struct{ name, body string }{
		{"schedule", "[[schedules]]\ncron = \"0 9 * * *\"\nprompt = \"probe\"\n"},
		{"theme", "[themes]\nname = \"probe\"\n"},
		{"hooks", "[[hooks]]\nevents = [\"SessionStart\"]\ncommand = \"not-started\"\n"},
		{"mixed-hooks", "skills = [\"SKILL.md\"]\n[[hooks]]\nevents = [\"SessionStart\"]\ncommand = \"not-started\"\n"},
		{"empty", ""},
		{"version", "manifest_version = 99\nskills = [\"SKILL.md\"]\n"},
		{"missing-skill", "skills = [\"missing.md\"]\n"},
		{"duplicate-mcp", "[mcp_server]\ncommand = \"fixture\"\n[[mcp_servers]]\nname = \"default\"\ncommand = \"fixture\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, source := nativeValidationFixture(t, "name = \"probe\"\nversion = \"0.0.1\"\n"+tc.body)
			_, err := NewManager().installFromPath("probe", "fixture", source, marketplacePluginEntry{Name: "probe"})
			if err == nil {
				t.Fatal("invalid native plugin was installed")
			}
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("invalid manifest error not classified: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(home, "plugins", "probe")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected plugin destination exists: %v", statErr)
			}
			staging, globErr := filepath.Glob(filepath.Join(home, "plugins", ".install-*"))
			if globErr != nil || len(staging) != 0 {
				t.Fatalf("failed install staging = %v, err=%v", staging, globErr)
			}
		})
	}
}

func TestTranslatedPluginIsValidatedBeforeInstallCommit(t *testing.T) {
	home, source := t.TempDir(), t.TempDir()
	t.Setenv("METIS_HOME", home)
	_, err := NewManager().installFromPath("probe", "fixture", source, marketplacePluginEntry{
		Name: "probe",
		MCPServers: []pubplugin.MCPServerSpec{
			{Name: "same", Command: "fixture"},
			{Name: "same", Command: "fixture"},
		},
	})
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("invalid translated MCP declaration error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "plugins", "probe")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid translated plugin published: %v", statErr)
	}
	staging, globErr := filepath.Glob(filepath.Join(home, "plugins", ".install-*"))
	if globErr != nil || len(staging) != 0 {
		t.Fatalf("translated failure staging = %v, err=%v", staging, globErr)
	}
}

func TestInstallValidationErrorDoesNotExposeCredentialValues(t *testing.T) {
	const secret = "synthetic-credential-not-for-error-output"
	_, source := nativeValidationFixture(t, "name = \"probe\"\n[mcp_server]\ncommand = \"fixture\"\nenv = { TOKEN = [\""+secret+"\"] }\n")
	_, err := NewManager().installFromPath("probe", "fixture", source, marketplacePluginEntry{Name: "probe"})
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("malformed declaration error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("manifest validation error disclosed a credential value")
	}
}

func TestCatalogRejectsInvalidNativeCapabilityClaims(t *testing.T) {
	_, source := nativeValidationFixture(t, "name = \"probe\"\n[themes]\nname = \"unsupported\"\n")
	item := catalogPlugin("fixture", marketplacePluginEntry{Name: "probe", Capabilities: []string{"Themes", "Hooks"}}, source, "path", nil)
	if item.Installable || item.Compatibility != "invalid" || item.Unavailable == "" {
		t.Fatalf("invalid native catalog entry = %+v", item)
	}
	if len(item.Capabilities) != 0 {
		t.Fatalf("unsupported capabilities advertised: %v", item.Capabilities)
	}
}

func TestCatalogNativeCapabilitiesComeFromValidatedManifest(t *testing.T) {
	_, source := nativeValidationFixture(t, "manifest_version = 1\nname = \"probe\"\nskills = [\"SKILL.md\"]\n[mcp_server]\ncommand = \"fixture\"\n")
	item := catalogPlugin("fixture", marketplacePluginEntry{Name: "probe", Capabilities: []string{"Themes", "Hooks"}}, source, "path", nil)
	if !item.Installable || item.Compatibility != "native" || item.Unavailable != "" {
		t.Fatalf("valid native catalog entry = %+v", item)
	}
	joined := strings.Join(item.Capabilities, ",")
	if !strings.Contains(joined, "Skills") || !strings.Contains(joined, "MCP") || strings.Contains(joined, "Themes") || strings.Contains(joined, "Hooks") {
		t.Fatalf("native capabilities = %v", item.Capabilities)
	}
	result, err := NewManager().installFromPath("probe", "fixture", source, marketplacePluginEntry{Name: "probe"})
	if err != nil {
		t.Fatalf("valid native install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestEcosystemDoesNotAdvertiseUnimplementedNativeHooks(t *testing.T) {
	for _, ecosystem := range ecosystemViews(0, 0, nil, 0, nil) {
		if ecosystem.ID != "metis" {
			continue
		}
		for _, component := range ecosystem.Components {
			if component.Kind == "hooks" && component.Support != "unsupported" {
				t.Fatalf("native hooks support = %q", component.Support)
			}
		}
	}
}
