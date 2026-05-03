package tui

import (
	"strings"
	"testing"
	"time"
)

// TestSpinner_UploadPhaseShowsUpArrow — pre-stream (firstStreamAt
// not yet set) the spinner shows ↑ with the last input count.
// Mirrors claude-code's image #17 ("↑ 507 tokens").
func TestSpinner_UploadPhaseShowsUpArrow(t *testing.T) {
	m := minimalModel(200000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerVerb = "thinking"
	// Simulate a prior round that left lastIn=8200 (next round about
	// to start, no stream yet).
	m.totalTokens.add(8200, 0, 0, 0)
	// firstStreamAt is zero — upload phase.

	out := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(out, "↑") {
		t.Errorf("upload phase should show ↑; got:\n%s", out)
	}
	if strings.Contains(out, "↓") {
		t.Errorf("upload phase should NOT show ↓; got:\n%s", out)
	}
	if !strings.Contains(out, "8.2k") {
		t.Errorf("upload phase should show last input value 8.2k; got:\n%s", out)
	}
}

// TestSpinner_ReceivePhaseShowsDownArrow — once firstStreamAt is set
// AND streamingText/thinkingText non-empty, spinner flips to ↓.
// Mirrors claude-code's image #18 ("↓ 1.2k tokens").
func TestSpinner_ReceivePhaseShowsDownArrow(t *testing.T) {
	m := minimalModel(200000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerVerb = "thinking"
	m.firstStreamAt = m.startTime.Add(2 * time.Second)
	m.streamingText = strings.Repeat("hello world ", 50) // ~600 chars → ~150 tok est
	m.totalTokens.add(8200, 0, 0, 0)                     // last in (irrelevant in receive phase)

	out := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(out, "↓") {
		t.Errorf("receive phase should show ↓; got:\n%s", out)
	}
	if strings.Contains(out, "↑") {
		t.Errorf("receive phase should NOT show ↑; got:\n%s", out)
	}
}

// TestSpinner_ReceivePhaseUsesLiveEstimate — during streaming, count
// grows with streamingText length even before EventTokens fires (which
// happens at end of stream). Without this, the spinner counter freezes
// on the previous round's lastOut for the entire streaming duration.
func TestSpinner_ReceivePhaseUsesLiveEstimate(t *testing.T) {
	m := minimalModel(200000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerVerb = "thinking"
	m.firstStreamAt = m.startTime.Add(time.Second)
	// 1000 chars → ~250 token estimate (chars/4).
	m.streamingText = strings.Repeat("a", 1000)
	// lastOut from previous round was 50 — live estimate must beat it.
	m.totalTokens.add(0, 50, 0, 0)

	out := stripANSI(renderSpinnerStatus(m))
	// Count from estimate (250) ≫ lastOut (50). With 250 in formatTokens:
	// 250 < 1000 → "250" raw. So look for "250" or higher.
	if !strings.Contains(out, "250") {
		t.Errorf("expected live token estimate (250) in spinner; got:\n%s", out)
	}
}

// TestSpinner_NeverShowsBothArrows — the user-visible regression:
// previously metis showed "↑ N · ↓ M tokens" simultaneously which
// looked nothing like claude-code. Make sure no code path shows both.
func TestSpinner_NeverShowsBothArrows(t *testing.T) {
	cases := []struct {
		name           string
		setReceiving   bool
		streamingText  string
		lastIn, lastOut int
	}{
		{"upload, both counts present", false, "", 5000, 100},
		{"receive, both counts present", true, "hello", 5000, 100},
		{"upload, no prior round", false, "", 0, 0},
		{"receive, no prior round", true, "hello world!", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := minimalModel(200000)
			m.spinnerActive = true
			m.spinnerStartedAt = m.startTime
			m.spinnerVerb = "x"
			if tc.setReceiving {
				m.firstStreamAt = m.startTime.Add(time.Second)
				m.streamingText = tc.streamingText
			}
			m.totalTokens.add(tc.lastIn, tc.lastOut, 0, 0)

			out := stripANSI(renderSpinnerStatus(m))
			hasUp := strings.Contains(out, "↑")
			hasDown := strings.Contains(out, "↓")
			if hasUp && hasDown {
				t.Errorf("both ↑ and ↓ rendered (regression); output:\n%s", out)
			}
		})
	}
}
