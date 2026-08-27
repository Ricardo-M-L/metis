package tui

import (
	"strings"
	"testing"
	"time"
)

func TestCompactionSpinnerUsesIndependentActivityState(t *testing.T) {
	m := minimalModel(200_000)
	m.width = 120
	m.spinnerActive = true
	m.spinnerStartedAt = time.Now().Add(-9*time.Hour - 16*time.Minute)
	m.compactionStartedAt = time.Now().Add(-5 * time.Second)
	m.spinnerOverride = "Compacting conversation..."
	m.spinnerPhase = "requesting"
	m.firstStreamAt = m.spinnerStartedAt.Add(8 * time.Hour)
	m.totalTokens.add(568, 123, 0, 0)

	out := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(out, "(5.0s)") {
		t.Fatalf("compaction row did not use its own start time:\n%s", out)
	}
	for _, leaked := range []string{"9h", "568", "↑", "thought for"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("parent-turn state %q leaked into compaction row:\n%s", leaked, out)
		}
	}
}

func TestCompactionSpinnerShowsApproximateSummaryOutput(t *testing.T) {
	m := minimalModel(200_000)
	m.width = 120
	m.spinnerActive = true
	m.spinnerStartedAt = time.Now().Add(-time.Hour)
	m.compactionStartedAt = time.Now().Add(-2 * time.Second)
	m.spinnerOverride = "Compacting conversation..."
	m.spinnerCompactionBytes = 4_000

	out := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(out, "↓ ≈1.0k summary tokens") {
		t.Fatalf("missing approximate summary-output estimate:\n%s", out)
	}
}
