package agent

// compact_anchor_test.go — pins the active-task anchor that Compact
// uses to keep the user's latest text prompt in the post-compact
// kept tail. 2026-05-13 regression: ProtectFirst=1 + ProtectLast=5
// + a long tool-using turn folded the user's question ("compare
// token consumption") into the summary boundary, and the model
// then answered the ONLY remaining user-text message — msgs[0] —
// which was an unrelated greeting ("你好"). The fix walks back
// from the cut point to find the last user-text message and pulls
// cut earlier so that message survives in keepLast verbatim.
//
// Test helpers (fakeSummarizer / fakeStream / msg / toolUseMsg /
// toolResultMsg / newCompactorFor) live in compact_test.go and are
// reused here.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestLastUserTextBefore_FindsTextMessage(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, "你好"),        // 0
		msg(llm.RoleAssistant, "hi"),   // 1
		msg(llm.RoleUser, "real task"), // 2 ← should win
		msg(llm.RoleAssistant, "ok"),   // 3
		toolUseMsg("u1", "Bash"),       // 4
		toolResultMsg("u1", "output"),  // 5
	}
	got := lastUserTextBefore(msgs, len(msgs))
	if got != 2 {
		t.Errorf("expected index 2 (most recent user-text); got %d", got)
	}
}

func TestLastUserTextBefore_IgnoresToolResults(t *testing.T) {
	// tool_results are user-role but should NOT anchor the active task.
	msgs := []llm.Message{
		msg(llm.RoleUser, "real task"), // 0
		msg(llm.RoleAssistant, "ok"),   // 1
		toolUseMsg("u1", "Bash"),       // 2
		toolResultMsg("u1", "out"),     // 3 — user-role, but tool_result only
	}
	got := lastUserTextBefore(msgs, len(msgs))
	if got != 0 {
		t.Errorf("expected index 0 (tool_results don't count); got %d", got)
	}
}

func TestLastUserTextBefore_NoMatchReturnsMinusOne(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleAssistant, "only assistant here"),
		toolUseMsg("u1", "Bash"),
		toolResultMsg("u1", "out"),
	}
	got := lastUserTextBefore(msgs, len(msgs))
	if got != -1 {
		t.Errorf("expected -1 when no user-text exists; got %d", got)
	}
}

func TestCompact_PreservesUserTaskOverGreeting(t *testing.T) {
	// Reproduces the 2026-05-13 transcript bug. Layout:
	//   msgs[0]: "你好" (USER greeting — would survive ProtectFirst=1)
	//   msgs[1..3]: irrelevant chitchat
	//   msgs[4]: "看看不同的agent token消耗对比" (USER's active task)
	//   msgs[5..14]: 10 tool cycles (5 tool_use + 5 tool_result)
	// Without the anchor fix, msgs[4] gets folded into the assistant
	// summary and the model answers msgs[0] instead. With the fix,
	// msgs[4] survives as a user-role text message in the kept tail.
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	c.ProtectFirst = 1
	c.ProtectLast = 5
	c.MaxSummaryTokens = 64

	all := []llm.Message{msg(llm.RoleUser, "你好")}
	all = append(all, msg(llm.RoleAssistant, "hello, how can I help"))
	all = append(all, msg(llm.RoleUser, "can you read some file"))
	all = append(all, msg(llm.RoleAssistant, "sure"))
	all = append(all, msg(llm.RoleUser, "看看不同的agent token消耗对比 和我metis对比"))
	for i := 0; i < 5; i++ {
		id := "u" + string(rune('1'+i))
		all = append(all, toolUseMsg(id, "Read"))
		all = append(all, toolResultMsg(id, "file body excerpt"))
	}
	// Total: 1 greeting + 1 asst + 1 user filler + 1 asst + 1 ACTIVE-TASK + 10 tool cycles = 15

	out, err := c.Compact(context.Background(), all)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Find every USER text message in the compacted output.
	var userTexts []string
	for _, m := range out {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				userTexts = append(userTexts, b.Text)
			}
		}
	}

	// MUST contain the active-task prompt verbatim.
	found := false
	for _, t := range userTexts {
		if strings.Contains(t, "token消耗对比") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("active user task lost after compact! user-text messages: %#v", userTexts)
	}
}

func TestCompact_NoOpWhenActiveTaskAlreadyInKeepLast(t *testing.T) {
	// When the active user prompt sits within ProtectLast tail already,
	// no extra cut adjustment is needed. Confirm the post-compact
	// output still has it.
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	c.ProtectFirst = 1
	c.ProtectLast = 5
	c.MaxSummaryTokens = 64

	all := []llm.Message{msg(llm.RoleUser, "你好")}
	for i := 0; i < 10; i++ {
		all = append(all, msg(llm.RoleAssistant, "asst chitchat"))
		all = append(all, msg(llm.RoleUser, "user chitchat"))
	}
	all = append(all, msg(llm.RoleUser, "THE_REAL_TASK"))
	all = append(all, msg(llm.RoleAssistant, "on it"))

	out, err := c.Compact(context.Background(), all)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	found := false
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "THE_REAL_TASK") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("active task lost when it should have been in ProtectLast tail")
	}
}
