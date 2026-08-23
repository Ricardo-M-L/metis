package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// A shallow tier may remain available as an internal helper, but crossing its
// historical threshold alone must not mutate live context or emit a separate
// user-visible compression. This prevents the observed 85% -> 75% -> no heavy
// checkpoint behavior.
func TestLoop_MaybeCompact_DoesNotRunStandaloneSnip(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.MinimumTokens = 0
	p := &fakeSummarizer{}
	c := NewCompactor(cfg, "test", 2_000, p)
	l := &Loop{Compactor: c}
	l.Messages = []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", strings.Repeat("a", 6000)),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}
	if !c.ShouldSnip(l.Messages) || c.ShouldCompact(l.Messages) {
		t.Fatalf("precondition: estimate=%d snip=%v compact=%v", estimateTokens(l.Messages), c.ShouldSnip(l.Messages), c.ShouldCompact(l.Messages))
	}
	before := l.Messages[2].Content[0].ToolResult
	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)
	if got := l.Messages[2].Content[0].ToolResult; got != before {
		t.Fatal("standalone Snip mutated history below the heavy threshold")
	}
	if p.calls != 0 {
		t.Fatalf("summarizer calls below full threshold = %d", p.calls)
	}
	for ev := range out {
		if ev.Kind == EventContextCompacted || strings.Contains(ev.Info, "snip") {
			t.Fatalf("unexpected shallow compaction event: %+v", ev)
		}
	}
}

func TestLoop_MaybeCompact_EmitsOneHeavyCompaction(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	p := &fakeSummarizer{}
	c := NewCompactor(cfg, "test", 1_000, p)
	l := &Loop{Compactor: c}
	l.Messages = []llm.Message{msg(llm.RoleUser, "seed")}
	for i := 0; i < 12; i++ {
		l.Messages = append(l.Messages,
			msg(llm.RoleAssistant, strings.Repeat("a", 180)),
			msg(llm.RoleUser, strings.Repeat("u", 180)),
		)
	}
	if !c.ShouldCompact(l.Messages) {
		t.Fatalf("precondition: history estimate %d below trigger %d", estimateTokens(l.Messages), c.TriggerTokens())
	}
	out := make(chan Event, 64)
	l.maybeCompact(context.Background(), out)
	close(out)
	starts, successes, ends := 0, 0, 0
	for ev := range out {
		switch ev.Kind {
		case EventCompactionStart:
			starts++
		case EventContextCompacted:
			successes++
		case EventCompactionEnd:
			ends++
		}
		if strings.Contains(ev.Info, "snip") || strings.Contains(ev.Info, "collapse") {
			t.Fatalf("legacy tier leaked into unified lifecycle: %+v", ev)
		}
	}
	if starts != 1 || successes != 1 || ends != 1 || p.calls != 1 {
		t.Fatalf("lifecycle starts=%d successes=%d ends=%d summaryCalls=%d", starts, successes, ends, p.calls)
	}
}
