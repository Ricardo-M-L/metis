package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestMicrocompact_KeepRecentToolResults_ProtectsLastN — proves the
// CC-mode keepRecent guard kicks in: when ProtectLast=2 happens to cover
// only trailing chatter messages, the previous batch of tool_results
// stays inline because KeepRecentToolResults=3 catches them.
//
// Layout (10 messages):
//
//	idx 0: user "seed"            (ProtectFirst)
//	idx 1: assistant tool_use t1
//	idx 2: user tool_result t1 (5kB)   <- oldest, should be offloaded
//	idx 3: assistant tool_use t2
//	idx 4: user tool_result t2 (5kB)   <- oldest-2, should be offloaded
//	idx 5: assistant tool_use t3
//	idx 6: user tool_result t3 (5kB)   <- KEEP (in last 3)
//	idx 7: assistant tool_use t4
//	idx 8: user tool_result t4 (5kB)   <- KEEP (ProtectLast tail)
//	idx 9: user "继续"                  <- KEEP (ProtectLast tail)
//
// With ProtectLast=2 and KeepRecentToolResults=3 the eligible range is
// [1..7]. tool_results in that range live at idx 2/4/6. The last 3 are
// {2, 4, 6} — but wait, that's all of them. So actually none of the
// in-range tool_results get offloaded. Adjust to verify the boundary:
// we expect 2 and 4 to be in-range AND the most-recent-3 set includes
// both of them plus tool_result at idx 6.
func TestMicrocompact_KeepRecentToolResults_ProtectsLastN(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 2
	cfg.MicrocompactDir = t.TempDir()
	cfg.MicrocompactMinChars = 4000
	cfg.KeepRecentToolResults = 1 // only ONE tool_result protected — others should offload

	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{})

	body := strings.Repeat("a", 5000)
	messages := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", body), // idx 2 — should offload
		toolUseMsg("t2", "Bash"),
		toolResultMsg("t2", body), // idx 4 — should offload
		toolUseMsg("t3", "Bash"),
		toolResultMsg("t3", body), // idx 6 — KEEP (most-recent in-range, KeepRecentToolResults=1)
		toolUseMsg("t4", "Bash"),
		toolResultMsg("t4", body), // idx 8 — in ProtectLast tail
		msg(llm.RoleUser, "继续"),  // idx 9 — in ProtectLast tail
	}

	out := c.Microcompact(messages)

	check := func(idx int, wantOffloaded bool, label string) {
		got := out[idx].Content[0].ToolResult
		offloaded := strings.HasPrefix(got, "[output cached at")
		if offloaded != wantOffloaded {
			t.Errorf("idx %d (%s): offloaded=%v, want=%v; head=%q", idx, label, offloaded, wantOffloaded, got[:min(60, len(got))])
		}
	}

	check(2, true, "oldest in eligible range")
	check(4, true, "older in eligible range")
	check(6, false, "most-recent in eligible range — protected by keepRecent=1")
	check(8, false, "ProtectLast tail")
}

// TestMicrocompact_KeepRecentToolResults_ProtectsBatchBeforeTrailingChatter
// — the killer scenario the CC keepRecent guard fixes: trailing messages
// are short user/assistant chatter ("继续", "好"), and without the
// keepRecent guard the previous batch of large tool_results would be
// silently evicted to disk.
//
// Layout (ProtectFirst=1, ProtectLast=5, KeepRecentToolResults=5):
//
//	idx 0:  user "seed"
//	idx 1:  assistant tool_use t1
//	idx 2:  user tool_result t1 (5kB)
//	idx 3:  assistant tool_use t2
//	idx 4:  user tool_result t2 (5kB)
//	idx 5:  assistant tool_use t3
//	idx 6:  user tool_result t3 (5kB)
//	idx 7:  assistant tool_use t4
//	idx 8:  user tool_result t4 (5kB)
//	idx 9:  assistant tool_use t5
//	idx 10: user tool_result t5 (5kB)
//	idx 11: assistant "summary of 5 calls"
//	idx 12: user "继续"          ← ProtectLast=5 covers idx 8-12
//	idx 13: assistant "好的"
//	idx 14: user "再做一个"
//	idx 15: assistant "好"
//	idx 16: user "ok"
//
// Without keepRecent: ProtectLast=5 = idx 12..16 → all 5 chatter messages.
// Range [1..12) contains tool_results at idx 2,4,6,8,10 — ALL get offloaded.
// With keepRecent=5: the most-recent 5 tool_results in range = {2,4,6,8,10}
// → all PROTECTED. Nothing offloads.
func TestMicrocompact_KeepRecentToolResults_ProtectsBatchBeforeTrailingChatter(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 5
	cfg.MicrocompactDir = t.TempDir()
	cfg.MicrocompactMinChars = 4000
	cfg.KeepRecentToolResults = 5

	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{})

	body := strings.Repeat("a", 5000)
	messages := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"), toolResultMsg("t1", body),
		toolUseMsg("t2", "Bash"), toolResultMsg("t2", body),
		toolUseMsg("t3", "Bash"), toolResultMsg("t3", body),
		toolUseMsg("t4", "Bash"), toolResultMsg("t4", body),
		toolUseMsg("t5", "Bash"), toolResultMsg("t5", body),
		msg(llm.RoleAssistant, "summary"),
		msg(llm.RoleUser, "继续"),
		msg(llm.RoleAssistant, "好的"),
		msg(llm.RoleUser, "再做一个"),
		msg(llm.RoleAssistant, "好"),
		msg(llm.RoleUser, "ok"),
	}

	out := c.Microcompact(messages)

	// All 5 tool_results in the eligible range should still be inline,
	// thanks to keepRecent=5.
	for _, idx := range []int{2, 4, 6, 8, 10} {
		got := out[idx].Content[0].ToolResult
		if strings.HasPrefix(got, "[output cached at") {
			t.Errorf("idx %d was evicted but keepRecent=5 should protect it; head=%q", idx, got[:min(60, len(got))])
		}
	}
}

// TestMicrocompact_KeepRecentZero_LegacyBehaviourPreserved — opt-out
// path. Set KeepRecentToolResults=0 to get the pre-2026-05-15 behaviour
// where ONLY ProtectLast guards the tail.
func TestMicrocompact_KeepRecentZero_LegacyBehaviourPreserved(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 5
	cfg.MicrocompactDir = t.TempDir()
	cfg.MicrocompactMinChars = 4000
	cfg.KeepRecentToolResults = 0 // disabled

	c := NewCompactor(cfg, "test", 100000, &fakeSummarizer{})

	body := strings.Repeat("a", 5000)
	messages := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"), toolResultMsg("t1", body),
		toolUseMsg("t2", "Bash"), toolResultMsg("t2", body),
		toolUseMsg("t3", "Bash"), toolResultMsg("t3", body),
		msg(llm.RoleUser, "继续"),
		msg(llm.RoleAssistant, "好"),
		msg(llm.RoleUser, "ok"),
		msg(llm.RoleAssistant, "ok"),
		msg(llm.RoleUser, "done"),
	}

	out := c.Microcompact(messages)

	// Eligible range [1..7) covers tool_results at idx 2,4,6.
	// With KeepRecentToolResults=0, ALL three should be evicted.
	evicted := 0
	for _, idx := range []int{2, 4, 6} {
		got := out[idx].Content[0].ToolResult
		if strings.HasPrefix(got, "[output cached at") {
			evicted++
		}
	}
	if evicted != 3 {
		t.Errorf("expected all 3 tool_results to be evicted with KeepRecent=0; got %d/3", evicted)
	}
}
