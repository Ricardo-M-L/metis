//go:build dump

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/themes"
)

// Fixed offline examples rendered by the production message/tool components.
// No shell commands or model requests are executed. The same fixture can run
// against v0.4.105 to provide an honest before/after capture.
func TestTranscriptFollowupVisualDump(t *testing.T) {
	previous := themes.Current().Name
	themes.SwitchTheme("dark")
	t.Cleanup(func() { themes.SwitchTheme(previous) })
	const width = 96
	write := func(name, output string) {
		t.Helper()
		for _, line := range strings.Split(output, "\n") {
			if xansi.StringWidth(line) > width {
				t.Fatalf("%s exceeds viewport: %q", name, stripANSI(line))
			}
		}
		dir := os.Getenv("METIS_FOLLOWUP_PREVIEW_DIR")
		if dir == "" {
			t.Fatal("METIS_FOLLOWUP_PREVIEW_DIR is required")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(output+"\x1b[0m"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	heading := func(text string) string { return styleDim.Render(text) + "\n" }
	var prose strings.Builder
	prose.WriteString(heading("Metis · 正文与配色 · 离线固定样例"))
	prose.WriteString("\n" + heading("生成中"))
	const reply = "第一行：颜色一致。\n第二行：起点对齐。"
	prose.WriteString((&inProgressStreamingItem{text: reply}).Render(width))
	prose.WriteString("\n" + heading("生成完成"))
	prose.WriteString(renderMessage(Message{Role: "assistant", Content: reply}, width, false))
	prose.WriteString(heading("普通回复与强调"))
	prose.WriteString(renderMessage(Message{Role: "assistant", Content: "你好，Ricardo！有什么我可以帮你的吗？"}, width, false))
	prose.WriteString(renderMessage(Message{Role: "assistant", Content: "当前模型为 **gpt-6.1-terra**，仓库为 `metis`。"}, width, false))
	prose.WriteString(heading("列表开头只保留列表标记"))
	prose.WriteString(renderMessage(Message{Role: "assistant", Content: "- 正文保持易读，`行内代码` 使用强调色。\n- 生成前后保持相同的正文起点。"}, width, false))
	write("prose.ansi", prose.String())

	var tools strings.Builder
	tools.WriteString(heading("Metis · 工具层级与选中态 · 离线固定样例"))
	tools.WriteString("\n" + heading("子代理工具：命令与结果沿用同一层级"))
	tools.WriteString(renderToolEventAtWidth(ToolEvent{Kind: "end", ToolName: "Agent", Input: map[string]any{"description": "核对排版"}, Duration: time.Second}, false, width))
	tools.WriteString(renderToolEventAtWidth(ToolEvent{Kind: "end", ToolName: "sub: Bash", SubAgentParentID: "parent", Input: map[string]any{"command": "python3 check-layout.py", "description": "检查子工具缩进"}, Duration: 120 * time.Millisecond, Output: "第一行：子工具结果完整保留。\n第二行：输出正文与命令在同一列。"}, true, width))

	m := newSlashTestModel(t)
	m.messages = nil
	m.width = width + 2
	m.toolEvents = []ToolEvent{
		{ID: "empty", Kind: "end", ToolName: "Bash", StartTime: time.Unix(1, 0), Input: map[string]any{"command": "git diff --quiet", "description": "检查是否有未提交的改动"}, Duration: 65 * time.Millisecond},
		{ID: "other", Kind: "end", ToolName: "Bash", StartTime: time.Unix(2, 0), Input: map[string]any{"command": "git status --short", "description": "检查工作区"}, Duration: 20 * time.Millisecond},
	}
	capture := func(label string) {
		t.Helper()
		tools.WriteString("\n" + heading(label))
		for _, item := range m.buildChatItems() {
			if tool, ok := item.(*toolEventItem); ok && tool.te.ID == m.selectedToolID {
				tools.WriteString(strings.TrimRight(item.Render(width), "\n") + "\n")
				return
			}
		}
		t.Fatal("selected tool missing")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	capture("选中工具：收起")
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	capture("同一工具：展开")
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.selectedToolID != "empty" || m.expandedToolID != "" || len(m.messages) != 0 {
		t.Fatal("inspection changed selection or conversation history")
	}
	tools.WriteString("\n" + heading("示例命令未执行；选择与展开使用真实按键处理。"))
	write("tools.ansi", tools.String())
}
