package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

type contractBashProbeTool struct{}

func (*contractBashProbeTool) Name() string        { return "Bash" }
func (*contractBashProbeTool) Description() string { return "contract hook probe" }
func (*contractBashProbeTool) InputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"command"},
		"additionalProperties": false,
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
		},
	}
}
func (*contractBashProbeTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (*contractBashProbeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (*contractBashProbeTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}
func (*contractBashProbeTool) IsEnabled() bool { return true }

func TestContractObservesPostHookEffectiveToolInput(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&contractBashProbeTool{})
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(_ context.Context, _ pubhook.Context, _ *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		return &pubhook.ModifiedPreToolUse{ModifiedInput: map[string]any{
			"command": "sed -i 's/old/new/' internal/a.go",
		}}
	}))
	loop := &Loop{
		Registry: registry,
		Gate:     permission.New(permission.ModeBypassPermissions),
		Hooks:    hooks,
	}
	uses := []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "bash-hook", ToolName: "Bash",
		ToolInput: map[string]any{"command": "printf harmless"},
	}}

	results, err := loop.executeBatch(context.Background(), uses, make(chan Event, 16), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("unexpected results: %+v", results)
	}
	if got := toolInputString(uses[0].ToolInput, "command"); got != "sed -i 's/old/new/' internal/a.go" {
		t.Fatalf("executeBatch did not expose finalized hook input: %q", got)
	}

	var tracker contractTracker
	tracker.observeToolUses(uses)
	if !tracker.thresholdMet() {
		t.Fatalf("post-hook Bash mutation escaped verifier threshold: %+v", tracker)
	}
}

func TestMergeEffectiveToolUsesPreservesSameAssistantBatchEpoch(t *testing.T) {
	all := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "verify", ToolName: "Agent", ToolInput: map[string]any{"subagent_type": "verify"}},
		{Type: "tool_use", ToolUseID: "bash", ToolName: "Bash", ToolInput: map[string]any{"command": "printf harmless"}},
	}
	bashPhase := []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "bash", ToolName: "Bash",
		ToolInput: map[string]any{"command": "python -c 'from pathlib import Path; Path(\"a\").write_text(\"b\")'"},
	}}
	mergeEffectiveToolUses(all, bashPhase)

	var tracker contractTracker
	tracker.observeToolUses(all)
	if tracker.verifyDispatched {
		t.Fatal("same-assistant-batch verifier incorrectly attested a sibling mutation")
	}
	if !tracker.thresholdMet() {
		t.Fatalf("merged effective Bash mutation escaped threshold: %+v", tracker)
	}
}
