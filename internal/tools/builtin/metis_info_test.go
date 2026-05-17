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
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// stubProvider satisfies the bare minimum of llm.Provider needed by
// the [model] section — Name() and MaxContextTokens(). Complete /
// Stream panic if called; the introspection path never hits them.
type stubProvider struct {
	name   string
	window int
}

func (s *stubProvider) Name() string          { return s.name }
func (s *stubProvider) MaxContextTokens() int { return s.window }
func (s *stubProvider) ModelID() string       { return "" }
func (s *stubProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	panic("stubProvider.Complete not used in this test")
}
func (s *stubProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	panic("stubProvider.Stream not used in this test")
}

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

func TestMetisInfo_ModelSection(t *testing.T) {
	// The model section is the LLM's self-introspection path: lets a
	// DeepSeek-V4 instance verify metis really gave it a 1M window
	// (not the old 200k fallback). Pin the exact output shape so the
	// model's parser doesn't break silently if we ever reword the INI.
	tool := NewMetisInfo(nil, nil, nil, nil, nil).
		WithModel(&stubProvider{name: "openai", window: 1_000_000}, "deepseek-v4-pro")

	res, err := tool.Execute(context.Background(), map[string]any{"section": "model"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{
		"[model]",
		"transport = openai",
		"id = deepseek-v4-pro",
		"context_window = 1000000",
	} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("model section missing %q\n---\n%s", want, res.Output)
		}
	}
}

func TestMetisInfo_CatalogSection_LoadedShape(t *testing.T) {
	// When the catalog singleton has loaded (the common run-time
	// path), the [catalog] section must surface at least: loaded
	// flag, provider/model counts, and the cache file path. We
	// don't pin specific counts — models.dev grows over time —
	// only that the keys exist so the LLM can parse them. Test
	// runs in the same package as other tests that touch the
	// singleton so by the time this runs, Default() has likely
	// fired; we just verify the rendered shape is sane.
	tool := NewMetisInfo(nil, nil, nil, nil, nil)
	res, _ := tool.Execute(context.Background(), map[string]any{"section": "catalog"})
	// Catalog section appears iff catalog.Default() returned non-nil;
	// when it did, the loaded-flag line is mandatory. If singleton
	// was nil-pinned by an earlier METIS_CATALOG_DISABLE, the section
	// vanishes entirely — both shapes are acceptable.
	if strings.Contains(res.Output, "[catalog]") {
		for _, want := range []string{"loaded =", "cache_path ="} {
			if !strings.Contains(res.Output, want) {
				t.Errorf("[catalog] section missing key %q\n---\n%s", want, res.Output)
			}
		}
	}
}

func TestMetisInfo_ModelSection_NilProviderHidden(t *testing.T) {
	// `metis tools` listing path constructs MetisInfo without a live
	// provider. The [model] section must vanish rather than render as
	// "(unavailable)" — saves model tokens on every full dump.
	tool := NewMetisInfo(nil, nil, nil, nil, nil)
	res, _ := tool.Execute(context.Background(), map[string]any{"section": "model"})
	if strings.Contains(res.Output, "[model]") {
		t.Errorf("expected [model] section to be hidden when provider is nil; got %q", res.Output)
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
