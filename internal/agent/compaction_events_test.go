package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestMaybeCompact_EmitsCompactionStartEnd — both LLM-driven tiers
// (Collapse and Compact) must bracket their summarize calls with a
// CompactionStart / ContextCompacted pair so the TUI can swap the
// spinner label. Without this the user sees "Thinking..." for 5-30s
// during summarize and assumes the input area is frozen.
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
	for i := 0; i < 30; i++ {
		l.Messages = append(l.Messages, msg(llm.RoleAssistant, strings.Repeat("x", 100)))
	}

	out := make(chan Event, 64)
	l.maybeCompact(context.Background(), out)
	close(out)

	var startTiers, endTiers []string
	for ev := range out {
		switch ev.Kind {
		case EventCompactionStart:
			startTiers = append(startTiers, ev.Info)
		case EventContextCompacted:
			endTiers = append(endTiers, ev.Info)
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
		if ev.Kind == EventCompactionStart || ev.Kind == EventContextCompacted {
			t.Errorf("unexpected compaction event below threshold: kind=%v info=%q", ev.Kind, ev.Info)
		}
	}
}
