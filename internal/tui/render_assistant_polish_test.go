package tui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/themes"
)

func TestAssistantColorsFollowThemeAcrossCachedProseAndTables(t *testing.T) {
	previous := themes.Current().Name
	t.Cleanup(func() { themes.SwitchTheme(previous) })
	item := &messageItem{msg: Message{Role: "assistant", Content: "普通正文与 `cordis-rs`。"}, cache: newRenderCache(0, 0)}
	for _, theme := range []string{"dark", "light", "nord", "dark"} {
		themes.SwitchTheme(theme)
		body := renderAssistantBody("普通正文", 80)
		if !strings.Contains(body, styleText.Render("普通正文")) {
			t.Errorf("%s: plain text lost the body foreground: %q", theme, body)
		}
		accent := strings.Split(styleAccent.Render("code"), "code")[0]
		foreground := strings.Split(styleText.Render("body"), "body")[0]
		out := item.Render(80)
		if !strings.Contains(out, accent) || !strings.Contains(out, foreground) || strings.Contains(out, "38;5;203") {
			t.Errorf("%s: cached Markdown did not adopt current body/code colors: %q", theme, out)
		}
		table := renderAssistantBody("| 名称 | 数量 |\n|---|---|\n| `cordis-rs` | `800ms` |", 80)
		if !strings.Contains(table, styleAccent.Render("cordis-rs")) || !strings.Contains(table, styleAccent.Render("800ms")) {
			t.Errorf("%s: table code differs from prose code: %q", theme, table)
		}
	}
}

func TestAssistantProseAndListsKeepReadableContinuationColumns(t *testing.T) {
	plain := stripANSI(renderMessage(Message{Role: "assistant", Content: "First line\nSecond line"}, 80, false))
	plainLines := strings.Split(plain, "\n")
	if strings.TrimRight(plainLines[0], " ") != "  "+glyphBullet+" First line" || plainLines[1] != "    Second line" {
		t.Fatalf("assistant continuation moved out of the text column: %q", plain)
	}
	const uuid = "fb171c30-7d3f-4192-84a0-2b252c8866e0"
	const command = "feat!: rename crates to cordis-di-*"
	content := "- " + strings.Repeat("查询历史记录", 4) + " `" + uuid + "`，这是相关会话。\n- " + strings.Repeat("检查改名记录", 4) + " `" + command + "`，这是提交说明。"
	out := stripANSI((&messageItem{msg: Message{Role: "assistant", Content: content}}).Render(80))
	if !strings.Contains(out, uuid) || !strings.Contains(out, command) {
		t.Fatalf("short inline code split despite fitting a continuation row:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, uuid) || strings.Contains(line, command) {
			if !strings.HasPrefix(line, "      ") {
				t.Errorf("list continuation did not hang beneath item text: %q", line)
			}
		}
		if cells := xansi.StringWidth(line); cells > 80 {
			t.Errorf("list line exceeds viewport: %d cells: %q", cells, line)
		}
	}
}

func TestAssistantNarrowResizeRetainsCompleteCodeAndCJK(t *testing.T) {
	const longCode = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	contents := []string{
		"中文说明" + strings.Repeat("，继续检查所有内容", 10),
		"- `" + longCode + "`\n- **检查所有中文内容**",
		"```text\n    " + longCode + "\n• literal code bullet\n```\n\n- actual list item",
		"| 项目 | 内容 |\n|---|---|\n| 中文 | `" + longCode + "` |",
	}
	for _, content := range contents {
		item := &messageItem{msg: Message{Role: "assistant", Content: content}, cache: newRenderCache(0, 0)}
		for _, width := range []int{80, 76, 40, 36, 24} {
			out := stripANSI(item.Render(width))
			for _, line := range strings.Split(out, "\n") {
				if cells := xansi.StringWidth(line); cells > width {
					t.Errorf("width=%d: output would be clipped (%d cells): %q\n%s", width, cells, line, out)
				}
			}
			if strings.Contains(content, longCode) {
				joined := strings.Map(func(r rune) rune {
					if r == ' ' || r == '\n' || r == '│' || r == '─' || r == '┼' || r == '├' || r == '┤' {
						return -1
					}
					return r
				}, out)
				if !strings.Contains(joined, longCode) {
					t.Errorf("width=%d: code tail was lost:\n%s", width, out)
				}
			}
			if strings.Contains(out, "metis-list") || strings.Contains(out, "metis-code") || strings.ContainsAny(out, "\ue000\ue001") {
				t.Errorf("internal wrapping markers leaked: %q", out)
			}
		}
	}
}

func TestAssistantNestedListRestoresParentParagraphIndent(t *testing.T) {
	content := "1. Outer item.\n   - Inner item " + strings.Repeat("nested words ", 9) + "`end-identifier`\n\n     More inner paragraph.\n\n   Back at outer paragraph.\n\nAfter list."
	for _, width := range []int{60, 40} {
		out := stripANSI((&messageItem{msg: Message{Role: "assistant", Content: content}}).Render(width))
		columns := make(map[string]int)
		for _, line := range strings.Split(out, "\n") {
			for _, text := range []string{"Outer item.", "Inner item", "Back at outer", "After list."} {
				if at := strings.Index(line, text); at >= 0 {
					columns[text] = xansi.StringWidth(line[:at])
				}
			}
			if strings.HasPrefix(strings.TrimLeft(line, " "), "nested words") {
				columns["inner continuation"] = len(line) - len(strings.TrimLeft(line, " "))
			}
			if xansi.StringWidth(line) > width {
				t.Errorf("nested list overflows width %d: %q", width, line)
			}
		}
		if len(columns) != 5 || columns["Back at outer"] != columns["Outer item."] || columns["inner continuation"] != columns["Inner item"] || columns["After list."] != 4 {
			t.Errorf("nested scope did not restore parent indentation at width %d: %v\n%s", width, columns, out)
		}
		if strings.Contains(out, "metis-list") {
			t.Errorf("internal list scope marker leaked: %q", out)
		}
	}
}
