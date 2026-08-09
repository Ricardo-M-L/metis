package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type customTurnTool struct {
	tools.BaseTool
	name string
}

func (t customTurnTool) Name() string                               { return t.name }
func (customTurnTool) Description() string                          { return "test tool" }
func (customTurnTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (customTurnTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (customTurnTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (customTurnTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

func customTurnLoop(model string, names ...string) *agent.Loop {
	registry := tools.NewRegistry()
	for _, name := range names {
		registry.Register(customTurnTool{name: name})
	}
	return &agent.Loop{Registry: registry, Gate: permission.New(permission.ModeDefault), Model: model}
}

func TestPrepareCustomCommandTurn_RejectsTrustedModelMismatch(t *testing.T) {
	cmd := &slash.Cmd{Name: "review", Custom: true, Trusted: true, Model: "other-model"}
	_, _, err := prepareCustomCommandTurn(context.Background(), cmd, customTurnLoop("current-model"))
	if err == nil || !strings.Contains(err.Error(), "/model other-model") {
		t.Fatalf("error = %v, want explicit model-switch guidance", err)
	}
}

func TestPrepareCustomCommandTurn_UntrustedOverridesAreExplicitlyIgnored(t *testing.T) {
	cmd := &slash.Cmd{
		Name: "project-check", Custom: true, Trusted: false,
		Model: "other-model", AllowedTools: []string{"Bash"},
	}
	ctx := context.Background()
	gotCtx, warnings, err := prepareCustomCommandTurn(ctx, cmd, customTurnLoop("current-model", "Bash"))
	if err != nil {
		t.Fatalf("untrusted metadata should be ignored, not fail the prompt: %v", err)
	}
	if gotCtx != ctx {
		t.Fatal("untrusted command unexpectedly attached turn policy")
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "ignored model") || !strings.Contains(joined, "ignored allowed-tools") {
		t.Fatalf("warnings = %q, want both ignored-override notices", joined)
	}
}

func TestCustomCommandAllowRules_PreservesPrecisePatternAndIgnoresUnknown(t *testing.T) {
	loop := customTurnLoop("model", "Bash", "Read")
	cmd := &slash.Cmd{
		Name: "check", Custom: true, Trusted: true,
		AllowedTools: []string{"Bash(git status:*)", "Read", "Missing", "Bash(unclosed"},
	}
	rules, warnings := customCommandAllowRules(cmd, loop)
	if len(rules) != 2 {
		t.Fatalf("rules = %+v, want precise Bash + Read", rules)
	}
	if rules[0].Tool != "Bash" || rules[0].Match != "git status:*" || rules[0].Verb != permission.DecisionAllow {
		t.Fatalf("Bash rule was widened or malformed: %+v", rules[0])
	}
	if rules[1].Tool != "Read" || rules[1].Match != "" {
		t.Fatalf("Read rule = %+v", rules[1])
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "unknown") || !strings.Contains(joined, "malformed") {
		t.Fatalf("warnings = %q", joined)
	}
}

func TestCustomCommandNeedsFreshTurn(t *testing.T) {
	loop := customTurnLoop("current", "Bash")
	if customCommandNeedsFreshTurn(&slash.Cmd{Custom: true, Trusted: true, Model: "current"}, loop) {
		t.Fatal("same-model prompt does not need a fresh turn")
	}
	if !customCommandNeedsFreshTurn(&slash.Cmd{Custom: true, Trusted: true, Model: "other"}, loop) {
		t.Fatal("different model must not be injected into an already-running turn")
	}
	if !customCommandNeedsFreshTurn(&slash.Cmd{Custom: true, Trusted: true, AllowedTools: []string{"Bash"}}, loop) {
		t.Fatal("temporary permission rules need a fresh turn boundary")
	}
	if customCommandNeedsFreshTurn(&slash.Cmd{Custom: true, Trusted: false, Model: "other", AllowedTools: []string{"Bash"}}, loop) {
		t.Fatal("untrusted overrides are ignored and therefore should not block prompt-only steering")
	}
}
