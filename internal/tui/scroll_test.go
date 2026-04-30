package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestLongContent_ViewportClampsAndScrolls verifies the dynamic
// viewport sizing logic: with content much taller than the terminal,
// the viewport should clamp to terminal-minus-chrome and the scrollbar
// becomes active.
//
// The user's complaint was "我这个会话滚动多了也有问题" — long sessions
// scroll oddly. Lock the contract here so future tweaks to viewport
// sizing don't reintroduce the bug.
func TestLongContent_ViewportClampsAndScrolls(t *testing.T) {
	m := newScrollTestModel(80, 30)

	// Generate 200 lines worth of messages.
	for i := 0; i < 200; i++ {
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   "line " + strings.Repeat("x", 40),
			Timestamp: time.Now(),
		})
	}
	// Render once — View() does the dynamic resize + viewport.SetContent.
	out := m.View()
	if out == "" {
		t.Fatal("View() returned empty for non-empty transcript")
	}

	// Viewport height should be capped at terminal-10 (chrome reserve),
	// not grown to fit all 200 content lines. m.height is 30 so cap is 20.
	if m.viewport.Height > 20 {
		t.Errorf("viewport height = %d, want <= 20 (terminal 30 - chrome 10)", m.viewport.Height)
	}
	if m.viewport.Height < 5 {
		t.Errorf("viewport height = %d, want >= 5", m.viewport.Height)
	}

	// TotalLineCount should exceed Height — that's the precondition for
	// the scrollbar to appear.
	if m.viewport.TotalLineCount() <= m.viewport.Height {
		t.Fatalf("expected total lines (%d) > viewport height (%d)",
			m.viewport.TotalLineCount(), m.viewport.Height)
	}

	// Scrolling up should change the offset.
	startOffset := m.viewport.YOffset
	m.viewport.GotoTop()
	if m.viewport.YOffset == startOffset && startOffset != 0 {
		t.Error("GotoTop did not move offset")
	}
	m.viewport.GotoBottom()
	if !m.viewport.AtBottom() {
		t.Error("GotoBottom should land at bottom")
	}
}

// TestLongContent_ScrollbarRenders verifies that renderScrollbar
// produces visible track + thumb glyphs and that the thumb height is
// proportional to coverage.
func TestLongContent_ScrollbarRenders(t *testing.T) {
	vp := viewport.New(40, 10)
	vp.SetContent(strings.Repeat("hello\n", 100))

	view := vp.View()
	out := renderScrollbar(view, &vp)
	if !strings.ContainsRune(out, '│') {
		t.Error("scrollbar should include track glyph │")
	}
	if !strings.ContainsRune(out, '█') {
		t.Error("scrollbar should include thumb glyph █")
	}

	// Thumb count should be < height (since content is much taller).
	thumbCount := strings.Count(out, "█")
	if thumbCount >= vp.Height {
		t.Errorf("thumb count %d >= viewport height %d; thumb should be smaller for long content", thumbCount, vp.Height)
	}
	if thumbCount < 1 {
		t.Errorf("thumb count = 0; expected >=1")
	}
}

// TestLongContent_AutoScrollFollowsBottom verifies that when the user
// is at the bottom of the transcript, new content auto-follows to the
// bottom; when scrolled up, new content does NOT yank them back.
func TestLongContent_AutoScrollFollowsBottom(t *testing.T) {
	m := newScrollTestModel(80, 30)
	for i := 0; i < 50; i++ {
		m.messages = append(m.messages, Message{
			Role: "assistant", Content: "msg", Timestamp: time.Now(),
		})
	}
	m.View()
	m.viewport.GotoBottom()

	// Append more content while at bottom.
	for i := 0; i < 50; i++ {
		m.messages = append(m.messages, Message{
			Role: "assistant", Content: "more", Timestamp: time.Now(),
		})
	}
	m.View()
	if !m.viewport.AtBottom() {
		t.Error("at-bottom user should auto-follow new content")
	}

	// Scroll up, append more, expect the offset to stay near where the
	// user left it (NOT auto-jump to bottom).
	m.viewport.GotoTop()
	topOffset := m.viewport.YOffset
	for i := 0; i < 50; i++ {
		m.messages = append(m.messages, Message{
			Role: "assistant", Content: "even more", Timestamp: time.Now(),
		})
	}
	m.View()
	if m.viewport.AtBottom() {
		t.Error("scrolled-up user should NOT auto-follow new content")
	}
	if m.viewport.YOffset != topOffset {
		// Some drift is OK if SetContent reflows, but a jump to bottom
		// would mean YOffset >> topOffset — flag that specifically.
		if m.viewport.YOffset > topOffset+10 {
			t.Errorf("scroll position drifted too far: was %d, now %d", topOffset, m.viewport.YOffset)
		}
	}
}

// newScrollTestModel builds a Model with sized viewport + textarea
// suitable for the scroll tests above. We don't need agent.Loop or
// the full chat plumbing — only what View() touches.
func newScrollTestModel(termW, termH int) *Model {
	ti := textarea.New()
	ti.SetWidth(termW - 8)
	vp := viewport.New(termW-2, termH-10)
	vp.MouseWheelEnabled = true
	vp.MouseWheelDelta = 1
	return &Model{
		gate:        permission.New(permission.ModeAuto),
		startTime:   time.Now(),
		input:       ti,
		viewport:    vp,
		width:       termW,
		height:      termH,
		firstRender: false,
		showBanner:  false,
	}
}
