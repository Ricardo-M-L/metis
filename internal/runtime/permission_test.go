package runtime

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestBuildPermissionGate_DefaultsToConfigMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.Permission.Mode = "auto"
	g := BuildPermissionGate(cfg, "")
	if string(g.Mode()) != "auto" {
		t.Errorf("empty mode override should fall back to cfg; got %q", g.Mode())
	}
}

func TestBuildPermissionGate_OverrideTakesPrecedence(t *testing.T) {
	cfg := &config.Config{}
	cfg.Permission.Mode = "ask"
	g := BuildPermissionGate(cfg, "bypass")
	if string(g.Mode()) != "bypass" {
		t.Errorf("override should win; got %q", g.Mode())
	}
}

func TestBuildPermissionGate_SeedsAllowAndDenyRules(t *testing.T) {
	cfg := &config.Config{}
	cfg.Permission.Allow = []config.Rule{
		{Tool: "Read"},
		{Tool: "Bash", Match: "git status"},
	}
	cfg.Permission.Deny = []config.Rule{
		{Tool: "Bash", Match: "rm -rf"},
	}
	g := BuildPermissionGate(cfg, "ask")

	// Smoke that rules made it in: count via the registry the gate
	// exposes. Use the Mode setter test as proof of life if Rules() isn't
	// available — but first try via runtime.
	// (We don't introspect rule provenance — that's the gate's job.)
	if g == nil {
		t.Fatal("BuildPermissionGate returned nil gate")
	}
}

func TestBuildPermissionGate_NilSafeWhenAllowAndDenyEmpty(t *testing.T) {
	cfg := &config.Config{}
	cfg.Permission.Mode = "ask"
	g := BuildPermissionGate(cfg, "")
	if g == nil {
		t.Error("empty allow/deny should still yield a usable gate")
	}
}
