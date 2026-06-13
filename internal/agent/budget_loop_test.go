package agent

// budget_loop_test.go — locks the --max-budget-usd integration added
// 2026-06-11 (claude-code's maxBudgetUsd, QueryEngine.ts:145-149):
// usage lands on the shared tracker after every stream, the 90%
// warning is injected once as a user message, and an exceeded budget
// stops the loop at the pre-request boundary with stop=budget_usd.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// usageProvider replies with an end-of-turn stream that reports token
// usage, so Loop.Run exercises the AddUsage path.
type usageProvider struct {
	in, out int
}

func (p *usageProvider) Name() string          { return "usage-stub" }
func (p *usageProvider) ModelID() string       { return "" }
func (p *usageProvider) MaxContextTokens() int { return 200_000 }
func (p *usageProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{StopReason: "end_turn"}, nil
}
func (p *usageProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &usageStream{in: p.in, out: p.out}, nil
}

type usageStream struct {
	in, out int
	step    int
}

func (s *usageStream) Close() error { return nil }
func (s *usageStream) Recv() (llm.StreamEvent, error) {
	s.step++
	switch s.step {
	case 1:
		return llm.StreamEvent{Type: "message_start", InputTokens: s.in}, nil
	case 2:
		return llm.StreamEvent{Type: "text_delta", TextDelta: "done"}, nil
	case 3:
		return llm.StreamEvent{Type: "message_delta", StopReason: "end_turn", OutputTokens: s.out}, nil
	case 4:
		return llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"}, nil
	}
	return llm.StreamEvent{}, io.EOF
}

func TestLoopRun_RecordsUsageOnBudget(t *testing.T) {
	p := &usageProvider{in: 1_000_000, out: 1_000_000}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Budget = budget.NewTracker(0, budget.Rates{InputPerMTok: 3, OutputPerMTok: 15})
	loop.AppendUser("ping")

	out := make(chan Event, 32)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if got := loop.Budget.SpentUSD(); got != 18 {
		t.Errorf("SpentUSD = %v, want 18", got)
	}
}

// An already-exceeded budget must stop BEFORE the LLM call with
// stop_reason=budget_usd (clean pre-request boundary, no orphans).
func TestLoopRun_StopsWhenBudgetExceeded(t *testing.T) {
	p := &usageProvider{in: 10, out: 10}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	tr := budget.NewTracker(0.01, budget.Rates{InputPerMTok: 10})
	tr.AddUsage(1_000_000, 0, 0, 0) // $10 — way past the 1¢ cap
	loop.Budget = tr
	loop.AppendUser("ping")

	out := make(chan Event, 32)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	close(out)
	var stop string
	var sawInfo bool
	for ev := range out {
		if ev.Kind == EventLoopDone {
			stop = ev.StopReason
		}
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "budget") {
			sawInfo = true
		}
	}
	if stop != "budget_usd" {
		t.Errorf("StopReason = %q, want budget_usd", stop)
	}
	if !sawInfo {
		t.Error("expected an EventInfo explaining the budget stop")
	}
}

// Past 90%, the one-shot wrap-up reminder is injected as a user
// message at the NEXT pre-request boundary — never between an
// assistant reply and the messages it produced (2026-06-11 review:
// the original injection point sat before the assistant append and
// corrupted transcript order).
func TestLoopRun_InjectsBudgetWarning(t *testing.T) {
	p := &usageProvider{in: 10, out: 0}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Budget = budget.NewTracker(1.0, budget.Rates{InputPerMTok: 10})
	loop.Budget.AddUsage(95_000, 0, 0, 0) // $0.95 of $1 — already past 90%
	loop.AppendUser("ping")

	out := make(chan Event, 32)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	hist := loop.History()
	warnIdx, assistantIdx := -1, -1
	for i, m := range hist {
		if m.Role == llm.RoleUser {
			for _, b := range m.Content {
				if strings.Contains(b.Text, "Budget check") {
					warnIdx = i
				}
			}
		}
		if m.Role == llm.RoleAssistant {
			assistantIdx = i
		}
	}
	if warnIdx < 0 {
		t.Fatal("90% budget warning was not injected into the transcript")
	}
	// Ordering invariant: the warning precedes the assistant reply
	// generated AFTER it — i.e. the model saw the warning.
	if assistantIdx >= 0 && warnIdx > assistantIdx {
		t.Errorf("warning at %d landed after assistant reply at %d — transcript order corrupted", warnIdx, assistantIdx)
	}
}
