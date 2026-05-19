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

// TestContainsEnterPlanMode — guards the mid-turn upgrade path.
// 2026-05-18 audit: if the model batches [EnterPlanMode, Write]
// in the same turn, the loop must detect the EnterPlanMode and
// collect the rest, not execute Write first.
func TestContainsEnterPlanMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []llm.ContentBlock
		want bool
	}{
		{"empty", nil, false},
		{"no enter", []llm.ContentBlock{{ToolName: "Read"}, {ToolName: "Write"}}, false},
		{"has enter", []llm.ContentBlock{{ToolName: "Read"}, {ToolName: "EnterPlanMode"}}, true},
		{"only enter", []llm.ContentBlock{{ToolName: "EnterPlanMode"}}, true},
		{"enter and write together", []llm.ContentBlock{{ToolName: "EnterPlanMode"}, {ToolName: "Write"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containsEnterPlanMode(c.in); got != c.want {
				t.Errorf("containsEnterPlanMode = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSplitEnterPlanModeTools(t *testing.T) {
	t.Parallel()
	in := []llm.ContentBlock{
		{ToolName: "Write", ToolUseID: "1"},
		{ToolName: "EnterPlanMode", ToolUseID: "2"},
		{ToolName: "Read", ToolUseID: "3"},
		{ToolName: "EnterPlanMode", ToolUseID: "4"},
	}
	enter, other := splitEnterPlanModeTools(in)
	if len(enter) != 2 {
		t.Errorf("expected 2 enter tools; got %d", len(enter))
	}
	if len(other) != 2 {
		t.Errorf("expected 2 other tools; got %d", len(other))
	}
	if enter[0].ToolUseID != "2" || enter[1].ToolUseID != "4" {
		t.Errorf("enter IDs wrong: %v", []string{enter[0].ToolUseID, enter[1].ToolUseID})
	}
	if other[0].ToolName != "Write" || other[1].ToolName != "Read" {
		t.Errorf("other tools wrong: %v", []string{other[0].ToolName, other[1].ToolName})
	}
}

// fakeReadOnlyChecker — test stub for splitReadOnlyTools.
type fakeReadOnlyChecker struct {
	readOnly map[string]bool
}

func (f fakeReadOnlyChecker) IsReadOnly(tool, _ string) bool {
	return f.readOnly[tool]
}

func TestSplitReadOnlyTools(t *testing.T) {
	t.Parallel()
	checker := fakeReadOnlyChecker{
		readOnly: map[string]bool{
			"Read":  true,
			"LS":    true,
			"Grep":  true,
			"Write": false,
			"Edit":  false,
			"Bash":  false,
		},
	}
	in := []llm.ContentBlock{
		{ToolName: "Read", ToolUseID: "1"},
		{ToolName: "Write", ToolUseID: "2"},
		{ToolName: "Grep", ToolUseID: "3"},
		{ToolName: "Edit", ToolUseID: "4"},
	}
	ro, se := splitReadOnlyTools(in, checker)
	if len(ro) != 2 || ro[0].ToolUseID != "1" || ro[1].ToolUseID != "3" {
		t.Errorf("read-only partition wrong: %+v", ro)
	}
	if len(se) != 2 || se[0].ToolUseID != "2" || se[1].ToolUseID != "4" {
		t.Errorf("side-effect partition wrong: %+v", se)
	}
}

func TestSplitReadOnlyTools_NilGate(t *testing.T) {
	t.Parallel()
	in := []llm.ContentBlock{
		{ToolName: "Read"},
		{ToolName: "Write"},
	}
	// nil checker → everything treated as side-effect (fallback to
	// pre-2026-05-18 "collect all" plan-mode behavior).
	ro, se := splitReadOnlyTools(in, nil)
	if len(ro) != 0 {
		t.Errorf("nil checker should yield zero read-only; got %d", len(ro))
	}
	if len(se) != 2 {
		t.Errorf("nil checker should yield all-side-effect; got %d", len(se))
	}
}

func TestStringifyToolInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"nil", nil, ""},
		{"command (Bash)", map[string]any{"command": "ls /tmp"}, "ls /tmp"},
		{"path (Read/Write)", map[string]any{"path": "/etc/passwd"}, "/etc/passwd"},
		{"file_path", map[string]any{"file_path": "/x"}, "/x"},
		{"query (Grep)", map[string]any{"query": "TODO"}, "TODO"},
		{"url (WebFetch)", map[string]any{"url": "https://x"}, "https://x"},
		{"unknown key", map[string]any{"weird": "y"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stringifyToolInput(c.in); got != c.want {
				t.Errorf("stringifyToolInput(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestLoop_PrePlanMode_RoundTrips — PrePlanMode/SetPrePlanMode round-trip.
func TestLoop_PrePlanMode_RoundTrips(t *testing.T) {
	t.Parallel()
	l := &Loop{}
	if l.PrePlanMode() != "" {
		t.Errorf("default PrePlanMode should be empty; got %q", l.PrePlanMode())
	}
	l.SetPrePlanMode("acceptEdits")
	if l.PrePlanMode() != "acceptEdits" {
		t.Errorf("after Set, PrePlanMode = %q, want %q", l.PrePlanMode(), "acceptEdits")
	}
	l.SetPrePlanMode("")
	if l.PrePlanMode() != "" {
		t.Errorf("after Set('') PrePlanMode should be empty; got %q", l.PrePlanMode())
	}
}
