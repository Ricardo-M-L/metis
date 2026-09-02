package bash

import (
	"context"
	"strings"
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

func TestBashCanUse_BypassSilentlyDeniesDestructiveSystemMutation(t *testing.T) {
	t.Parallel()
	gate := permission.New(permission.ModeBypassPermissions)
	tool := New(gate, config.ToolBashSettings{})
	for _, command := range []string{
		`sudo -n /usr/bin/apt-get remove openssh-server`,
		`echo ready && kubectl delete namespace production`,
		`command env LANG=C docker system prune -af`,
	} {
		got, source := tool.CanUse(context.Background(), map[string]any{"command": command})
		if got != tools.PermissionDeny || !strings.Contains(source, "destructive system mutation") {
			t.Fatalf("destructive command %q = %v (%s), want silent deny", command, got, source)
		}
	}
}

func TestBashCanUse_BypassAllowsScopedDockerAndKubectlOperations(t *testing.T) {
	t.Parallel()
	gate := permission.New(permission.ModeBypassPermissions)
	tool := New(gate, config.ToolBashSettings{})
	for _, command := range []string{
		`docker build -t local/test .`,
		`docker container rm stopped-container`,
		`kubectl apply -f deployment.yaml`,
		`kubectl delete pod one-broken-pod`,
	} {
		got, source := tool.CanUse(context.Background(), map[string]any{"command": command})
		if got != tools.PermissionAllow || source != "mode:bypassPermissions" {
			t.Fatalf("scoped command %q = %v (%s), want bypass allow", command, got, source)
		}
	}
}

func TestBashCanUse_BypassSilentlyDeniesDirectClaudeSkillWrite(t *testing.T) {
	t.Parallel()
	gate := permission.New(permission.ModeBypassPermissions)
	tool := New(gate, config.ToolBashSettings{})
	const command = `mkdir -p ~/.claude/skills/untrusted`

	got, source := tool.CanUse(context.Background(), map[string]any{"command": command})
	if got != tools.PermissionDeny || source != "safety_check:bypass_immune" {
		t.Fatalf("direct Claude skill write = %v (%s), want deny safety_check:bypass_immune", got, source)
	}
}

func TestBashCanUse_BypassSensitiveDenyCannotBePromotedBySandboxAutoAllow(t *testing.T) {
	t.Parallel()
	gate := permission.New(permission.ModeBypassPermissions)
	tool := autoAllowBashForTest(t, gate)
	const command = `echo pwned > ~/.metis/config.toml`

	got, source := tool.CanUse(context.Background(), map[string]any{"command": command})
	if got != tools.PermissionDeny || source != "safety_check:bypass_immune" {
		t.Fatalf("bypass sensitive write + sandbox auto-allow = %v (%s), want deny safety_check:bypass_immune", got, source)
	}
}

func TestBashCanUse_FullAccessSkipsImplicitDangerChecks(t *testing.T) {
	gate := permission.New(permission.ModeFullAccess)
	tool := New(gate, config.ToolBashSettings{})
	// CanUse only: never execute this regression-test command.
	got, source := tool.CanUse(context.Background(), map[string]any{"command": `rm -rf /`})
	if got != tools.PermissionAllow || source != "mode:fullAccess" {
		t.Fatalf("fullAccess dangerous command admission = %v (%s), want allow mode:fullAccess", got, source)
	}
}
