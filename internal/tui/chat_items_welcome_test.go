package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestBuildChatSurfaceItems_ActiveChatKeepsWelcomeAsPrologue locks the Claude
// Code lifecycle: LogoHeader is the first child of Messages, so the same
// welcome card remains above the first prompt and scrolls with the transcript.
// The one-time "Type a message to start" hint must not survive submission.
func TestBuildChatSurfaceItems_ActiveChatKeepsWelcomeAsPrologue(t *testing.T) {
	m := &Model{
		model: "claude-opus-4-7",
		width: 120,
		gate:  permission.New(permission.ModeBypass),
		messages: []Message{
			{Role: "user", Content: "hi", Timestamp: time.Now()},
			{Role: "assistant", Content: "hello", Timestamp: time.Now().Add(time.Second)},
		},
	}

	items := m.buildChatSurfaceItems()
	if len(items) != 3 {
		t.Fatalf("expected welcome prologue plus 2 message items; got %d", len(items))
	}

	welcome, ok := items[0].(*staticItem)
	if !ok {
		t.Fatalf("first active-chat item should be the welcome prologue; got %T", items[0])
	}
	rendered := welcome.Render(120)
	if !strings.Contains(rendered, "metis") ||
		!strings.Contains(rendered, metisOwlGlyphLines[0]) {
		t.Errorf("active-chat prologue is missing the welcome identity: %q", rendered)
	}
	if strings.Contains(rendered, "Type a message to start") {
		t.Errorf("active-chat prologue retained the one-time start hint: %q", rendered)
	}

	firstMessage, ok := items[1].(*messageItem)
	if !ok || firstMessage.msg.Content != "hi" {
		t.Fatalf("first transcript message after welcome = %#v (%T), want user hi", items[1], items[1])
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
