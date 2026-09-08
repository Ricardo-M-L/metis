package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type checkpointProvider struct {
	headlessBoundaryProvider
	calls   atomic.Int32
	blocked chan struct{}
	release chan struct{}
}

func (p *checkpointProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	if p.calls.Add(1) == 1 {
		return &headlessBoundaryStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "checkpoint-tool", ToolName: "CheckpointProbe"},
			{Type: "tool_input_delta", ToolUseID: "checkpoint-tool", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "checkpoint-tool"},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	close(p.blocked)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
		return p.headlessBoundaryProvider.Stream(ctx, req)
	}
}

func newCheckpointRuntime(t *testing.T) (*agent.Loop, *session.Store, *checkpointProvider, *headlessCheckpoint) {
	t.Helper()
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	t.Setenv("METIS_RUN_MAX_SECONDS", "0")
	t.Setenv("METIS_TURN_MAX_SECONDS", "0")
	provider := &checkpointProvider{blocked: make(chan struct{}), release: make(chan struct{})}
	registry := tools.NewRegistry()
	registry.Register(&cronPermissionProbeTool{
		name: "CheckpointProbe", permission: tools.PermissionAllow, readOnly: true,
		seen: make(chan map[string]any, 8),
	})
	loop := agent.NewLoop(provider, registry, nil, nil, "system", 20)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeader("headless-checkpoint", "model", "system"); err != nil {
		t.Fatal(err)
	}
	loop.AppendUser("inspect this task")
	if err := store.AppendMessage("headless-checkpoint", loop.History()[0]); err != nil {
		t.Fatal(err)
	}
	return loop, store, provider, newHeadlessCheckpoint(loop, store, "headless-checkpoint")
}

func startCheckpointLoop(t *testing.T, loop *agent.Loop) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		err := loop.Run(ctx, events)
		close(events)
		done <- err
	}()
	go func() {
		// Event consumers deliberately never read history or persist cursors.
		for range events {
		}
	}()
	t.Cleanup(cancel)
	return cancel, done
}

func waitCheckpointProvider(t *testing.T, p *checkpointProvider) {
	t.Helper()
	select {
	case <-p.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not reach the blocked next provider request")
	}
}

func waitCheckpointLoop(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not join")
		return nil
	}
}

func TestHeadlessCheckpointDurableBeforeNextProvider(t *testing.T) {
	loop, store, provider, checkpoint := newCheckpointRuntime(t)
	cancel, done := startCheckpointLoop(t, loop)
	waitCheckpointProvider(t, provider)
	// This is a fresh Store object while Run is still alive, not an in-memory
	// cursor assertion or a final-only flush after the provider is released.
	resumer, err := session.NewStore(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	_, intermediate, err := resumer.Load("headless-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if len(intermediate) != 3 || intermediate[1].Content[0].Type != "tool_use" || intermediate[2].Content[0].Type != "tool_result" {
		cancel()
		_ = waitCheckpointLoop(t, done)
		t.Fatalf("completed tool iteration not recoverable before next provider: %#v", intermediate)
	}
	cancel()
	if err := waitCheckpointLoop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run = %v", err)
	}
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	_, final, err := resumer.Load("headless-checkpoint")
	if err != nil || !reflect.DeepEqual(final, intermediate) {
		t.Fatalf("final flush duplicated or changed durable tool history: got=%#v err=%v", final, err)
	}
}

func TestHeadlessCheckpointWriteFailureStopsBeforeNextProvider(t *testing.T) {
	loop, store, provider, checkpoint := newCheckpointRuntime(t)
	validDir := store.Dir
	badDir := filepath.Join(t.TempDir(), "missing")
	persist := loop.HistoryCheckpoint
	loop.HistoryCheckpoint = func(history []llm.Message) error {
		if len(history) > 1 {
			store.Dir = badDir
		}
		return persist(history)
	}
	_, done := startCheckpointLoop(t, loop)
	err := waitCheckpointLoop(t, done)
	if err == nil || !strings.Contains(err.Error(), "checkpoint history append") {
		t.Fatalf("expected explicit persistence failure, got %v", err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("write failure allowed %d provider calls, want only the first", got)
	}
	store.Dir = validDir
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(store.Dir, "headless-checkpoint.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(store.Dir, "headless-checkpoint.jsonl"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("recovered final checkpoint duplicated ledger entries: %v", err)
	}
	_, recovered, err := store.Load("headless-checkpoint")
	if err != nil || len(recovered) != 3 {
		t.Fatalf("recovered history = %#v, err=%v", recovered, err)
	}
}

type blockingCheckpointTool struct {
	cronPermissionProbeTool
	started chan struct{}
}

func (t *blockingCheckpointTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	close(t.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHeadlessCheckpointNeverPersistsLiveUnpairedTool(t *testing.T) {
	loop, store, _, checkpoint := newCheckpointRuntime(t)
	tool := &blockingCheckpointTool{
		cronPermissionProbeTool: cronPermissionProbeTool{name: "CheckpointProbe", permission: tools.PermissionAllow, readOnly: true},
		started:                 make(chan struct{}),
	}
	loop.Registry = tools.NewRegistry()
	loop.Registry.Register(tool)
	cancel, done := startCheckpointLoop(t, loop)
	select {
	case <-tool.started:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	_, during, err := store.Load("headless-checkpoint")
	if err != nil || len(during) != 1 {
		t.Fatalf("in-flight unpaired tool call reached disk: %#v err=%v", during, err)
	}
	cancel()
	if err := waitCheckpointLoop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	_, repaired, err := store.Load("headless-checkpoint")
	if err != nil || len(repaired) != 3 || repaired[2].Content[0].Type != "tool_result" || !repaired[2].Content[0].IsError {
		t.Fatalf("cancel did not checkpoint repaired tool history: %#v err=%v", repaired, err)
	}
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
}

type checkpointFuncProvider struct {
	headlessBoundaryProvider
	stream func(context.Context, llm.Request) (llm.StreamReader, error)
}

func (p *checkpointFuncProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	return p.stream(ctx, req)
}

func checkpointText(text string) llm.StreamReader {
	return &headlessBoundaryStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: text},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}
}

func TestHeadlessCheckpointSchemaCorrectionUsesSameCursor(t *testing.T) {
	loop, store, _, checkpoint := newCheckpointRuntime(t)
	var calls int
	loop.Provider = &checkpointFuncProvider{stream: func(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
		calls++
		if calls == 1 {
			return checkpointText("not JSON"), nil
		}
		return checkpointText(`{"ok":true}`), nil
	}}
	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	enforcer, err := rtpkg.NewOutputSchemaEnforcer(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := rtpkg.RunLoopCollectText(context.Background(), loop, "headless-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	_, schemaErr := enforcer.Validate(first)
	if schemaErr == nil {
		t.Fatal("invalid fixture unexpectedly passed schema validation")
	}
	_, initial, err := store.Load("headless-checkpoint")
	if err != nil || len(initial) != 2 {
		t.Fatalf("first schema candidate was not checkpointed: %#v err=%v", initial, err)
	}
	loop.AppendUser(enforcer.RetryMessage(schemaErr))
	second, err := rtpkg.RunLoopCollectText(context.Background(), loop, "headless-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Validate(second); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	_, final, err := store.Load("headless-checkpoint")
	if err != nil || len(final) != 4 || calls != 2 {
		t.Fatalf("schema retry lost or duplicated history: %#v calls=%d err=%v", final, calls, err)
	}
}

func checkpointLongHistory() []llm.Message {
	var history []llm.Message
	for i := 0; i < 12; i++ {
		history = append(history,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("old task %d", i)}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("old task completed ", 20)}}},
		)
	}
	return history
}

func TestHeadlessCheckpointCompactionAndFinalSaveShareCursor(t *testing.T) {
	loop, store, _, checkpoint := newCheckpointRuntime(t)
	loop.Restore(append(checkpointLongHistory(), loop.History()...))
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	cfg := agent.DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop.Compactor = agent.NewCompactor(cfg, "model", 2000, &headlessBoundaryProvider{})
	result, err := loop.CompactNow(context.Background(), agent.CompactOptions{Trigger: "checkpoint-test", Force: true})
	if err != nil || !result.Applied {
		t.Fatalf("compaction result=%+v err=%v", result, err)
	}
	ledger := filepath.Join(store.Dir, "headless-checkpoint.jsonl")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(ledger)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("post-compaction save duplicated entries: %v", err)
	}
	_, loaded, err := store.Load("headless-checkpoint")
	if err != nil || !reflect.DeepEqual(loaded, loop.History()) {
		t.Fatalf("loaded compacted history differs: err=%v", err)
	}
}

func TestHeadlessCheckpointPreservesRawPromptAcrossAnchorChange(t *testing.T) {
	loop, store, provider, checkpoint := newCheckpointRuntime(t)
	const raw = "inspect this task"
	const enriched = "<hidden-directory-hints>private instructions</hidden-directory-hints>\n\ninspect this task"
	history := loop.History()
	history[0].Content[0].Text = enriched
	loop.Restore(history)
	checkpoint.MarkPrompt(enriched, raw)
	// prepareTurnMemory can attach recall after Mark; the anchor must remain
	// immutable while any replacement still writes the original raw prompt.
	history = loop.History()
	history[0].Content = append(history[0].Content, llm.ContentBlock{Type: "text", Text: "synthetic recall", Synthetic: true})
	loop.Restore(history)
	cancel, done := startCheckpointLoop(t, loop)
	waitCheckpointProvider(t, provider)
	cancel()
	_ = waitCheckpointLoop(t, done)
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(store.Dir, "headless-checkpoint.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("hidden-directory-hints")) {
		t.Fatalf("LLM-only instructions leaked into session ledger: %s", body)
	}
	_, loaded, err := store.Load("headless-checkpoint")
	if err != nil || len(loaded) != 3 || loaded[0].Content[0].Text != raw {
		t.Fatalf("raw prompt mapping lost/duplicated: %#v err=%v", loaded, err)
	}
	if loop.History()[0].Content[0].Text != enriched {
		t.Fatal("persistence changed the provider-facing prompt")
	}
	// Explicitly check that every physical ledger record remains old-format
	// compatible rather than silently introducing a new checkpoint entry kind.
	for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
		var entry session.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type != "header" && entry.Type != "message" && entry.Type != "history_replace" {
			t.Fatalf("new incompatible entry type %q", entry.Type)
		}
	}
}

func TestHeadlessCheckpointPrecedesSecondWindSummarizer(t *testing.T) {
	loop, store, provider, _ := newCheckpointRuntime(t)
	loop.Restore(append(checkpointLongHistory(), loop.History()...))
	loop.MaxIters = 1
	summaryStarted := make(chan struct{})
	summarizer := &checkpointFuncProvider{stream: func(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
		close(summaryStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cfg := agent.DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	// A large window suppresses pressure-based auto-compaction, while the
	// exhausted iteration budget still invokes forced second-wind compaction.
	loop.Compactor = agent.NewCompactor(cfg, "model", 200_000, summarizer)
	cancel, done := startCheckpointLoop(t, loop)
	select {
	case <-summaryStarted:
	case <-time.After(3 * time.Second):
		cancel()
		_ = waitCheckpointLoop(t, done)
		t.Fatal("second-wind summarizer did not start")
	}
	_, history, err := store.Load("headless-checkpoint")
	toolResults := 0
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolUseID == "checkpoint-tool" {
				toolResults++
			}
		}
	}
	if err != nil || toolResults != 1 || provider.calls.Load() != 1 {
		t.Errorf("completed tool round was not durable before blocked second wind: results=%d calls=%d err=%v", toolResults, provider.calls.Load(), err)
	}
	cancel()
	_ = waitCheckpointLoop(t, done)
}

func TestHeadlessCheckpointAutoCompactionWriteFailureIsFatal(t *testing.T) {
	loop, store, provider, checkpoint := newCheckpointRuntime(t)
	loop.Restore(append(checkpointLongHistory(), loop.History()...))
	validDir := store.Dir
	badDir := filepath.Join(t.TempDir(), "missing")
	var summaries atomic.Int32
	summarizer := &checkpointFuncProvider{stream: func(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
		summaries.Add(1)
		// Simulate a disk destination becoming unavailable only after the raw
		// pre-compaction checkpoint has succeeded and summarization has begun.
		store.Dir = badDir
		return checkpointText("Earlier tasks completed. Continue current task."), nil
	}}
	cfg := agent.DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop.Compactor = agent.NewCompactor(cfg, "model", 2000, summarizer)
	loop.Compactor.Threshold = 0.25
	cancel, done := startCheckpointLoop(t, loop)
	defer cancel()
	err := waitCheckpointLoop(t, done)
	if err == nil || summaries.Load() != 1 || provider.calls.Load() != 0 {
		t.Fatalf("failed compaction checkpoint allowed next provider: err=%v summaries=%d calls=%d", err, summaries.Load(), provider.calls.Load())
	}
	if checkpoint.compactionErr == nil {
		t.Fatalf("fixture did not reach the compaction persistence callback: %v", err)
	}
	store.Dir = validDir
	// Final persistence may now succeed, but cannot retroactively erase the
	// failed durable boundary and turn this invocation into a false success.
	if err := checkpoint.Save(); err == nil {
		t.Fatal("successful final repair erased the compaction failure")
	}
}
