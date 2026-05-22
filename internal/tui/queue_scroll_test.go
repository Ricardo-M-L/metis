package tui

// queue_scroll_test.go — pins the queue-display contract.
//
// History:
//  - Pre-2026-05-08: a sticky pill above the input showed `queued × N: …`
//    and never moved when the user scrolled (fixed-bottom chrome).
//  - 2026-05-08: pill replaced by an in-stream `(queued × N …): peek`
//    info-role Message that scrolled with the chat.
//  - 2026-05-14: in-stream notice removed to match claude-code's
//    CoordinatorAgentStatus model — the only surface for queue state
//    is the status-bar `◷ N queued` chip. Adding a row per enqueue
//    cluttered scroll-back (user feedback w/ screenshot).
//
// Current contract:
//  1. Enqueue adds NOTHING to m.messages (no chat-history row).
//  2. m.queuedPrompts grows by one.
//  3. The status bar `◷ N queued` chip surfaces the count (always
//     visible regardless of scroll).
//  4. The sticky pill above the input is gone.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestEnqueue_AddsNoChatRow pins behavior #1: enqueue must NOT push
// a chat-history Message. Only m.queuedPrompts grows. (The old
// in-stream `(queued × N …): peek` row was deleted 2026-05-14 — user
// flagged it as scroll-back clutter.)
func TestEnqueue_AddsNoChatRow(t *testing.T) {
	m := newSlashTestModel(t)
	priorMsgCount := len(m.messages)

	prompt := "为什么cd执行了这么久还没好"
	for _, r := range prompt {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.turnActive = true
	pressEnter(t, m)

	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt; got %d", len(m.queuedPrompts))
	}
	if got := len(m.messages); got != priorMsgCount {
		t.Errorf("enqueue must not add chat-history Messages; len(messages) went %d → %d", priorMsgCount, got)
	}
	// Sanity: status bar chip still surfaces the count.
	m.width = 120
	bar := renderStatusBar(m)
	if !strings.Contains(bar, "◷ 1 queued") {
		t.Errorf("status bar must carry `◷ 1 queued` chip after enqueue; got:\n%s", bar)
	}
}

// TestEnqueue_LongPromptDoesNotPolluteChat — long CJK pastes used to
// produce a long info-row peek. After 2026-05-14 they should produce
// zero chat rows; the queued text is only retrieved at next-turn
// submission time.
func TestEnqueue_LongPromptDoesNotPolluteChat(t *testing.T) {
	m := newSlashTestModel(t)
	priorMsgCount := len(m.messages)

	long := strings.Repeat("中文测试", 40)
	for _, r := range long {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.turnActive = true
	pressEnter(t, m)

	if got := len(m.messages); got != priorMsgCount {
		t.Errorf("long prompt enqueue must not add chat rows; len(messages) went %d → %d", priorMsgCount, got)
	}
	if len(m.queuedPrompts) != 1 || m.queuedPrompts[0].Text != long {
		t.Errorf("queued prompt should preserve the full prompt verbatim; got %+v", m.queuedPrompts)
	}
}

// TestStatusBar_QueuedChipVisible pins behavior #3: when prompts are
// queued, the status bar shows a compact `◷ N queued` indicator.
// This is the always-visible counterpart to the in-stream notice;
// scrolling far away doesn't hide the count.
func TestStatusBar_QueuedChipVisible(t *testing.T) {
	m := newSlashTestModel(t)
	m.width = 120
	m.queuedPrompts = []queuedItem{{Text:"first queued"}, {Text:"second queued"}}

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
	m.queuedPrompts = []queuedItem{{Text:"please don't render me as a sticky pill"}}

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
