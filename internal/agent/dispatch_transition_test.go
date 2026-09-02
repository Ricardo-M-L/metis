package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

type transitionDispatchProbe struct {
	name               string
	permission         tools.Permission
	schemas            atomic.Int64
	canUses            atomic.Int64
	executes           atomic.Int64
	concurrencies      atomic.Int64
	canUseEntered      chan struct{}
	releaseCanUse      chan struct{}
	concurrencyEntered chan struct{}
	releaseConcurrency chan struct{}
	executeEntered     chan struct{}
	releaseExecute     chan struct{}
}

func (p *transitionDispatchProbe) Name() string        { return p.name }
func (p *transitionDispatchProbe) Description() string { return "transition probe" }
func (p *transitionDispatchProbe) IsEnabled() bool     { return true }
func (p *transitionDispatchProbe) InputSchema() map[string]any {
	p.schemas.Add(1)
	return map[string]any{"type": "object"}
}
func (p *transitionDispatchProbe) Concurrency(map[string]any) tools.Concurrency {
	p.concurrencies.Add(1)
	if p.concurrencyEntered != nil {
		close(p.concurrencyEntered)
		<-p.releaseConcurrency
	}
	return tools.ConcurrencyExclusive
}
func (p *transitionDispatchProbe) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	p.canUses.Add(1)
	if p.canUseEntered != nil {
		close(p.canUseEntered)
		<-p.releaseCanUse
	}
	return p.permission, "probe permission"
}
func (p *transitionDispatchProbe) Execute(context.Context, map[string]any) (*tools.Result, error) {
	p.executes.Add(1)
	if p.executeEntered != nil {
		close(p.executeEntered)
		<-p.releaseExecute
	}
	return &tools.Result{Output: "executed"}, nil
}

func startBlockedModeListener(t *testing.T, gate *permission.Gate, mode permission.Mode) (release func()) {
	t.Helper()
	entered := make(chan struct{})
	releaseListener := make(chan struct{})
	gate.SetModeChangeListener(func(observed permission.Mode) {
		if observed == mode {
			close(entered)
			<-releaseListener
		}
	})
	go gate.SetMode(mode)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("mode listener did not start")
	}
	return func() { close(releaseListener) }
}

func assertTransitionDeniedResult(t *testing.T, results []llm.ContentBlock) {
	t.Helper()
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "mode:transition") {
		t.Fatalf("transition result = %+v, want mode:transition denial", results)
	}
}

func TestDispatchTransitionRejectsMCPAllowBeforeSchemaHookCanUseAndExecute(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	gate := permission.New(permission.ModeFullAccess)
	releaseListener := startBlockedModeListener(t, gate, permission.ModeDefault)
	defer releaseListener()

	probe := &transitionDispatchProbe{name: "mcp__transition__mutate", permission: tools.PermissionAllow}
	reg := tools.NewRegistry()
	reg.Register(probe)
	probe.schemas.Store(0)
	hookCalls := atomic.Int64{}
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(context.Context, pubhook.Context, *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		hookCalls.Add(1)
		return nil
	}))
	loop := &Loop{Registry: reg, Gate: gate, Hooks: hooks}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "transition-mcp", ToolName: probe.Name(), ToolInput: map[string]any{},
	}}, make(chan Event, 16), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	assertTransitionDeniedResult(t, results)
	if got := probe.schemas.Load(); got != 0 {
		t.Fatalf("schema calls during transition = %d, want 0", got)
	}
	if got := hookCalls.Load(); got != 0 {
		t.Fatalf("hook calls during transition = %d, want 0", got)
	}
	if got := probe.canUses.Load(); got != 0 {
		t.Fatalf("CanUse calls during transition = %d, want 0", got)
	}
	if got := probe.executes.Load(); got != 0 {
		t.Fatalf("Execute calls during transition = %d, want 0", got)
	}
}

func TestActiveTransitionWriterRejectsBatchBeforeSchemaHookCanUseAndExecute(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	gate := permission.New(permission.ModeFullAccess)
	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- gate.RunModeTransition(func() error {
			close(writerEntered)
			<-releaseWriter
			return nil
		})
	}()
	select {
	case <-writerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("transition writer did not enter")
	}

	probe := &transitionDispatchProbe{name: "mcp__transition__writer_active", permission: tools.PermissionAllow}
	reg := tools.NewRegistry()
	reg.Register(probe)
	probe.schemas.Store(0)
	hookCalls := atomic.Int64{}
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(context.Context, pubhook.Context, *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		hookCalls.Add(1)
		return nil
	}))
	loop := &Loop{Registry: reg, Gate: gate, Hooks: hooks}
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "writer-active", ToolName: probe.Name(), ToolInput: map[string]any{},
	}}, make(chan Event, 16), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	assertTransitionDeniedResult(t, results)
	if got := probe.schemas.Load(); got != 0 {
		t.Fatalf("schema calls with active writer = %d, want 0", got)
	}
	if got := hookCalls.Load(); got != 0 {
		t.Fatalf("hook calls with active writer = %d, want 0", got)
	}
	if got := probe.canUses.Load(); got != 0 {
		t.Fatalf("CanUse calls with active writer = %d, want 0", got)
	}
	if got := probe.executes.Load(); got != 0 {
		t.Fatalf("Execute calls with active writer = %d, want 0", got)
	}
	close(releaseWriter)
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("transition writer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transition writer did not finish")
	}
}

func TestDispatchTransitionStartingInsideHookStopsBeforeCanUse(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	gate := permission.New(permission.ModeFullAccess)
	probe := &transitionDispatchProbe{name: "DirectAllow", permission: tools.PermissionAllow}
	reg := tools.NewRegistry()
	reg.Register(probe)
	hookEntered := make(chan struct{})
	releaseHook := make(chan struct{})
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(context.Context, pubhook.Context, *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		close(hookEntered)
		<-releaseHook
		return nil
	}))
	loop := &Loop{Registry: reg, Gate: gate, Hooks: hooks}

	done := make(chan []llm.ContentBlock, 1)
	go func() {
		results, _ := loop.executeBatch(context.Background(), []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "transition-hook", ToolName: probe.Name(), ToolInput: map[string]any{},
		}}, make(chan Event, 16), HookContext{})
		done <- results
	}()
	select {
	case <-hookEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-tool hook did not start")
	}
	releaseListener := startBlockedModeListener(t, gate, permission.ModeDefault)
	close(releaseHook)
	select {
	case results := <-done:
		assertTransitionDeniedResult(t, results)
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not reject after transition began inside hook")
	}
	if got := probe.canUses.Load(); got != 0 {
		t.Fatalf("CanUse calls after hook transition = %d, want 0", got)
	}
	if got := probe.executes.Load(); got != 0 {
		t.Fatalf("Execute calls after hook transition = %d, want 0", got)
	}
	releaseListener()
}

func TestDispatchCompletedTransitionInsideCanUseInvalidatesOldFullAccessAdmission(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	gate := permission.New(permission.ModeFullAccess)
	canUseEntered := make(chan struct{})
	releaseCanUse := make(chan struct{})
	probe := &transitionDispatchProbe{
		name: "mcp__transition__allow", permission: tools.PermissionAllow,
		canUseEntered: canUseEntered, releaseCanUse: releaseCanUse,
	}
	reg := tools.NewRegistry()
	reg.Register(probe)
	loop := &Loop{Registry: reg, Gate: gate}

	done := make(chan []llm.ContentBlock, 1)
	go func() {
		results, _ := loop.executeBatch(context.Background(), []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "transition-can-use", ToolName: probe.Name(), ToolInput: map[string]any{},
		}}, make(chan Event, 16), HookContext{})
		done <- results
	}()
	select {
	case <-canUseEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("CanUse did not start")
	}
	// Let the complete transition settle before CanUse returns. A check that
	// only asks "is a transition active right now?" would miss this stale
	// fullAccess authorization and execute under the new default posture.
	gate.SetMode(permission.ModeDefault)
	close(releaseCanUse)
	select {
	case results := <-done:
		assertTransitionDeniedResult(t, results)
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not reject an admission invalidated inside CanUse")
	}
	if got := probe.executes.Load(); got != 0 {
		t.Fatalf("Execute calls after completed CanUse transition = %d, want 0", got)
	}
}

func TestDispatchTransitionBeforeRunExecuteStopsPreviouslyAllowedTool(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	gate := permission.New(permission.ModeFullAccess)
	concurrencyEntered := make(chan struct{})
	releaseConcurrency := make(chan struct{})
	probe := &transitionDispatchProbe{
		name: "LegacyFullAccessAsk", permission: tools.PermissionAsk,
		concurrencyEntered: concurrencyEntered, releaseConcurrency: releaseConcurrency,
	}
	reg := tools.NewRegistry()
	reg.Register(probe)
	loop := &Loop{Registry: reg, Gate: gate}

	done := make(chan []llm.ContentBlock, 1)
	go func() {
		results, _ := loop.executeBatch(context.Background(), []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "transition-execute", ToolName: probe.Name(), ToolInput: map[string]any{},
		}}, make(chan Event, 16), HookContext{})
		done <- results
	}()
	select {
	case <-concurrencyEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Concurrency did not start")
	}
	releaseListener := startBlockedModeListener(t, gate, permission.ModeDefault)
	close(releaseConcurrency)
	select {
	case results := <-done:
		assertTransitionDeniedResult(t, results)
	case <-time.After(2 * time.Second):
		t.Fatal("runExecute did not fail closed during transition")
	}
	if got := probe.executes.Load(); got != 0 {
		t.Fatalf("Execute calls during transition = %d, want 0", got)
	}
	releaseListener()
}

func TestToolExecutionLeaseDelaysModeTransitionUntilExecuteReturns(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	gate := permission.New(permission.ModeFullAccess)
	executeEntered := make(chan struct{})
	releaseExecute := make(chan struct{})
	probe := &transitionDispatchProbe{
		name: "mcp__transition__long_mutation", permission: tools.PermissionAllow,
		executeEntered: executeEntered, releaseExecute: releaseExecute,
	}
	reg := tools.NewRegistry()
	reg.Register(probe)
	loop := &Loop{Registry: reg, Gate: gate}

	dispatchDone := make(chan []llm.ContentBlock, 1)
	go func() {
		results, _ := loop.executeBatch(context.Background(), []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "leased-execute", ToolName: probe.Name(), ToolInput: map[string]any{},
		}}, make(chan Event, 16), HookContext{})
		dispatchDone <- results
	}()
	select {
	case <-executeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("tool Execute did not start")
	}

	listenerEntered := make(chan struct{})
	releaseListener := make(chan struct{})
	gate.SetModeChangeListener(func(mode permission.Mode) {
		if mode == permission.ModeDefault {
			close(listenerEntered)
			<-releaseListener
		}
	})
	transitionDone := make(chan error, 1)
	go func() {
		transitionDone <- gate.RunModeTransition(func() error {
			gate.SetModeAndWait(permission.ModeDefault)
			return nil
		})
	}()
	select {
	case <-listenerEntered:
		t.Fatal("mode transition entered while an admitted tool was still executing")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseExecute)
	select {
	case results := <-dispatchDone:
		if len(results) != 1 || results[0].IsError {
			t.Fatalf("leased tool result = %+v, want successful completion before transition", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool dispatch did not finish after Execute release")
	}
	select {
	case <-listenerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("mode transition did not enter after Execute returned")
	}
	close(releaseListener)
	select {
	case err := <-transitionDone:
		if err != nil {
			t.Fatalf("mode transition: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mode transition did not settle")
	}
}

func TestPendingTransitionWriterPreventsReaderBarge(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	gate := permission.New(permission.ModeFullAccess)
	executeEntered := make(chan struct{})
	releaseExecute := make(chan struct{})
	first := &transitionDispatchProbe{
		name: "FirstLeasedMutation", permission: tools.PermissionAllow,
		executeEntered: executeEntered, releaseExecute: releaseExecute,
	}
	firstRegistry := tools.NewRegistry()
	firstRegistry.Register(first)
	firstLoop := &Loop{Registry: firstRegistry, Gate: gate}
	firstDone := make(chan []llm.ContentBlock, 1)
	go func() {
		results, _ := firstLoop.executeBatch(context.Background(), []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "first-reader", ToolName: first.Name(), ToolInput: map[string]any{},
		}}, make(chan Event, 16), HookContext{})
		firstDone <- results
	}()
	select {
	case <-executeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first leased Execute did not start")
	}

	writerDone := make(chan error, 1)
	go func() {
		writerDone <- gate.RunModeTransition(func() error {
			gate.SetModeAndWait(permission.ModeDefault)
			return nil
		})
	}()
	deadline := time.After(2 * time.Second)
	for {
		_, allowed, reason := gate.ToolDispatchAdmission()
		if !allowed && reason == "mode:transition" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("transition writer never became pending")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	second := &transitionDispatchProbe{name: "mcp__transition__reader_barge", permission: tools.PermissionAllow}
	secondRegistry := tools.NewRegistry()
	secondRegistry.Register(second)
	second.schemas.Store(0)
	secondLoop := &Loop{Registry: secondRegistry, Gate: gate}
	secondResults, err := secondLoop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "second-reader", ToolName: second.Name(), ToolInput: map[string]any{},
	}}, make(chan Event, 16), HookContext{})
	if err != nil {
		t.Fatalf("second executeBatch: %v", err)
	}
	assertTransitionDeniedResult(t, secondResults)
	if second.schemas.Load() != 0 || second.canUses.Load() != 0 || second.executes.Load() != 0 {
		t.Fatalf("pending writer was reader-barged: schema=%d CanUse=%d Execute=%d",
			second.schemas.Load(), second.canUses.Load(), second.executes.Load())
	}

	close(releaseExecute)
	select {
	case results := <-firstDone:
		if len(results) != 1 || results[0].IsError {
			t.Fatalf("first leased result = %+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first leased batch did not finish")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("pending writer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending writer did not finish")
	}
}
