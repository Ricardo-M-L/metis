package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/tui/list"
	"github.com/charmbracelet/x/ansi"
)

func TestScrollAnchor_LiveReplyBecomesHistoryBeforeNextStream(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newE2EModel(t, 145, 44, 0)
	m.chatList.SetGap(chatItemGap)
	m.turnActive, m.spinnerActive = true, true
	m.spinnerStartedAt = time.Now()
	var history strings.Builder
	for row := 1; row <= 240; row++ {
		fmt.Fprintf(&history, "ROW:HISTORY:%06d | fixed-width deterministic history anchor %06d...................\n", row, row)
	}
	m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: history.String()})
	m.View()
	m.chatList.ScrollToBottom()
	for scroll := 0; scroll < 10; scroll++ {
		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		m.View()
	}
	before := firstScrollMarker(m.chatList.Render())
	beforeItem, beforeLine := m.chatList.ItemAtY(0)
	t.Logf("before: item=%d line=%d height=%d type=%T", beforeItem, beforeLine,
		strings.Count(m.chatList.ItemContent(beforeItem), "\n")+1, m.chatList.ItemAt(beforeItem))
	if !strings.Contains(before, "ROW:HISTORY:") || m.stickyBottom {
		t.Fatalf("setup did not reach detached live history: marker=%q sticky=%v", before, m.stickyBottom)
	}
	m.eventCh = make(chan agent.Event, 3)
	m.eventCh <- agent.Event{Kind: agent.EventToolStart, ToolUseID: "read-fixture", ToolName: "Read", ToolInput: map[string]any{"path": "/tmp/scroll_fixture.txt"}}
	m.eventCh <- agent.Event{Kind: agent.EventToolResult, ToolUseID: "read-fixture", ToolName: "Read", ToolResult: &agent.ToolResult{Output: "read complete"}}
	m.eventCh <- agent.Event{Kind: agent.EventTextDelta, TextDelta: "ROW:TOOL_OK:000001 | Read fixture result verified locally.\nROW:LIVE:000001 | next reply started\n"}
	m.Update(spinnerTick{})
	m.View()
	after := firstScrollMarker(m.chatList.Render())
	afterItem, afterLine := m.chatList.ItemAtY(0)
	t.Logf("after: item=%d line=%d height=%d type=%T", afterItem, afterLine,
		strings.Count(m.chatList.ItemContent(afterItem), "\n")+1, m.chatList.ItemAt(afterItem))
	for index := 0; index < m.chatList.Len(); index++ {
		t.Logf("after item %d: height=%d type=%T first=%q", index,
			strings.Count(m.chatList.ItemContent(index), "\n")+1, m.chatList.ItemAt(index), firstScrollMarker(m.chatList.ItemContent(index)))
	}
	if after != before {
		t.Fatalf("live-to-history handoff moved visible content: %q -> %q; sticky=%v", before, after, m.stickyBottom)
	}
}

func firstScrollMarker(rendered string) string {
	for _, line := range strings.Split(ansi.Strip(rendered), "\n") {
		if start := strings.Index(line, "ROW:"); start >= 0 {
			return strings.Fields(line[start:])[0]
		}
	}
	return ""
}

func TestScrollAnchor_ThinkingBecomesHistoryBeforeReply(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newE2EModel(t, 145, 44, 0)
	m.spinnerActive = true
	var thinking strings.Builder
	for row := 1; row <= 20; row++ {
		fmt.Fprintf(&thinking, "ROW:THINKING:%06d | deterministic thinking line\n", row)
	}
	m.handleAgentEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: thinking.String()})
	m.View()
	m.chatList.ScrollToIndex(1)
	m.chatList.ScrollBy(2)
	m.stickyBottom = false
	before := firstScrollMarker(m.chatList.Render())
	m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "ROW:LIVE:000001 | next reply\n"})
	m.View()
	after := firstScrollMarker(m.chatList.Render())
	if before == "" || before != after {
		t.Fatalf("thinking-to-history handoff moved visible content: %q -> %q", before, after)
	}
}

// Completed exploration tools collapse into one row. The message the reader
// reached after those tools must stay visible even though its list index moves.
func TestScrollAnchor_ToolCompletionGroupingPreservesMessage(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newE2EModel(t, 80, 30, 0)
	m.chatList.SetGap(chatItemGap)
	base := time.Now().Add(-time.Hour)
	m.messages = []Message{{Role: "user", Content: "initial", Timestamp: base}}
	for i := 0; i < 50; i++ {
		m.messages = append(m.messages, Message{
			Role: "assistant", Content: fmt.Sprintf("message-after-tools-%02d", i),
			Timestamp: base.Add(time.Second + time.Duration(i)*time.Millisecond),
		})
		m.toolEvents = append(m.toolEvents, ToolEvent{
			ID: fmt.Sprintf("read-%d", i), Kind: "start", ToolName: "Read",
			Input:     map[string]any{"path": fmt.Sprintf("/tmp/file-%d", i)},
			StartTime: base.Add(time.Duration(i+1) * time.Millisecond),
		})
	}
	m.turnActive, m.spinnerActive = true, true
	m.spinnerStartedAt = time.Now()
	m.View()
	anchor := "message-after-tools-00"
	anchorIndex := -1
	for i := 0; i < m.chatList.Len(); i++ {
		if strings.Contains(m.chatList.ItemContent(i), anchor) {
			anchorIndex = i
			break
		}
	}
	if anchorIndex < 0 {
		t.Fatal("setup: message anchor is missing")
	}
	m.chatList.ScrollToIndex(anchorIndex)
	m.stickyBottom = false
	beforeCount := m.chatList.Len()
	m.eventCh = make(chan agent.Event, 50)
	for i := 0; i < 50; i++ {
		m.eventCh <- agent.Event{
			Kind: agent.EventToolResult, ToolUseID: fmt.Sprintf("read-%d", i),
			ToolName: "Read", ToolResult: &agent.ToolResult{Output: "read complete"},
		}
	}
	m.Update(spinnerTick{})
	m.View()
	if beforeCount != 103 || m.chatList.Len() != 54 {
		t.Fatalf("setup: grouping changed item counts %d -> %d, want 103 -> 54", beforeCount, m.chatList.Len())
	}
	if m.chatList.TotalLineCount() <= m.chatList.Height() {
		t.Fatal("setup: transcript must remain taller than the viewport")
	}
	if !strings.Contains(m.chatList.Render(), anchor) {
		t.Fatalf("tool grouping lost the reader's message: sticky=%v atBottom=%v\n%s",
			m.stickyBottom, m.chatList.AtBottom(), m.chatList.Render())
	}
	if m.stickyBottom || m.chatList.AtBottom() {
		t.Fatal("reading history must remain detached from the live tail")
	}
}

func TestScrollAnchor_ExpandedGroupKeepsVisibleMember(t *testing.T) {
	for _, kind := range []string{"exploration", "recovered"} {
		t.Run(kind, func(t *testing.T) {
			member := func(id string) ToolEvent {
				var rows []string
				for row := 0; row < 20; row++ {
					rows = append(rows, fmt.Sprintf("%s-output-%02d", id, row))
				}
				return ToolEvent{ID: id, Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/" + id}, Output: strings.Join(rows, "\n")}
			}
			group := func(events []ToolEvent, expanded bool) list.Item {
				if kind == "exploration" {
					return &explorationGroupItem{events: events, expand: expanded, navigation: toolNavigationVisual{enabled: true, selected: true, expanded: expanded}}
				}
				return &recoveredErrorGroupItem{events: events, expand: expanded, retriedCount: len(events), navigation: toolNavigationVisual{enabled: true, selected: true, expanded: expanded}}
			}
			original := []ToolEvent{member("a"), member("b"), member("c")}
			l := list.NewList(group(original, true), &messageItem{msg: Message{ID: "tail", Role: "assistant", Content: "following message"}})
			l.SetSize(70, 6)
			needle := "b-output-12"
			row := -1
			for index, line := range strings.Split(ansi.Strip(l.ItemContent(0)), "\n") {
				if strings.Contains(line, needle) {
					row = index
					break
				}
			}
			if row < 0 {
				t.Fatal("setup: member output missing")
			}
			l.ScrollBy(row)
			before := strings.Split(ansi.Strip(l.Render()), "\n")[0]
			withPrefix := append([]ToolEvent{member("new")}, original...)
			l.SetItemsKeepScroll(group(withPrefix, true), &messageItem{msg: Message{ID: "tail", Role: "assistant", Content: "following message"}})
			after := strings.Split(ansi.Strip(l.Render()), "\n")[0]
			if before != after || !strings.Contains(after, needle) {
				t.Fatalf("regroup changed visible member row: %q -> %q", before, after)
			}
			l.SetItemsKeepScroll(group(withPrefix, false), &messageItem{msg: Message{ID: "tail", Role: "assistant", Content: "following message"}})
			item, _ := l.ItemAtY(0)
			if item != 0 {
				t.Fatalf("collapsed member anchor escaped its group: first item=%d\n%s", item, ansi.Strip(l.Render()))
			}
		})
	}
}
