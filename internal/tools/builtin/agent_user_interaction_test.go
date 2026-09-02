package builtin

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// askThenRecoverProvider asks one question and, after receiving the structured
// tool error produced by an unattended child, finishes normally. It models the
// exact two-request loop a real provider uses around AskUser.
type askThenRecoverProvider struct {
	calls atomic.Int32
}

func (*askThenRecoverProvider) Name() string          { return "ask-then-recover" }
func (*askThenRecoverProvider) MaxContextTokens() int { return 100_000 }
func (*askThenRecoverProvider) ModelID() string       { return "ask-then-recover" }
func (*askThenRecoverProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}

func (p *askThenRecoverProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	if p.calls.Add(1) == 1 {
		input := `{"question":"choose a default","options":["safe","fast"]}`
		return &fakeStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "ask-child", ToolName: "AskUser"},
			{Type: "tool_input_delta", ToolUseID: "ask-child", InputDelta: input},
			{Type: "tool_use_stop", ToolUseID: "ask-child", InputDelta: input},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "continued with unattended default"},
		{Type: "message_delta", StopReason: "end_turn", OutputTokens: 1},
		{Type: "message_stop"},
	}}, nil
}

func fullAccessAgentWithAsk(provider llm.Provider) (Agent, *agent.Roster, *jobs.Registry) {
	gate := permission.New(permission.ModeFullAccess)
	registry := tools.NewRegistry()
	registry.Register(AskUser{gate: gate})
	roster := agent.NewRoster(0)
	jobPool := jobs.NewRegistry("")
	tool := NewAgent(gate, provider, registry, "model", "system").
		WithRoster(roster).
		WithJobsPool(jobPool)
	return tool, roster, jobPool
}

func TestFullAccessSubAgentAskUserReturnsPromptlyInForeground(t *testing.T) {
	provider := &askThenRecoverProvider{}
	tool, _, jobPool := fullAccessAgentWithAsk(provider)
	t.Cleanup(func() { jobPool.ResetAndWait(0) })

	started := time.Now()
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":          "finish without a human",
		"timeout_seconds": 1,
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("foreground fullAccess child = (%+v, %v), want successful recovery", res, err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("foreground child waited for an AskUser reply until timeout: %s", elapsed)
	}
	if !strings.Contains(res.Output, "continued with unattended default") {
		t.Fatalf("foreground output = %q, want post-denial recovery text", res.Output)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want AskUser turn plus recovery turn", calls)
	}
}

func TestFullAccessSubAgentAskUserReturnsPromptlyInBackground(t *testing.T) {
	provider := &askThenRecoverProvider{}
	tool, roster, jobPool := fullAccessAgentWithAsk(provider)
	t.Cleanup(func() {
		roster.CancelAll()
		jobPool.ResetAndWait(0)
	})

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "finish without a human",
		"run_in_background": true,
		"timeout_seconds":   1,
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("background fullAccess child spawn = (%+v, %v)", res, err)
	}
	agentID, _ := res.Meta["agent_id"].(string)
	if agentID == "" {
		t.Fatalf("background result missing agent_id: %+v", res)
	}
	waitForRosterCount(t, roster, 0, 2*time.Second)

	output, err := NewSubAgentOutput(permission.New(permission.ModeFullAccess), roster).Execute(
		context.Background(), map[string]any{"agent_id": agentID},
	)
	if err != nil || output == nil || output.IsError {
		t.Fatalf("background output = (%+v, %v), want completed recovery", output, err)
	}
	if !strings.Contains(output.Output, "status=completed") ||
		!strings.Contains(output.Output, "continued with unattended default") {
		t.Fatalf("background output = %q, want completed post-denial recovery", output.Output)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want AskUser turn plus recovery turn", calls)
	}
}

// Top-level fullAccess remains interactive. Only Agent-created child contexts
// are marked unattended; a real parent UI must still receive and answer AskUser.
func TestFullAccessTopLevelAskUserStillReachesUI(t *testing.T) {
	provider := &askThenRecoverProvider{}
	gate := permission.New(permission.ModeFullAccess)
	registry := tools.NewRegistry()
	registry.Register(AskUser{gate: gate})
	loop := agent.NewLoop(provider, registry, gate, agent.NewHookRegistry(), "system", 4)
	loop.Model = "model"
	loop.AppendUser("ask me, then finish")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events := make(chan agent.Event, 32)
	done := make(chan error, 1)
	go func() {
		done <- loop.Run(ctx, events)
		close(events)
	}()

	var text strings.Builder
	var sawAsk bool
	for ev := range events {
		switch ev.Kind {
		case agent.EventAskUser:
			sawAsk = true
			if ev.AskUserReply == nil {
				t.Fatal("top-level AskUser event has nil reply channel")
			}
			ev.AskUserReply <- "safe"
		case agent.EventTextDelta:
			text.WriteString(ev.TextDelta)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("top-level loop: %v", err)
	}
	if !sawAsk {
		t.Fatal("top-level fullAccess AskUser was rejected instead of reaching the UI")
	}
	if got := text.String(); got != "continued with unattended default" {
		t.Fatalf("top-level final text = %q", got)
	}
}

func TestFullAccessForkAskUserReturnsPromptly(t *testing.T) {
	provider := &askThenRecoverProvider{}
	gate := permission.New(permission.ModeFullAccess)
	registry := tools.NewRegistry()
	registry.Register(AskUser{gate: gate})
	tool := NewFork(gate, provider, registry)
	ctx := agent.WithParentSnapshot(context.Background(), agent.ParentSnapshot{
		System: "parent system",
		Model:  "model",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type: "text",
				Text: "parent context",
			}},
		}},
	})
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	started := time.Now()
	res, err := tool.Execute(ctx, map[string]any{"directive": "finish without a human"})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("fullAccess Fork = (%+v, %v), want successful recovery", res, err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("Fork waited for an AskUser reply until timeout: %s", elapsed)
	}
	if !strings.Contains(res.Output, "continued with unattended default") {
		t.Fatalf("Fork output = %q, want post-denial recovery text", res.Output)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want AskUser turn plus recovery turn", calls)
	}
}
