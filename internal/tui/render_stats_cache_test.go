package tui

// render_stats_cache_test.go — locks the /stats prompt-cache breakdown
// rows added in Phase E.2. Hand-builds a Loop with SystemSections so
// the renderer is exercised without booting the full TUI.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestRenderStats_ShowsSystemSectionBreakdown(t *testing.T) {
	loop := &agent.Loop{
		Model:    "test-model",
		MaxIters: 12,
		SystemSections: []llm.SystemSection{
			{Name: "identity", Body: strings.Repeat("a", 200), Cache: true},
			{Name: "style", Body: strings.Repeat("b", 800), Cache: true},
			{Name: "env", Body: strings.Repeat("c", 150), Volatile: true},
		},
	}
	m := &Model{loop: loop, sessionID: "abc"}

	out := renderStats(m)

	if !strings.Contains(out, "system sections") {
		t.Errorf("stats missing 'system sections' row; got:\n%s", out)
	}
	// 3 sections rendered.
	if !strings.Contains(out, "3") {
		t.Errorf("stats should show section count 3; got:\n%s", out)
	}
	// total = 200 + 800 + 150 = 1150 → "1,150"
	if !strings.Contains(out, "1,150") {
		t.Errorf("stats should show total system chars 1,150; got:\n%s", out)
	}
	// cacheable = 200 + 800 = 1000 → "1,000"
	if !strings.Contains(out, "1,000") {
		t.Errorf("stats should show cacheable 1,000; got:\n%s", out)
	}
	// volatile = 150
	if !strings.Contains(out, "cacheable") {
		t.Errorf("stats should label cacheable row; got:\n%s", out)
	}
	if !strings.Contains(out, "volatile") {
		t.Errorf("stats should label volatile row; got:\n%s", out)
	}
}

func TestRenderStats_DistinguishesActualIterationsFromPerTurnCap(t *testing.T) {
	loop := &agent.Loop{Model: "test-model", MaxIters: 12}
	m := &Model{loop: loop, sessionID: "abc"}

	out := ansi.Strip(renderStats(m))
	if !strings.Contains(out, "loop iterations: 0") {
		t.Errorf("stats should report the live iteration count, got:\n%s", out)
	}
	if !strings.Contains(out, "iteration cap:   12") {
		t.Errorf("stats should label MaxIters as a per-turn cap, got:\n%s", out)
	}
}

func TestFormatIterationCap_Unlimited(t *testing.T) {
	if got := formatIterationCap(0); got != "unlimited" {
		t.Fatalf("formatIterationCap(0) = %q", got)
	}
}

func TestRenderStats_FallsBackToFlatStringWhenNoSections(t *testing.T) {
	loop := &agent.Loop{
		Model:    "test-model",
		MaxIters: 12,
		System:   "you are metis. be brief.",
		// SystemSections intentionally nil.
	}
	m := &Model{loop: loop, sessionID: "abc"}

	out := renderStats(m)
	if !strings.Contains(out, "system chars") {
		t.Errorf("expected flat system chars row; got:\n%s", out)
	}
	if !strings.Contains(out, "flat") {
		t.Errorf("expected '(flat / no cache flags)' marker; got:\n%s", out)
	}
}

func TestRenderStats_OmitsSystemBlockWhenEmpty(t *testing.T) {
	loop := &agent.Loop{
		Model:    "test-model",
		MaxIters: 12,
		// Neither System nor SystemSections set.
	}
	m := &Model{loop: loop, sessionID: "abc"}

	out := renderStats(m)
	if strings.Contains(out, "system sections") {
		t.Errorf("should not render system block when no prompt is loaded; got:\n%s", out)
	}
	if strings.Contains(out, "system chars") {
		t.Errorf("should not render system chars when no prompt is loaded; got:\n%s", out)
	}
}
