package pluginmanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

func manifestFixture(t *testing.T, extra string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plugin.toml"), []byte("name = \"fixture\"\n"+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: example\ndescription: fixture\n---\nfixture body"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReadAcceptsSupportedCapabilitiesWithoutStartingProcesses(t *testing.T) {
	root := manifestFixture(t, `skills = ["SKILL.md"]
hooks = []
[mcp_server]
command = "unavailable-disabled-test-command"
disabled = true
[[mcp_servers]]
name = "remote"
url = "https://example.invalid/mcp"
headers = { Authorization = "private-header" }
[[mcp_servers]]
name = "local"
command = "unavailable-test-command"
args = ["private-argument"]
env = { API_KEY = "private-env" }
working_dir = "."
`)
	m, err := Read(root, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if m.ManifestVersion != 1 || len(m.Skills) != 1 || len(m.MCPServers) != 2 || m.MCPServer == nil || !m.MCPServer.Disabled {
		t.Fatalf("supported declarations changed: %#v", m)
	}
}

func TestReadAllowsDisabledMCPWithSkillsAndEmptyHooks(t *testing.T) {
	root := manifestFixture(t, "skills = [\"SKILL.md\"]\nhooks = []\n[mcp_server]\ncommand = \"not-started\"\ndisabled = true\n")
	if _, err := Read(root, "fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestReadPreservesEnvironmentTemplatesWithoutResolvingThem(t *testing.T) {
	t.Setenv("METIS_PLUGIN_UNRESOLVED_AUDIT", "invalid live value must not be inspected")
	for _, tc := range []struct{ name, field, value string }{
		{"full-url", "url", "${METIS_PLUGIN_UNRESOLVED_AUDIT}/mcp"},
		{"host-url", "url", "https://${METIS_PLUGIN_UNRESOLVED_AUDIT}/mcp"},
		{"port-url", "url", "http://127.0.0.1:${METIS_PLUGIN_UNRESOLVED_AUDIT}/mcp"},
		{"scheme-url", "url", "${METIS_PLUGIN_MISSING_AUDIT}://example.invalid/mcp"},
		{"scheme-and-host-url", "url", "${METIS_PLUGIN_MISSING_SCHEME_AUDIT}://${METIS_PLUGIN_MISSING_HOST_AUDIT}/mcp"},
		{"default-url", "url", "${METIS_PLUGIN_MISSING_AUDIT:-https://example.invalid/mcp}"},
		{"command", "command", "${METIS_PLUGIN_MISSING_AUDIT:-node}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Read(manifestFixture(t, "[mcp_server]\n"+tc.field+" = \""+tc.value+"\"\n"), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			value := m.MCPServer.URL
			if tc.field == "command" {
				value = m.MCPServer.Command
			}
			if value != tc.value {
				t.Fatal("validation changed the environment template")
			}
		})
	}
	for _, malformed := range []string{"https://${UNCLOSED/mcp", "ftp://${HOST}/mcp"} {
		if _, err := Read(manifestFixture(t, "[mcp_server]\nurl = \""+malformed+"\"\n"), "fixture"); err == nil {
			t.Fatalf("invalid URL template accepted")
		}
	}
}

func TestReadRejectsUnknownFieldsIncludingEmptyTables(t *testing.T) {
	for _, extra := range []string{
		"skills = [\"SKILL.md\"]\n[themes]\n",
		"skills = [\"SKILL.md\"]\n[[schedules]]\n",
		"skills = [\"SKILL.md\"]\ncommands = []\n",
		"[mcp_server]\ncommand = \"probe\"\ntiemout = 1\n",
	} {
		t.Run(strings.ReplaceAll(extra, "\n", " "), func(t *testing.T) {
			if _, err := Read(manifestFixture(t, extra), "fixture"); err == nil || !strings.Contains(err.Error(), "unsupported manifest field") {
				t.Fatalf("unknown declaration accepted: %v", err)
			}
		})
	}
}

func TestReadChecksResources(t *testing.T) {
	cases := []struct{ name, extra, want string }{
		{"missing-skill", "skills = [\"missing.md\"]\n", "missing.md"},
		{"skill-directory", "skills = [\".\"]\n", "wrong file type"},
		{"skill-parent", "skills = [\"../outside.md\"]\n", "escapes"},
		{"skill-windows-parent", "skills = ['..\\outside.md']\n", "escapes"},
		{"skill-windows-absolute", "skills = ['C:\\outside.md']\n", "relative"},
		{"skill-absolute", "skills = [\"/outside.md\"]\n", "relative"},
		{"missing-working-dir", "[mcp_server]\ncommand = \"probe\"\nworking_dir = \"missing\"\n", "working_dir"},
		{"working-dir-parent", "[mcp_server]\ncommand = \"probe\"\nworking_dir = \"..\"\n", "escapes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Read(manifestFixture(t, tc.extra), "fixture"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resource error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadRejectsInvalidSkillsAndSymlinkEscapes(t *testing.T) {
	t.Run("invalid-skill", func(t *testing.T) {
		root := manifestFixture(t, "skills = [\"SKILL.md\"]\n")
		if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: [invalid\n---\nbody"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(root, "fixture"); err == nil || !strings.Contains(err.Error(), "invalid skill") {
			t.Fatalf("malformed skill accepted: %v", err)
		}
	})
	for _, directory := range []bool{false, true} {
		name := "skill"
		extra := "skills = [\"outside.md\"]\n"
		if directory {
			name = "working-dir"
			extra = "[mcp_server]\ncommand = \"probe\"\nworking_dir = \"outside.md\"\n"
		}
		t.Run(name, func(t *testing.T) {
			root := manifestFixture(t, extra)
			outside := t.TempDir()
			if !directory {
				outside = filepath.Join(outside, "SKILL.md")
				if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(root, "outside.md")); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if _, err := Read(root, "fixture"); err == nil || !strings.Contains(err.Error(), "escapes") {
				t.Fatalf("symlink escape accepted: %v", err)
			}
		})
	}
}

func TestReadErrorsDoNotExposeCredentialValues(t *testing.T) {
	const private = "private-value-1028"
	for _, tc := range []struct{ name, extra string }{
		{"unknown-field", "[mcp_server]\ncommand = \"probe\"\nargs = [\"" + private + "\"]\nunknown = \"" + private + "\"\n"},
		{"invalid-field-type", "[mcp_server]\ncommand = \"probe\"\nheaders = \"" + private + "\"\n"},
		{"invalid-env-type", "[mcp_server]\ncommand = \"probe\"\nenv = \"" + private + "\"\n"},
		{"invalid-multi-env-type", "[[mcp_servers]]\nname = \"first\"\ncommand = \"probe\"\nenv = \"" + private + "\"\n[[mcp_servers]]\nname = \"second\"\ncommand = \"probe\"\nenv = { API_KEY = \"valid\" }\n"},
		{"invalid-command", "[mcp_server]\ncommand = \"probe;" + private + "\"\n"},
		{"invalid-url", "[mcp_server]\nurl = \"invalid-" + private + "\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Read(manifestFixture(t, tc.extra), "fixture"); err == nil || strings.Contains(err.Error(), private) {
				t.Fatalf("error absent or leaked credential: %v", err)
			}
		})
	}
}

func TestReadRejectsUnsafeExpectedNameBeforeFilesystemAccess(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "absent"), "../outside"); err == nil || err.Error() != "invalid plugin name" {
		t.Fatalf("unsafe name did not fail before reading: %v", err)
	}
}

func TestValidateRejectsInvalidDeclarations(t *testing.T) {
	cases := []struct {
		name, want string
		manifest   pubplugin.Manifest
	}{
		{"nil", "required", pubplugin.Manifest{}},
		{"version", "manifest_version", pubplugin.Manifest{Name: "fixture", ManifestVersion: 99, Skills: []string{"SKILL.md"}}},
		{"name", "must match", pubplugin.Manifest{Name: "other", Skills: []string{"SKILL.md"}}},
		{"unsafe-name", "manifest name", pubplugin.Manifest{Name: "bad/name", Skills: []string{"SKILL.md"}}},
		{"empty", "capability", pubplugin.Manifest{Name: "fixture"}},
		{"hooks", "hooks", pubplugin.Manifest{Name: "fixture", Skills: []string{"SKILL.md"}, Hooks: []pubplugin.HookSpec{{Command: "probe"}}}},
		{"duplicate", "duplicate", pubplugin.Manifest{Name: "fixture", MCPServer: &pubplugin.MCPServerSpec{Command: "probe"}, MCPServers: []pubplugin.MCPServerSpec{{Name: "default", Command: "probe"}}}},
		{"unsafe-server-name", ".name", pubplugin.Manifest{Name: "fixture", MCPServers: []pubplugin.MCPServerSpec{{Name: "bad:name", Command: "probe"}}}},
		{"dual-transport", "command or url", pubplugin.Manifest{Name: "fixture", MCPServer: &pubplugin.MCPServerSpec{Command: "probe", URL: "https://example.invalid/mcp"}}},
		{"invalid-url", "HTTP", pubplugin.Manifest{Name: "fixture", MCPServer: &pubplugin.MCPServerSpec{URL: "file:///tmp/mcp"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &tc.manifest
			if tc.name == "nil" {
				m = nil
			}
			if err := Validate(m, "fixture"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
