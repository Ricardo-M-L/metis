package tui

// render_subagent_chip_test.go — covers the sub-agent status pill
// added 2026-05-18. Without progress fields the chip used to render
// "◇ Name" only — the user couldn't tell a busy sub-agent from a
// stuck one. The new chip surfaces LastTool, elapsed time, and
// ToolsCount per the same ordering claude-code uses for its Task
// pill.

import (
	"strings"
	"testing"
	"time"
)

func TestRenderSubAgentChip_NameOnly_WhenNoProgress(t *testing.T) {
	t.Parallel()
	got := renderSubAgentChip(SubAgentInfo{Name: "alice"})
	if got != "◇ alice" {
		t.Errorf("expected bare name chip, got %q", got)
	}
}

func TestRenderSubAgentChip_WithToolAndElapsed(t *testing.T) {
	t.Parallel()
	sa := SubAgentInfo{
		Name:       "alice",
		LastTool:   "Read",
		StartedAt:  time.Now().Add(-23 * time.Second),
		ToolsCount: 7,
	}
	got := renderSubAgentChip(sa)
	if !strings.Contains(got, "alice") || !strings.Contains(got, "Read") {
		t.Errorf("chip missing name or tool: %q", got)
	}
	if !strings.Contains(got, "7t") {
		t.Errorf("chip missing tool count: %q", got)
	}
	if !strings.Contains(got, "s") {
		t.Errorf("chip missing elapsed seconds: %q", got)
	}
}

func TestRenderSubAgentChip_LongRunMinutes(t *testing.T) {
	t.Parallel()
	sa := SubAgentInfo{Name: "verifier", StartedAt: time.Now().Add(-4 * time.Minute)}
	got := renderSubAgentChip(sa)
	if !strings.Contains(got, "4m") {
		t.Errorf("expected '4m' elapsed, got %q", got)
	}
	if strings.Contains(got, "240s") {
		t.Errorf("should switch to minutes past 60s, got %q", got)
	}
}

func TestRenderSubAgentChip_CompletedShowsCheck(t *testing.T) {
	t.Parallel()
	sa := SubAgentInfo{
		Name:       "alice",
		Status:     "completed",
		StartedAt:  time.Now().Add(-47 * time.Second),
		FinishedAt: time.Now(),
		ToolsCount: 12,
		LastTool:   "Read", // should NOT appear in terminal-state chip
	}
	got := renderSubAgentChip(sa)
	if !strings.HasPrefix(got, "✓ alice") {
		t.Errorf("completed chip should lead with ✓ alice; got %q", got)
	}
	if strings.Contains(got, "Read") {
		t.Errorf("completed chip should omit LastTool (stale); got %q", got)
	}
	if !strings.Contains(got, "12t") {
		t.Errorf("completed chip should keep tools count; got %q", got)
	}
}

func TestRenderSubAgentChip_FailedShowsCross(t *testing.T) {
	t.Parallel()
	sa := SubAgentInfo{
		Name:       "qa",
		Status:     "failed",
		StartedAt:  time.Now().Add(-3 * time.Second),
		FinishedAt: time.Now(),
	}
	got := renderSubAgentChip(sa)
	if !strings.HasPrefix(got, "✗ qa") {
		t.Errorf("failed chip should lead with ✗ qa; got %q", got)
	}
}

func TestRenderSubAgentChip_ElapsedFrozenAfterFinish(t *testing.T) {
	t.Parallel()
	started := time.Now().Add(-10 * time.Second)
	finished := started.Add(2 * time.Second) // 2s actual run
	sa := SubAgentInfo{
		Name:       "bob",
		Status:     "completed",
		StartedAt:  started,
		FinishedAt: finished,
	}
	got := renderSubAgentChip(sa)
	// Elapsed should be 2s (FinishedAt - StartedAt), NOT 10s
	// (now - StartedAt). Frozen-at-finish keeps the chip honest.
	if !strings.Contains(got, "2s") {
		t.Errorf("elapsed should be frozen at 2s; got %q", got)
	}
	if strings.Contains(got, "10s") {
		t.Errorf("elapsed should NOT tick past finish time; got %q", got)
	}
}

func TestFormatSubAgentElapsed_UnitSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d        time.Duration
		contains string
	}{
		{30 * time.Second, "s"},
		{2 * time.Minute, "m"},
		{3 * time.Hour, "h"},
	}
	for _, c := range cases {
		got := formatSubAgentElapsed(c.d)
		if !strings.HasSuffix(got, c.contains) {
			t.Errorf("formatSubAgentElapsed(%v) = %q; want suffix %q", c.d, got, c.contains)
		}
	}
}
