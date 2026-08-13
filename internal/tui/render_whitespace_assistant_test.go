package tui

import "testing"

func TestWhitespaceOnlyAssistantRendersNoBullet(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"", "   ", "\n\n", "\r\n \t\n"} {
		msg := Message{Role: "assistant", Content: content}
		if got := renderMessage(msg, 80, false); got != "" {
			t.Errorf("rich assistant %q rendered %q, want empty", content, got)
		}
		if got := renderMessagePlain(msg, 80, false); got != "" {
			t.Errorf("plain assistant %q rendered %q, want empty", content, got)
		}
		if got := (&inProgressStreamingItem{text: content}).Render(80); got != "" {
			t.Errorf("streaming assistant %q rendered %q, want empty", content, got)
		}
	}
}

func TestIndentedAssistantContentStillRenders(t *testing.T) {
	t.Parallel()
	const content = "    indented code"
	msg := Message{Role: "assistant", Content: content}
	if got := renderMessage(msg, 80, false); got == "" {
		t.Fatal("rich renderer dropped authored indentation")
	}
	if got := (&inProgressStreamingItem{text: content}).Render(80); got == "" {
		t.Fatal("streaming renderer dropped authored indentation")
	}
}

func TestBuildChatItemsSkipsWhitespaceOnlyAssistant(t *testing.T) {
	t.Parallel()
	m := &Model{messages: []Message{
		{Role: "assistant", Content: "\n \t\n"},
		{Role: "assistant", Content: "visible"},
	}}
	items := m.buildChatItems()
	if len(items) != 1 {
		t.Fatalf("buildChatItems returned %d items, want only visible assistant", len(items))
	}
}
