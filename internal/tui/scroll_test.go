package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

// TestLongContent_ChatListClampsAndScrolls verifies the dynamic
// chat-list sizing logic: with content much taller than the terminal,
// the list height should clamp to terminal-minus-chrome and the
// scrollbar becomes active.
//
// The user's complaint was "我这个会话滚动多了也有问题" — long sessions
// scroll oddly. Lock the contract here so future tweaks to list
// sizing don't reintroduce the bug.
func TestLongContent_ChatListClampsAndScrolls(t *testing.T) {
	m := newScrollTestModel(80, 30)

	// Generate 200 lines worth of messages.
	for i := 0; i < 200; i++ {
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   "line " + strings.Repeat("x", 40),
			Timestamp: time.Now(),
		})
	}
	// Render once — View() does the dynamic resize + chatList.SetItemsKeepScroll.
	out := m.View().Content
	if out == "" {
		t.Fatal("View() returned empty for non-empty transcript")
	}

	// chatList height should be capped at terminal-10 (chrome reserve),
	// not grown to fit all 200 content lines. m.height is 30 so cap is 20.
	if m.chatList.Height() > 20 {
		t.Errorf("chatList height = %d, want <= 20 (terminal 30 - chrome 10)", m.chatList.Height())
	}
	if m.chatList.Height() < 5 {
		t.Errorf("chatList height = %d, want >= 5", m.chatList.Height())
	}

	// TotalLineCount should exceed Height — that's the precondition for
	// the scrollbar to appear.
	if m.chatList.TotalLineCount() <= m.chatList.Height() {
		t.Fatalf("expected total lines (%d) > chatList height (%d)",
			m.chatList.TotalLineCount(), m.chatList.Height())
	}

	// Scrolling up should change the offset.
	m.chatList.ScrollToBottom()
	bottomBefore := m.chatList.AtBottom()
	if !bottomBefore {
		t.Error("ScrollToBottom should land at bottom")
	}
	m.chatList.ScrollToTop()
	if m.chatList.AtBottom() {
		t.Error("after ScrollToTop, AtBottom should be false")
	}
}

// TestLongConversation_FrameFitsPhysicalTerminal locks the invariant the
// bubbletea renderer relies on: one logical row in View.Content must fit on
// one physical terminal row, and the full frame must fit inside the terminal.
// If either bound is exceeded, iTerm2/tmux can auto-wrap while bubbletea still
// counts newline-delimited rows, leaving old-frame fragments in the middle of
// long conversations.
func TestLongConversation_FrameFitsPhysicalTerminal(t *testing.T) {
	const termW, termH = 80, 24
	m := newScrollTestModel(termW, termH)
	base := time.Now()
	for i := 0; i < 16; i++ {
		m.messages = append(m.messages,
			Message{
				Role:      "user",
				Content:   "这是第几轮问题？请比较 loop 工程和 graph 工程在长任务中的差异。",
				Timestamp: base.Add(time.Duration(i*2) * time.Millisecond),
			},
			Message{
				Role:      "assistant",
				Content:   "Loop 根据模型反馈动态推进，Graph 用显式节点和边约束控制流。任务轮数增加时，两者仍应保持稳定排版。",
				Timestamp: base.Add(time.Duration(i*2+1) * time.Millisecond),
			},
		)
	}
	// Streaming deltas can arrive as one long paragraph before the final
	// markdown renderer gets a chance to wrap them.
	m.streamingText = strings.Repeat("streaming-response-without-newline-", 8)

	view := m.View().Content
	rows := strings.Split(view, "\n")
	if got := len(rows); got > termH {
		t.Errorf("frame has %d logical rows, terminal height is %d", got, termH)
	}
	for i, row := range rows {
		// Keep one cell free at the right margin. Several terminals enter
		// pending-wrap state after writing the final column, so width==termW
		// is not a safe single-row frame even though it looks exact on paper.
		if got := ansi.StringWidth(row); got >= termW {
			t.Errorf("row %d uses %d cells, want < %d: %q", i, got, termW, ansi.Strip(row))
		}
	}
}

// TestLongContent_ScrollbarRenders verifies that renderScrollbar
// produces visible track + thumb glyphs and that the thumb height is
// proportional to coverage.
func TestLongContent_ScrollbarRenders(t *testing.T) {
	cl := list.NewList()
	cl.SetSize(40, 10)
	// Fill with 30 single-line items → total ≈ 30 lines, viewport 10.
	items := make([]list.Item, 30)
	for i := range items {
		items[i] = &fakeScrollItem{line: "hello"}
	}
	cl.SetItems(items...)

	view := cl.Render()
	out := renderScrollbar(view, cl)
	if !strings.ContainsRune(out, '│') {
		t.Error("scrollbar should include track glyph │")
	}
	if !strings.ContainsRune(out, '█') {
		t.Error("scrollbar should include thumb glyph █")
	}

	// Thumb count should be < height (since content is much taller).
	thumbCount := strings.Count(out, "█")
	if thumbCount >= cl.Height() {
		t.Errorf("thumb count %d >= chatList height %d; thumb should be smaller for long content", thumbCount, cl.Height())
	}
	if thumbCount < 1 {
		t.Errorf("thumb count = 0; expected >=1")
	}
}

// TestScrollbar_AlignsToRightEdgeWithMixedLineWidths verifies the bar
// forms a straight column even when the chat-list view has lines of
// uneven visual width. Previously renderScrollbar appended " │"/" █"
// directly to each line — short lines pushed the gutter inward and the
// bar zig-zagged. Now we pad each line to chatList.Width() before
// appending the glyph, so every annotated row has visual width = Width + 2.
func TestScrollbar_AlignsToRightEdgeWithMixedLineWidths(t *testing.T) {
	const vpWidth, vpHeight = 40, 12
	cl := list.NewList()
	cl.SetSize(vpWidth, vpHeight)
	// Mix short and long lines so width irregularities are visible.
	mixed := []string{
		"short",
		strings.Repeat("x", 30),
		"",
		"medium width line",
		strings.Repeat("y", 20),
		strings.Repeat("z", 35),
		"a",
		strings.Repeat("w", 25),
		"end",
		"",
		"tail",
		strings.Repeat("q", 38),
	}
	items := make([]list.Item, len(mixed))
	for i, line := range mixed {
		items[i] = &fakeScrollItem{line: line}
	}
	cl.SetItems(items...)

	view := cl.Render()
	out := renderScrollbar(view, cl)
	lines := strings.Split(out, "\n")

	// Each annotated row should have visual width == vp.Width + 2:
	//   vp.Width content padding + 1 gap space + 1 glyph cell.
	want := vpWidth + 2
	for i := 0; i < vpHeight && i < len(lines); i++ {
		if got := lipgloss.Width(lines[i]); got != want {
			t.Errorf("line %d visual width = %d, want %d (line=%q)", i, got, want, lines[i])
		}
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
	m.chatList.ScrollToBottom()

	// Append more content while at bottom — View() must keep us pinned.
	for i := 0; i < 50; i++ {
		m.messages = append(m.messages, Message{
			Role: "assistant", Content: "more", Timestamp: time.Now(),
		})
	}
	m.View()
	if !m.chatList.AtBottom() {
		t.Error("at-bottom user should auto-follow new content")
	}

	// Scroll up, append more, expect AtBottom=false (NOT auto-jumped).
	m.chatList.ScrollToTop()
	for i := 0; i < 50; i++ {
		m.messages = append(m.messages, Message{
			Role: "assistant", Content: "even more", Timestamp: time.Now(),
		})
	}
	m.View()
	if m.chatList.AtBottom() {
		t.Error("scrolled-up user should NOT auto-follow new content")
	}
}

// fakeScrollItem is a minimal Item used by the scroll tests. Distinct
// from list/list_test.go's fakeItem because that one lives in the list
// package and isn't importable from the tui package.
type fakeScrollItem struct {
	line string
}

func (f *fakeScrollItem) Render(width int) string {
	return f.line
}

// newScrollTestModel builds a Model with sized chatList + textarea
// suitable for the scroll tests above. We don't need agent.Loop or
// the full chat plumbing — only what View() touches.
func newScrollTestModel(termW, termH int) *Model {
	ti := textarea.New()
	ti.SetWidth(termW - 8)
	cl := list.NewList()
	cl.SetSize(termW-2, termH-10)
	cl.SetMouseWheelDelta(1)
	return &Model{
		gate:         permission.New(permission.ModeAcceptEdits),
		startTime:    time.Now(),
		input:        ti,
		chatList:     cl,
		width:        termW,
		height:       termH,
		firstRender:  false,
		showBanner:   false,
		stickyBottom: true,
	}
}
