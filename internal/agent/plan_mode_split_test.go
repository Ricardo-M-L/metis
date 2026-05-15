package agent

// plan_mode_split_test.go — exercises splitExitPlanModeTools, the
// partition the PlanMode gate uses to whitelist ExitPlanMode while
// still emitting other tools as plan tool_use blocks.
//
// 2026-05-15 fix: before this, plan-mode tool batches all went to
// emitPlan(), so a model that called ExitPlanMode in plan mode never
// actually exited — its own call got collected as the plan instead of
// running.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestSplitExitPlanModeTools_PartitionsCorrectly(t *testing.T) {
	t.Parallel()
	in := []llm.ContentBlock{
		{Type: "tool_use", ToolName: "Read", ToolUseID: "1"},
		{Type: "tool_use", ToolName: "ExitPlanMode", ToolUseID: "2"},
		{Type: "tool_use", ToolName: "Edit", ToolUseID: "3"},
		{Type: "tool_use", ToolName: "ExitPlanMode", ToolUseID: "4"},
	}
	exit, other := splitExitPlanModeTools(in)
	if len(exit) != 2 {
		t.Errorf("expected 2 exit tools; got %d", len(exit))
	}
	if len(other) != 2 {
		t.Errorf("expected 2 other tools; got %d", len(other))
	}
	if exit[0].ToolUseID != "2" || exit[1].ToolUseID != "4" {
		t.Errorf("exit IDs wrong: %v", []string{exit[0].ToolUseID, exit[1].ToolUseID})
	}
	if other[0].ToolName != "Read" || other[1].ToolName != "Edit" {
		t.Errorf("other tools wrong: %v", []string{other[0].ToolName, other[1].ToolName})
	}
}

func TestSplitExitPlanModeTools_AllOther(t *testing.T) {
	t.Parallel()
	in := []llm.ContentBlock{
		{Type: "tool_use", ToolName: "Read"},
		{Type: "tool_use", ToolName: "Edit"},
	}
	exit, other := splitExitPlanModeTools(in)
	if len(exit) != 0 {
		t.Errorf("exit should be empty; got %d", len(exit))
	}
	if len(other) != 2 {
		t.Errorf("other should be 2; got %d", len(other))
	}
}

func TestSplitExitPlanModeTools_AllExit(t *testing.T) {
	t.Parallel()
	in := []llm.ContentBlock{
		{Type: "tool_use", ToolName: "ExitPlanMode"},
	}
	exit, other := splitExitPlanModeTools(in)
	if len(exit) != 1 {
		t.Errorf("exit should be 1; got %d", len(exit))
	}
	if len(other) != 0 {
		t.Errorf("other should be empty; got %d", len(other))
	}
}

func TestSplitExitPlanModeTools_Empty(t *testing.T) {
	t.Parallel()
	exit, other := splitExitPlanModeTools(nil)
	if exit != nil || other != nil {
		t.Errorf("nil input should yield nil/nil; got exit=%v other=%v", exit, other)
	}
}

// TestLoop_SetPlanMode_SatisfiesPlanController — compile-time check
// that *Loop satisfies the PlanController interface that builtin
// plan-mode tools pull from context. If someone renames SetPlanMode
// or changes its signature, this stops compiling and we catch it
// before the wiring silently breaks.
func TestLoop_SetPlanMode_SatisfiesPlanController(t *testing.T) {
	t.Parallel()
	var _ PlanController = (*Loop)(nil)
	l := &Loop{}
	l.SetPlanMode(true)
	if !l.PlanMode {
		t.Error("SetPlanMode(true) did not flip the bool")
	}
	l.SetPlanMode(false)
	if l.PlanMode {
		t.Error("SetPlanMode(false) did not flip the bool back")
	}
}
