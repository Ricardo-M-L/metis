package bash

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func autoAllowBashForTest(t *testing.T, gate *permission.Gate) Bash {
	t.Helper()
	manager, err := sandbox.NewManager(string(sandbox.ModeAutoAllow))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return NewWithSandbox(gate, config.ToolBashSettings{
		Sandbox: config.SandboxBashSettings{Mode: SandboxModeAutoAllow},
	}, manager)
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

func TestBashCanUse_PermissionsModeDoesNotReplaceAsk(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	tool := NewWithSandbox(permission.New(permission.ModeDefault), config.ToolBashSettings{}, manager)

	got, source := tool.CanUse(context.Background(), map[string]any{"command": "touch ordinary-ask"})
	if got != tools.PermissionAsk || source == "sandbox auto-allow" {
		t.Fatalf("permissions-mode decision = %v (%s), want original ask", got, source)
	}
}

func TestBashCanUse_SandboxAutoAllowDoesNotRelabelExistingAllow(t *testing.T) {
	gate := permission.New(permission.ModeDefault)
	gate.AppendRules(permission.Rule{Tool: "Bash", Verb: permission.DecisionAllow, Source: "test-explicit-allow"})
	tool := autoAllowBashForTest(t, gate)

	got, source := tool.CanUse(context.Background(), map[string]any{"command": "touch explicitly-allowed"})
	if got != tools.PermissionAllow || source == "sandbox auto-allow" {
		t.Fatalf("explicit allow + sandbox auto-allow = %v (%s), want original allow source", got, source)
	}
}

func TestBashCanUse_BypassAllowsReadOnlyClaudeSkillsProbe(t *testing.T) {
	t.Parallel()
	gate := permission.New(permission.ModeBypassPermissions)
	tool := New(gate, config.ToolBashSettings{})
	const command = `ls ~/.claude/skills/ 2>/dev/null || echo "no claude skills dir"`

	got, source := tool.CanUse(context.Background(), map[string]any{"command": command})
	if got != tools.PermissionAllow || source != "mode:bypassPermissions" {
		t.Fatalf("bypass Claude skills probe = %v (%s), want allow mode:bypassPermissions", got, source)
	}
}

func TestBashCanUse_BypassAllowsCanonicalSkillInstallCommands(t *testing.T) {
	t.Parallel()
	cases := []string{
		`mkdir -p ~/.agents/skills`,
		`curl --fail --location https://uizze.com/.well-known/agent-skills/index.json`,
		`npx skills add https://uizze.com --skill ui-radar --yes --global`,
		`npx hyperframes skills update`,
		`git clone --depth 1 https://github.com/heygen-com/hyperframes ~/.agents/skills/hyperframes`,
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			gate := permission.New(permission.ModeBypassPermissions)
			tool := New(gate, config.ToolBashSettings{})

			got, source := tool.CanUse(context.Background(), map[string]any{"command": command})
			if got != tools.PermissionAllow || source != "mode:bypassPermissions" {
				t.Fatalf("canonical skill install command = %v (%s), want allow mode:bypassPermissions", got, source)
			}
		})
	}
}

func TestBashCanUse_BypassStillAsksForDirectClaudeSkillWrite(t *testing.T) {
	t.Parallel()
	gate := permission.New(permission.ModeBypassPermissions)
	tool := New(gate, config.ToolBashSettings{})
	const command = `mkdir -p ~/.claude/skills/untrusted`

	got, source := tool.CanUse(context.Background(), map[string]any{"command": command})
	if got != tools.PermissionAsk || source != "safety_check:bypass_immune" {
		t.Fatalf("direct Claude skill write = %v (%s), want ask safety_check:bypass_immune", got, source)
	}
}
