package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestLoop_TimeBasedMicrocompact_FiresWhenIdleExceeded — proves the
// Tier 1.5 path force-runs a Microcompact pass even when SnipThreshold
// isn't crossed, provided IdleMaxSeconds has elapsed since the last
// time-based pass. Models CC's services/compact/timeBasedMC, which
// kicks in around the 60-min provider-cache TTL to avoid re-sending
// tool_results the cache has already aged out.
func TestLoop_TimeBasedMicrocompact_FiresWhenIdleExceeded(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.MicrocompactMinChars = 4000
	cfg.MicrocompactDir = t.TempDir()
	cfg.KeepRecentToolResults = 0 // disable CC keepRecent guard — this test only cares about the time-based fire path
	cfg.IdleMaxSeconds = 1
	cfg.SnipThreshold = 0.99 // intentionally high — token path WON'T trigger
	cfg.CollapseThreshold = 0
	cfg.Threshold = 0.99

	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{})
	l := &Loop{Compactor: c}
	l.lastTimeBasedMicrocompactAt = time.Now().Add(-5 * time.Second)

	// Build a convo with a fat tool_result. Token total is BELOW SnipThreshold
	// (we set it to 0.99 so the threshold path stays silent), so only the
	// time-based path can fire.
	l.Messages = []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", strings.Repeat("a", 8000)),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}
	if c.ShouldSnip(l.Messages) {
		t.Fatalf("precondition: SnipThreshold should NOT trigger (we want time-based path only)")
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)

	timeBased := 0
	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "microcompacted (time-based") {
			timeBased++
		}
	}
	if timeBased != 1 {
		t.Errorf("expected exactly 1 time-based microcompact notice; got %d", timeBased)
	}
	got := l.Messages[2].Content[0].ToolResult
	if len(got) >= 8000 {
		t.Errorf("tool_result not microcompacted; len=%d (want < 8000)", len(got))
	}
	if time.Since(l.lastTimeBasedMicrocompactAt) > 2*time.Second {
		t.Errorf("lastTimeBasedMicrocompactAt was not refreshed after fire")
	}
}

// TestLoop_TimeBasedMicrocompact_QuietWhenWithinWindow — Tier 1.5 must
// NOT fire when the idle window hasn't elapsed. Without this guard the
// first maybeCompact of any session would always rewrite the cache and
// invalidate the provider prompt cache for no reason.
func TestLoop_TimeBasedMicrocompact_QuietWhenWithinWindow(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.MicrocompactMinChars = 4000
	cfg.MicrocompactDir = t.TempDir()
	cfg.KeepRecentToolResults = 0 // disable CC keepRecent guard — this test only cares about the time-based fire path
	cfg.IdleMaxSeconds = 3600
	cfg.SnipThreshold = 0.99
	cfg.CollapseThreshold = 0
	cfg.Threshold = 0.99

	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{})
	l := &Loop{Compactor: c}
	l.lastTimeBasedMicrocompactAt = time.Now() // just initialised

	l.Messages = []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", strings.Repeat("a", 8000)),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)

	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "time-based") {
			t.Errorf("time-based microcompact fired prematurely (still inside idle window); ev=%q", ev.Info)
		}
	}
	if len(l.Messages[2].Content[0].ToolResult) != 8000 {
		t.Errorf("tool_result was rewritten when it shouldn't have been; len=%d", len(l.Messages[2].Content[0].ToolResult))
	}
}

// TestLoop_TimeBasedMicrocompact_DisabledWhenIdleSecondsZero — opt-out
// path. Settig IdleMaxSeconds=0 returns metis to the pre-2026-05-15
// threshold-only behaviour. Important because TUI users on tiny convos
// don't want disk writes triggered by wall-clock alone.
func TestLoop_TimeBasedMicrocompact_DisabledWhenIdleSecondsZero(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.MicrocompactMinChars = 4000
	cfg.MicrocompactDir = t.TempDir()
	cfg.KeepRecentToolResults = 0 // disable CC keepRecent guard — this test only cares about the time-based fire path
	cfg.IdleMaxSeconds = 0        // disabled
	cfg.SnipThreshold = 0.99
	cfg.CollapseThreshold = 0
	cfg.Threshold = 0.99

	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{})
	l := &Loop{Compactor: c}
	l.lastTimeBasedMicrocompactAt = time.Now().Add(-24 * time.Hour) // very old

	l.Messages = []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", strings.Repeat("a", 8000)),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)

	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "time-based") {
			t.Errorf("time-based path fired with IdleMaxSeconds=0; ev=%q", ev.Info)
		}
	}
}
