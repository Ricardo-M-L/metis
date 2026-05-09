package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestBuildChatItems_WelcomeBannerFirst confirms the welcome card is
// prepended as the first item in the chat list once messages exist —
// the user-visible behavior is "the brand strip stays at the top of
// the transcript and scrolls naturally with the conversation" instead
// of disappearing the moment the user types.
func TestBuildChatItems_WelcomeBannerFirst(t *testing.T) {
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
	if len(items) != 3 {
		t.Fatalf("expected 3 items (1 banner + 2 messages); got %d", len(items))
	}

	first, ok := items[0].(*staticItem)
	if !ok {
		t.Fatalf("first item should be the welcome banner staticItem; got %T", items[0])
	}
	rendered := first.Render(120)
	if rendered == "" {
		t.Fatal("welcome banner item should not render empty")
	}
	// "metis" branding appears in the banner title.
	if !strings.Contains(rendered, "metis") {
		t.Errorf("welcome banner should contain 'metis'; got %q", rendered)
	}
	// The "Type a message to start" hint is for the empty-state path —
	// once we have messages, the active-chat banner drops it.
	if strings.Contains(rendered, "Type a message to start") {
		t.Error("active-chat welcome banner should NOT show 'Type a message to start' hint")
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
