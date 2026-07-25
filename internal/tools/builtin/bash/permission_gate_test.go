package bash

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func autoAllowBashForTest(t *testing.T, gate *permission.Gate) Bash {
	t.Helper()
	previous := RuntimeSandboxMode()
	SetRuntimeSandboxMode("")
	t.Cleanup(func() { SetRuntimeSandboxMode(previous) })
	return New(gate, config.ToolBashSettings{
		Sandbox: config.SandboxBashSettings{Mode: SandboxModeAutoAllow},
	})
}

func TestBashCanUse_SandboxAutoAllowCannotEscapePlan(t *testing.T) {
	gate := permission.New(permission.ModePlan)
	tool := autoAllowBashForTest(t, gate)

	got, source := tool.CanUse(context.Background(), map[string]any{"command": "touch /tmp/metis-plan-escape"})
	if got != tools.PermissionDeny {
		t.Fatalf("Plan mode + sandbox auto-allow = %v (%s), want deny", got, source)
	}
}

func TestBashCanUse_SandboxAutoAllowPreservesExplicitDeny(t *testing.T) {
	gate := permission.New(permission.ModeDefault)
	gate.AppendRules(permission.Rule{Tool: "Bash", Verb: permission.DecisionDeny, Source: "test"})
	tool := autoAllowBashForTest(t, gate)

	got, source := tool.CanUse(context.Background(), map[string]any{"command": "touch /tmp/metis-rule-escape"})
	if got != tools.PermissionDeny {
		t.Fatalf("explicit deny + sandbox auto-allow = %v (%s), want deny", got, source)
	}
}

func TestBashCanUse_SandboxAutoAllowStillReplacesOrdinaryAsk(t *testing.T) {
	gate := permission.New(permission.ModeDefault)
	tool := autoAllowBashForTest(t, gate)

	got, source := tool.CanUse(context.Background(), map[string]any{"command": "touch /tmp/metis-approved-in-sandbox"})
	if got != tools.PermissionAllow || source != "sandbox auto-allow" {
		t.Fatalf("ordinary ask + sandbox auto-allow = %v (%s), want allow", got, source)
	}
}
