package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// A cancellation-insensitive atomic tool models bounded cleanup after Esc.
// No shell, network, credentials or real provider is involved.
type cancellingFixtureTool struct {
	tools.BaseTool
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (*cancellingFixtureTool) Name() string                { return "CancellationFixture" }
func (*cancellingFixtureTool) Description() string         { return "local cancellation fixture" }
func (*cancellingFixtureTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (*cancellingFixtureTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*cancellingFixtureTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (*cancellingFixtureTool) InterruptBehavior() tools.InterruptBehavior {
	return tools.InterruptBlock
}
func (tool *cancellingFixtureTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	close(tool.started)
	<-tool.release
	close(tool.finished)
	return &tools.Result{Output: "atomic work completed"}, nil
}

type cancellingFixtureProvider struct {
	fakeProvider
	requests chan llm.Request
	calls    atomic.Int32
}

func (p *cancellingFixtureProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.requests <- req
	if p.calls.Add(1) == 1 {
		return &backgroundTestStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolName: "CancellationFixture", ToolUseID: "cancel-fixture-1"},
			{Type: "tool_use_stop", ToolUseID: "cancel-fixture-1", InputDelta: "{}"},
			{Type: "message_stop", StopReason: "tool_use"},
		}}, nil
	}
	return &backgroundTestStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "fresh turn completed"},
		{Type: "message_stop", StopReason: "end_turn"},
	}}, nil
}

func TestCancellingTurnQueuesInputUntilPreviousRunReturns(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	p := &cancellingFixtureProvider{requests: make(chan llm.Request, 4)}
	tool := &cancellingFixtureTool{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	registry := tools.NewRegistry()
	registry.Register(tool)
	m.loop = agent.NewLoop(p, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
	m.eventCh = make(chan agent.Event, 256)
	m.doneCh = make(chan error, 1)
	m.ctx = context.Background()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(tool.release) }) }
	t.Cleanup(release)
	m.input.SetValue("first task")
	pressEnter(t, m)
	select {
	case <-tool.started:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture tool did not start")
	}
	<-p.requests
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.turnActive || !m.turnCancelledByUser || m.spinnerActive {
		t.Fatalf("Esc must enter cancelling, not idle: active=%v cancelled=%v spinner=%v", m.turnActive, m.turnCancelledByUser, m.spinnerActive)
	}
	if !strings.Contains(dumpMessages(m), "canceling") {
		t.Error("Esc falsely reports completion before the tool has returned")
	}
	m.input.SetValue("continue with the new task")
	pressEnter(t, m)
	if len(m.queuedPrompts) != 1 || m.queuedPrompts[0].Text != "continue with the new task" {
		t.Errorf("cancelling input not reliably queued: %+v", m.queuedPrompts)
	}
	if steer := m.loop.SteerInjectDrainForTest(); steer != "" {
		t.Errorf("input was injected into the cancelled Run: %q", steer)
	}
	if p.calls.Load() != 1 {
		t.Error("new provider turn overlapped the unfinished old tool")
	}
	release()
	select {
	case err := <-m.doneCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old Run error = %v, want cancellation", err)
		}
		m.drainAgentEvents(len(m.eventCh))
		m.finalizeTurn(err)
	case <-time.After(3 * time.Second):
		t.Fatal("old Run did not return after cleanup")
	}
	if m.turnActive || m.turnCancelledByUser || !m.queuePending {
		t.Errorf("cancellation completion did not prepare fresh queued turn: active=%v cancelled=%v pending=%v", m.turnActive, m.turnCancelledByUser, m.queuePending)
	}
	if !strings.Contains(dumpMessages(m), "interrupted") {
		t.Error("completed cancellation was not acknowledged")
	}
	m.Update(spinnerTick{})
	select {
	case req := <-p.requests:
		found := false
		for _, msg := range req.Messages {
			for _, block := range msg.Content {
				if strings.Contains(block.Text, "[user steer mid-turn] continue") {
					t.Error("new prompt was persisted as cancelled-turn steering")
				}
				if block.Text == "continue with the new task" {
					found = true
				}
			}
		}
		if !found {
			t.Error("fresh provider request lost the queued prompt")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued prompt never started after old Run returned")
	}
	finishBackgroundTestTurn(t, m)
	if m.turnActive || m.turnCancelledByUser || p.calls.Load() != 2 {
		t.Fatalf("fresh turn did not finish normally: active=%v cancelled=%v calls=%d", m.turnActive, m.turnCancelledByUser, p.calls.Load())
	}
	m.cfg = nil // string-only switch; never load credentials or build a provider.
	if err := m.switchModel("new-model", ""); err != nil {
		t.Fatalf("model switch stayed blocked after cancellation: %v", err)
	}
}

func TestCancellingTurnModelCommandExplainsPendingCleanup(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.turnCancelledByUser = true
	m.input.SetValue("/model new-model")
	pressEnter(t, m)
	if !strings.Contains(dumpMessages(m), "canceling") {
		t.Errorf("model refusal does not explain pending cancellation: %s", dumpMessages(m))
	}
	if m.input.Value() != "/model new-model" || len(m.queuedPrompts) != 0 || m.activeScreen != nil {
		t.Fatal("model command must preserve explicit choice without running or queueing it")
	}
}

func TestCancellingTurnRepeatedEscClearsNewQueue(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.turnCancelledByUser = true
	m.enqueueQueuedItem("new queued task", QueuePriorityNext)
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.queuedPrompts) != 0 || m.queuePending || !m.turnActive {
		t.Fatalf("second Esc must clear new queue without declaring old Run idle: queue=%+v pending=%v active=%v", m.queuedPrompts, m.queuePending, m.turnActive)
	}
}

func TestCancellingTurnDoesNotOverwriteNewDraftOrImages(t *testing.T) {
	for _, withImage := range []bool{false, true} {
		t.Run(map[bool]string{false: "draft", true: "image"}[withImage], func(t *testing.T) {
			m := newSlashTestModel(t)
			m.turnActive = true
			m.turnCancelledByUser = true
			m.enqueueQueuedItem("queued text", QueuePriorityNext)
			if withImage {
				m.imagePaste = map[int]string{1: "new-image.png"}
			} else {
				m.input.SetValue("new unsent draft")
			}
			m.finalizeTurn(context.Canceled)
			if m.queuePending || len(m.queuedPrompts) != 1 {
				t.Fatal("draft/image must pause automatic queue drain")
			}
			if !withImage && m.input.Value() != "new unsent draft" {
				t.Fatal("new draft was overwritten")
			}
			if withImage && m.imagePaste[1] != "new-image.png" {
				t.Fatal("new image was consumed by an older queued prompt")
			}
			if !strings.Contains(dumpMessages(m), "queued") || !strings.Contains(dumpMessages(m), "draft") {
				t.Error("paused queue needs an explicit draft-preserved notice")
			}
		})
	}
}

func TestCancellingTurnDoesNotRestartAfterMixedFailureOrSessionCancel(t *testing.T) {
	for _, sessionCancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed-failure", true: "session-cancel"}[sessionCancelled], func(t *testing.T) {
			m := newSlashTestModel(t)
			m.turnActive = true
			m.turnCancelledByUser = true
			m.enqueueQueuedItem("new task", QueuePriorityNext)
			err := errors.Join(context.Canceled, errors.New("cleanup failed"))
			if sessionCancelled {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				m.ctx = ctx
				err = context.Canceled
			}
			m.finalizeTurn(err)
			if m.queuePending || len(m.queuedPrompts) != 1 {
				t.Fatal("failed cleanup or cancelled session must not automatically restart")
			}
			if !sessionCancelled && !strings.Contains(dumpMessages(m), "cleanup failed") {
				t.Error("mixed cleanup failure was hidden as ordinary user cancellation")
			}
		})
	}
}

func TestCancellingTurnPausedQueueClearsOnIdleEscWithoutLosingDraft(t *testing.T) {
	m := newSlashTestModel(t)
	m.enqueueQueuedItem("old queued text", QueuePriorityNext)
	m.input.SetValue("unsent draft")
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.queuedPrompts) != 0 || m.queuePending || m.input.Value() != "unsent draft" {
		t.Fatalf("idle Esc must clear paused queue, preserving draft: queue=%+v pending=%v draft=%q", m.queuedPrompts, m.queuePending, m.input.Value())
	}
}

func TestCancellingTurnInterruptEntrypointsQueueRatherThanSteer(t *testing.T) {
	for _, entry := range []string{"esc", "ctrl+c", "/abort"} {
		t.Run(entry, func(t *testing.T) {
			m := newSlashTestModel(t)
			m.turnActive = true
			m.turnCancel = func() {}
			m.spinnerActive = true
			if entry == "/abort" {
				m.input.SetValue(entry)
				pressEnter(t, m)
			} else if entry == "esc" {
				m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
			} else {
				m.handleKey(teaKeyCtrlC())
			}
			for _, text := range []string{"plain follow-up", "/now urgent", "/later deferred"} {
				m.input.SetValue(text)
				pressEnter(t, m)
			}
			if !m.turnActive || !m.turnCancelledByUser || len(m.queuedPrompts) != 3 {
				t.Fatalf("cancel entrypoint lost follow-ups or went idle: active=%v cancelled=%v queue=%+v", m.turnActive, m.turnCancelledByUser, m.queuedPrompts)
			}
			if got := m.loop.SteerInjectDrainForTest(); got != "" {
				t.Fatalf("cancel entrypoint injected follow-ups into old Run: %q", got)
			}
			if m.queuedPrompts[1].Priority != QueuePriorityNow || m.queuedPrompts[2].Priority != QueuePriorityLater {
				t.Fatal("cancel queue lost priority overrides")
			}
		})
	}
}

func TestCancellingTurnCustomPromptKeepsInvocationInsteadOfReparsingBody(t *testing.T) {
	m := newSlashTestModel(t)
	m.cfg = nil
	p := &backgroundTestProvider{requests: make(chan llm.Request, 4)}
	m.loop.Provider = p
	m.eventCh = make(chan agent.Event, 256)
	m.doneCh = make(chan error, 1)
	m.slash.Register(slash.Cmd{
		Name: "literalmodel",
		Handler: func(args string) (string, slash.Signal) {
			return "/model this-is-prompt-text-not-a-local-switch " + args, slash.SignalCustomPrompt
		},
	})
	m.turnActive = true
	m.turnCancelledByUser = true
	for _, text := range []string{"/literalmodel first", "ordinary follow-up", "/literalmodel second"} {
		m.input.SetValue(text)
		pressEnter(t, m)
	}
	if len(m.queuedPrompts) != 3 || m.queuedPrompts[0].Text != "/literalmodel first" {
		t.Fatalf("custom invocation must be resolved at its fresh turn boundary, not reparsed as body commands: %+v", m.queuedPrompts)
	}
	m.finalizeTurn(context.Canceled)
	for _, want := range []string{"/model this-is-prompt-text-not-a-local-switch first", "ordinary follow-up", "/model this-is-prompt-text-not-a-local-switch second"} {
		m.Update(spinnerTick{})
		select {
		case req := <-p.requests:
			last := req.Messages[len(req.Messages)-1]
			if len(last.Content) != 1 || last.Content[0].Text != want {
				t.Fatalf("custom/text queue merged, dropped or reinterpreted input: got %+v, want %q", last, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("custom prompt became a local command rather than a fresh provider turn")
		}
		finishBackgroundTestTurn(t, m)
	}
	if m.turnActive || m.queuePending || len(m.queuedPrompts) != 0 || len(p.requests) != 0 {
		t.Fatal("mixed custom/plain cancellation queue did not execute exactly once")
	}
}
