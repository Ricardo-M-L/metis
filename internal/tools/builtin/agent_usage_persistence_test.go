package builtin

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Exercise the actual Agent.Execute -> child Loop -> provider stream path.
// Its persisted usage must come from provider receipts, while the shared
// budget keeps its existing one-write-per-attempt accounting contract.
func TestAgentExecute_PersistsProviderUsageWithoutChangingBudget(t *testing.T) {
	cases := []struct {
		name      string
		usage     []llm.StreamEvent
		available bool
		input     int64
		output    int64
		cacheRead int64
		cacheMake int64
	}{
		{
			name: "absolute receipts replace repeated and corrected snapshots",
			usage: []llm.StreamEvent{
				{Type: "usage", InputTokens: 20, OutputTokens: 8, CacheReadInputTokens: 4, CacheCreationInputTokens: 5, HasInputTokens: true, HasOutputTokens: true, HasCacheReadInputTokens: true, HasCacheCreationInputTokens: true},
				{Type: "usage", InputTokens: 20, OutputTokens: 8, CacheReadInputTokens: 4, CacheCreationInputTokens: 5, HasInputTokens: true, HasOutputTokens: true, HasCacheReadInputTokens: true, HasCacheCreationInputTokens: true},
				{Type: "usage", InputTokens: 10, OutputTokens: 3, HasInputTokens: true, HasOutputTokens: true},
			},
			available: true, input: 10, output: 3, cacheRead: 4, cacheMake: 5,
		},
		{
			name: "reported zero is available",
			usage: []llm.StreamEvent{
				{Type: "usage", HasInputTokens: true, HasOutputTokens: true, HasCacheReadInputTokens: true, HasCacheCreationInputTokens: true},
			},
			available: true,
		},
		{
			name: "missing counters remain unknown",
			usage: []llm.StreamEvent{
				{Type: "usage"},
			},
		},
		{
			name: "legacy positive counters count as reported",
			usage: []llm.StreamEvent{
				{Type: "message_delta", InputTokens: 7, OutputTokens: 2},
			},
			available: true, input: 7, output: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			events := []llm.StreamEvent{{Type: "text_delta", TextDelta: "completed"}}
			events = append(events, tc.usage...)
			events = append(events, llm.StreamEvent{Type: "message_delta", StopReason: "end_turn"}, llm.StreamEvent{Type: "message_stop"})
			tracker := budget.NewTracker(0, budget.Rates{
				InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000,
				CacheReadPerMTok: 1_000_000, CacheWritePerMTok: 1_000_000,
			})
			budgetWrites := 0
			tracker.SetPersistence(func(float64) error { budgetWrites++; return nil })
			ctx := agent.WithBudget(context.Background(), tracker)
			tool := NewAgent(permission.New(permission.ModeBypass), &fakeProvider{events: events}, tools.NewRegistry(), "model", "system").
				WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "usage-parent")
			agentID := executeUsageAgent(t, ctx, tool, map[string]any{"prompt": "inspect usage"})
			got, err := agent.LoadSubAgentUsage(dir, agentID)
			if err != nil {
				t.Fatalf("LoadSubAgentUsage: %v", err)
			}
			if got.Available != tc.available || got.InputTokens != tc.input || got.OutputTokens != tc.output || got.CacheReadInputTokens != tc.cacheRead || got.CacheCreationInputTokens != tc.cacheMake {
				t.Fatalf("persisted usage = %+v; want available=%v input=%d output=%d cacheRead=%d cacheCreate=%d", got, tc.available, tc.input, tc.output, tc.cacheRead, tc.cacheMake)
			}
			if got.ParentSessionID != "usage-parent" {
				t.Fatalf("usage owner = %q, want usage-parent", got.ParentSessionID)
			}
			if tc.available && got.Revision == "" {
				t.Fatal("reported usage needs a revision for live conversation invalidation")
			}
			wantSpend := float64(tc.input + tc.output + tc.cacheRead + tc.cacheMake)
			if gotSpend := tracker.SpentUSD(); gotSpend != wantSpend || budgetWrites != 1 {
				t.Fatalf("shared budget = %v (%d writes), want %v (1 write)", gotSpend, budgetWrites, wantSpend)
			}
		})
	}
}

func TestAgentExecute_UsageResumeKeepsOnlyTheCurrentParentLineage(t *testing.T) {
	dir := t.TempDir()
	run := func(parent, resume string, inputTokens int) string {
		t.Helper()
		provider := &fakeProvider{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "completed"},
			{Type: "usage", InputTokens: inputTokens, HasInputTokens: true},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		}}
		tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "model", "system").
			WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, parent)
		input := map[string]any{"prompt": "continue"}
		if resume != "" {
			input["resume_from"] = resume
		}
		return executeUsageAgent(t, context.Background(), tool, input)
	}
	agentID := run("parent-a", "", 10)
	if gotID := run("parent-a", agentID, 2); gotID != agentID {
		t.Fatalf("resume replaced stable agent ID: %q != %q", gotID, agentID)
	}
	continued, err := agent.LoadSubAgentUsage(dir, agentID)
	if err != nil || !continued.Available || continued.ParentSessionID != "parent-a" || continued.InputTokens != 12 {
		t.Fatalf("same-parent continuation usage = %+v, %v; want parent-a input=12", continued, err)
	}
	if gotID := run("parent-b", agentID, 4); gotID != agentID {
		t.Fatalf("cross-parent resume replaced stable agent ID: %q != %q", gotID, agentID)
	}
	moved, err := agent.LoadSubAgentUsage(dir, agentID)
	if err != nil || !moved.Available || moved.ParentSessionID != "parent-b" || moved.InputTokens != 4 {
		t.Fatalf("cross-parent continuation leaked old usage = %+v, %v; want parent-b input=4", moved, err)
	}
}

func executeUsageAgent(t *testing.T, ctx context.Context, tool Agent, input map[string]any) string {
	t.Helper()
	result, err := tool.Execute(ctx, input)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Agent.Execute = %+v, %v", result, err)
	}
	metadata, ok := result.Presentation["subagent"].(map[string]any)
	if !ok {
		t.Fatalf("missing subagent presentation: %#v", result.Presentation)
	}
	agentID, _ := metadata["agentId"].(string)
	if agentID == "" {
		t.Fatalf("missing stable agent ID: %#v", metadata)
	}
	return agentID
}
