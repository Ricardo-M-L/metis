package builtin

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestSearchCanUse_IncludesRootInRuleMatching(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*permission.Gate) (tools.Permission, string)
	}{
		{
			name: "Glob",
			call: func(g *permission.Gate) (tools.Permission, string) {
				return (Glob{gate: g}).CanUse(context.Background(), map[string]any{
					"root": "/private/denied-tree", "pattern": "**/*.go",
				})
			},
		},
		{
			name: "Grep",
			call: func(g *permission.Gate) (tools.Permission, string) {
				return (Grep{gate: g}).CanUse(context.Background(), map[string]any{
					"root": "/private/denied-tree", "pattern": "token",
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := permission.New(permission.ModeBypass)
			gate.AppendRules(permission.Rule{
				Tool: tc.name, Match: "/private/denied-tree",
				Verb: permission.DecisionDeny, Source: "test-root",
			})
			got, source := tc.call(gate)
			if got != tools.PermissionDeny || source != "test-root" {
				t.Fatalf("root-scoped deny = %v (%s), want deny from test-root", got, source)
			}
		})
	}
}

func TestGrepCanUse_SecretRootIsSilentlyDeniedInBypass(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	tool := Grep{gate: gate}
	got, source := tool.CanUse(context.Background(), map[string]any{
		"root": "/Users/alice/.ssh", "pattern": "PRIVATE KEY",
	})
	if got != tools.PermissionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("Grep secret root = %v (%s), want bypass-immune deny", got, source)
	}
}
