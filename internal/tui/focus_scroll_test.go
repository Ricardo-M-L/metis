package tui

// focus_scroll_test.go — pins claude-code-parity "switch back to this
// tab → see the latest message" behaviour added 2026-05-17 in response
// to user screenshot 37 ("我发现每次我切换 claude code 都会回到 claude
// code 当前会话最底层展示, 但是我的 metis 却不会"). xterm DECSET 1004
// focus reporting → bubbletea v2 FocusMsg → ScrollToBottom + sticky-on.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestFocusMsg_ScrollsToBottom — when the terminal regains focus, the
// Update handler must snap the chat list back to the latest content.
// Verified by scrolling up first, sending FocusMsg, then asserting
// AtBottom() is true.
func TestFocusMsg_ScrollsToBottom(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50) // 50 messages → forces a scrollable list
	_ = m.View()                    // initialize the chat list layout

	// Scroll up so we're NOT at the bottom — simulates the user
	// having paged back to read history before tab-switching away.
	m.chatList.ScrollToTop()
	if m.chatList.AtBottom() {
		t.Fatal("precondition: chat list should NOT be at bottom after ScrollToTop")
	}
	m.stickyBottom = false

	updated, _ := m.Update(tea.FocusMsg{})
	if got, ok := updated.(*Model); !ok || got != m {
		t.Fatalf("Update should return same *Model; got %T", updated)
	}

	if !m.chatList.AtBottom() {
		t.Errorf("FocusMsg should snap chat list to bottom; AtBottom() = false")
	}
	if !m.stickyBottom {
		t.Errorf("FocusMsg should re-arm stickyBottom for follow-up streaming output; stickyBottom = false")
	}
}

// TestFocusMsg_SuppressedDuringCopyMode — copy mode flips alt-screen
// off so the user can use native terminal scrollback. Forcing the
// chat list to ScrollToBottom there would yank the view out from
// under the user mid-selection.
func TestFocusMsg_SuppressedDuringCopyMode(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToTop()
	m.stickyBottom = false
	m.copyMode = true

	m.Update(tea.FocusMsg{})

	if m.chatList.AtBottom() {
		t.Errorf("FocusMsg under copy mode should NOT scroll; AtBottom() = true")
	}
}

// TestFocusMsg_SuppressedDuringPermissionPrompt — permission prompt
// owns the keyboard / focus; the chat list isn't the user's target,
// so a re-snap during a confirmation dialog is just noise on a screen
// they're trying to read.
func TestFocusMsg_SuppressedDuringPermissionPrompt(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToTop()
	m.stickyBottom = false
	m.permActive = true

	m.Update(tea.FocusMsg{})

	if m.chatList.AtBottom() {
		t.Errorf("FocusMsg during permActive should NOT scroll; AtBottom() = true")
	}
}

// TestBlurMsg_NoOp — losing focus must not drop stickyBottom or
// touch scroll. The user might pop back in seconds and expect to see
// the latest content (FocusMsg handles that on return).
func TestBlurMsg_NoOp(t *testing.T) {
	m := newE2EModel(t, 80, 30, 50)
	_ = m.View()
	m.chatList.ScrollToBottom()
	m.stickyBottom = true

	m.Update(tea.BlurMsg{})

	if !m.stickyBottom {
		t.Errorf("BlurMsg should not clear stickyBottom")
	}
	if !m.chatList.AtBottom() {
		t.Errorf("BlurMsg should not change scroll position")
	}
}

// TestChatView_EnablesReportFocus — sanity check on the renderer
// side: the chat surface's tea.View must opt into ReportFocus so
// bubbletea v2 actually emits the xterm 1004 enable sequence and
// then dispatches FocusMsg / BlurMsg on the wire.
func TestChatView_EnablesReportFocus(t *testing.T) {
	m := newE2EModel(t, 120, 30, 5)
	v := m.View()
	if !v.ReportFocus {
		t.Errorf("chat tea.View should set ReportFocus=true; got false (xterm 1004 not enabled, FocusMsg won't arrive)")
	}
}
