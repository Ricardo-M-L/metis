package builtin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// The real Agent runner passes the child's context into this local provider.
// In the background case, recording waits until AFTER the parent selects a new
// plan; observing the ID at initial Stream entry would miss a shared binding.
type taskPlanContextProvider struct {
	entered chan struct{}
	release chan struct{}
	seen    chan string
	content string
}

func (*taskPlanContextProvider) Name() string          { return "task-plan-fixture" }
func (*taskPlanContextProvider) ModelID() string       { return "model" }
func (*taskPlanContextProvider) MaxContextTokens() int { return 100000 }
func (*taskPlanContextProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("unused")
}
func (p *taskPlanContextProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	if p.entered != nil {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	planID := tasks.PlanIDFromContext(ctx)
	if p.content != "" {
		if _, err := tasks.UpsertForContext(ctx, tasks.Item{Content: p.content, Status: "completed"}); err != nil {
			return nil, err
		}
	}
	p.seen <- planID
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "child task complete"},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}, nil
}

func newTaskPlanParentContext(t *testing.T, sid string) (context.Context, string) {
	t.Helper()
	plan, err := tasks.EnsureCurrentPlan(sid)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tasks.WithPlanID(tasks.WithSessionID(context.Background(), sid), plan)
	if _, err := tasks.ReplaceAllForContext(ctx, []tasks.Item{{Content: "original parent plan", Status: "completed"}}, false); err != nil {
		t.Fatal(err)
	}
	return ctx, plan
}

func rotateTaskPlanParent(t *testing.T, ctx context.Context, original string) string {
	t.Helper()
	if _, err := tasks.ReplaceAllForContext(ctx, []tasks.Item{{Content: "unrelated new parent plan", Status: "completed"}}, true); err != nil {
		t.Fatal(err)
	}
	next := tasks.PlanIDFromContext(ctx)
	if next == "" || next == original {
		t.Fatalf("fixture did not rotate the mutable parent binding: old=%q next=%q", original, next)
	}
	return next
}

func assertTaskPlanChildWrite(t *testing.T, parent context.Context, sid, original, content string) {
	t.Helper()
	current, err := tasks.PlanningItemsForContext(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range current {
		if item.Content == content {
			t.Fatalf("child's old-plan write leaked into the new parent plan: %+v", current)
		}
	}
	old, err := tasks.PlanningItemsForContext(tasks.WithPlanID(tasks.WithSessionID(context.Background(), sid), original))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range old {
		if item.Content == content {
			return
		}
	}
	t.Fatalf("child's write was not preserved in its original plan: %+v", old)
}

func TestAgentBackgroundTaskPlanRemainsFrozenAfterParentStartsNewPlan(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "background-parent-plan"
	parent, original := newTaskPlanParentContext(t, sid)
	provider := &taskPlanContextProvider{
		entered: make(chan struct{}), release: make(chan struct{}),
		seen: make(chan string, 4), content: "background child original-plan update",
	}
	roster := agent.NewRoster(2)
	t.Cleanup(roster.CancelAll)
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "model", "system").
		WithRoster(roster).WithSessionPersistence(t.TempDir(), sid)
	events := make(chan agent.Event, 64)
	ctx, cancel := context.WithTimeout(agent.WithEventOut(parent, events), 5*time.Second)
	defer cancel()
	result, err := tool.Execute(ctx, map[string]any{"prompt": "continue original plan", "run_in_background": true})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("start background Agent: %#v, %v", result, err)
	}
	id, _ := result.Meta["agent_id"].(string)
	if id == "" {
		t.Fatalf("missing child ID: %#v", result)
	}
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("background child did not reach the provider")
	}
	rotateTaskPlanParent(t, parent, original)
	close(provider.release)
	select {
	case got := <-provider.seen:
		if got != original {
			t.Fatalf("background child followed parent's mutable plan: got=%q want=%q", got, original)
		}
	case <-ctx.Done():
		t.Fatal("background child did not record its plan")
	}
	for {
		select {
		case event := <-events:
			if event.Kind != agent.EventSubAgentEnd {
				continue
			}
			if event.SubAgentID != id || event.SubAgentStatus != "completed" {
				t.Fatalf("unexpected child terminal event: %#v", event)
			}
			assertTaskPlanChildWrite(t, parent, sid, original, provider.content)
			return
		case <-ctx.Done():
			t.Fatal("background child did not complete")
		}
	}
}

func TestAgentDurableResumeRestoresOriginalTaskPlanUnderNewParentPlan(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "durable-parent-plan"
	parent, original := newTaskPlanParentContext(t, sid)
	provider := &taskPlanContextProvider{seen: make(chan string, 4)}
	dir := t.TempDir()
	newTool := func() Agent {
		roster := agent.NewRoster(2)
		t.Cleanup(roster.CancelAll)
		return NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "model", "system").
			WithRoster(roster).WithSessionPersistence(dir, sid)
	}
	initial, err := newTool().Execute(parent, map[string]any{"prompt": "initial child task"})
	if err != nil || initial == nil || initial.IsError {
		t.Fatalf("initial Agent: %#v, %v", initial, err)
	}
	meta, _ := initial.Presentation["subagent"].(map[string]any)
	id, _ := meta["agentId"].(string)
	if id == "" {
		t.Fatalf("missing durable child ID: %#v", initial)
	}
	if got := <-provider.seen; got != original {
		t.Fatalf("initial child plan=%q, want=%q", got, original)
	}
	snapshot, err := agent.LoadSubAgentSnapshot(dir, id)
	if err != nil || snapshot.Execution == nil || snapshot.Execution.TaskPlanID != original {
		t.Fatalf("original plan not saved with child execution: %#v, %v", snapshot, err)
	}
	rotateTaskPlanParent(t, parent, original)
	provider.content = "cold-resumed child original-plan update"
	// A fresh roster forces the completed child through durable restoration,
	// rather than accidentally retaining an already-frozen live child context.
	resumed, err := newTool().Execute(parent, map[string]any{"resume_from": id, "prompt": "continue saved child"})
	if err != nil || resumed == nil || resumed.IsError {
		t.Fatalf("resume Agent under changed parent plan: %#v, %v", resumed, err)
	}
	if got := <-provider.seen; got != original {
		t.Fatalf("cold-resumed child used current parent plan: got=%q want=%q", got, original)
	}
	resumedMeta, _ := resumed.Presentation["subagent"].(map[string]any)
	if resumedMeta["agentId"] != id {
		t.Fatalf("resume started another child: %#v", resumedMeta)
	}
	assertTaskPlanChildWrite(t, parent, sid, original, provider.content)
}
