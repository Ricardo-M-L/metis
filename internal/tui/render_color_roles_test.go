package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/themes"
)

func TestAssistantColorRolesAcrossRenderModes(t *testing.T) {
	previous := themes.Current().Name
	t.Cleanup(func() { themes.SwitchTheme(previous) })
	const content = "正文保持主题颜色"
	for _, theme := range themes.ThemeNames() {
		t.Run(theme, func(t *testing.T) {
			themes.SwitchTheme(theme)
			green := strings.Split(lipgloss.NewStyle().Foreground(accentGreen).Render("sample"), "sample")[0]
			for name, out := range map[string]string{
				"plain":   renderMessage(Message{Role: "assistant", Content: content}, 80, false),
				"minimal": renderMessagePlain(Message{Role: "assistant", Content: content}, 80, false),
				"stream":  (&inProgressStreamingItem{text: content}).Render(80),
			} {
				if !strings.Contains(out, styleText.Render(content)) || !strings.Contains(out, styleAccent.Render(glyphBullet+" ")) {
					t.Errorf("%s: body and role marker lost their separate colors: %q", name, out)
				}
				if green != "" && strings.Contains(out, green) {
					t.Errorf("%s: assistant reply inherited a success color: %q", name, out)
				}
			}
			markdown := renderMessage(Message{Role: "assistant", Content: "正文 **重点** 与 `identifier`，[链接](https://example.com)。"}, 80, false)
			if !strings.Contains(markdown, styleAccent.Render(" identifier ")) || (green != "" && strings.Contains(markdown, green)) {
				t.Errorf("Markdown emphasis is not separated from success: %q", markdown)
			}
			tool := renderToolEventAtWidth(ToolEvent{Kind: "end", ToolName: "Bash", Input: map[string]any{"command": "true"}, Duration: time.Millisecond}, false, 80)
			if !strings.Contains(tool, styleSuccess.Render("  "+glyphBullet+" ")) {
				t.Errorf("successful tool lost its success marker: %q", tool)
			}
		})
	}
}

func TestLeadingListsHaveOnlyTheirOwnMarker(t *testing.T) {
	for _, content := range []string{"- 第一项\n- 第二项", "\n\n* 第一项\n* 第二项", "1. 第一项\n2. 第二项", "+ 第一项", "  - 第一项"} {
		out := stripANSI(renderMessage(Message{Role: "assistant", Content: content}, 40, false))
		if strings.Contains(out, glyphBullet) || !strings.Contains(out, "第一项") {
			t.Errorf("leading list gained an extra role bullet or lost content: %q", out)
		}
		if !strings.HasPrefix(out, "    ") {
			t.Errorf("list lost the message gutter: %q", out)
		}
	}
	for _, content := range []string{"普通说明\n\n- 第一项", "```text\n- literal\n```", "    - indented code", "> - quoted list", "\\- escaped marker", "---"} {
		out := stripANSI(renderMessage(Message{Role: "assistant", Content: content}, 40, false))
		if !strings.HasPrefix(out, "  "+glyphBullet+" ") {
			t.Errorf("non-list opening lost its role marker: %q", out)
		}
	}
	minimal := stripANSI(renderMessagePlain(Message{Role: "assistant", Content: "- literal marker"}, 40, false))
	if !strings.HasPrefix(minimal, "  "+glyphBullet+" - literal marker") {
		t.Fatalf("minimal mode interpreted Markdown: %q", minimal)
	}
}
