package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestShouldInjectPlanTrigger_PositiveCases — coverage of trigger
// phrases we expect to fire on. Each is a realistic user message
// shape from the bug tracker (image #50 / session 5d9a38e5 + similar
// rewrite asks).
func TestShouldInjectPlanTrigger_PositiveCases(t *testing.T) {
	cases := []string{
		"帮我把 kimi-cli 用 go 重写",
		"我想 rewrite 整个 auth module",
		"port the whole project to Rust",
		"port to Rust please",
		"translate to Python",
		"refactor the whole logging layer",
		"refactor the entire memory subsystem",
		"refactor entire subsystem",
		"migrate to TypeScript next week",
		"convert the project to Bun",
		"convert the codebase to ESM",
		"reimplement the cache layer in Go",
		"用 go 写一份对应的 CLI",
		"换成 rust 实现",
		"全部翻译成 Python",
		"转写成 Go 版本",
		"迁移到 Go",
		"改写成 TypeScript",
		"改写为 Rust",
		// Verbose / mixed contexts still match (substring, not exact).
		"this is a long message asking you to rewrite the auth flow",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if !shouldInjectPlanTrigger(in, false) {
				t.Errorf("shouldInjectPlanTrigger(%q) = false; want true", in)
			}
		})
	}
}

// TestShouldInjectPlanTrigger_NegativeCases — coverage of inputs
// that look superficially similar but MUST NOT trigger. False
// positives here annoy the model with a forced plan-then-ask for
// non-project-scope work.
func TestShouldInjectPlanTrigger_NegativeCases(t *testing.T) {
	cases := []string{
		"",
		"hello",
		"refactor this function please",       // function-level, not "the whole X"
		"port 8080 is in use",                 // "port" alone (no "to/from")
		"could you migrate this single test?", // no "to" — narrow scope
		"how does the import work?",
		"please run go vet",
		"重构这个函数", // 重构 not in trigger list (重写 is)
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if shouldInjectPlanTrigger(in, false) {
				t.Errorf("shouldInjectPlanTrigger(%q) = true; want false", in)
			}
		})
	}
}

// TestShouldInjectPlanTrigger_AlreadyEntered — once the agent has
// called EnterPlanMode in this loop, subsequent user messages that
// happen to mention "rewrite" again should NOT re-inject the
// reminder (it would be noise — model is already in plan mode).
func TestShouldInjectPlanTrigger_AlreadyEntered(t *testing.T) {
	if shouldInjectPlanTrigger("let's rewrite that next", true) {
		t.Error("trigger should be suppressed when plan mode already entered")
	}
}

// TestDetectPlanModeEntered_FindsToolCall — the helper scans
// assistant messages for EnterPlanMode tool_use. Presence of the
// call → returns true; absence → false.
func TestDetectPlanModeEntered_FindsToolCall(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "go"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "text", Text: "ok let me plan"},
			{Type: "tool_use", ToolName: "EnterPlanMode", ToolUseID: "tu1"},
		}},
	}
	if !detectPlanModeEntered(msgs) {
		t.Error("expected detectPlanModeEntered=true after EnterPlanMode in history")
	}
}

func TestDetectPlanModeEntered_NoToolCall(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "rewrite the project"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "text", Text: "let me explore first"},
			{Type: "tool_use", ToolName: "Glob"},
		}},
	}
	if detectPlanModeEntered(msgs) {
		t.Error("Glob is not EnterPlanMode; should report false")
	}
}

// TestAppendUser_InjectsReminderOnTrigger — end-to-end: a trigger
// message routed through AppendUser results in TWO content blocks
// on the message (reminder first, then the actual text).
func TestAppendUser_InjectsReminderOnTrigger(t *testing.T) {
	l := &Loop{}
	l.AppendUser("帮我把 kimi-cli 用 go 重写")
	if len(l.Messages) != 1 {
		t.Fatalf("expected 1 message; got %d", len(l.Messages))
	}
	content := l.Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks (reminder + text); got %d", len(content))
	}
	if !strings.Contains(content[0].Text, "PLAN-THEN-ASK trigger") {
		t.Errorf("first block should be the reminder; got %q", content[0].Text)
	}
	if !strings.Contains(content[1].Text, "用 go 重写") {
		t.Errorf("second block should be the user text; got %q", content[1].Text)
	}
}

// TestAppendUser_NoInjectOnPlainMessage — the 99% case: a normal
// user message goes through unmodified.
func TestAppendUser_NoInjectOnPlainMessage(t *testing.T) {
	l := &Loop{}
	l.AppendUser("帮我看下这个文件")
	if len(l.Messages[0].Content) != 1 {
		t.Errorf("plain message should be single text block; got %d", len(l.Messages[0].Content))
	}
}

// TestAppendUser_NoReInjectAfterPlanEntered — second turn with
// trigger phrase, but assistant already called EnterPlanMode in
// turn 1 → no re-inject.
func TestAppendUser_NoReInjectAfterPlanEntered(t *testing.T) {
	l := &Loop{}
	l.AppendUser("帮我重写 X")
	// Simulate assistant entering plan mode.
	l.Messages = append(l.Messages, llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: "tool_use", ToolName: "EnterPlanMode"},
		},
	})
	// Second user message with trigger phrase.
	l.AppendUser("再重写 Y")
	last := l.Messages[len(l.Messages)-1]
	if len(last.Content) != 1 {
		t.Errorf("second trigger after plan-entered should NOT re-inject; got %d blocks", len(last.Content))
	}
}

// TestAppendUserBlocks_InjectsReminderOnTrigger — TUI path with
// image attachments + text. Trigger detection should still fire
// on the text portion, and the reminder gets prepended before all
// existing blocks (images preserved).
func TestAppendUserBlocks_InjectsReminderOnTrigger(t *testing.T) {
	l := &Loop{}
	blocks := []llm.ContentBlock{
		{Type: "text", Text: "请把这个项目用 rust 重写"},
		{Type: "image", MediaType: "image/png", Data: "fake-base64"},
	}
	l.AppendUserBlocks(blocks)
	got := l.Messages[0].Content
	if len(got) != 3 {
		t.Fatalf("expected 3 blocks (reminder + text + image); got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "PLAN-THEN-ASK") {
		t.Errorf("first block should be reminder; got %q", got[0].Text)
	}
	if got[2].Type != "image" {
		t.Errorf("image block must be preserved; got type=%q", got[2].Type)
	}
}
