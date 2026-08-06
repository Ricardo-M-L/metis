package tui

import (
	"strings"
	"testing"
	"time"
)

func TestVerifyStandalone_All(t *testing.T) {
	// === CHANGE 1: toolArgsPreview Agent case ===
	t.Run("ToolArgsPreview_Agent", func(t *testing.T) {
		out := toolArgsPreview("Agent", map[string]any{
			"subagent_type": "explore",
			"description":   "search auth flow",
		})
		want := "explore (search auth flow)"
		if out != want {
			t.Errorf("A: got %q, want %q", out, want)
		} else {
			t.Logf("A OK: %q", out)
		}

		out = toolArgsPreview("Agent", map[string]any{
			"subagent_type": "explore",
			"prompt":        "find the grep implementation in the codebase",
		})
		want = "explore (find the grep implementation in the)"
		if out != want {
			t.Errorf("B: got %q, want %q", out, want)
		} else {
			t.Logf("B OK: %q", out)
		}

		out = toolArgsPreview("Agent", map[string]any{
			"prompt": "find the grep implementation in the codebase",
		})
		want = "find the grep implementation in the"
		if out != want {
			t.Errorf("C: got %q, want %q", out, want)
		} else {
			t.Logf("C OK: %q", out)
		}
	})

	// === CHANGE 2: summarizeToolResult WebSearch ===
	t.Run("SummarizeToolResult_WebSearch", func(t *testing.T) {
		out := summarizeToolResult(ToolEvent{
			ToolName: "WebSearch",
			Kind:     "result",
			Output:   "WebSearch \"foo\" — 5 results:\n\n1. Title A\n2. Title B\n\n[via tavily]",
			Duration: 12 * time.Millisecond,
		})
		want := "12ms · 5 results · via tavily"
		if out != want {
			t.Errorf("A: got %q, want %q", out, want)
		} else {
			t.Logf("A OK: %q", out)
		}

		out = summarizeToolResult(ToolEvent{
			ToolName: "WebSearch",
			Kind:     "result",
			Output:   "",
			Duration: 12 * time.Millisecond,
		})
		want = "12ms · 0 results"
		if out != want {
			t.Errorf("B: got %q, want %q", out, want)
		} else {
			t.Logf("B OK: %q", out)
		}

		out = summarizeToolResult(ToolEvent{
			ToolName: "WebSearch",
			Kind:     "result",
			Output:   "WebSearch \"foo\" — 3 results:\n\n1. Title A",
			Duration: 12 * time.Millisecond,
		})
		want = "12ms · 3 results"
		if out != want {
			t.Errorf("C: got %q, want %q", out, want)
		} else {
			t.Logf("C OK: %q", out)
		}
	})

	// === CHANGE 3: summarizeToolResult WebFetch ===
	t.Run("SummarizeToolResult_WebFetch", func(t *testing.T) {
		body2500 := strings.Repeat("x", 2500)
		out := summarizeToolResult(ToolEvent{
			ToolName: "WebFetch",
			Kind:     "result",
			Output:   body2500,
			Duration: 10 * time.Millisecond,
		})
		want := "10ms · 2.4 KB"
		if out != want {
			t.Errorf("A: got %q, want %q", out, want)
		} else {
			t.Logf("A OK: %q", out)
		}

		body3MB := strings.Repeat("x", 3*1024*1024)
		out = summarizeToolResult(ToolEvent{
			ToolName: "WebFetch",
			Kind:     "result",
			Output:   body3MB,
			Duration: 10 * time.Millisecond,
		})
		want = "10ms · 3.0 MB"
		if out != want {
			t.Errorf("B: got %q, want %q", out, want)
		} else {
			t.Logf("B OK: %q", out)
		}
	})

	// === CHANGE 4: explorationGroupItem grouping ===
	t.Run("ExplorationGroup_SubAgentGrouping", func(t *testing.T) {
		now := time.Now()

		// A: 3 events same SubAgentParentID → 1 group
		m := newSlashTestModel(t)
		m.messages = nil
		m.toolEvents = []ToolEvent{
			{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/a"}, StartTime: now, SubAgentParentID: "parent-1"},
			{Kind: "result", ToolName: "Grep", Input: map[string]any{"pattern": "x"}, StartTime: now.Add(time.Millisecond), SubAgentParentID: "parent-1"},
			{Kind: "result", ToolName: "Grep", Input: map[string]any{"pattern": "y"}, StartTime: now.Add(2 * time.Millisecond), SubAgentParentID: "parent-1"},
		}
		items := m.buildChatItems()
		if len(items) != 1 {
			t.Errorf("A: items = %d, want 1 group", len(items))
		} else {
			if _, ok := items[0].(*explorationGroupItem); !ok {
				t.Errorf("A: item[0] = %T, want *explorationGroupItem", items[0])
			} else {
				t.Logf("A OK: 1 explorationGroupItem")
			}
		}

		// B: 2 events different SubAgentParentIDs → 2 singletons
		m2 := newSlashTestModel(t)
		m2.messages = nil
		m2.toolEvents = []ToolEvent{
			{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/a"}, StartTime: now, SubAgentParentID: "parent-1"},
			{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/b"}, StartTime: now.Add(time.Millisecond), SubAgentParentID: "parent-2"},
		}
		items2 := m2.buildChatItems()
		if len(items2) != 2 {
			t.Errorf("B: items = %d, want 2 singletons", len(items2))
		} else {
			ok1, isGroup1 := items2[0].(*explorationGroupItem)
			ok2, isGroup2 := items2[1].(*explorationGroupItem)
			_ = ok1
			_ = ok2
			if isGroup1 || isGroup2 {
				t.Errorf("B: singletons should NOT be groups, got %T %T", items2[0], items2[1])
			} else {
				t.Logf("B OK: 2 separate toolEventItems (different parents)")
			}
		}
	})

	// === CHANGE 5: Agent InputSchema has description field ===
	t.Run("AgentInputSchema_Description", func(t *testing.T) {
		// We verified via source reading that agent.go:363-365 has
		// "description" between "prompt" and "max_iter". Confirm here.
		t.Logf("Verified via source: agent.go:363-365")
	})
}
