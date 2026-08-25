package webui

import (
	"strings"
	"testing"
)

func TestDesktopTodoPlanProjectsComplexTaskProgress(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		content, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(content)
	}

	html := read("index.html")
	for _, want := range []string{
		`id="todoPlanDock"`,
		`id="todoPlanPopover"`,
		`id="todoPlanList"`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing todo-plan surface %q", want)
		}
	}

	js := read("chat.js")
	for _, want := range []string{
		"function isTodoWriteTool(name)",
		"function applyTodoSnapshot(name, input)",
		"function restoreTodoPlanFromHistory(history)",
		"const failedToolUses = new Set()",
		"function toggleTodoPlan()",
		"applyTodoSnapshot(name, chip.getAttribute('data-args') || '')",
		"queueMicrotask(() => restoreTodoPlanFromHistory(history))",
		"async function runTurnItem(item) {\n  clearTodoPlan();",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("chat.js missing todo-plan behavior %q", want)
		}
	}

	css := read("style.css")
	for _, want := range []string{
		".todo-plan-dock",
		".todo-plan-popover",
		".todo-plan-item[data-status=\"in_progress\"]",
		"@keyframes todo-plan-spin",
		"max-height: 220px",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("style.css missing todo-plan styling %q", want)
		}
	}
}
