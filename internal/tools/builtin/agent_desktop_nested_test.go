package builtin

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func desktopAgentCallEvents(id, input string) []llm.StreamEvent {
	return []llm.StreamEvent{
		{Type: "tool_use_start", ToolUseID: id, ToolName: "Agent"},
		{Type: "tool_input_delta", ToolUseID: id, InputDelta: input},
		{Type: "tool_use_stop", ToolUseID: id, InputDelta: input},
	}
}

func desktopAgentToolStream(events []llm.StreamEvent) llm.StreamReader {
	return &fakeStream{events: append(events,
		llm.StreamEvent{Type: "message_delta", StopReason: "tool_use"},
		llm.StreamEvent{Type: "message_stop"})}
}

func desktopCheckToolResults(req llm.Request) error {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.IsError {
				return fmt.Errorf("delegation failed: %s", block.ToolResult)
			}
		}
	}
	return nil
}

// The sequence is deliberately synchronous: root delegates to child, child
// delegates to grandchild, and both ancestors need another model turn after
// their foreground Agent result. Every boundary runs through the real Loop
// and executeBatch rather than directly exercising lease helpers.
type desktopNestedProvider struct {
	llm.Provider
	calls atomic.Int32
}

func (p *desktopNestedProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	if err := desktopCheckToolResults(req); err != nil {
		return nil, err
	}
	switch p.calls.Add(1) {
	case 1:
		return desktopAgentToolStream(desktopAgentCallEvents("child", `{"prompt":"child delegates","name":"child"}`)), nil
	case 2:
		return desktopAgentToolStream(desktopAgentCallEvents("grandchild", `{"prompt":"grandchild finishes","name":"grandchild"}`)), nil
	default:
		return p.Provider.Stream(ctx, req)
	}
}

func desktopRootLoop(t *testing.T, provider llm.Provider, roster *agent.Roster) *agent.Loop {
	t.Helper()
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	tool := NewAgent(gate, provider, registry, "model", "system").WithRoster(roster)
	tool.MaxDepth = 3
	registry.Register(tool)
	loop := agent.NewLoop(provider, registry, gate, agent.NewHookRegistry(), "system", 12)
	loop.AppendUser("delegate the task")
	return loop
}

func runDesktopTestLoop(t *testing.T, loop *agent.Loop, roster *agent.Roster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer func() {
		roster.CancelAll()
		waitForRosterCount(t, roster, 0, 2*time.Second)
	}()
	events := make(chan agent.Event, 512)
	if err := loop.Run(ctx, events); err != nil {
		t.Fatalf("root Loop.Run failed: %v", err)
	}
	for len(events) > 0 {
		ev := <-events
		if ev.Kind == agent.EventError || (ev.ToolResult != nil && ev.ToolResult.IsError) {
			t.Fatalf("delegation error event: %+v", ev)
		}
	}
}

func TestAgentDesktopNestedForegroundReleasesAncestorPermits(t *testing.T) {
	for _, desktop := range []bool{true, false} {
		t.Run(fmt.Sprintf("desktop_%t", desktop), func(t *testing.T) {
			t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", "")
			if desktop {
				t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", t.TempDir())
			}
			t.Setenv("METIS_DESKTOP_SUBAGENT_SLOTS", "1")
			t.Setenv("METIS_DESKTOP_SUBAGENT_CAP", "1")
			provider := &desktopNestedProvider{Provider: helloProvider()}
			roster := agent.NewRoster(8)
			runDesktopTestLoop(t, desktopRootLoop(t, provider, roster), roster)
			if provider.calls.Load() != 5 {
				t.Fatalf("nested model requests = %d, want root + child + grandchild + child resume + root resume", provider.calls.Load())
			}
			for _, name := range []string{"child", "grandchild"} {
				teammate, _, ok := roster.LookupRecentlyFinished(name)
				if !ok || teammate.Snapshot().Status != agent.StatusCompleted {
					t.Fatalf("%s did not complete: %+v", name, teammate)
				}
			}
		})
	}
}

type desktopBackgroundTreeProvider struct {
	llm.Provider
	roster    *agent.Roster
	rootCalls atomic.Int32
	children  atomic.Int32
	active    atomic.Int32
	peak      atomic.Int32
}

func (p *desktopBackgroundTreeProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for peak := p.peak.Load(); active > peak; peak = p.peak.Load() {
		if p.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	if err := desktopCheckToolResults(req); err != nil {
		return nil, err
	}
	depth, _ := ctx.Value(agentDepthKey{}).(int)
	if depth > 0 {
		p.children.Add(1)
		// Keep each child executing long enough that remaining children queue.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
		return p.Provider.Stream(ctx, req)
	}
	if p.rootCalls.Add(1) == 1 {
		var events []llm.StreamEvent
		for i := 0; i < 4; i++ {
			id := fmt.Sprintf("background-%d", i)
			input := fmt.Sprintf(`{"prompt":"complete background work","name":%q,"run_in_background":true}`, id)
			events = append(events, desktopAgentCallEvents(id, input)...)
		}
		return desktopAgentToolStream(events), nil
	}
	// The parent's next real provider request stays in flight while the
	// remaining children finish. With a total budget of two, children must
	// continue to make progress through their independent execution leases.
	for p.roster.Count() != 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return p.Provider.Stream(ctx, req)
}

func TestAgentDesktopBackgroundTreeCompletesWithTwoSlots(t *testing.T) {
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", t.TempDir())
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOTS", "2")
	t.Setenv("METIS_DESKTOP_SUBAGENT_CAP", "2")
	roster := agent.NewRoster(1)
	provider := &desktopBackgroundTreeProvider{Provider: helloProvider(), roster: roster}
	runDesktopTestLoop(t, desktopRootLoop(t, provider, roster), roster)
	if provider.children.Load() != 4 || provider.rootCalls.Load() < 2 {
		t.Fatalf("root requests=%d child requests=%d", provider.rootCalls.Load(), provider.children.Load())
	}
	if provider.peak.Load() > 2 {
		t.Fatalf("provider calls exceeded total execution capacity: %d", provider.peak.Load())
	}
	if len(roster.List()) != 4 {
		t.Fatalf("completed child count=%d", len(roster.List()))
	}
	for _, teammate := range roster.List() {
		if teammate.Status != agent.StatusCompleted {
			t.Fatalf("background child did not complete: %+v", teammate)
		}
	}
}
