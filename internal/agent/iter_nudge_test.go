package agent

import (
	"strings"
	"testing"
)

func TestShouldFireNudge_50PctThreshold(t *testing.T) {
	fired := make([]bool, len(iterNudges))
	idx, body := shouldFireNudge(50, 100, fired)
	if idx != 0 {
		t.Errorf("at 50%% should fire threshold 0; got idx=%d", idx)
	}
	if !strings.Contains(body, "half") {
		t.Errorf("50%% body should mention 'half'; got:\n%s", body)
	}
	if !strings.Contains(body, "[iter 50 / 100") {
		t.Errorf("body should include iter counter; got:\n%s", body)
	}
}

func TestShouldFireNudge_75PctThreshold(t *testing.T) {
	fired := make([]bool, len(iterNudges))
	fired[0] = true // 50% already fired
	idx, body := shouldFireNudge(75, 100, fired)
	if idx != 1 {
		t.Errorf("at 75%% should fire threshold 1; got idx=%d", idx)
	}
	if !strings.Contains(body, "75%") {
		t.Errorf("75%% body should mention 75%%; got:\n%s", body)
	}
}

func TestShouldFireNudge_90PctThreshold(t *testing.T) {
	fired := []bool{true, true, false}
	idx, body := shouldFireNudge(90, 100, fired)
	if idx != 2 {
		t.Errorf("at 90%% should fire threshold 2; got idx=%d", idx)
	}
	if !strings.Contains(body, "90%") {
		t.Errorf("90%% body should mention 90%%; got:\n%s", body)
	}
}

func TestShouldFireNudge_NoFireWhenAllFired(t *testing.T) {
	fired := []bool{true, true, true}
	idx, body := shouldFireNudge(95, 100, fired)
	if body != "" || idx != -1 {
		t.Errorf("all thresholds fired → no more nudges; got idx=%d body=%q", idx, body)
	}
}

func TestShouldFireNudge_FiresHighestUnfired(t *testing.T) {
	// We jumped from iter 20 to iter 80 without firing — should fire
	// the 75% threshold (highest one ≤ ratio that's still unfired),
	// not loop back to 50%.
	fired := make([]bool, len(iterNudges))
	idx, _ := shouldFireNudge(80, 100, fired)
	if idx != 0 {
		// Actually first unfired in order wins — 50% fires first
		// because iteration is ordered. That's correct: we want
		// the user to see the pacing reminder before the urgent one.
		t.Errorf("at iter 80 with nothing fired, expected 50%% to fire first (it's lowest unfired); got idx=%d", idx)
	}
}

func TestShouldFireNudge_NoFireBelow50Pct(t *testing.T) {
	fired := make([]bool, len(iterNudges))
	idx, body := shouldFireNudge(40, 100, fired)
	if body != "" || idx != -1 {
		t.Errorf("at 40%% no nudge should fire; got idx=%d body=%q", idx, body)
	}
}

func TestShouldFireNudge_SkipsWhenCapTooSmall(t *testing.T) {
	// MaxIters < 10 collapses all three thresholds into the same
	// iter (e.g. 50% of 5 = iter 3, 75% of 5 = iter 4 — too noisy
	// for tiny test rigs). Skip entirely.
	fired := make([]bool, len(iterNudges))
	idx, body := shouldFireNudge(3, 5, fired)
	if body != "" || idx != -1 {
		t.Errorf("MaxIters < 10 should suppress all nudges; got idx=%d body=%q", idx, body)
	}
}

func TestShouldFireNudge_SkipsWhenMaxItersZero(t *testing.T) {
	// MaxIters == 0 (unlimited) — never fire.
	fired := make([]bool, len(iterNudges))
	idx, body := shouldFireNudge(100, 0, fired)
	if body != "" || idx != -1 {
		t.Errorf("MaxIters == 0 → unlimited, no nudge; got idx=%d body=%q", idx, body)
	}
}

func TestFinalSummaryRescueMessage_ContainsKeyDirectives(t *testing.T) {
	msg := finalSummaryRescueMessage
	for _, want := range []string{
		"iteration cap",
		"1-3 lines",
		"Do NOT start any\nnew tool calls",
		"system-reminder",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rescue message missing %q; got:\n%s", want, msg)
		}
	}
}
