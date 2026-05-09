package tui

// chat_e2e_test.go — interaction-level coverage of the chat surface.
//
// These tests don't spin up a tea.Program (that would require an
// agent.Loop, an LLM, an API key, and a real terminal); instead they
// drive the Model directly via Update(msg) which mutates state in
// place, then read View() to assert the visual contract.
//
// What's covered:
//   * PgUp / PgDn / Home / End route to the chat list
//   * Mouse wheel scrolls the chat list (no longer goes to viewport.Update)
//   * Streaming text appears AFTER the chat list in the View output
//     (regression guard for "streaming visually disconnects from list"
//     concern flagged during C-rollout review)
//   * WindowSizeMsg invalidates the render cache
//   * Resize then re-render produces the right list dimensions
//   * AtBottom auto-scroll: at-bottom users follow new content,
//     scrolled-up users don't get yanked
//   * Scrollbar appears when content > viewport
//
// These match the manual-smoke checklist the user runs interactively.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

// newE2EModel builds a Model with N populated messages + a working
// renderCache so the cache-aware path runs. Mirrors newScrollTestModel
// but adds the cache (which scroll_test deliberately omits to isolate
// list behavior).
func newE2EModel(t *testing.T, termW, termH, numMessages int) *Model {
	t.Helper()
	ti := textarea.New()
	ti.SetWidth(termW - 8)
	cl := list.NewList()
	cl.SetSize(termW-2, termH-10)
	cl.SetMouseWheelDelta(1)
	m := &Model{
		gate:         permission.New(permission.ModeAuto),
		startTime:    time.Now(),
		input:        ti,
		chatList:     cl,
		width:        termW,
		height:       termH,
		firstRender:  false,
		showBanner:   false,
		renderCache:  newRenderCache(8, 100),
		stickyBottom: true,
	}
	base := time.Now()
	for i := 0; i < numMessages; i++ {
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   fmt.Sprintf("**msg %d** with markdown\n\n- item one\n- item two\n", i),
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
	return m
}

// drive sends a tea.Msg through Update, which mutates m in place
// (metis Update returns the same *Model after mutation).
func drive(t *testing.T, m *Model, msg tea.Msg) {
	t.Helper()
	updated, _ := m.Update(msg)
	if got, ok := updated.(*Model); !ok || got != m {
		t.Fatalf("Update should return same *Model; got %T", updated)
	}
}

func TestE2E_PgUpScrollsChatList(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	// Render once so dynamic-height + initial state populate.
	_ = m.View()
	m.chatList.ScrollToBottom()
	if !m.chatList.AtBottom() {
		t.Fatal("setup: chat list should start at bottom")
	}

	drive(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.chatList.AtBottom() {
		t.Error("after PgUp, chat list should not be at bottom")
	}
}

func TestE2E_PgDnReturnsToBottom(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToTop()

	// Repeatedly PgDn should eventually hit bottom.
	for i := 0; i < 50; i++ {
		drive(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
		if m.chatList.AtBottom() {
			return // success
		}
	}
	t.Error("after many PgDn presses, should reach bottom")
}

func TestE2E_HomeAndEnd(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToBottom()

	// Home: goes to top
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.chatList.AtBottom() {
		t.Error("after Home, should not be at bottom")
	}
	// Note: AtBottom=false doesn't strictly mean "at top" but
	// for 50 items in a 20-row viewport, top != bottom.

	// End: goes to bottom
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if !m.chatList.AtBottom() {
		t.Error("after End, should be at bottom")
	}
}

func TestE2E_HomeBlockedWhenInputHasContent(t *testing.T) {
	// Home/End with non-empty input should NOT scroll the list —
	// they belong to the textarea instead. Lock that contract.
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToBottom()
	m.input.SetValue("typing...")

	drive(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	if !m.chatList.AtBottom() {
		t.Error("Home with non-empty input should not move chat list")
	}
}

func TestE2E_MouseWheelUpScrolls(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToBottom()

	drive(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.chatList.AtBottom() {
		t.Error("WheelUp from bottom should scroll up")
	}
}

func TestE2E_StreamingAppearsAfterChatList(t *testing.T) {
	// Regression guard for "streaming visual position" worry: the
	// C rollout moved streaming/thinking text from inside the
	// virtualized list output to a tail rendered after it. This test
	// asserts streaming text appears AFTER the assistant messages in
	// the View() output — not before, not interleaved.
	m := newE2EModel(t, 80, 30, 5)
	m.streamingText = "STREAMING_TOKEN_MARKER"

	out := m.View().Content

	streamPos := strings.Index(out, "STREAMING_TOKEN_MARKER")
	if streamPos < 0 {
		t.Fatal("streaming text not found in View output")
	}

	// "msg 4" is the last (newest) assistant message — it should
	// appear earlier in the output than the streaming marker.
	lastMsgPos := strings.LastIndex(out, "msg 4")
	if lastMsgPos < 0 {
		t.Fatal("last list item ('msg 4') not found in View output")
	}
	if streamPos < lastMsgPos {
		t.Errorf("streaming text at pos %d should appear AFTER last list item at pos %d",
			streamPos, lastMsgPos)
	}
}

func TestE2E_ThinkingAppearsAfterChatList(t *testing.T) {
	// Same guard for thinking-deltas. Thinking and streaming render
	// in the same tail block; thinking should still come after the
	// list's content.
	m := newE2EModel(t, 80, 30, 5)
	m.thinkingText = "THINKING_TRACE_MARKER"

	out := m.View().Content

	thinkPos := strings.Index(out, "THINKING_TRACE_MARKER")
	if thinkPos < 0 {
		t.Fatal("thinking text not found in View output")
	}
	lastMsgPos := strings.LastIndex(out, "msg 4")
	if lastMsgPos < 0 {
		t.Fatal("last list item not found in View output")
	}
	if thinkPos < lastMsgPos {
		t.Errorf("thinking text at pos %d should appear AFTER last list item at pos %d",
			thinkPos, lastMsgPos)
	}
}

func TestE2E_WindowSizeInvalidatesCache(t *testing.T) {
	m := newE2EModel(t, 80, 30, 20)
	_ = m.View() // populate cache
	_, missBefore, _ := m.renderCache.Stats()
	if missBefore == 0 {
		t.Fatal("setup: first View should miss")
	}

	// All hits on second frame
	_ = m.View()
	_, missAfterSecondView, _ := m.renderCache.Stats()
	if missAfterSecondView != missBefore {
		t.Fatalf("baseline broken: second View should not miss")
	}

	// Resize: cache invalidates, third frame misses again.
	drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	_ = m.View()
	_, missAfterResize, _ := m.renderCache.Stats()
	if missAfterResize <= missAfterSecondView {
		t.Errorf("WindowSizeMsg should invalidate cache (miss before=%d after=%d)",
			missAfterSecondView, missAfterResize)
	}
}

func TestE2E_AutoScrollFollowsBottom(t *testing.T) {
	// User at bottom: append new content -> view follows.
	m := newE2EModel(t, 80, 30, 5)
	_ = m.View()
	m.chatList.ScrollToBottom()
	t.Logf("after first View+ScrollToBottom: AtBottom=%v len=%d height=%d totalLines=%d",
		m.chatList.AtBottom(), m.chatList.Len(), m.chatList.Height(), m.chatList.TotalLineCount())

	// Append more messages and re-render.
	base := time.Now()
	for i := 0; i < 10; i++ {
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   fmt.Sprintf("appended %d", i),
			Timestamp: base.Add(time.Duration(100+i) * time.Millisecond),
		})
	}
	t.Logf("after appending 10 messages (before View): list len=%d, m.messages=%d, AtBottom=%v",
		m.chatList.Len(), len(m.messages), m.chatList.AtBottom())
	_ = m.View()
	t.Logf("after second View: AtBottom=%v len=%d height=%d totalLines=%d",
		m.chatList.AtBottom(), m.chatList.Len(), m.chatList.Height(), m.chatList.TotalLineCount())
	if !m.chatList.AtBottom() {
		t.Error("at-bottom user: new content should auto-follow")
	}
}

func TestE2E_AutoScrollDoesNotYankScrolledUpUser(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToTop()
	if m.chatList.AtBottom() {
		t.Skip("can't test scrolled-up state when content fits viewport")
	}

	base := time.Now()
	for i := 0; i < 10; i++ {
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   fmt.Sprintf("appended %d", i),
			Timestamp: base.Add(time.Duration(1000+i) * time.Millisecond),
		})
	}
	_ = m.View()
	if m.chatList.AtBottom() {
		t.Error("scrolled-up user: should NOT auto-jump to bottom on new content")
	}
}

// TestE2E_ScrollbarPresentWhenContentExceedsViewport — historical name.
// Scrollbar was DISABLED on 2026-05-05 (feedback: deep-blue thumb was
// visually loud). The list still renders, just without the right-edge
// gutter. Test now asserts the OPPOSITE: those glyphs must NOT appear.
func TestE2E_ScrollbarPresentWhenContentExceedsViewport(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	out := m.View().Content

	if strings.ContainsRune(out, '█') {
		t.Error("scrollbar thumb █ must not render — disabled per 2026-05-05 feedback")
	}
}

func TestE2E_DynamicHeightShrinksForFewMessages(t *testing.T) {
	// 2 messages should NOT consume the full terminal height —
	// list dynamically sizes to content.
	m := newE2EModel(t, 80, 30, 2)
	_ = m.View()
	if m.chatList.Height() >= 20 {
		t.Errorf("with only 2 messages, list height=%d should be < 20 (terminal-chrome cap)",
			m.chatList.Height())
	}
}

func TestE2E_DynamicHeightCappedForManyMessages(t *testing.T) {
	// 100 messages should cap at terminal-chrome=20.
	m := newE2EModel(t, 80, 30, 100)
	_ = m.View()
	if m.chatList.Height() > 20 {
		t.Errorf("with 100 messages, list height=%d should cap at 20",
			m.chatList.Height())
	}
	if m.chatList.Height() < 5 {
		t.Errorf("list height %d should be at least 5", m.chatList.Height())
	}
}
