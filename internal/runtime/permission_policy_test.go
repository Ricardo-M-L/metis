package runtime

// permission_policy_test.go — locks the managed-policy tier added
// 2026-06-11 (claude-code's policySettings, types/permissions.ts:55-91):
// rules from METIS_POLICY_FILE land with "policy:" sources, which the
// gate ranks above interactive/config so they cannot be overridden.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestBuildPermissionGate_LoadsPolicyFile(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.toml")
	if err := os.WriteFile(policy, []byte(`
[[permission.deny]]
tool = "Bash"
match = "git push:*"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_POLICY_FILE", policy)

	cfg := &config.Config{}
	gate := BuildPermissionGate(cfg, "ask")
	// User clicks "always allow Bash" later in the session — must NOT
	// override the policy deny.
	gate.AppendRules(permission.Rule{Tool: "Bash", Verb: permission.DecisionAllow, Source: "interactive"})

	d, src := gate.Check(context.Background(), "Bash", "git push origin main")
	if d != permission.DecisionDeny {
		t.Errorf("policy deny must hold; got %v (src=%s)", d, src)
	}
	if d, _ := gate.Check(context.Background(), "Bash", "ls"); d != permission.DecisionAllow {
		t.Errorf("non-policy command should follow interactive allow, got %v", d)
	}
}

func TestBuildPermissionGate_NoPolicyFileIsFine(t *testing.T) {
	t.Setenv("METIS_POLICY_FILE", filepath.Join(t.TempDir(), "absent.toml"))
	gate := BuildPermissionGate(&config.Config{}, "ask")
	if d, _ := gate.Check(context.Background(), "Bash", "ls"); d != permission.DecisionAsk {
		t.Errorf("missing policy file: default ask expected, got %v", d)
	}
}
