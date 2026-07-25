package agent

// plan_mode_split_test.go — exercises splitExitPlanModeTools, the
// partition the PlanMode gate uses to whitelist EnterPlanMode and
// ExitPlanMode while routing other tools through normal permission checks.
//
// 2026-05-15 fix: before this, plan-mode tool batches all went to
// emitPlan(), so a model that called ExitPlanMode in plan mode never
// actually exited — its own call got collected as the plan instead of
// running.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type planBatchTestTool struct {
	tools.BaseTool
	name       string
	gate       *permission.Gate
	activate   bool
	exitMode   permission.Mode
	deny       bool
	executions *int
}

func (t planBatchTestTool) Name() string        { return t.name }
func (t planBatchTestTool) Description() string { return "plan batch regression tool" }
func (t planBatchTestTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t planBatchTestTool) Concurrency(map[string]any) tools.Concurrency {
	if t.name == "EnterPlanMode" || t.name == "ExitPlanMode" {
		return tools.ConcurrencyExclusive
	}
	return tools.ConcurrencySafe
}
func (t planBatchTestTool) CanUse(ctx context.Context, _ map[string]any) (tools.Permission, string) {
	if t.deny {
		return tools.PermissionDeny, "test rejected plan entry"
	}
	if t.name == "EnterPlanMode" || t.gate == nil {
		return tools.PermissionAllow, ""
	}
	decision, reason := t.gate.Check(ctx, t.name, "")
	switch decision {
	case permission.DecisionAllow:
		return tools.PermissionAllow, reason
	case permission.DecisionDeny:
		return tools.PermissionDeny, reason
	default:
		return tools.PermissionAsk, reason
	}
}
func (t planBatchTestTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	if t.executions != nil {
		*t.executions++
	}
	if t.activate {
		if t.gate != nil {
			t.gate.SetMode(permission.ModePlan)
		}
		if ctrl := PlanControllerFromContext(ctx); ctrl != nil {
			ctrl.SetPlanMode(true)
		}
	}
	if t.exitMode != "" {
		if t.gate != nil {
			t.gate.SetMode(t.exitMode)
		}
		if ctrl := PlanControllerFromContext(ctx); ctrl != nil {
			ctrl.SetPlanMode(false)
		}
	}
	return &tools.Result{Output: t.name + " ok"}, nil
}

type planModeStreamProvider struct {
	streams [][]llm.StreamEvent
	calls   int
}

func (p *planModeStreamProvider) Name() string          { return "plan-mode-script" }
func (p *planModeStreamProvider) ModelID() string       { return "test-model" }
func (p *planModeStreamProvider) MaxContextTokens() int { return 200_000 }
func (p *planModeStreamProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{StopReason: "end_turn"}, nil
}
func (p *planModeStreamProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	if p.calls >= len(p.streams) {
		return &mockStream{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "done"},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		}}, nil
	}
	events := p.streams[p.calls]
	p.calls++
	return &mockStream{events: events}, nil
}

func toolBatchEvents(calls ...llm.ContentBlock) []llm.StreamEvent {
	events := make([]llm.StreamEvent, 0, len(calls)*3+2)
	for _, call := range calls {
		events = append(events,
			llm.StreamEvent{Type: "tool_use_start", ToolUseID: call.ToolUseID, ToolName: call.ToolName},
			llm.StreamEvent{Type: "tool_input_delta", ToolUseID: call.ToolUseID, InputDelta: `{}`},
			llm.StreamEvent{Type: "tool_use_stop", ToolUseID: call.ToolUseID},
		)
	}
	return append(events,
		llm.StreamEvent{Type: "message_delta", StopReason: "tool_use"},
		llm.StreamEvent{Type: "message_stop"},
	)
}

func resultMessagesForIDs(history []llm.Message, ids map[string]bool) []llm.Message {
	var matched []llm.Message
	for _, msg := range history {
		if msg.Role != llm.RoleUser {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "tool_result" && ids[block.ToolUseID] {
				matched = append(matched, msg)
				break
			}
		}
	}
	return matched
}

func TestLoop_PlanEntryMixedBatchCommitsOneOrderedResultMessage(t *testing.T) {
	calls := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "enter-1", ToolName: "EnterPlanMode"},
		{Type: "tool_use", ToolUseID: "read-1", ToolName: "Read"},
		{Type: "tool_use", ToolUseID: "write-1", ToolName: "Write"},
	}
	provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{
		toolBatchEvents(calls...),
		{
			{Type: "text_delta", TextDelta: "plan recovered"},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		},
	}}
	gate := permission.New(permission.ModeAcceptEdits)
	gate.SetReadOnlyHook(func(name, _ string) bool { return name == "Read" })
	registry := tools.NewRegistry()
	registry.Register(planBatchTestTool{name: "EnterPlanMode", gate: gate, activate: true})
	registry.Register(planBatchTestTool{name: "Read", gate: gate})
	registry.Register(planBatchTestTool{name: "Write", gate: gate})
	loop := NewLoop(provider, registry, gate, nil, "system", 5)
	loop.AppendUser("plan before changing anything")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)
	for ev := range out {
		if ev.Kind == EventPlan {
			t.Fatalf("mixed batch should feed ordinary results back, not archive a pseudo-plan: %+v", ev)
		}
	}

	messages := resultMessagesForIDs(loop.History(), map[string]bool{
		"enter-1": true, "read-1": true, "write-1": true,
	})
	if len(messages) != 1 {
		t.Fatalf("mixed assistant batch produced %d result messages, want exactly 1", len(messages))
	}
	got := messages[0].Content
	if len(got) != 3 {
		t.Fatalf("result block count = %d, want 3: %+v", len(got), got)
	}
	wantIDs := []string{"enter-1", "read-1", "write-1"}
	for i, want := range wantIDs {
		if got[i].Type != "tool_result" || got[i].ToolUseID != want {
			t.Fatalf("result[%d] = (%q,%q), want tool_result %q", i, got[i].Type, got[i].ToolUseID, want)
		}
	}
	if !got[2].IsError || !strings.Contains(got[2].ToolResult, "denied") {
		t.Fatalf("write sibling should be denied in plan mode, got %+v", got[2])
	}
}

func TestLoop_DeniedPlanEntrySkipsWriteSibling(t *testing.T) {
	calls := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "enter-denied", ToolName: "EnterPlanMode"},
		{Type: "tool_use", ToolUseID: "write-skipped", ToolName: "Write"},
	}
	provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{
		toolBatchEvents(calls...),
		{
			{Type: "text_delta", TextDelta: "entry was rejected"},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		},
	}}
	gate := permission.New(permission.ModeAcceptEdits)
	gate.SetReadOnlyHook(func(name, _ string) bool { return name == "Read" })
	writeExecutions := 0
	registry := tools.NewRegistry()
	registry.Register(planBatchTestTool{name: "EnterPlanMode", gate: gate, deny: true})
	registry.Register(planBatchTestTool{name: "Write", gate: gate, executions: &writeExecutions})
	loop := NewLoop(provider, registry, gate, nil, "system", 5)
	loop.AppendUser("enter plan then write")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if writeExecutions != 0 {
		t.Fatalf("Write executed %d time(s) after EnterPlanMode was denied", writeExecutions)
	}
	messages := resultMessagesForIDs(loop.History(), map[string]bool{
		"enter-denied": true, "write-skipped": true,
	})
	if len(messages) != 1 {
		t.Fatalf("denied entry batch produced %d result messages, want 1", len(messages))
	}
	if got := messages[0].Content; len(got) != 2 ||
		got[0].ToolUseID != "enter-denied" || got[1].ToolUseID != "write-skipped" ||
		!got[1].IsError || !strings.Contains(got[1].ToolResult, "sibling execution was refused") {
		t.Fatalf("unexpected denied-entry result batch: %+v", got)
	}
}

func TestLoop_ExitPlanModeApprovalRefusesPreApprovalSiblings(t *testing.T) {
	calls := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "read-before-approval", ToolName: "Read"},
		{Type: "tool_use", ToolUseID: "exit-approved", ToolName: "ExitPlanMode"},
		{Type: "tool_use", ToolUseID: "write-before-approval", ToolName: "Write"},
	}
	provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{
		toolBatchEvents(calls...),
		{
			{Type: "text_delta", TextDelta: "approval result received; regenerate implementation calls"},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		},
	}}
	gate := permission.New(permission.ModePlan)
	gate.SetReadOnlyHook(func(name, _ string) bool { return name == "Read" })
	readExecutions, exitExecutions, writeExecutions := 0, 0, 0
	registry := tools.NewRegistry()
	registry.Register(planBatchTestTool{name: "Read", gate: gate, executions: &readExecutions})
	registry.Register(planBatchTestTool{
		name: "ExitPlanMode", gate: gate, executions: &exitExecutions,
		exitMode: permission.ModeAcceptEdits,
	})
	registry.Register(planBatchTestTool{name: "Write", gate: gate, executions: &writeExecutions})
	loop := NewLoop(provider, registry, gate, nil, "system", 5)
	loop.SetPlanMode(true)
	loop.AppendUser("approve this plan")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitExecutions != 1 || readExecutions != 0 || writeExecutions != 0 {
		t.Fatalf("execution counts exit/read/write = %d/%d/%d, want 1/0/0", exitExecutions, readExecutions, writeExecutions)
	}
	if gate.Mode() != permission.ModeAcceptEdits || loop.IsPlanMode() {
		t.Fatalf("approval did not leave plan mode: gate=%q loopPlan=%v", gate.Mode(), loop.IsPlanMode())
	}
	messages := resultMessagesForIDs(loop.History(), map[string]bool{
		"read-before-approval": true, "exit-approved": true, "write-before-approval": true,
	})
	if len(messages) != 1 || len(messages[0].Content) != 3 {
		t.Fatalf("approval batch must produce one ordered result message: %+v", messages)
	}
	for _, idx := range []int{0, 2} {
		got := messages[0].Content[idx]
		if !got.IsError || !strings.Contains(got.ToolResult, "approval boundary") {
			t.Fatalf("sibling result[%d] was not refused: %+v", idx, got)
		}
	}
}

func TestLoop_PlanModeWithoutGateStillPairsEveryToolResult(t *testing.T) {
	provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{
		toolBatchEvents(llm.ContentBlock{Type: "tool_use", ToolUseID: "read-no-gate", ToolName: "Read"}),
		{
			{Type: "text_delta", TextDelta: "cannot explore without gate metadata"},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		},
	}}
	loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 5)
	loop.SetPlanMode(true)
	loop.AppendUser("inspect safely")
	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	messages := resultMessagesForIDs(loop.History(), map[string]bool{"read-no-gate": true})
	if len(messages) != 1 || len(messages[0].Content) != 1 {
		t.Fatalf("nil-gate plan batch was not paired exactly once: %+v", messages)
	}
	result := messages[0].Content[0]
	if !result.IsError || !strings.Contains(result.ToolResult, "no permission gate") {
		t.Fatalf("nil-gate fallback should return an explicit denial, got %+v", result)
	}
	for _, msg := range loop.History() {
		for _, block := range msg.Content {
			if strings.Contains(block.ToolResult, orphanRepairMessage) {
				t.Fatal("nil-gate plan fallback left an orphan tool result")
			}
		}
	}
}

func TestLoop_BuildRequestTracksLivePlanOverlay(t *testing.T) {
	const overlay = "# dynamic plan overlay\nread only until approval"
	loop := &Loop{
		System:           "base system\n\n" + overlay + "\n\n" + overlay,
		SystemSections:   []llm.SystemSection{{Name: "base", Body: "base system", Cache: true}, {Name: "plan_mode", Body: overlay, Cache: true}},
		PlanSystemPrompt: overlay,
	}
	loop.SetPlanMode(true)

	assertCount := func(active bool) {
		t.Helper()
		req := loop.buildRequest(nil)
		want := 0
		if active {
			want = 1
		}
		if got := strings.Count(req.System, overlay); got != want {
			t.Fatalf("System overlay count = %d, want %d (active=%v)", got, want, active)
		}
		sectionCount := 0
		for _, section := range req.SystemSections {
			if section.Name == "plan_mode" {
				sectionCount++
				if section.Body != overlay || section.Cache || !section.Volatile {
					t.Fatalf("dynamic plan section metadata/body wrong: %+v", section)
				}
			}
		}
		if sectionCount != want {
			t.Fatalf("SystemSections plan count = %d, want %d (active=%v)", sectionCount, want, active)
		}
	}

	assertCount(true)
	loop.SetPlanMode(false)
	assertCount(false)
	loop.SetPlanMode(true)
	assertCount(true)
}

func TestNewLoop_HasPlanOverlayFallbackForDirectAndSubagentBuilders(t *testing.T) {
	loop := NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModePlan), nil, "base", 5)
	loop.SetPlanMode(true)
	active := loop.buildRequest(nil)
	if !strings.Contains(active.System, "ExitPlanMode") ||
		!strings.Contains(active.System, "read-only tools") {
		t.Fatalf("direct NewLoop plan request lacks agent-local fallback: %q", active.System)
	}
	loop.SetPlanMode(false)
	inactive := loop.buildRequest(nil)
	if strings.Contains(inactive.System, agentPlanSystemPromptFallback) {
		t.Fatal("agent-local plan fallback remained after leaving plan mode")
	}
}

func TestMergeBatchResults_EmptyIDsUseOriginalToolOrder(t *testing.T) {
	original := []llm.ContentBlock{
		{Type: "tool_use", ToolName: "Read"},
		{Type: "tool_use", ToolName: "Read"},
		{Type: "tool_use", ToolUseID: "write-1", ToolName: "Write"},
	}
	slots := make([]llm.ContentBlock, len(original))
	filled := make([]bool, len(original))
	mergeBatchResults(original, original[:2], []llm.ContentBlock{
		{Type: "tool_result", ToolResult: "first"},
		{Type: "tool_result", ToolResult: "second"},
	}, slots, filled)
	mergeBatchResults(original, original[2:], []llm.ContentBlock{
		{Type: "tool_result", ToolUseID: "write-1", ToolResult: "third"},
	}, slots, filled)
	ordered := orderedBatchResults(original, slots, filled)
	if got := []string{ordered[0].ToolResult, ordered[1].ToolResult, ordered[2].ToolResult}; got[0] != "first" || got[1] != "second" || got[2] != "third" {
		t.Fatalf("merged result order = %v", got)
	}
}

func TestLoop_PlanModeRedundantEnterExecutesInsteadOfArchiving(t *testing.T) {
	provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{
		{
			{Type: "tool_use_start", ToolUseID: "enter-1", ToolName: "EnterPlanMode"},
			{Type: "tool_input_delta", ToolUseID: "enter-1", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "enter-1"},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		},
		{
			{Type: "text_delta", TextDelta: "The plan is ready."},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		},
	}}
	registry := tools.NewRegistry()
	registry.Register(forkFakeTool{name: "EnterPlanMode"})
	gate := permission.New(permission.ModePlan)
	loop := NewLoop(provider, registry, gate, nil, "system", 5)
	loop.SetPlanMode(true)
	loop.AppendUser("make a plan")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)
	for ev := range out {
		if ev.Kind == EventPlan {
			t.Fatalf("redundant EnterPlanMode must execute as metadata, not emit archived plan: %+v", ev)
		}
	}
	if provider.calls != 2 {
		t.Fatalf("loop should continue after EnterPlanMode result; provider calls=%d, want 2", provider.calls)
	}

	var sawResult bool
	for _, msg := range loop.History() {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.ToolUseID == "enter-1" {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Fatal("redundant EnterPlanMode should produce a normal tool_result")
	}
}

func TestSplitExitPlanModeTools_PartitionsCorrectly(t *testing.T) {
	t.Parallel()
	in := []llm.ContentBlock{
		{Type: "tool_use", ToolName: "Read", ToolUseID: "1"},
		{Type: "tool_use", ToolName: "ExitPlanMode", ToolUseID: "2"},
		{Type: "tool_use", ToolName: "Edit", ToolUseID: "3"},
		{Type: "tool_use", ToolName: "ExitPlanMode", ToolUseID: "4"},
		{Type: "tool_use", ToolName: "EnterPlanMode", ToolUseID: "5"},
	}
	exit, other := splitExitPlanModeTools(in)
	if len(exit) != 3 {
		t.Errorf("expected 3 plan meta tools; got %d", len(exit))
	}
	if len(other) != 2 {
		t.Errorf("expected 2 other tools; got %d", len(other))
	}
	if exit[0].ToolUseID != "2" || exit[1].ToolUseID != "4" || exit[2].ToolUseID != "5" {
		t.Errorf("plan meta IDs wrong: %v", []string{exit[0].ToolUseID, exit[1].ToolUseID, exit[2].ToolUseID})
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
	if !l.IsPlanMode() {
		t.Error("SetPlanMode(true) did not flip the bool")
	}
	l.SetPlanMode(false)
	if l.IsPlanMode() {
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
		{"unknown key", map[string]any{"weird": "y"}, `{"weird":"y"}`},
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
