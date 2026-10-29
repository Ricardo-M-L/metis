//go:build dump

// To preview the new chat-surface rendering visually, run:
//
//	go test -tags dump -v -run TestVisualDump ./internal/tui/
//
// Build tag keeps this out of `go test ./...` so the regular suite stays
// silent. Use the interactive CLI for a true-color terminal preview.

package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// TestToolPolishRoundTwoVisualDump renders two short, reproducible terminal
// pages through production renderers and keyboard handlers. All data is
// synthetic: no shell command, provider request or user session is involved.
// METIS_TOOL_ROUND_TWO_PREVIEW_DIR saves round-two-a.ansi and round-two-b.ansi.
func TestToolPolishRoundTwoVisualDump(t *testing.T) {
	const width = 100
	t.Run("prose-and-long-output", func(t *testing.T) {
		var preview strings.Builder
		preview.WriteString("Metis 第二轮 A · 离线模拟，未执行任何命令\n\n")
		preview.WriteString("[1] 展开长输出：超过 120 字符，检查最后的 END-OF-LONG-OUTPUT\n")
		longOutput := "BEGIN-LONG-OUTPUT " + strings.Repeat("0123456789 ", 16) + "END-OF-LONG-OUTPUT"
		preview.WriteString(renderToolEventAtWidth(ToolEvent{
			Kind: "result", ToolName: "Bash", Input: map[string]any{"command": "python3 inspect.py"},
			Output: longOutput, Duration: 388 * time.Millisecond,
		}, true, width))
		preview.WriteString("[2] 普通文本\n")
		preview.WriteString(renderMessage(Message{Role: "assistant", Content: "已核对仓库 cordis-di，当前没有执行错误。"}, width, false))
		preview.WriteString("[2] 同一句 Markdown：仅仓库名使用行内代码\n")
		preview.WriteString(renderMessage(Message{Role: "assistant", Content: "已核对仓库 `cordis-di`，当前没有执行错误。"}, width, false))
		preview.WriteString("[3] 中文列表续行对齐；完整 UUID 移到下一行\n")
		const id = "fb171c30-7d3f-4192-84a0-2b252c8866e0"
		preview.WriteString(renderMessage(Message{Role: "assistant", Content: "- 本机历史已核对，本轮检索没有发现改名讨论；用于进一步定位的完整会话编号为 `" + id + "`。\n" +
			"- 仓库记录已核对，提交说明保留原文；这里需要完整阅读代码片段 `feat!: rename crates to cordis-di-*`。",
		}, width, false))
		out := preview.String()
		for _, want := range []string{"END-OF-LONG-OUTPUT", id} {
			if !strings.Contains(stripANSI(out), want) {
				t.Fatalf("production renderer lost complete evidence %q:\n%s", want, stripANSI(out))
			}
		}
		columnOf := func(needle string) int {
			for _, line := range strings.Split(stripANSI(out), "\n") {
				if index := strings.Index(line, needle); index >= 0 {
					return xansi.StringWidth(line[:index])
				}
			}
			return -1
		}
		if textCol, continuationCol := columnOf("本机历史"), columnOf(id); textCol < 0 || continuationCol < textCol {
			t.Fatalf("list continuation is left of its text column: text=%d UUID=%d:\n%s", textCol, continuationCol, stripANSI(out))
		}
		writeToolRoundTwoPreview(t, "round-two-a.ansi", out, width)
	})

	t.Run("interaction", func(t *testing.T) {
		m := newSlashTestModel(t)
		m.messages = nil
		m.width = width + 2
		m.toolEvents = []ToolEvent{
			{ID: "history-fixture", Kind: "end", ToolName: "Bash", StartTime: time.Unix(1, 0),
				Input:    map[string]any{"command": "python3 inspect-history.py", "description": "检查历史会话中的改名讨论"},
				Duration: 65 * time.Millisecond},
			{ID: "commits-fixture", Kind: "end", ToolName: "Bash", StartTime: time.Unix(2, 0),
				Input:    map[string]any{"command": "git log --oneline -5", "description": "核对仓库中的改名提交"},
				Duration: 20 * time.Millisecond},
		}
		var preview strings.Builder
		preview.WriteString("Metis 第二轮 B · 离线模拟，未执行任何命令\n")
		preview.WriteString("[4] 两个工具；逐步摘录选中的工具块，全部经过真实按键处理与渲染\n\n")
		capture := func(label string) {
			t.Helper()
			preview.WriteString(label + "\n")
			for _, item := range m.buildChatItems() {
				if tool, ok := item.(*toolEventItem); ok && tool.te.ID == m.selectedToolID {
					preview.WriteString(strings.TrimRight(item.Render(width), "\n"))
					preview.WriteString("\n\n")
					return
				}
			}
			t.Fatal("selected tool is missing from production chat items")
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
		if m.selectedToolID != "history-fixture" || m.expandedToolID != "" {
			t.Fatalf("Alt+Up did not select the previous collapsed tool: selected=%q expanded=%q", m.selectedToolID, m.expandedToolID)
		}
		capture("[4a] Alt+↑：选中前一个工具（1/2），保持收起")
		m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
		if m.expandedToolID != "history-fixture" {
			t.Fatalf("Ctrl+O expanded a different tool: %q", m.expandedToolID)
		}
		capture("[4b] Ctrl+O：展开同一工具，查看真实命令")
		m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
		if m.selectedToolID != "history-fixture" || m.expandedToolID != "" {
			t.Fatalf("second Ctrl+O switched targets instead of collapsing: selected=%q expanded=%q", m.selectedToolID, m.expandedToolID)
		}
		capture("[4c] 再按 Ctrl+O：收起同一工具，仍停留在 1/2")
		if len(m.messages) != 0 {
			t.Fatalf("tool inspection added %d messages to the conversation", len(m.messages))
		}
		preview.WriteString("检查结果：对话新增消息 0 条；选择位置未跳走。\n")
		writeToolRoundTwoPreview(t, "round-two-b.ansi", preview.String(), width)
	})
}

func writeToolRoundTwoPreview(t *testing.T, name, output string, width int) {
	t.Helper()
	plain := strings.TrimRight(stripANSI(output), "\n")
	lines := strings.Split(plain, "\n")
	if len(lines) > 32 {
		t.Fatalf("preview %s has %d rows; keep each capture within 32:\n%s", name, len(lines), plain)
	}
	for _, line := range lines {
		if cells := xansi.StringWidth(line); cells > width {
			t.Fatalf("preview %s row exceeds %d columns (%d): %q", name, width, cells, line)
		}
	}
	output += "\x1b[0m"
	if dir := os.Getenv("METIS_TOOL_ROUND_TWO_PREVIEW_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+string(os.PathSeparator)+name, []byte(output), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	fmt.Print(output)
}

// TestToolPolishVisualDump is a reproducible offline preview of the real tool
// renderer. All events below are synthetic; no commands or model calls run.
// Set METIS_TOOL_PREVIEW_FILE to save only the ANSI preview, without go test's
// status lines, for opening in a terminal.
func TestToolPolishVisualDump(t *testing.T) {
	const width = 100
	events := []ToolEvent{
		{
			Kind: "result", ToolName: "Bash",
			Input: map[string]any{
				"command":     "python3 - <<'PY'\n# Synthetic offline fixture; not executed.\nprint('September 1-15 activity: fixture data')\nPY",
				"description": "汇总九月前半月活动与改名提交",
			},
			Output: strings.Join([]string{
				"September 1-15 activity: [('2026-09-01', 9), ('2026-09-02', 14)]",
				"early cordis-di inputs: []",
				"sample-a 2026-09-08 docs: explain the renamed crates",
				"sample-b 2026-09-07 feat: rename crates to cordis-di-*",
				"sample-c 2026-09-07 fix: preserve queue capacity",
				"sample-d 2026-09-07 ci: retry watched-file write",
				"sample-e 2026-09-07 ci: wait for backend state",
				"sample-f 2026-09-07 test: cover manifest generation",
				"sample-g 2026-09-07 chore: format source files",
			}, "\n"),
			Duration: 388 * time.Millisecond,
		},
		{
			Kind: "end", ToolName: "Bash",
			Input: map[string]any{
				"command":     "python3 - <<'PY'\n# Synthetic empty-result fixture; not executed.\npass\nPY",
				"description": "检索历史会话中的改名讨论",
			},
			Duration: 65 * time.Millisecond,
		},
	}
	var preview strings.Builder
	preview.WriteString("\nMetis 工具块预览（以下均为模拟数据，未执行命令）\n\n")
	preview.WriteString("正常工具调用 · 100 列\n\n")
	for _, event := range events {
		preview.WriteString(renderToolEventAtWidth(event, false, width))
	}
	preview.WriteString("模拟错误展示（只检查错误样式，未执行命令）\n\n")
	preview.WriteString(renderToolEventAtWidth(ToolEvent{
		Kind: "result", ToolName: "Bash", IsError: true,
		Input:    map[string]any{"command": "git -C /tmp/missing-fixture status", "description": "模拟：仓库路径不存在"},
		Output:   "fatal: cannot change to '/tmp/missing-fixture': No such file or directory\nexit status 128",
		Duration: 20 * time.Millisecond,
	}, false, width))
	preview.WriteString(renderToolEventAtWidth(ToolEvent{
		Kind: "result", ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/metis-preview/example.go"},
		Output:   "package example\n\nfunc Example() {}",
		Duration: 12 * time.Millisecond,
	}, false, width))
	preview.WriteString("展开状态 · 原始命令与模拟输出\n\n")
	for _, event := range events {
		preview.WriteString(renderToolEventAtWidth(event, true, width))
	}
	preview.WriteString("\x1b[0m")
	if path := os.Getenv("METIS_TOOL_PREVIEW_FILE"); path != "" {
		if err := os.WriteFile(path, []byte(preview.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	fmt.Print(preview.String())
}

func TestVisualDump(t *testing.T) {
	dump := func(s string) { fmt.Print(s) }

	// Banner separator
	dump("\n────────────── METIS UI PREVIEW ──────────────\n")

	// Render the actual welcome card so the visual preview stays current.
	dump("\n")
	dump((&Model{model: "preview-model", width: 80}).renderWelcomeBannerCard())
	dump("\n\n")

	// User input (was "› ", now "❯ ")
	dump(renderMessage(Message{Role: "user", Content: "fix the bug in compaction trigger"}, 80, false))

	// Assistant text — plain
	dump(renderMessage(Message{Role: "assistant", Content: "Looking at the compaction trigger logic in agent/compact.go."}, 80, false))

	// Assistant text — markdown (code fence + bold + list)
	dump(renderMessage(Message{Role: "assistant", Content: "Here's the **fix**:\n\n```go\nif usage > threshold {\n    if err := Compact(); err != nil {\n        return err\n    }\n}\n```\n\nKey changes:\n- Wrapped the call in error check\n- Added a *log.Info* trace before compaction"}, 80, false))

	// Read — in-flight
	dump(renderToolEvent(ToolEvent{
		Kind:     "start",
		ToolName: "Read",
		Input:    map[string]any{"path": "/Users/me/metis/internal/agent/compact.go"},
	}, false))

	// Read — completed (with line-count summary)
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Read",
		Input:    map[string]any{"path": "/Users/me/metis/internal/agent/compact.go"},
		Output:   "package agent\n\nimport \"context\"\n\nfunc (l *Loop) Compact(ctx context.Context) error {\n\treturn nil\n}\n",
		Duration: 12 * time.Millisecond,
	}, false))

	// Edit — completed with structured diff (this is the big new pattern)
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Edit",
		Input: map[string]any{
			"path":       "internal/agent/compact.go",
			"old_string": "if usage > threshold {\n    return Compact()\n}\n",
			"new_string": "if usage > threshold {\n    log.Info(\"triggering compaction\")\n    if err := Compact(); err != nil {\n        return err\n    }\n}\n",
		},
		Duration: 8 * time.Millisecond,
	}, false))

	// Write — green-only preview
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Write",
		Input: map[string]any{
			"path":    "internal/agent/compact_metric.go",
			"content": "package agent\n\nimport \"sync/atomic\"\n\nvar compactCount atomic.Int64\n\nfunc bumpCompactCount() {\n\tcompactCount.Add(1)\n}\n",
		},
		Duration: 4 * time.Millisecond,
	}, false))

	// Bash — elapsed time + preview body, without repeating the first line
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "go test ./internal/agent/"},
		Output:   "ok  github.com/Ricardo-M-L/metis/internal/agent  2.412s\n? github.com/Ricardo-M-L/metis/internal/agent/transcript [no test files]\nPASS\nok all tests green\nfinal stats: 23 tests, 0 failures\n",
		Duration: 2412 * time.Millisecond,
	}, false))

	// Grep — match-count summary
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Grep",
		Input:    map[string]any{"pattern": "Compact"},
		Output:   "internal/agent/compact.go:1\ninternal/agent/compact.go:42\ninternal/agent/loop.go:80\ninternal/agent/loop.go:215",
		Duration: 18 * time.Millisecond,
	}, false))

	// Bash error path (red ✗)
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "go test ./broken/"},
		Output:   "build failed",
		IsError:  true,
		Duration: 200 * time.Millisecond,
	}, false))

	// Streaming token line (post-30s, mid-stream — the format we just shipped)
	// Note: this is what renderSpinnerStatus produces; we emulate the line.
	dump("\n  ⏺ ")
	dump(lipgloss.NewStyle().Foreground(lipgloss.Color("#909090")).Render("Pondering"))
	dump(lipgloss.NewStyle().Foreground(lipgloss.Color("#606060")).Render(" (12s · ↓ 3.1k tokens · thought for 2s)"))
	dump("\n")

	// Turn-end thought summary
	dump(renderMessage(Message{
		Role:    "thought-summary",
		Content: fmt.Sprintf("%s for %s", "Cogitated", formatTurnDuration(78*time.Second)),
	}, 80, false))

	// Recap (structural, derived from the turn's tool events)
	mockEvents := []ToolEvent{
		{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "compact.go"}},
		{Kind: "result", ToolName: "Edit", Input: map[string]any{"path": "compact.go"}},
		{Kind: "result", ToolName: "Write", Input: map[string]any{"path": "compact_metric.go"}},
		{Kind: "result", ToolName: "Bash", Input: map[string]any{"command": "go test ./internal/agent/"}},
	}
	if recap := buildTurnRecap(mockEvents); recap != "" {
		dump(renderMessage(Message{Role: "recap", Content: recap}, 80, false))
	}

	// Compaction banner
	dump(renderMessage(Message{
		Role:    "compaction",
		Content: "context compacted: 47 → 12 messages",
	}, 80, false))

	// Final ANSI reset so the user's prompt color isn't stuck
	dump("\033[0m")
	dump("──────────────────────────────────────────────\n\n")
}
