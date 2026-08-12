package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestMaybeCompact_EmitsCompactionStartEnd — both LLM-driven tiers
// (Collapse and Compact) must bracket their summarize calls with a
// CompactionStart / CompactionEnd pair so the TUI can swap the spinner
// label without conflating lifecycle completion with successful history
// replacement. ContextCompacted is success-only.
func TestMaybeCompact_EmitsCompactionStartEnd(t *testing.T) {
	// Force both tiers to trigger by stuffing the convo with text bulk
	// that puts us above CollapseThreshold (0.78) and Threshold (0.85).
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.SnipThreshold = 0
	cfg.MicrocompactDir = ""
	cfg.IdleMaxSeconds = 0
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})
	l := &Loop{Compactor: c}

	// 30 mid-sized messages — enough to cross both Collapse and Compact
	// thresholds against the 1000-token cap.
	l.Messages = []llm.Message{msg(llm.RoleUser, "seed")}
	for i := 0; i < 15; i++ {
		l.Messages = append(l.Messages,
			msg(llm.RoleUser, strings.Repeat("u", 100)),
			msg(llm.RoleAssistant, strings.Repeat("a", 100)),
		)
	}

	out := make(chan Event, 64)
	l.maybeCompact(context.Background(), out)
	close(out)

	var startTiers, endTiers []string
	var successes []Event
	for ev := range out {
		switch ev.Kind {
		case EventCompactionStart:
			startTiers = append(startTiers, ev.Info)
		case EventCompactionEnd:
			endTiers = append(endTiers, ev.Info)
		case EventContextCompacted:
			successes = append(successes, ev)
		}
	}
	if len(startTiers) == 0 {
		t.Fatalf("no EventCompactionStart emitted; expected at least one of collapse/compact")
	}
	if len(startTiers) != len(endTiers) {
		t.Errorf("start/end count mismatch: starts=%v ends=%v", startTiers, endTiers)
	}
	for i, tier := range startTiers {
		if tier == "" {
			t.Errorf("start[%d].Info empty; expected tier name", i)
		}
		if i < len(endTiers) && endTiers[i] != tier {
			t.Errorf("start[%d]=%q but end[%d]=%q — pairs must match", i, tier, i, endTiers[i])
		}
	}
	if len(successes) == 0 {
		t.Fatal("no successful EventContextCompacted emitted")
	}
	last := successes[len(successes)-1]
	if last.ContextTokens <= 0 || last.PreviousContextTokens <= last.ContextTokens {
		t.Errorf("success event must carry a real context drop; before=%d after=%d",
			last.PreviousContextTokens, last.ContextTokens)
	}
	if got := int(l.estTokens.Load()); got != estimateTokens(l.Messages) {
		t.Errorf("cached post-compact estimate = %d, want final history estimate %d",
			got, estimateTokens(l.Messages))
	}
}

// TestMaybeCompact_FailureEndsProgressWithoutSuccess pins the Claude Code
// lifecycle split: compact_end is emitted from the failure path so the spinner
// clears, but no ContextCompacted event may claim that history changed.
func TestMaybeCompact_FailureEndsProgressWithoutSuccess(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.SnipThreshold = 0
	cfg.CollapseThreshold = 0
	cfg.MicrocompactDir = ""
	cfg.IdleMaxSeconds = 0
	c := NewCompactor(cfg, "test", 1000, &errSummarizer{err: errors.New("summary unavailable")})
	l := &Loop{Compactor: c}
	l.Messages = []llm.Message{msg(llm.RoleUser, "seed")}
	for i := 0; i < 15; i++ {
		l.Messages = append(l.Messages,
			msg(llm.RoleUser, strings.Repeat("u", 100)),
			msg(llm.RoleAssistant, strings.Repeat("a", 100)),
		)
	}
	before := append([]llm.Message(nil), l.Messages...)

	out := make(chan Event, 32)
	l.maybeCompact(context.Background(), out)
	close(out)

	starts, ends, successes := 0, 0, 0
	for ev := range out {
		switch ev.Kind {
		case EventCompactionStart:
			starts++
		case EventCompactionEnd:
			ends++
			if ev.Err == nil {
				t.Error("failed attempt emitted CompactionEnd without its error")
			}
		case EventContextCompacted:
			successes++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("failure lifecycle starts=%d ends=%d, want 1/1", starts, ends)
	}
	if successes != 0 {
		t.Fatalf("failed compaction emitted %d success events", successes)
	}
	if !reflect.DeepEqual(l.Messages, before) {
		t.Error("failed compaction mutated conversation history")
	}
}

// TestMaybeCompact_EmitsPreCompactHook — the full Compact tier must fire
// the PreCompact hook (trigger "auto") just before summarizing, so an
// observer can back up the transcript. Below-threshold runs must NOT fire
// it.
func TestMaybeCompact_EmitsPreCompactHook(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.SnipThreshold = 0
	cfg.MicrocompactDir = ""
	cfg.IdleMaxSeconds = 0
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})

	reg := NewHookRegistry()
	var fired int
	var gotTrigger string
	var gotCount int
	reg.Register(PreCompactHandler(func(_ context.Context, _ HookContext, p *PreCompact) {
		fired++
		gotTrigger = p.Trigger
		gotCount = p.MessageCount
	}))
	l := &Loop{Compactor: c, Hooks: reg}

	l.Messages = []llm.Message{msg(llm.RoleUser, "seed")}
	for i := 0; i < 30; i++ {
		l.Messages = append(l.Messages, msg(llm.RoleAssistant, strings.Repeat("x", 100)))
	}
	wantCount := len(l.Messages)

	out := make(chan Event, 64)
	l.maybeCompact(context.Background(), out)
	close(out)

	if fired != 1 {
		t.Fatalf("expected PreCompact to fire exactly once, got %d", fired)
	}
	if gotTrigger != "auto" {
		t.Errorf("expected trigger=auto, got %q", gotTrigger)
	}
	if gotCount != wantCount {
		t.Errorf("expected MessageCount=%d, got %d", wantCount, gotCount)
	}
}

// TestMaybeCompact_NoPreCompactBelowThreshold — PreCompact must stay
// silent when the threshold isn't crossed (no summarization happens).
func TestMaybeCompact_NoPreCompactBelowThreshold(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.SnipThreshold = 0
	cfg.MicrocompactDir = ""
	cfg.IdleMaxSeconds = 0
	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{}) // huge cap → no trigger

	reg := NewHookRegistry()
	var fired int
	reg.Register(PreCompactHandler(func(_ context.Context, _ HookContext, _ *PreCompact) { fired++ }))
	l := &Loop{Compactor: c, Hooks: reg}
	l.Messages = []llm.Message{
		msg(llm.RoleUser, "seed"),
		msg(llm.RoleAssistant, "hi"),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)

	if fired != 0 {
		t.Errorf("PreCompact fired %d times below threshold; want 0", fired)
	}
}

// TestMaybeCompact_NoCompactionEventsWhenBelowThreshold — confirms the
// new events don't fire on every iteration; they're scoped to the
// LLM-driven tiers and silent when the threshold isn't crossed.
func TestMaybeCompact_NoCompactionEventsWhenBelowThreshold(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.SnipThreshold = 0
	cfg.MicrocompactDir = ""
	cfg.IdleMaxSeconds = 0
	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{}) // huge cap → no trigger
	l := &Loop{Compactor: c}
	l.Messages = []llm.Message{
		msg(llm.RoleUser, "seed"),
		msg(llm.RoleAssistant, "hi"),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)

	for ev := range out {
		if ev.Kind == EventCompactionStart || ev.Kind == EventCompactionEnd || ev.Kind == EventContextCompacted {
			t.Errorf("unexpected compaction event below threshold: kind=%v info=%q", ev.Kind, ev.Info)
		}
	}
}
