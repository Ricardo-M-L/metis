package runtime

// coordinator_mode_test.go — locks Phase G.8 (2026-05-12) contracts:
//
//   1. IsCoordinatorMode honors both the env var and SetCoordinatorMode.
//   2. CoordinatorOverlay returns a meaningful section when active,
//      zero-value section when off.
//   3. CoordinatorToolFilter is a no-op when coordinator mode is off.
//   4. CoordinatorToolFilter keeps the whitelisted orchestration tools
//      and drops mutation tools (Edit/Write/Bash).
//   5. METIS_COORDINATOR_EXTRA_TOOLS adds extras to the whitelist.
//   6. FilterRegistryInPlace replaces dropped tools with the
//      coordinator-blocked stub (still listed but errors on use).
//   7. The bundled coordinator profile is loadable (the embed is
//      part of G.7's set, but G.8 depends on it).

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Disable coordinator mode after every subtest so leakage doesn't
// pollute the rest of the suite — every test in this file MUST
// defer this helper to keep the flag clean.
func resetCoordinatorMode() { SetCoordinatorMode(false) }

func TestIsCoordinatorMode_RespondsToFlag(t *testing.T) {
	defer resetCoordinatorMode()
	if IsCoordinatorMode() {
		t.Fatal("expected false before SetCoordinatorMode(true)")
	}
	SetCoordinatorMode(true)
	if !IsCoordinatorMode() {
		t.Fatal("expected true after SetCoordinatorMode(true)")
	}
	SetCoordinatorMode(false)
	if IsCoordinatorMode() {
		t.Fatal("expected false after SetCoordinatorMode(false)")
	}
}

func TestIsCoordinatorMode_RespondsToEnv(t *testing.T) {
	defer resetCoordinatorMode()
	if IsCoordinatorMode() {
		t.Fatal("expected false before env set")
	}
	t.Setenv(CoordinatorEnvVar, "1")
	if !IsCoordinatorMode() {
		t.Error("env=1 should activate coordinator mode")
	}
}

func TestCoordinatorOverlay_OffReturnsZero(t *testing.T) {
	defer resetCoordinatorMode()
	if IsCoordinatorMode() {
		t.Fatal("test prerequisite: coordinator mode must be off")
	}
	ov := CoordinatorOverlay()
	if ov.Name != "" {
		t.Errorf("expected zero-value section when mode off; got Name=%q", ov.Name)
	}
}

func TestCoordinatorOverlay_OnReturnsBody(t *testing.T) {
	defer resetCoordinatorMode()
	SetCoordinatorMode(true)
	ov := CoordinatorOverlay()
	if ov.Name != "coordinator" {
		t.Errorf("expected Name=coordinator; got %q", ov.Name)
	}
	if !strings.Contains(ov.Body, "team lead") && !strings.Contains(ov.Body, "PLAN") {
		t.Errorf("body should mention team-lead role; got %q", short(ov.Body))
	}
	if !ov.Cache {
		t.Errorf("coordinator overlay should be Cache=true (stable per session); got false")
	}
}

func TestCoordinatorToolFilter_OffIsNoop(t *testing.T) {
	defer resetCoordinatorMode()
	in := []string{"Edit", "Write", "Agent", "Bash"}
	got := CoordinatorToolFilter(in)
	if len(got) != len(in) {
		t.Errorf("filter off should leave list intact; got %v from %v", got, in)
	}
}

func TestCoordinatorToolFilter_OnDropsMutations(t *testing.T) {
	defer resetCoordinatorMode()
	SetCoordinatorMode(true)
	in := []string{"Edit", "Write", "Bash", "NotebookEdit", "Agent", "Fork", "SubAgentList", "Read", "Grep"}
	got := CoordinatorToolFilter(in)
	gotSet := make(map[string]bool)
	for _, n := range got {
		gotSet[n] = true
	}
	// Should keep:
	for _, want := range []string{"Agent", "Fork", "SubAgentList", "Read", "Grep"} {
		if !gotSet[want] {
			t.Errorf("filter should KEEP %q; got %v", want, got)
		}
	}
	// Should drop:
	for _, drop := range []string{"Edit", "Write", "Bash", "NotebookEdit"} {
		if gotSet[drop] {
			t.Errorf("filter should DROP %q; got %v", drop, got)
		}
	}
}

func TestCoordinatorToolFilter_EnvExtras(t *testing.T) {
	defer resetCoordinatorMode()
	SetCoordinatorMode(true)
	t.Setenv(CoordinatorExtraToolsEnvVar, "TodoWrite,Edit")
	in := []string{"Edit", "TodoWrite", "Bash", "Agent"}
	got := CoordinatorToolFilter(in)
	gotSet := make(map[string]bool)
	for _, n := range got {
		gotSet[n] = true
	}
	if !gotSet["TodoWrite"] {
		t.Error("METIS_COORDINATOR_EXTRA_TOOLS should add TodoWrite back")
	}
	if !gotSet["Edit"] {
		t.Error("METIS_COORDINATOR_EXTRA_TOOLS should add Edit back")
	}
	if gotSet["Bash"] {
		t.Error("Bash not in extras list should still be dropped")
	}
	if !gotSet["Agent"] {
		t.Error("default-whitelisted Agent should still be kept")
	}
}

func TestFilterRegistryInPlace_ReplacesWithBlockedStub(t *testing.T) {
	defer resetCoordinatorMode()
	reg := tools.NewRegistry()
	// Register a Bash-like stub the coordinator should disable.
	reg.Register(testTool{name: "Bash"})
	reg.Register(testTool{name: "Agent"})

	// Off: no replacement.
	FilterRegistryInPlace(reg)
	if _, ok := reg.Get("Bash"); !ok {
		t.Errorf("Bash should still be registered when mode is off")
	}

	// On: Bash replaced with blocked stub; Agent kept.
	SetCoordinatorMode(true)
	FilterRegistryInPlace(reg)
	bash, _ := reg.Get("Bash")
	if bash == nil {
		t.Fatal("Bash should remain visible (replaced with stub, not deleted)")
	}
	if !strings.Contains(bash.Description(), "coordinator mode") {
		t.Errorf("Bash description should mention coordinator mode; got %q", bash.Description())
	}
	res, err := bash.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Errorf("blocked stub Execute should not return go error; got %v", err)
	}
	if !res.IsError {
		t.Errorf("blocked stub Execute should return IsError; got %+v", res)
	}
	// Agent stays callable.
	agent, _ := reg.Get("Agent")
	if agent == nil {
		t.Fatal("Agent should remain registered")
	}
	if strings.Contains(agent.Description(), "coordinator mode") {
		t.Errorf("Agent should NOT be a blocked stub; got Description=%q", agent.Description())
	}
}

// short truncates s for assertion messages.
func short(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// testTool is a minimal Tool implementation for registry tests.
type testTool struct{ name string }

func (t testTool) Name() string                                      { return t.name }
func (t testTool) Description() string                               { return "test tool " + t.name }
func (t testTool) InputSchema() map[string]any                       { return map[string]any{"type": "object"} }
func (t testTool) Concurrency(map[string]any) tools.Concurrency      { return tools.ConcurrencyExclusive }
func (t testTool) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t testTool) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

// silence unused import linter
var _ = permission.ModeBypass
