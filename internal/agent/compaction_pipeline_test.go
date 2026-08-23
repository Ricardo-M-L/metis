package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func unifiedPipelineHistory() []llm.Message {
	messages := []llm.Message{msg(llm.RoleUser, "ancient greeting")}
	for i := 0; i < 10; i++ {
		messages = append(messages,
			msg(llm.RoleAssistant, strings.Repeat("old answer ", 12)),
			msg(llm.RoleUser, "old follow-up "+istr(i)),
		)
	}
	messages = append(messages,
		msg(llm.RoleUser, "current task"),
		msg(llm.RoleAssistant, "working"),
		msg(llm.RoleUser, "continue with the current task"),
		msg(llm.RoleAssistant, "latest answer"),
	)
	return messages
}

func TestCompactNow_AllTriggersShareOneRetentionPipeline(t *testing.T) {
	triggers := []string{"auto", "manual", "overflow", "second-wind"}
	var want []llm.Message
	for _, trigger := range triggers {
		t.Run(trigger, func(t *testing.T) {
			p := &fakeSummarizer{}
			cfg := DefaultCompactionConfig()
			cfg.MaxSummarizeInputTokens = 0
			c := NewCompactor(cfg, "test", 2_000, p)
			loop := &Loop{Compactor: c, Model: "test", Messages: unifiedPipelineHistory()}
			result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: trigger, Force: true})
			if err != nil {
				t.Fatalf("CompactNow: %v", err)
			}
			if !result.Applied {
				t.Fatal("forced pipeline did not apply")
			}
			if p.calls != 1 {
				t.Fatalf("summary calls = %d, want exactly 1", p.calls)
			}
			if want == nil {
				want = result.History
			} else if !reflect.DeepEqual(result.History, want) {
				t.Fatalf("trigger %q produced different retention\n got: %#v\nwant: %#v", trigger, result.History, want)
			}
		})
	}
}

func TestCompactNow_AutoDecisionCannotBeCancelledByShallowTrim(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.45
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 1_000, p)
	loop := &Loop{Compactor: c, Messages: unifiedPipelineHistory()}
	if !c.ShouldCompact(loop.Messages) {
		t.Fatalf("precondition: history estimate %d did not cross trigger %d", estimateTokens(loop.Messages), c.TriggerTokens())
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "auto"})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("full checkpoint was skipped: applied=%v calls=%d", result.Applied, p.calls)
	}
	if result.AfterTokens >= result.BeforeTokens {
		t.Fatalf("auto compaction was shallow: %d -> %d", result.BeforeTokens, result.AfterTokens)
	}
}

func TestCompactNow_PersistFailureRollsBackHistoryAndSummary(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}
	loop.turnIdx = 7
	loop.iterIdx = 3

	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "manual",
		Force:   true,
		Persist: func([]llm.Message) error { return errors.New("disk full") },
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("persist error = %v", err)
	}
	if result.Applied {
		t.Fatal("failed durable commit reported Applied")
	}
	if !reflect.DeepEqual(loop.History(), before) {
		t.Fatal("persist failure changed live history")
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("persist failure changed LastSummary: %q", c.LastSummary)
	}
	if loop.turnIdx != 7 || loop.iterIdx != 3 {
		t.Fatalf("manual pipeline reset counters: turn=%d iter=%d", loop.turnIdx, loop.iterIdx)
	}
}

func TestCompactNow_CompactErrorRollsBackLastSummary(t *testing.T) {
	p := &streamingProvider{resp: strings.Repeat("oversized checkpoint ", 5_000)}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	for i := range before {
		before[i].Content[0].Text += strings.Repeat(" original context", 40)
	}
	if estimateTokens(before) < 1_000 {
		t.Fatalf("precondition: history estimate %d is below strict progress gate", estimateTokens(before))
	}
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err == nil || !strings.Contains(err.Error(), "no token progress") {
		t.Fatalf("CompactNow error = %v, want no-token-progress failure", err)
	}
	if result.Applied {
		t.Fatal("failed compaction reported Applied")
	}
	if !reflect.DeepEqual(loop.History(), before) {
		t.Fatal("failed compaction changed live history")
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("failed compaction changed LastSummary: %q", c.LastSummary)
	}
	if c.consecutiveFailures != 1 {
		t.Fatalf("real no-progress failure count = %d, want 1", c.consecutiveFailures)
	}
}

func TestCompactNow_ContextTerminationDoesNotBurnCircuit(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultCompactionConfig()
			cfg.MaxSummaryRetries = 0
			cfg.MaxSummarizeInputTokens = 0
			c := NewCompactor(cfg, "test", 2_000, &errSummarizer{err: tc.err})
			c.LastSummary = "prior checkpoint"
			c.consecutiveFailures = 1
			loop := &Loop{Compactor: c, Messages: unifiedPipelineHistory()}

			result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
			if !errors.Is(err, tc.err) {
				t.Fatalf("CompactNow error = %v, want %v", err, tc.err)
			}
			if result.Applied {
				t.Fatal("terminated compaction reported Applied")
			}
			if c.consecutiveFailures != 1 {
				t.Fatalf("context termination changed failure count to %d, want 1", c.consecutiveFailures)
			}
			if c.LastSummary != "prior checkpoint" {
				t.Fatalf("context termination changed LastSummary: %q", c.LastSummary)
			}
		})
	}
}

func TestCompactNow_SummaryFailureKeepsCircuitFailure(t *testing.T) {
	summaryErr := errors.New("summary backend failed")
	cfg := DefaultCompactionConfig()
	cfg.MaxSummaryRetries = 0
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, &errSummarizer{err: summaryErr})
	c.LastSummary = "prior checkpoint"
	c.consecutiveFailures = 1
	loop := &Loop{Compactor: c, Messages: unifiedPipelineHistory()}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "auto", Force: true})
	if err == nil || !strings.Contains(err.Error(), summaryErr.Error()) {
		t.Fatalf("CompactNow error = %v, want summary failure", err)
	}
	if result.Applied {
		t.Fatal("failed summary reported Applied")
	}
	if c.consecutiveFailures != 2 {
		t.Fatalf("summary failure count = %d, want 2", c.consecutiveFailures)
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("summary failure changed LastSummary: %q", c.LastSummary)
	}
}

func TestCompactNow_LoopCheckpointReceivesExactBeforeAndAfter(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}

	var gotBefore, gotAfter []llm.Message
	legacyCalled := false
	loop.CompactionCheckpoint = func(checkpointBefore, checkpointAfter []llm.Message) error {
		gotBefore = cloneMessages(checkpointBefore)
		gotAfter = cloneMessages(checkpointAfter)
		return nil
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "auto",
		Force:   true,
		Persist: func([]llm.Message) error {
			legacyCalled = true
			return nil
		},
	})
	if err != nil || !result.Applied {
		t.Fatalf("CompactNow result=%+v err=%v", result, err)
	}
	if legacyCalled {
		t.Fatal("legacy final-only persist callback ran in addition to loop checkpoint")
	}
	if !reflect.DeepEqual(gotBefore, before) {
		t.Fatalf("checkpoint before differs from raw history\n got: %#v\nwant: %#v", gotBefore, before)
	}
	if !reflect.DeepEqual(gotAfter, result.History) {
		t.Fatalf("checkpoint after differs from result\n got: %#v\nwant: %#v", gotAfter, result.History)
	}
	if !reflect.DeepEqual(loop.History(), gotAfter) {
		t.Fatal("live history differs from durably checkpointed replacement")
	}
}

func TestCompactNow_LoopCheckpointFailureRollsBack(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}
	loop.CompactionCheckpoint = func(checkpointBefore, checkpointAfter []llm.Message) error {
		if !reflect.DeepEqual(checkpointBefore, before) {
			t.Fatal("checkpoint callback did not receive exact raw history")
		}
		if reflect.DeepEqual(checkpointAfter, before) {
			t.Fatal("checkpoint callback did not receive compacted replacement")
		}
		return errors.New("checkpoint disk full")
	}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "overflow", Force: true})
	if err == nil || !strings.Contains(err.Error(), "checkpoint disk full") {
		t.Fatalf("checkpoint error = %v", err)
	}
	if result.Applied {
		t.Fatal("failed checkpoint reported Applied")
	}
	if !reflect.DeepEqual(loop.History(), before) {
		t.Fatal("checkpoint failure changed live history")
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("checkpoint failure changed LastSummary: %q", c.LastSummary)
	}
}

func TestCompactNow_EmitsSingleLifecycle(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{Compactor: NewCompactor(cfg, "test", 2_000, p), Messages: unifiedPipelineHistory()}
	var events []Event
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "manual",
		Force:   true,
		Emit:    func(ev Event) { events = append(events, ev) },
	})
	if err != nil || !result.Applied {
		t.Fatalf("CompactNow result=%+v err=%v", result, err)
	}
	starts, successes, ends := 0, 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case EventCompactionStart:
			starts++
		case EventContextCompacted:
			successes++
		case EventCompactionEnd:
			ends++
		}
	}
	if starts != 1 || successes != 1 || ends != 1 {
		t.Fatalf("lifecycle starts=%d successes=%d ends=%d events=%#v", starts, successes, ends, events)
	}
}

func TestCompactNow_EmitCanReadHistoryWithoutDeadlock(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{Compactor: NewCompactor(cfg, "test", 2_000, p), Messages: unifiedPipelineHistory()}

	type outcome struct {
		result CompactResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := loop.CompactNow(context.Background(), CompactOptions{
			Trigger: "manual",
			Force:   true,
			Emit: func(Event) {
				_ = loop.History()
			},
		})
		done <- outcome{result: result, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil || !got.result.Applied {
			t.Fatalf("CompactNow result=%+v err=%v", got.result, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CompactNow deadlocked when Emit called History")
	}
}

func TestCompactNow_PostHookConcurrentHistoryChangesAreNotLost(t *testing.T) {
	t.Run("append is merged after hook context", func(t *testing.T) {
		p := &fakeSummarizer{}
		cfg := DefaultCompactionConfig()
		cfg.MaxSummarizeInputTokens = 0
		entered := make(chan struct{})
		release := make(chan struct{})
		hooks := NewHookRegistry()
		hooks.Register(PostCompactHandler(func(context.Context, HookContext, *PostCompact) *ModifiedPostCompact {
			close(entered)
			<-release
			return &ModifiedPostCompact{AdditionalContext: "keep the release checklist"}
		}))
		loop := &Loop{
			Compactor: NewCompactor(cfg, "test", 2_000, p),
			Hooks:     hooks,
			Messages:  unifiedPipelineHistory(),
		}

		type outcome struct {
			result CompactResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
			done <- outcome{result: result, err: err}
		}()
		<-entered
		loop.AppendUser("concurrent note from the user")
		close(release)

		select {
		case got := <-done:
			if got.err != nil || !got.result.Applied {
				t.Fatalf("CompactNow result=%+v err=%v", got.result, got.err)
			}
			history := loop.History()
			if !historyHasText(history, "[post-compact hook context] keep the release checklist") {
				t.Fatal("PostCompact AdditionalContext was lost")
			}
			if !historyHasText(history, "concurrent note from the user") {
				t.Fatal("AppendUser during PostCompact was lost")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("CompactNow did not finish after PostCompact append")
		}
	})

	t.Run("restore wins with an explicit conflict", func(t *testing.T) {
		p := &fakeSummarizer{}
		cfg := DefaultCompactionConfig()
		cfg.MaxSummarizeInputTokens = 0
		c := NewCompactor(cfg, "test", 2_000, p)
		c.LastSummary = "prior checkpoint"
		entered := make(chan struct{})
		release := make(chan struct{})
		hooks := NewHookRegistry()
		hooks.Register(PostCompactHandler(func(context.Context, HookContext, *PostCompact) *ModifiedPostCompact {
			close(entered)
			<-release
			return &ModifiedPostCompact{AdditionalContext: "must not overwrite restore"}
		}))
		loop := &Loop{Compactor: c, Hooks: hooks, Messages: unifiedPipelineHistory()}
		restored := []llm.Message{msg(llm.RoleUser, "restored session is authoritative")}

		done := make(chan error, 1)
		go func() {
			_, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
			done <- err
		}()
		<-entered
		loop.Restore(restored)
		close(release)

		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "history replaced") {
				t.Fatalf("CompactNow error = %v, want explicit history conflict", err)
			}
			if got := loop.History(); !reflect.DeepEqual(got, restored) {
				t.Fatalf("Restore during PostCompact was overwritten\n got: %#v\nwant: %#v", got, restored)
			}
			if c.LastSummary != "prior checkpoint" {
				t.Fatalf("aborted compaction changed LastSummary: %q", c.LastSummary)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("CompactNow did not finish after PostCompact restore")
		}
	})
}

func TestCompactNow_PersistFailurePreservesConcurrentAppend(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}

	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "manual",
		Force:   true,
		Persist: func([]llm.Message) error {
			_ = loop.History()
			loop.AppendUser("concurrent note while persistence failed")
			return errors.New("disk full after append")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "disk full after append") {
		t.Fatalf("persist error = %v", err)
	}
	if result.Applied {
		t.Fatal("failed durable commit reported Applied")
	}
	want := append(cloneMessages(before), msg(llm.RoleUser, "concurrent note while persistence failed"))
	if got := loop.History(); !reflect.DeepEqual(got, want) {
		t.Fatalf("persist rollback lost concurrent append\n got: %#v\nwant: %#v", got, want)
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("persist rollback changed LastSummary: %q", c.LastSummary)
	}
}

func TestCompactNow_PersistSuccessCompensatesConcurrentAppend(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{Compactor: NewCompactor(cfg, "test", 2_000, p), Messages: unifiedPipelineHistory()}

	var persisted [][]llm.Message
	loop.CompactionCheckpoint = func(_, after []llm.Message) error {
		persisted = append(persisted, cloneMessages(after))
		if len(persisted) == 1 {
			loop.AppendUser("concurrent note after first checkpoint")
		}
		return nil
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err != nil || !result.Applied {
		t.Fatalf("CompactNow result=%+v err=%v", result, err)
	}
	if len(persisted) != 2 {
		t.Fatalf("checkpoint calls = %d, want initial + compensation", len(persisted))
	}
	if !historyHasText(result.History, "concurrent note after first checkpoint") {
		t.Fatal("result lost append made during persistence")
	}
	if !reflect.DeepEqual(persisted[len(persisted)-1], result.History) {
		t.Fatal("compensating replacement differs from final live history")
	}
}
