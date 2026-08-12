package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

func TestChatTimelineHasOneGapBetweenTopLevelItems(t *testing.T) {
	now := time.Now()
	items := []list.Item{
		&messageItem{msg: Message{Role: "user", Content: "first", Timestamp: now}},
		&toolEventItem{te: ToolEvent{
			Kind:      "result",
			ToolName:  "Bash",
			Input:     map[string]any{"command": "true"},
			StartTime: now.Add(time.Millisecond),
			Duration:  5 * time.Millisecond,
		}},
		&messageItem{msg: Message{Role: "assistant", Content: "second", Timestamp: now.Add(2 * time.Millisecond)}},
	}

	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = item.Render(100)
		if strings.HasPrefix(parts[i], "\n") || strings.HasSuffix(parts[i], "\n") {
			t.Fatalf("item %d manufactures outer whitespace: %q", i, parts[i])
		}
	}
	expected := strings.Join(parts, "\n\n")

	chat := list.NewList(items...)
	chat.SetGap(chatItemGap)
	chat.SetSize(100, strings.Count(expected, "\n")+1)
	if got := chat.Render(); got != expected {
		t.Fatalf("timeline rhythm mismatch\nwant: %q\n got: %q", expected, got)
	}

	tool := stripANSI(parts[1])
	if strings.Contains(tool, "\n\n") {
		t.Fatalf("tool invocation and result rows were split by a blank line: %q", tool)
	}
	toolLines := strings.Split(tool, "\n")
	if len(toolLines) != 2 || !strings.Contains(toolLines[0], "bash") || !strings.Contains(toolLines[1], glyphTreeLeaf) {
		t.Fatalf("tool should remain one compact two-row block: %q", tool)
	}
}
