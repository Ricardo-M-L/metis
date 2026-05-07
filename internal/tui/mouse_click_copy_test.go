package tui

// Regression tests for click-to-copy. User feedback 2026-05-07:
// "metis 得要鼠标点击复制" — the previous keyboard-only path
// (Ctrl+Y / Ctrl+Shift+Y / Ctrl+S) had low discoverability and no way
// to grab a single mid-transcript row without scrolling-then-yank.
//
// Tests cover: (a) left click on a chat row appends an info line and
// (we approximate) writes to clipboard; (b) clicks during permission
// dialog / copy mode / overlays are no-ops so they don't fight whatever
// owns the keyboard; (c) clicks on header / chrome are no-ops.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func makeChatTestModel(t *testing.T) *Model {
	t.Helper()
	m := newSlashTestModel(t)
	m.height = 30
	m.width = 100
	// Force a few transcript rows so chatList has visible items at
	// known y positions. WindowSizeMsg sizes the list; pump one
	// through Update so the layout is consistent with prod.
	m.messages = append(m.messages,
		Message{Role: "user", Content: "first user line", Timestamp: time.Now()},
		Message{Role: "assistant", Content: "assistant reply with some content", Timestamp: time.Now()},
		Message{Role: "user", Content: "second user line", Timestamp: time.Now()},
	)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// View() runs SetItems + SetSize for the list; trigger one frame
	// so ItemAtY has fresh layout.
	_ = m.View()
	return m
}

// TestClickCopy_ChatRowYanksToClipboard — a complete left button
// down→up sequence on a chat row must append an "(copied N chars …)"
// info row. Single-click row-copy is deferred to MouseReleaseMsg now
// (was on MouseClickMsg) so drag-to-select can win priority — see
// tui_update.go's three-way release branch.
func TestClickCopy_ChatRowYanksToClipboard(t *testing.T) {
	m := makeChatTestModel(t)
	preCount := len(m.messages)

	// y=2 is the first chat list row (header y=0, separator y=1).
	m.Update(tea.MouseClickMsg{X: 5, Y: 2, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 5, Y: 2, Button: tea.MouseLeft})

	if len(m.messages) <= preCount {
		t.Fatalf("expected an info row appended; messages went %d → %d",
			preCount, len(m.messages))
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "info" {
		t.Errorf("appended row should be role=info; got %q", last.Role)
	}
	if !strings.Contains(last.Content, "copied") {
		t.Errorf("info content should mention 'copied'; got %q", last.Content)
	}
}

// TestClickCopy_NonLeftButtonIgnored — middle/right clicks must NOT
// trigger copy. (Right-click is conventionally context menu; we leave
// it free for later.)
func TestClickCopy_NonLeftButtonIgnored(t *testing.T) {
	m := makeChatTestModel(t)
	preCount := len(m.messages)
	m.Update(tea.MouseClickMsg{X: 5, Y: 2, Button: tea.MouseRight})
	m.Update(tea.MouseReleaseMsg{X: 5, Y: 2, Button: tea.MouseRight})
	if len(m.messages) != preCount {
		t.Errorf("right-click should be a no-op; messages changed")
	}
}

// TestClickCopy_PermissionActiveBlocks — when a permission prompt is
// up the keyboard and mouse belong to that dialog; a stray click must
// not yank a chat row out from under it.
func TestClickCopy_PermissionActiveBlocks(t *testing.T) {
	m := makeChatTestModel(t)
	m.permActive = true
	m.permChoices = []permChoice{{Label: "Yes", Key: "y"}}
	m.permReply = make(chan agent.PermissionDecision, 1)
	preCount := len(m.messages)
	m.Update(tea.MouseClickMsg{X: 5, Y: 2, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 5, Y: 2, Button: tea.MouseLeft})
	if len(m.messages) != preCount {
		t.Errorf("click during permission prompt must not trigger copy")
	}
}

// TestClickCopy_HeaderClickIgnored — clicks on y=0 (brand) or y=1
// (separator) are above the chat list; do nothing.
func TestClickCopy_HeaderClickIgnored(t *testing.T) {
	m := makeChatTestModel(t)
	preCount := len(m.messages)
	m.Update(tea.MouseClickMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	m.Update(tea.MouseClickMsg{X: 5, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 5, Y: 1, Button: tea.MouseLeft})
	if len(m.messages) != preCount {
		t.Errorf("clicks on header rows must not yank; messages changed")
	}
}

// TestClickCopy_BottomChromeIgnored — clicks far below the chat list
// land on the input/hints/status chrome; the textarea owns those rows
// so click-to-copy must skip.
func TestClickCopy_BottomChromeIgnored(t *testing.T) {
	m := makeChatTestModel(t)
	preCount := len(m.messages)
	// y way past the chat region (height=30, chat region tops out
	// around y=20-22 once chrome subtracts).
	m.Update(tea.MouseClickMsg{X: 5, Y: 28, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 5, Y: 28, Button: tea.MouseLeft})
	if len(m.messages) != preCount {
		t.Errorf("click on chrome row must not yank; messages changed")
	}
}
