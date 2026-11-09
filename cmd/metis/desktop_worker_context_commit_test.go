package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestDesktopWorkerStatusKeepsCommittedUsageUntilCompactionSaved(t *testing.T) {
	provider := &headlessBoundaryProvider{}
	loop := agent.NewLoop(provider, tools.NewRegistry(), nil, nil, "worker system", 2)
	loop.Model = provider.ModelID()
	cfg := agent.DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop.Compactor = agent.NewCompactor(cfg, loop.Model, provider.MaxContextTokens(), provider)
	history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "original task"}}}}
	for range 24 {
		history = append(history,
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("old detailed answer ", 100)}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "old follow-up"}}},
		)
	}
	loop.Restore(history)
	loop.EstimateRequestContextTokens(nil)
	sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
	before := sampler.snapshot(false)
	if before.Context == nil || before.Context.Used <= 0 || before.Context.CompactionStage != "" {
		t.Fatalf("initial committed context=%+v", before.Context)
	}

	persistStarted, persistRelease := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := loop.CompactNow(ctx, agent.CompactOptions{Force: true,
			Persist: func([]llm.Message) error {
				close(persistStarted)
				select {
				case <-persistRelease:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
		if err == nil && !result.Applied {
			t.Error("fixture did not apply a real compaction replacement")
		}
		done <- err
	}()
	select {
	case <-persistStarted:
	case err := <-done:
		t.Fatalf("compaction ended before persistence: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("compaction did not reach persistence")
	}
	if candidate := loop.ContextUsageSnapshot().Used; candidate >= before.Context.Used {
		t.Fatalf("fixture must install a smaller candidate: candidate=%d before=%d", candidate, before.Context.Used)
	}
	pending := sampler.snapshot(false)
	if pending.Context == nil || pending.Context.Used != before.Context.Used || pending.Context.CompactionStage != "saving" {
		t.Fatalf("worker exposed uncommitted candidate: before=%+v pending=%+v", before.Context, pending.Context)
	}
	close(persistRelease)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("compaction did not finish after persistence")
	}
	committed := sampler.snapshot(false)
	want := loop.ContextDisplaySnapshot()
	if committed.Context == nil || committed.Context.CompactionStage != "" || committed.Context.Used != want.Used || committed.Context.Used >= before.Context.Used {
		t.Fatalf("worker did not publish committed result: got=%+v want=%+v", committed.Context, want)
	}
	if pending.Context.CompactionStage != "saving" || pending.Context.Used != before.Context.Used {
		t.Fatalf("subsequent status mutated an already emitted frame: %+v", pending.Context)
	}
	sampler.loop = nil
	final := sampler.snapshot(true)
	if final.Context == nil || final.Context.CompactionStage != "" || *final.Context != *committed.Context {
		t.Fatalf("final IPC frame lost committed usage or retained compaction: %+v", final.Context)
	}
}

func TestDesktopWorkerFinalStatusClearsInterruptedCompactionPhase(t *testing.T) {
	prior := &desktopipc.ContextPressure{Used: 96000, Window: 128000, Model: "worker-model", CompactionStage: "saving"}
	sampler := desktopWorkerStatusSampler{lastContext: prior}
	final := sampler.snapshot(true)
	if final.Context == nil || final.Context.CompactionStage != "" || final.Context.Used != prior.Used || final.Context.Window != prior.Window || final.Context.Model != prior.Model {
		t.Fatalf("final frame retained compression after worker cleanup: %+v", final.Context)
	}
	if prior.CompactionStage != "saving" {
		t.Fatal("cleanup mutated an already emitted active frame")
	}
}
