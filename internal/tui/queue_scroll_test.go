package tui

// queue_scroll_test.go — pins the post-2026-05-08 fix for the
// "queued × N" sticky-pill bug.
//
// Old behavior: a sticky pill above the input showed `queued × N: …`
// and never moved when the user scrolled. The pill was fixed-bottom
// chrome (rendered in the `lower` builder of tui_render.go), so wheel
// scrolling the chat list left the pill anchored.
//
// New behavior:
//  1. The enqueue notice is a regular info-role message in m.messages
//     (and therefore in the chatList) so it scrolls with the stream.
//  2. The notice content includes a peek of the user's prompt so a
//     scroll-back reader sees what was queued, not just the count.
//  3. The status bar carries a compact `◷ N queued` chip — always
//     visible regardless of scroll, but doesn't block the message
//     stream.
//  4. The sticky pill above the input is gone; renderQueuePill itself
//     still exists for backward-compat (kept under a separate test)
//     but is no longer wired into the View pipeline.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestEnqueueNotice_IncludesUserPromptPeek pins behavior #2: the
// in-stream notice carries the first ~80 runes of what the user typed,
// so scrolling back to it later is informative.
func TestEnqueueNotice_IncludesUserPromptPeek(t *testing.T) {
	m := newSlashTestModel(t)
	prompt := "为什么cd执行了这么久还没好"
	for _, r := range prompt {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.turnActive = true

	pressEnter(t, m)

	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt; got %d", len(m.queuedPrompts))
	}
	// Walk messages in reverse to find the latest info-role notice.
	var notice string
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == "info" {
			notice = m.messages[i].Content
			break
		}
	}
	if notice == "" {
		t.Fatalf("expected an info-role notice in m.messages after enqueue; got none")
	}
	if !strings.Contains(notice, "queued × 1") {
		t.Errorf("notice should mention queue count; got %q", notice)
	}
	if !strings.Contains(notice, prompt) {
		t.Errorf("notice should embed the user's prompt peek %q; got %q", prompt, notice)
	}
}

// TestEnqueueNotice_LongPromptTruncated guards the rune-counted
// truncation so a CJK paste doesn't slice mid-codepoint and the line
// stays a sensible length.
func TestEnqueueNotice_LongPromptTruncated(t *testing.T) {
	m := newSlashTestModel(t)
	long := strings.Repeat("中文测试", 40) // 160 runes — well past the 80-rune cap
	for _, r := range long {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.turnActive = true
	pressEnter(t, m)

	var notice string
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == "info" {
			notice = m.messages[i].Content
			break
		}
	}
	if !strings.HasSuffix(notice, "…") {
		t.Errorf("long prompt should be truncated with …; got %q", notice)
	}
	// The notice should not contain the entire 160-rune prompt; cap is
	// 80 runes for the peek itself plus a small fixed prefix.
	if rs := []rune(notice); len(rs) > 160 {
		t.Errorf("notice grew unexpectedly long (%d runes): %q", len(rs), notice)
	}
}

// TestStatusBar_QueuedChipVisible pins behavior #3: when prompts are
// queued, the status bar shows a compact `◷ N queued` indicator.
// This is the always-visible counterpart to the in-stream notice;
// scrolling far away doesn't hide the count.
func TestStatusBar_QueuedChipVisible(t *testing.T) {
	m := newSlashTestModel(t)
	m.width = 120
	m.queuedPrompts = []string{"first queued", "second queued"}

	bar := renderStatusBar(m)
	if !strings.Contains(bar, "◷ 2 queued") {
		t.Errorf("status bar should contain `◷ 2 queued` chip; got:\n%s", bar)
	}
}

// TestStatusBar_NoQueuedChipWhenEmpty — keep the chrome minimal: the
// chip only appears when there's something to surface.
func TestStatusBar_NoQueuedChipWhenEmpty(t *testing.T) {
	m := newSlashTestModel(t)
	m.width = 120
	m.queuedPrompts = nil

	bar := renderStatusBar(m)
	if strings.Contains(bar, "queued") {
		t.Errorf("empty queue should leave status bar free of `queued`; got:\n%s", bar)
	}
}

// TestSpinnerStatus_UsesToolElapsedWhileToolInFlight pins the
// 2026-05-08 fix for the misleading 1h 18m display: when a tool is
// running (m.spinnerSub != ""), the elapsed clock shown next to the
// tool preview must come from the tool's own StartTime, not from
// m.spinnerStartedAt (which is the whole-turn clock).
//
// Old behavior: the spinner row read `executing · cd … (1h 18m · …)`
// because spinnerStartedAt = turn start; the user reasonably read
// that as "this cd command has been running for 1h 18m" — but the
// turn had been looping for an hour and the cd just started. New
// behavior shows the tool-local elapsed (≈seconds), making real
// hangs visible and ruling out misattribution.
func TestSpinnerStatus_UsesToolElapsedWhileToolInFlight(t *testing.T) {
	now := time.Now()
	m := &Model{
		width:            120,
		spinnerActive:    true,
		spinnerStartedAt: now.Add(-90 * time.Minute), // turn started 90m ago
		spinnerVerb:      "executing",
		spinnerSub:       "cd /Users/ricardo/…",
		spinnerPhase:     "tool",
		toolEvents: []ToolEvent{
			// An older tool that already finished — must be ignored.
			{Kind: "result", ToolName: "Bash", StartTime: now.Add(-80 * time.Minute), Duration: 5 * time.Second},
			// Current in-flight tool — started 12s ago.
			{Kind: "start", ToolName: "Bash", StartTime: now.Add(-12 * time.Second)},
		},
	}

	out := renderSpinnerStatus(m)
	// 12s tool-local elapsed should render via formatElapsed → "12s".
	// 90m turn-elapsed must NOT appear.
	if !strings.Contains(out, "12s") {
		t.Errorf("expected `12s` (tool-local elapsed) in spinner; got:\n%s", out)
	}
	if strings.Contains(out, "1h 30m") || strings.Contains(out, "90m") {
		t.Errorf("turn elapsed leaked into spinner row; got:\n%s", out)
	}
}

// TestSpinnerStatus_FallsBackToTurnElapsedWhenIdle — after the tool
// finishes (spinnerSub cleared), the elapsed display reverts to the
// turn clock so the "thinking · 30s" idle stretch is honest about
// how long the model has been deliberating since turn start.
func TestSpinnerStatus_FallsBackToTurnElapsedWhenIdle(t *testing.T) {
	now := time.Now()
	m := &Model{
		width:            120,
		spinnerActive:    true,
		spinnerStartedAt: now.Add(-30 * time.Second),
		spinnerVerb:      "thinking",
		spinnerSub:       "", // no tool in flight
		toolEvents: []ToolEvent{
			{Kind: "result", ToolName: "Bash", StartTime: now.Add(-25 * time.Second), Duration: 3 * time.Second},
		},
	}
	out := renderSpinnerStatus(m)
	if !strings.Contains(out, "30s") {
		t.Errorf("expected `30s` (turn elapsed) in idle spinner; got:\n%s", out)
	}
}

// TestRender_StickyPillNoLongerCalled — full-View regression guard.
// With queue items but no other chrome reasons to render a pill row,
// the View must NOT contain the old sticky `queued × N: <peek>` line
// floating above the input box.
//
// We assert by checking that no line in the View matches the pill's
// distinctive prefix sequence ("◷ " followed immediately by italic
// "queued × N"). The in-stream notice uses parens (`(queued × N …)`)
// so a substring match for that is fine.
func TestRender_StickyPillNoLongerCalled(t *testing.T) {
	m := newSlashTestModel(t)
	m.width = 100
	m.height = 30
	m.queuedPrompts = []string{"please don't render me as a sticky pill"}

	view := m.View().Content
	// The sticky pill format from render_queue_pill.go renders the
	// peek without parens: `◷ queued × 1: please don't render me as a sticky pill`.
	// The in-stream notice uses parens: `(queued × 1 · Ctrl+C to clear): please…`.
	// We confirm the sticky form is absent — the bare-paren-free
	// "queued × 1: please" is the unique smoking gun for the pill.
	if strings.Contains(view, "◷ queued × 1: please") {
		t.Errorf("View() should no longer render the sticky queue pill; found it in:\n%s", view)
	}
}
