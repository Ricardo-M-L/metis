package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestBuildChatItems_ActiveChatDoesNotRepeatWelcome confirms the large fresh
// session card is not retained as a scrollable transcript item. Active chat
// already has a compact sticky header; rendering both wastes most of a short
// terminal and gives fullscreen renderers a large shared block to hard-scroll
// during the structurally different welcome-to-chat transition.
func TestBuildChatItems_ActiveChatDoesNotRepeatWelcome(t *testing.T) {
	m := &Model{
		model: "claude-opus-4-7",
		width: 120,
		gate:  permission.New(permission.ModeBypass),
		messages: []Message{
			{Role: "user", Content: "hi", Timestamp: time.Now()},
			{Role: "assistant", Content: "hello", Timestamp: time.Now().Add(time.Second)},
		},
	}

	items := m.buildChatItems()
	if len(items) != 2 {
		t.Fatalf("expected exactly 2 message items; got %d", len(items))
	}

	first, ok := items[0].(*messageItem)
	if !ok {
		t.Fatalf("first active-chat item should be a message, not welcome chrome; got %T", items[0])
	}
	rendered := first.Render(120)
	if strings.Contains(rendered, "Type a message to start") ||
		strings.Contains(rendered, metisOwlGlyphLines[0]) {
		t.Errorf("active-chat item unexpectedly contains welcome chrome: %q", rendered)
	}
}

// TestRenderWelcomeBanner_RetainsHintInEmptyState verifies the
// backward-compatible behaviour: when called via the public method
// (the empty-state path uses this), the hint is still emitted.
func TestRenderWelcomeBanner_RetainsHintInEmptyState(t *testing.T) {
	m := &Model{
		model: "claude-opus-4-7",
		width: 120,
		gate:  permission.New(permission.ModeBypass),
	}
	got := m.renderWelcomeBanner()
	if !strings.Contains(got, "Type a message to start") {
		t.Errorf("empty-state welcome banner must keep the hint; got %q", got)
	}
}
