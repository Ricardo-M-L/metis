package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

func TestLoadPluginsRejectsUnsupportedCapabilitiesBeforeLaunch(t *testing.T) {
	cases := []struct{ name, extra, want string }{
		{"schedule", "[[schedules]]\ncron = \"0 9 * * *\"\n", "schedules"},
		{"empty-theme-table", "[themes]\n", "themes"},
		{"unknown-mcp-field", "[mcp_server]\ncommand = \"probe\"\ntypo = true\n", "typo"},
		{"hooks", "[[hooks]]\nevents = [\"SessionStart\"]\ncommand = \"probe\"\n", "hooks"},
		{"mixed-hooks", "[mcp_server]\ncommand = \"probe\"\n[[hooks]]\nevents = [\"PreToolUse\"]\ncommand = \"probe\"\n", "hooks"},
		{"empty", "", "capability"},
		{"disabled-only", "[mcp_server]\ncommand = \"probe\"\ndisabled = true\n", "capability"},
		{"legacy-duplicate", "[mcp_server]\ncommand = \"probe\"\n[[mcp_servers]]\nname = \"default\"\ncommand = \"probe\"\n", "duplicate"},
		{"ambiguous-transport", "[mcp_server]\ncommand = \"probe\"\nurl = \"https://example.invalid/mcp\"\n", "command or url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			writePluginManifest(t, "validation", "manifest_version = 1\nname = \"validation\"\n"+tc.extra)
			launches := 0
			original := launchPluginMCPServer
			launchPluginMCPServer = func(context.Context, mcp.ServerEntry, *tools.Registry, *sandbox.Manager) (*mcptools.Server, error) {
				launches++
				return nil, nil
			}
			t.Cleanup(func() { launchPluginMCPServer = original })
			reg, errs := LoadPlugins(context.Background(), tools.NewRegistry())
			if len(errs) != 1 || !strings.Contains(strings.ToLower(errs[0].Error()), tc.want) {
				t.Fatalf("errors = %v, want %q", errs, tc.want)
			}
			if len(reg.All()) != 0 || launches != 0 {
				t.Fatalf("invalid plugin published or launched: plugins=%d launches=%d", len(reg.All()), launches)
			}
		})
	}
}

func TestLoadPluginsChecksAllResourcesBeforeLaunch(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writePluginManifest(t, "missing-skill", `name = "missing-skill"
skills = ["missing.md"]
[mcp_server]
command = "probe"
`)
	launches := 0
	original := launchPluginMCPServer
	launchPluginMCPServer = func(context.Context, mcp.ServerEntry, *tools.Registry, *sandbox.Manager) (*mcptools.Server, error) {
		launches++
		return nil, nil
	}
	t.Cleanup(func() { launchPluginMCPServer = original })
	reg, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "missing.md") {
		t.Fatalf("errors = %v, want missing skill", errs)
	}
	if len(reg.All()) != 0 || launches != 0 {
		t.Fatalf("preflight failed to prevent launch: plugins=%d launches=%d", len(reg.All()), launches)
	}
}

func TestLoadPluginsRejectsSkillSymlinkEscape(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	dir := writePluginManifest(t, "symlink-skill", "name = \"symlink-skill\"\nskills = [\"outside.md\"]\n")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "outside.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	reg, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "escapes") || len(reg.All()) != 0 {
		t.Fatalf("symlink escape accepted: errors=%v plugins=%d", errs, len(reg.All()))
	}
}
