package builtin

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type traceCapture struct {
	mu     sync.Mutex
	events []agent.Event
}

type gatedCancelProvider struct {
	started   chan struct{}
	observed  chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

type forkLifecycleErrorProvider struct {
	err error
}

func (p *forkLifecycleErrorProvider) Name() string          { return "fork-lifecycle-error" }
func (p *forkLifecycleErrorProvider) MaxContextTokens() int { return 100_000 }
func (p *forkLifecycleErrorProvider) ModelID() string       { return "fork-lifecycle-error" }
func (p *forkLifecycleErrorProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, p.err
}
func (p *forkLifecycleErrorProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, p.err
}

func (p *gatedCancelProvider) Name() string          { return "gated-cancel" }
func (p *gatedCancelProvider) MaxContextTokens() int { return 100_000 }
func (p *gatedCancelProvider) ModelID() string       { return "gated-cancel" }
func (p *gatedCancelProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (p *gatedCancelProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	p.startOnce.Do(func() { close(p.started) })
	return &gatedCancelStream{ctx: ctx, observed: p.observed, release: p.release}, nil
}

type gatedCancelStream struct {
	ctx      context.Context
	observed chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (s *gatedCancelStream) Recv() (llm.StreamEvent, error) {
	<-s.ctx.Done()
	s.once.Do(func() { close(s.observed) })
	<-s.release
	return llm.StreamEvent{}, s.ctx.Err()
}

func (*gatedCancelStream) Close() error { return nil }

func (c *traceCapture) add(ev agent.Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *traceCapture) snapshot() []agent.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agent.Event(nil), c.events...)
}

func traceInvocationContext(id, publicParent string) context.Context {
	ctx := agent.WithTraceInvocationID(context.Background(), id)
	return agent.WithParentToolUseID(ctx, publicParent)
}

func assertTraceLifecycle(t *testing.T, events []agent.Event, id string) {
	t.Helper()
	var starts, ends int
	for _, ev := range events {
		if ev.TraceInvocationID != id {
			continue
		}
		switch ev.Kind {
		case agent.EventTraceInvocationStart:
			starts++
		case agent.EventTraceInvocationEnd:
			ends++
		}
	}
	if starts == 0 || ends == 0 {
		t.Fatalf("trace lifecycle start/end = %d/%d in %+v", starts, ends, events)
	}
}

func TestAgentPropagatesInternalTraceInvocationThroughChildLoop(t *testing.T) {
	var capture traceCapture
	agent.SetTraceHook(capture.add)
	t.Cleanup(func() { agent.SetTraceHook(nil) })

	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	res, err := tool.Execute(traceInvocationContext("agent-internal", "agent-public"), map[string]any{"prompt": "work"})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("Agent Execute = res:%+v err:%v", res, err)
	}
	events := capture.snapshot()
	assertTraceLifecycle(t, events, "agent-internal")
	var childText bool
	for _, ev := range events {
		if ev.Kind == agent.EventTextDelta && ev.TraceInvocationID == "agent-internal" && ev.SubAgentParentID == "agent-public" {
			childText = true
		}
	}
	if !childText {
		t.Fatalf("child event did not carry internal/public origins: %+v", events)
	}
}

func TestForegroundTraceInvocationEndsOnlyAfterChildRunExits(t *testing.T) {
	var capture traceCapture
	agent.SetTraceHook(capture.add)
	t.Cleanup(func() { agent.SetTraceHook(nil) })

	provider := &gatedCancelProvider{
		started:  make(chan struct{}),
		observed: make(chan struct{}),
		release:  make(chan struct{}),
	}
	tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "model", "system")
	ctx, cancel := context.WithCancel(traceInvocationContext("cancel-internal", "cancel-public"))
	result := make(chan *tools.Result, 1)
	go func() {
		res, _ := tool.Execute(ctx, map[string]any{"prompt": "wait"})
		result <- res
	}()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("child provider did not start")
	}
	cancel()
	select {
	case <-provider.observed:
	case <-time.After(2 * time.Second):
		t.Fatal("child provider did not observe cancellation")
	}
	select {
	case res := <-result:
		t.Fatalf("foreground Agent returned before child Run unwound: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}
	for _, ev := range capture.snapshot() {
		if ev.Kind == agent.EventTraceInvocationEnd && ev.TraceInvocationID == "cancel-internal" {
			t.Fatal("trace invocation ended before child Run exited")
		}
	}
	close(provider.release)
	select {
	case res := <-result:
		if res == nil || !res.IsError {
			t.Fatalf("cancelled foreground Agent result = %+v, want IsError", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground Agent did not return after child Run exited")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range capture.snapshot() {
			if ev.Kind == agent.EventTraceInvocationEnd && ev.TraceInvocationID == "cancel-internal" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("trace invocation never ended after child exit: %+v", capture.snapshot())
}

func TestBackgroundTraceInvocationEndsOnlyAfterChildRunExitsOnError(t *testing.T) {
	var capture traceCapture
	errorObserved := make(chan struct{})
	releaseErrorHook := make(chan struct{})
	var errorOnce sync.Once
	agent.SetTraceHook(func(ev agent.Event) {
		capture.add(ev)
		if ev.Kind == agent.EventError && ev.TraceInvocationID == "background-error-internal" {
			errorOnce.Do(func() { close(errorObserved) })
			// Loop.emit puts EventError on the consumer channel before invoking
			// the trace hook. Holding the hook creates the exact window where the
			// detached consumer has returned but the actual child Loop.Run has not.
			<-releaseErrorHook
		}
	})
	t.Cleanup(func() {
		select {
		case <-releaseErrorHook:
		default:
			close(releaseErrorHook)
		}
		agent.SetTraceHook(nil)
	})

	roster := agent.NewRoster(0)
	tool := NewAgent(
		permission.New(permission.ModeBypass),
		&forkLifecycleErrorProvider{err: errors.New("background child failed")},
		tools.NewRegistry(),
		"model",
		"system",
	).WithRoster(roster)
	res, err := tool.Execute(traceInvocationContext("background-error-internal", "background-error-public"), map[string]any{
		"prompt":            "fail",
		"run_in_background": true,
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("background Agent handshake = res:%+v err:%v", res, err)
	}

	select {
	case <-errorObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("background child error did not reach the trace hook")
	}
	for _, ev := range capture.snapshot() {
		if ev.Kind == agent.EventTraceInvocationEnd && ev.TraceInvocationID == "background-error-internal" {
			t.Fatal("trace invocation ended before background child Run exited")
		}
	}

	close(releaseErrorHook)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ends int
		for _, ev := range capture.snapshot() {
			if ev.Kind == agent.EventTraceInvocationEnd && ev.TraceInvocationID == "background-error-internal" {
				ends++
			}
		}
		if ends == 1 {
			return
		}
		if ends > 1 {
			t.Fatalf("trace invocation ended %d times", ends)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("background trace invocation never ended after child exit: %+v", capture.snapshot())
}

func TestForkPropagatesInternalTraceInvocationThroughChildLoop(t *testing.T) {
	var capture traceCapture
	agent.SetTraceHook(capture.add)
	t.Cleanup(func() { agent.SetTraceHook(nil) })

	tool := NewFork(permission.New(permission.ModeBypass), helloForkProvider(), tools.NewRegistry())
	ctx := traceInvocationContext("fork-internal", "fork-public")
	ctx = agent.WithParentSnapshot(ctx, agent.ParentSnapshot{
		System: "system", Model: "model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "history"}}}},
	})
	res, err := tool.Execute(ctx, map[string]any{"directive": "continue"})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("Fork Execute = res:%+v err:%v", res, err)
	}
	events := capture.snapshot()
	assertTraceLifecycle(t, events, "fork-internal")
	var childText bool
	for _, ev := range events {
		if ev.Kind == agent.EventTextDelta && ev.TraceInvocationID == "fork-internal" && ev.SubAgentParentID == "fork-public" {
			childText = true
		}
	}
	if !childText {
		t.Fatalf("Fork child event did not carry internal/public origins: %+v", events)
	}
}

func TestForkTraceInvocationEndsOnlyAfterChildRunExitsOnError(t *testing.T) {
	var capture traceCapture
	errorObserved := make(chan struct{})
	releaseErrorHook := make(chan struct{})
	var errorOnce sync.Once
	agent.SetTraceHook(func(ev agent.Event) {
		capture.add(ev)
		if ev.Kind == agent.EventError && ev.TraceInvocationID == "fork-error-internal" {
			errorOnce.Do(func() { close(errorObserved) })
			// emit sends EventError to Fork.Execute before notifying the trace
			// hook. Holding the child here gives us a deterministic window in
			// which Execute has returned but Loop.Run has not yet unwound.
			<-releaseErrorHook
		}
	})
	t.Cleanup(func() {
		select {
		case <-releaseErrorHook:
		default:
			close(releaseErrorHook)
		}
		agent.SetTraceHook(nil)
	})

	tool := NewFork(
		permission.New(permission.ModeBypass),
		&forkLifecycleErrorProvider{err: errors.New("fork child failed")},
		tools.NewRegistry(),
	)
	ctx := traceInvocationContext("fork-error-internal", "fork-error-public")
	ctx = agent.WithParentSnapshot(ctx, agent.ParentSnapshot{
		System: "system", Model: "model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "history"}}}},
	})

	result := make(chan *tools.Result, 1)
	go func() {
		res, _ := tool.Execute(ctx, map[string]any{"directive": "fail"})
		result <- res
	}()
	select {
	case <-errorObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("child error did not reach the trace hook")
	}
	select {
	case res := <-result:
		if res == nil || !res.IsError || !strings.Contains(res.Output, "fork child failed") {
			t.Fatalf("Fork Execute result = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fork Execute did not return after receiving EventError")
	}
	for _, ev := range capture.snapshot() {
		if ev.Kind == agent.EventTraceInvocationEnd && ev.TraceInvocationID == "fork-error-internal" {
			t.Fatal("trace invocation ended before child Run exited")
		}
	}

	close(releaseErrorHook)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ends int
		for _, ev := range capture.snapshot() {
			if ev.Kind == agent.EventTraceInvocationEnd && ev.TraceInvocationID == "fork-error-internal" {
				ends++
			}
		}
		if ends == 1 {
			return
		}
		if ends > 1 {
			t.Fatalf("trace invocation ended %d times", ends)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("trace invocation never ended after child exit: %+v", capture.snapshot())
}

func TestRalphSignalsOneOuterTraceLifecycleAcrossRepeatedRounds(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var capture traceCapture
	agent.SetTraceHook(capture.add)
	t.Cleanup(func() { agent.SetTraceHook(nil) })
	r, spawner := newRalphTool(t)
	spawner.reports = []string{"status: progress\nkeep going", "status: complete\ndone"}
	res, err := r.Execute(traceInvocationContext("ralph-internal", "ralph-public"), map[string]any{
		"objective": "finish", "maxRounds": 3,
	})
	if err != nil || res == nil || res.IsError || !strings.Contains(res.Output, "rounds: 2/3") {
		t.Fatalf("Ralph Execute = res:%+v err:%v", res, err)
	}
	assertTraceLifecycle(t, capture.snapshot(), "ralph-internal")
}
