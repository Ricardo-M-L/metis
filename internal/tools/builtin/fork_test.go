package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// helloForkProvider — same shape as helloProvider in agent_test.go
// but a different text payload so a test asserting on the output
// can't accidentally tell whether Agent or Fork ran.
func helloForkProvider() llm.Provider {
	return &fakeProvider{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "fork"},
		{Type: "text_delta", TextDelta: "ed-text"},
		{Type: "message_delta", StopReason: "end_turn", OutputTokens: 1},
		{Type: "message_stop"},
	}}
}

func TestFork_RequiresDirective(t *testing.T) {
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("expected error for missing directive")
	}
}

func TestFork_EmptyDirectiveIsError(t *testing.T) {
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	_, err := tool.Execute(context.Background(), map[string]any{"directive": "   "})
	if err == nil {
		t.Error("blank directive must error")
	}
}

func TestFork_NilProviderErrors(t *testing.T) {
	tool := NewFork(permission.New(permission.ModeBypass), nil, nil)
	_, err := tool.Execute(context.Background(), map[string]any{"directive": "x"})
	if err == nil {
		t.Error("nil provider should error")
	}
}

func TestFork_NoParentSnapshotIsError(t *testing.T) {
	// Fork called outside a live loop — no parent snapshot in ctx.
	// Should produce an IsError result, not a panic.
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	res, err := tool.Execute(context.Background(), map[string]any{"directive": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when no parent snapshot, got: %+v", res)
	}
	if !strings.Contains(res.Output, "no parent context") {
		t.Errorf("error message should explain why: %q", res.Output)
	}
}

func TestFork_EmptyParentMessagesIsError(t *testing.T) {
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	ctx := agent.WithParentSnapshot(context.Background(), agent.ParentSnapshot{
		System: "test", Model: "test-model", Messages: nil,
	})
	res, err := tool.Execute(ctx, map[string]any{"directive": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected IsError for empty parent history, got: %+v", res)
	}
	if !strings.Contains(res.Output, "use Agent") {
		t.Errorf("error should hint at Agent for cold-start, got: %q", res.Output)
	}
}

func TestFork_InheritsAndReturnsText(t *testing.T) {
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	// Build a parent snapshot with non-empty history.
	ctx := agent.WithParentSnapshot(context.Background(), agent.ParentSnapshot{
		System: "test-system",
		Model:  "test-model",
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}},
		},
	})
	res, err := tool.Execute(ctx, map[string]any{"directive": "now do the thing"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("expected success, got error: %s", res.Output)
	}
	// Provider streams "fork" + "ed-text" → child final text should
	// concatenate to "forked-text".
	if !strings.Contains(res.Output, "forked-text") {
		t.Errorf("expected fork's child text in output, got: %q", res.Output)
	}
}

func TestFork_EmptyFinalIsError(t *testing.T) {
	provider := &fakeProvider{events: []llm.StreamEvent{
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}
	tool := NewFork(permission.New(permission.ModeBypass), provider, tools.NewRegistry())
	ctx := agent.WithParentSnapshot(context.Background(), agent.ParentSnapshot{
		System: "test-system",
		Model:  "test-model",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	res, err := tool.Execute(ctx, map[string]any{"directive": "finish the task"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "without text output") {
		t.Fatalf("empty fork completion must be an error result: %+v", res)
	}
}

func TestFork_NestingLimitEnforced(t *testing.T) {
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	ctx := agent.WithParentSnapshot(context.Background(), agent.ParentSnapshot{
		System: "s", Model: "m",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	// Pretend we're already at max depth.
	ctx = context.WithValue(ctx, forkDepthKey{}, defaultMaxForkDepth)
	res, err := tool.Execute(ctx, map[string]any{"directive": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "nesting limit") {
		t.Errorf("expected nesting-limit error, got: %+v", res)
	}
}

// TestFork_DefaultCapIsOne (2026-05-15) pins the new default. Fork from
// the top level (depth=0) is allowed; nested Fork from depth=1 must be
// rejected without an explicit config override. Matches CC's
// FORK_BOILERPLATE_TAG rule which forbids Fork-inside-Fork entirely.
func TestFork_DefaultCapIsOne(t *testing.T) {
	if defaultMaxForkDepth != 1 {
		t.Fatalf("default cap = %d, want 1 (the new default)", defaultMaxForkDepth)
	}
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	parentSnap := agent.ParentSnapshot{
		System: "s", Model: "m",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "x"}}}},
	}

	t.Run("top-level fork allowed (depth=0)", func(t *testing.T) {
		ctx := agent.WithParentSnapshot(context.Background(), parentSnap)
		// No forkDepthKey set → ctx.Value returns nil → depth = 0
		res, err := tool.Execute(ctx, map[string]any{"directive": "do x"})
		if err != nil {
			t.Fatalf("top-level fork should NOT error: %v", err)
		}
		if res.IsError {
			t.Errorf("top-level fork should succeed; got error: %s", res.Output)
		}
	})

	t.Run("nested fork rejected (depth=1)", func(t *testing.T) {
		ctx := agent.WithParentSnapshot(context.Background(), parentSnap)
		// Simulate: we are inside a fork child; depth = 1.
		ctx = context.WithValue(ctx, forkDepthKey{}, 1)
		res, err := tool.Execute(ctx, map[string]any{"directive": "nested fork"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !res.IsError {
			t.Error("nested fork (depth=1) MUST be rejected under new default cap=1")
		}
		if !strings.Contains(res.Output, "nesting limit") {
			t.Errorf("error message should mention nesting limit; got: %s", res.Output)
		}
		if !strings.Contains(res.Output, "max_fork_depth") {
			t.Errorf("error should suggest config override; got: %s", res.Output)
		}
	})
}

// TestFork_ConfigOverrideRaisesCap — users who want fork-in-fork can set
// [agents].max_fork_depth = 2 in config. With the override, depth=1 is
// permitted and only depth=2 hits the wall.
func TestFork_ConfigOverrideRaisesCap(t *testing.T) {
	// Construct a Fork with MaxDepth=2 (simulates config.Agents.MaxForkDepth=2).
	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	tool.MaxDepth = 2

	parentSnap := agent.ParentSnapshot{
		System: "s", Model: "m",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "x"}}}},
	}

	// depth=1 should now be allowed (it wasn't under default cap=1).
	ctx := agent.WithParentSnapshot(context.Background(), parentSnap)
	ctx = context.WithValue(ctx, forkDepthKey{}, 1)
	res, _ := tool.Execute(ctx, map[string]any{"directive": "ok"})
	if res.IsError {
		t.Errorf("with cap=2, depth=1 should pass; got error: %s", res.Output)
	}

	// depth=2 still rejected.
	ctx = agent.WithParentSnapshot(context.Background(), parentSnap)
	ctx = context.WithValue(ctx, forkDepthKey{}, 2)
	res, _ = tool.Execute(ctx, map[string]any{"directive": "deep"})
	if !res.IsError {
		t.Errorf("with cap=2, depth=2 should fail; got: %+v", res)
	}
}

func TestFork_Schema(t *testing.T) {
	tool := Fork{}
	schema := tool.InputSchema()
	if schema["type"] != "object" {
		t.Errorf("schema type wrong: %v", schema["type"])
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "directive" {
		t.Errorf("required = %v, want [directive]", required)
	}
}

func TestFork_CanUseGoesThroughGate(t *testing.T) {
	tool := NewFork(permission.New(permission.ModeDeny), helloForkProvider(), tools.NewRegistry())
	p, _ := tool.CanUse(context.Background(), map[string]any{"directive": "x"})
	if p != tools.PermissionDeny {
		t.Errorf("deny mode should propagate; got %v", p)
	}
}

func TestForkDirective_BoilerplateAddsFocusReminder(t *testing.T) {
	got := buildForkDirective("draft the migration")
	if !strings.Contains(got, "[fork]") {
		t.Error("missing [fork] tag")
	}
	if !strings.Contains(got, "draft the migration") {
		t.Error("user directive lost in boilerplate")
	}
	if !strings.Contains(got, "Do not ask") {
		t.Error("focus reminder missing — child may default to clarifying questions")
	}
}

// ─── ParentSnapshot context key plumbing ─────────────────────────

func TestParentSnapshot_RoundTripsThroughContext(t *testing.T) {
	snap := agent.ParentSnapshot{
		System: "sys", Model: "mdl",
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}
	ctx := agent.WithParentSnapshot(context.Background(), snap)
	got := agent.ParentSnapshotFromContext(ctx)
	if got == nil {
		t.Fatal("snapshot lost in round-trip")
	}
	if got.System != "sys" || got.Model != "mdl" {
		t.Errorf("snapshot fields: %+v", got)
	}
	if len(got.Messages) != 1 {
		t.Errorf("messages: got %d, want 1", len(got.Messages))
	}
}

func TestParentSnapshot_DefensiveCopyOfMessages(t *testing.T) {
	original := []llm.Message{
		{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "first"}}},
	}
	ctx := agent.WithParentSnapshot(context.Background(), agent.ParentSnapshot{
		Messages: original,
	})
	// Mutate original AFTER attaching — snapshot must not see it.
	original[0] = llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "MUTATED"}}}
	got := agent.ParentSnapshotFromContext(ctx)
	if got.Messages[0].Content[0].Text != "first" {
		t.Errorf("snapshot was not defensively copied — sees mutation: %q", got.Messages[0].Content[0].Text)
	}
}

func TestParentSnapshot_NoSnapshotReturnsNil(t *testing.T) {
	got := agent.ParentSnapshotFromContext(context.Background())
	if got != nil {
		t.Errorf("absent key should yield nil, got: %+v", got)
	}
}
