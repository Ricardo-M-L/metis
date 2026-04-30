package security

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Minimal Tool stub used to exercise schema checks.
type stubTool struct {
	name   string
	schema map[string]any
}

func (s stubTool) Name() string                                                          { return s.name }
func (s stubTool) Description() string                                                    { return "stub" }
func (s stubTool) InputSchema() map[string]any                                            { return s.schema }
func (s stubTool) Concurrency(map[string]any) tools.Concurrency                                         { return tools.ConcurrencySafe }
func (s stubTool) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) { return tools.PermissionAllow, "" }
func (s stubTool) Execute(_ context.Context, _ map[string]any) (*tools.Result, error)    { return &tools.Result{}, nil }

// --- helpers ---------------------------------------------------------------

func minimalCfg() *config.Config {
	return &config.Config{
		Permission: config.Permission{Mode: "ask"},
		Tools: config.Tools{
			Bash: config.ToolBashSettings{
				Denylist: []string{"rm -rf /", "dd of=/dev", ":(){:|:&};:"},
			},
		},
	}
}

// --- tests -----------------------------------------------------------------

func TestRun_GreenPath(t *testing.T) {
	cfg := minimalCfg()
	reg := tools.NewRegistry()
	reg.Register(stubTool{name: "Read", schema: map[string]any{"type": "object"}})

	rep := Run(cfg, reg)
	crit, _, _ := rep.Counts()
	if crit != 0 {
		t.Errorf("clean config produced %d critical findings: %+v", crit, rep.Findings)
	}
}

func TestRun_BashDenylistMissingFlagsAllRequired(t *testing.T) {
	cfg := minimalCfg()
	cfg.Tools.Bash.Denylist = nil // empty
	rep := Run(cfg, tools.NewRegistry())

	count := 0
	for _, f := range rep.Findings {
		if f.Code == "BASH_DENYLIST_WEAK" {
			count++
		}
	}
	if count != len(requiredDenyPatterns) {
		t.Errorf("expected %d weak-denylist findings, got %d", len(requiredDenyPatterns), count)
	}
}

func TestRun_HardcodedAPIKey(t *testing.T) {
	cfg := minimalCfg()
	cfg.Provider.Anthropic.APIKey = "sk-direct"
	rep := Run(cfg, tools.NewRegistry())
	if !findingHasCode(rep, "API_KEY_HARDCODED") {
		t.Error("expected API_KEY_HARDCODED warning")
	}
}

func TestRun_BypassModeIsCritical(t *testing.T) {
	cfg := minimalCfg()
	cfg.Permission.Mode = "bypass"
	rep := Run(cfg, tools.NewRegistry())
	if !rep.HasCritical() {
		t.Error("bypass mode should produce a critical finding")
	}
	if !findingHasCode(rep, "PERMISSION_BYPASS") {
		t.Error("expected PERMISSION_BYPASS code")
	}
}

func TestRun_ToolSchemaWrongTopLevel(t *testing.T) {
	cfg := minimalCfg()
	reg := tools.NewRegistry()
	reg.Register(stubTool{name: "Bad", schema: map[string]any{"type": "string"}})
	rep := Run(cfg, reg)
	if !findingHasCode(rep, "TOOL_SCHEMA_NON_OBJECT") {
		t.Error("expected TOOL_SCHEMA_NON_OBJECT for non-object top-level")
	}
}

func TestRun_MCPServerRelativePathInfo(t *testing.T) {
	cfg := minimalCfg()
	cfg.MCP.Servers = []config.MCPServer{
		{Name: "fs", Command: "npx", Args: []string{"-y", "x"}},
		{Name: "abs", Command: "/usr/local/bin/foo"},
		{Name: "skip", Command: "ignored", Disabled: true},
	}
	rep := Run(cfg, tools.NewRegistry())

	found := 0
	for _, f := range rep.Findings {
		if f.Code == "MCP_SERVER_RELATIVE_COMMAND" {
			found++
		}
	}
	if found != 1 {
		t.Errorf("expected exactly 1 relative-command finding, got %d (findings=%+v)", found, rep.Findings)
	}
}

func TestReport_HasCriticalFalseWhenOnlyWarn(t *testing.T) {
	rep := &Report{Findings: []Finding{
		{Severity: SeverityWarn, Code: "X"},
		{Severity: SeverityInfo, Code: "Y"},
	}}
	if rep.HasCritical() {
		t.Error("HasCritical should be false without critical entries")
	}
}

func TestReport_Counts(t *testing.T) {
	rep := &Report{Findings: []Finding{
		{Severity: SeverityCritical},
		{Severity: SeverityWarn},
		{Severity: SeverityWarn},
		{Severity: SeverityInfo},
	}}
	c, w, i := rep.Counts()
	if c != 1 || w != 2 || i != 1 {
		t.Errorf("counts = (%d,%d,%d), want (1,2,1)", c, w, i)
	}
}

// helper

func findingHasCode(r *Report, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
