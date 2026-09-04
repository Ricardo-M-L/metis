package runtime

// coordinator_mode_test.go — locks Phase G.8 (2026-05-12) contracts:
//
//   1. IsCoordinatorMode honors both the env var and SetCoordinatorMode.
//   2. CoordinatorOverlay returns a meaningful section when active,
//      zero-value section when off.
//   3. CoordinatorToolFilter is a no-op when coordinator mode is off.
//   4. CoordinatorToolFilter keeps the complete structured Task* loop
//      and drops hands-on mutation tools (Edit/Write/Bash/TodoWrite).
//   5. METIS_COORDINATOR_EXTRA_TOOLS adds extras to the whitelist.
//   6. FilterRegistryInPlace installs a durable allowlist so late Skill,
//      plugin, MCP, and IDE registration cannot bypass coordinator mode.
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
	t.Setenv(CoordinatorEnvVar, "")
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
	t.Setenv(CoordinatorEnvVar, "")
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
	t.Setenv(CoordinatorEnvVar, "")
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
	for toolName := range coordinatorAllowedTools {
		if !strings.Contains(ov.Body, toolName) {
			t.Errorf("body should document whitelisted tool %q; got %q", toolName, short(ov.Body))
		}
	}
	if !ov.Cache {
		t.Errorf("coordinator overlay should be Cache=true (stable per session); got false")
	}
}

func TestCoordinatorToolFilter_OffIsNoop(t *testing.T) {
	defer resetCoordinatorMode()
	t.Setenv(CoordinatorEnvVar, "")
	in := []string{"Edit", "Write", "Agent", "Bash"}
	got := CoordinatorToolFilter(in)
	if len(got) != len(in) {
		t.Errorf("filter off should leave list intact; got %v from %v", got, in)
	}
}

func TestCoordinatorToolFilter_OnDropsMutations(t *testing.T) {
	defer resetCoordinatorMode()
	SetCoordinatorMode(true)
	t.Setenv(CoordinatorExtraToolsEnvVar, "")
	in := []string{
		"Edit", "Write", "Bash", "TodoWrite", "NotebookEdit",
		"Agent", "Fork", "SubAgentList", "Read", "Grep",
		"TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "TaskOutput", "TaskStop",
	}
	got := CoordinatorToolFilter(in)
	gotSet := make(map[string]bool)
	for _, n := range got {
		gotSet[n] = true
	}
	// Should keep:
	for _, want := range []string{
		"Agent", "Fork", "SubAgentList", "Read", "Grep",
		"TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "TaskOutput", "TaskStop",
	} {
		if !gotSet[want] {
			t.Errorf("filter should KEEP %q; got %v", want, got)
		}
	}
	// Should drop:
	for _, drop := range []string{"Edit", "Write", "Bash", "TodoWrite", "NotebookEdit"} {
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

func TestFilterRegistryInPlace_InstallsDurableAllowlist(t *testing.T) {
	defer resetCoordinatorMode()
	t.Setenv(CoordinatorEnvVar, "")
	reg := tools.NewRegistry()
	kept := []string{"Agent", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "TaskOutput", "TaskStop"}
	blocked := []string{"Edit", "Write", "Bash", "TodoWrite"}
	for _, name := range append(append([]string{}, kept...), blocked...) {
		reg.Register(testTool{name: name})
	}

	// Off: no replacement.
	FilterRegistryInPlace(reg)
	if _, ok := reg.Get("Bash"); !ok {
		t.Errorf("Bash should still be registered when mode is off")
	}

	// On: hands-on tools disappear; orchestration and the full structured task
	// lifecycle keep their original implementations.
	SetCoordinatorMode(true)
	t.Setenv(CoordinatorExtraToolsEnvVar, "")
	FilterRegistryInPlace(reg)
	for _, name := range blocked {
		if _, ok := reg.Get(name); ok {
			t.Errorf("%s should be removed from the coordinator registry", name)
		}
	}
	for _, name := range kept {
		tool, ok := reg.Get(name)
		if !ok || tool == nil {
			t.Errorf("%s should remain registered", name)
			continue
		}
		if _, ok := reg.GetForModel(name); !ok {
			t.Errorf("%s should remain visible to model-originated lookup", name)
		}
	}

	// Dynamic publication must obey the same policy. These are the two
	// production escape paths: Skill is replaced after plugin discovery and MCP
	// namespaces are replaced when a server connects or reconnects.
	reg.Replace(testTool{name: "Skill"})
	reg.Register(testTool{name: "LateMutation"})
	reg.ReplacePrefix("mcp__late__", []tools.Tool{
		testTool{name: "mcp__late__write"},
	})
	for _, name := range []string{"Skill", "LateMutation", "mcp__late__write"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("late tool %s bypassed coordinator allowlist", name)
		}
	}

	visible := make(map[string]bool)
	for _, tool := range reg.ModelToolsForCache() {
		visible[tool.Name()] = true
	}
	for _, name := range blocked {
		if visible[name] {
			t.Errorf("%s should be absent from ModelToolsForCache", name)
		}
	}
	for _, name := range kept {
		if !visible[name] {
			t.Errorf("%s should remain present in ModelToolsForCache", name)
		}
	}
}

func TestFilterRegistryInPlace_ExtraToolsRemainDurable(t *testing.T) {
	defer resetCoordinatorMode()
	SetCoordinatorMode(true)
	t.Setenv(CoordinatorExtraToolsEnvVar, "TodoWrite,mcp__trusted__read")
	reg := tools.NewRegistry()
	reg.Register(testTool{name: "Agent"})
	FilterRegistryInPlace(reg)

	reg.Replace(testTool{name: "TodoWrite"})
	reg.ReplacePrefix("mcp__trusted__", []tools.Tool{
		testTool{name: "mcp__trusted__read"},
	})
	for _, name := range []string{"Agent", "TodoWrite", "mcp__trusted__read"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("allowed late tool %s should remain registered", name)
		}
	}
}

func TestFilterRegistryInPlace_RejectsBroadMCPExtra(t *testing.T) {
	defer resetCoordinatorMode()
	SetCoordinatorMode(true)
	t.Setenv(CoordinatorExtraToolsEnvVar, "mcp__trusted,mcp__*")
	reg := tools.NewRegistry()
	reg.Register(testTool{name: "Agent"})
	FilterRegistryInPlace(reg)
	reg.ReplacePrefix("mcp__trusted__", []tools.Tool{
		testTool{name: "mcp__trusted__read"},
	})
	if _, ok := reg.Get("mcp__trusted__read"); ok {
		t.Fatal("broad MCP coordinator extra should not expose an entire server namespace")
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

func (t testTool) Name() string                                 { return t.name }
func (t testTool) Description() string                          { return "test tool " + t.name }
func (t testTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (t testTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }
func (t testTool) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t testTool) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

// silence unused import linter
var _ = permission.ModeBypass

func (testTool) IsEnabled() bool { return true }
