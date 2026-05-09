package builtin

// metis_info_test.go — pin the introspection tool's INI output shape.
// Test focus is "the right sections appear and contain the right
// keys"; we don't pin specific values that would break with every
// config tweak.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func newTestCfg() *config.Config {
	return &config.Config{
		Provider: config.ProviderSet{
			Default: "deepseek",
			OpenAI: config.ProviderOpenAI{
				APIKey: "sk-test",
			},
			Custom: map[string]config.ProviderRaw{
				"deepseek": {
					BaseURL: "https://api.deepseek.com/v1",
				},
			},
		},
		MCP: config.MCP{
			Servers: []config.MCPServer{
				{Name: "github-mcp", Command: "github-mcp", Disabled: false},
				{Name: "disabled-mcp", Command: "x", Disabled: true},
			},
		},
		Hooks: config.HooksConfig{
			PreToolUse: []config.HookSpec{
				{Type: "command", Command: "echo hello"},
			},
		},
		Tools: config.Tools{
			Disabled: []string{"WebFetch"},
			Bash: config.ToolBashSettings{
				Allowlist: []string{"git status", "ls"},
			},
		},
		UI:      config.UI{Theme: "dark"},
		Session: config.Session{SkillDir: "/tmp/skills"},
	}
}

func TestMetisInfo_FullDump(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	cfg := newTestCfg()
	pool := jobs.NewRegistry(t.TempDir())
	reg := tools.NewRegistry()
	reg.Register(Bash{})
	tool := NewMetisInfo(gate, cfg, pool, nil, reg)

	res, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output

	// Each section header should appear.
	for _, want := range []string{
		"[providers]",
		"[mcp]",
		"[tools]",
		"[hooks]",
		"[permission]",
		"[jobs]",
		"[options]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing section %q\n---\n%s", want, out)
		}
	}
	// Specific keys we promise:
	for _, want := range []string{
		"default = deepseek",
		"github-mcp = configured",
		"disabled-mcp = disabled",
		"mode = bypass",
		"pre_tool_use",
		"echo hello",
		"bash_allowlist = git status, ls",
		"disabled = WebFetch",
		"ui_theme = dark",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing key %q\n---\n%s", want, out)
		}
	}
}

func TestMetisInfo_SectionFilter(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	cfg := newTestCfg()
	tool := NewMetisInfo(gate, cfg, nil, nil, nil)

	res, _ := tool.Execute(context.Background(), map[string]any{"section": "providers"})
	if !strings.Contains(res.Output, "[providers]") {
		t.Errorf("section=providers should include [providers] header; got %q", res.Output)
	}
	for _, must_not := range []string{"[mcp]", "[hooks]", "[tools]"} {
		if strings.Contains(res.Output, must_not) {
			t.Errorf("section=providers should NOT include %q", must_not)
		}
	}
}

func TestMetisInfo_NoSectionsMatched(t *testing.T) {
	tool := NewMetisInfo(nil, nil, nil, nil, nil)
	res, _ := tool.Execute(context.Background(), map[string]any{"section": "doesnotexist"})
	if !strings.Contains(res.Output, "no sections matched") {
		t.Errorf("expected 'no sections matched' fallback; got %q", res.Output)
	}
}

func TestMetisInfo_NilReferences(t *testing.T) {
	// All nils — render shouldn't panic; should emit minimal sections.
	tool := NewMetisInfo(nil, nil, nil, nil, nil)
	res, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Providers prints "(unavailable)" when cfg is nil.
	if !strings.Contains(res.Output, "[providers]") {
		t.Errorf("expected [providers] section even with nil cfg")
	}
	if !strings.Contains(res.Output, "(unavailable)") {
		t.Errorf("expected unavailable marker; got %q", res.Output)
	}
}

func TestMetisInfo_ToolMetadata(t *testing.T) {
	tool := NewMetisInfo(nil, nil, nil, nil, nil)
	if tool.Name() != "MetisInfo" {
		t.Errorf("tool name = %q; want MetisInfo", tool.Name())
	}
	if tool.IsDestructive(nil) {
		t.Errorf("MetisInfo should not be destructive")
	}
	if !tool.IsReadOnly(nil) {
		t.Errorf("MetisInfo should be read-only")
	}
}
