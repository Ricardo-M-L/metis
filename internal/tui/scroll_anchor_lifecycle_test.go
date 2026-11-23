package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

func detachedLiveFixture(t *testing.T, role string, rows int) *Model {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	m := newE2EModel(t, 145, 44, 0)
	m.spinnerActive = true
	m.spinnerStartedAt = time.Now()
	var text strings.Builder
	for row := 0; row < rows; row++ {
		fmt.Fprintf(&text, "ROW:READING:%06d | deterministic observed line\n", row)
	}
	kind := agent.EventTextDelta
	if role == "thinking" {
		kind = agent.EventThinkingDelta
	}
	m.handleAgentEvent(agent.Event{Kind: kind, TextDelta: text.String()})
	m.View()
	m.chatList.ScrollToIndex(1)
	m.chatList.ScrollBy(2)
	m.stickyBottom = false
	return m
}

func finishDetachedFixture(m *Model, role string) {
	if role == "thinking" {
		m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "next reply\n"})
	} else {
		m.handleAgentEvent(agent.Event{Kind: agent.EventToolStart, ToolUseID: "first-tool", ToolName: "Read"})
	}
	m.View()
}

func TestScrollAnchor_ReadingRevisionOnlyReplacesTopLiveItem(t *testing.T) {
	m := detachedLiveFixture(t, "assistant", 10)
	finishDetachedFixture(m, "assistant")
	first := m.readingLiveRevision
	if first == nil {
		t.Fatal("observed reply revision was not captured")
	}
	cache := first.item.(*inProgressStreamingItem).cache
	if cache == &m.streamingRenderCache {
		t.Fatal("reading revision shares the next stream's cache")
	}
	m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "ROW:SECOND:000001 | second live reply\n"})
	m.View()
	start, end := m.chatList.VisibleItemIndices()
	secondIndex := -1
	for index := start; index <= end; index++ {
		if _, ok := m.chatList.ItemAt(index).(*inProgressStreamingItem); ok {
			secondIndex = index
		}
	}
	if secondIndex <= start {
		t.Fatalf("setup: old revision and second live reply must share the viewport: %d..%d second=%d", start, end, secondIndex)
	}
	m.handleAgentEvent(agent.Event{Kind: agent.EventToolStart, ToolUseID: "second-tool", ToolName: "Read"})
	m.View()
	if m.readingLiveRevision != first || !strings.Contains(m.chatList.Render(), "ROW:READING:") {
		t.Fatal("a lower visible live item replaced the revision still being read")
	}
	m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "ROW:THIRD:000001 | third live reply\n"})
	m.View()
	m.chatList.ScrollToIndex(m.chatList.Len() - 2)
	m.handleAgentEvent(agent.Event{Kind: agent.EventToolStart, ToolUseID: "third-tool", ToolName: "Read"})
	if m.readingLiveRevision == nil || m.readingLiveRevision == first || m.readingLiveRevision.id == first.id {
		t.Fatal("reading a different top live reply did not replace the single revision slot")
	}
}

func TestScrollAnchor_ReadingRevisionInvalidatesOnCanonicalChange(t *testing.T) {
	m := detachedLiveFixture(t, "assistant", 10)
	finishDetachedFixture(m, "assistant")
	id := m.readingLiveRevision.id
	for index := range m.messages {
		if m.messages[index].ID == id {
			m.messages[index].Content = "changed canonical reply"
		}
	}
	m.View()
	if m.readingLiveRevision != nil || !strings.Contains(m.chatList.ItemContent(1), "changed canonical reply") {
		t.Fatal("changed canonical text kept the stale reading revision")
	}
}

func TestScrollAnchor_FollowReleasesRevisionAndShowsUnobservedTail(t *testing.T) {
	m := detachedLiveFixture(t, "assistant", 30)
	m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "UNOBSERVEDTAILMARKER"})
	finishDetachedFixture(m, "assistant")
	if m.readingLiveRevision == nil || strings.Contains(m.chatList.ItemContent(1), "UNOBSERVEDTAILMARKER") {
		t.Fatal("reading snapshot unexpectedly included an unobserved tail")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	m.View()
	if m.readingLiveRevision != nil || !strings.Contains(m.chatList.ItemContent(1), "UNOBSERVEDTAILMARKER") {
		t.Fatal("explicit follow did not restore the complete canonical reply")
	}
}

func TestScrollAnchor_ThinkingDisplayChangeReleasesReadingRevision(t *testing.T) {
	for _, change := range [][2]string{{"auto", "show"}, {"show", "auto"}} {
		t.Run(change[0]+"-to-"+change[1], func(t *testing.T) {
			m := detachedLiveFixture(t, "thinking", 20)
			m.thinkingDisplay = change[0]
			m.View()
			finishDetachedFixture(m, "thinking")
			if m.readingLiveRevision == nil {
				t.Fatal("setup: thinking revision missing")
			}
			m.applyThinkingDisplay(change[1])
			m.View()
			if m.readingLiveRevision != nil {
				t.Fatal("explicit thinking display change kept the old expansion")
			}
		})
	}
	t.Run("direct-mode-change", func(t *testing.T) {
		m := detachedLiveFixture(t, "thinking", 20)
		finishDetachedFixture(m, "thinking")
		m.thinkingDisplay = "show" // Config screen updates the mode directly.
		m.View()
		if m.readingLiveRevision != nil {
			t.Fatal("direct display-mode change retained incompatible thinking expansion")
		}
	})
	for _, hidden := range []string{"hide", "streamlined"} {
		t.Run(hidden, func(t *testing.T) {
			m := detachedLiveFixture(t, "thinking", 20)
			finishDetachedFixture(m, "thinking")
			if hidden == "hide" {
				m.thinkingDisplay = "hide"
			} else {
				m.outputStyle = hidden
			}
			m.View()
			if strings.Contains(m.chatList.Render(), "ROW:READING:") {
				t.Fatal("reading revision bypassed thinking visibility policy")
			}
		})
	}
}

func TestScrollAnchor_SessionSwitchClearsLiveIdentityAndRevision(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.streamingMessageID, m.thinkingMessageID = "old-stream", "old-thinking"
	m.readingLiveRevision = &readingLiveRevision{id: "old-stream", role: "assistant", content: "old", item: &inProgressStreamingItem{text: "old"}}
	target := &session.Header{ID: "scroll-anchor-target", Model: "test-model", System: "base-system", Mode: "ask"}
	if err := store.WriteHeaderFull(*target); err != nil {
		t.Fatal(err)
	}
	if err := m.activateSession(target.ID, target, nil, true); err != nil {
		t.Fatal(err)
	}
	if m.readingLiveRevision != nil || m.streamingMessageID != "" || m.thinkingMessageID != "" {
		t.Fatal("session switch retained source live identity or reading revision")
	}
}

func TestScrollAnchor_LiveIdentitySurvivesFlushPaths(t *testing.T) {
	for _, path := range []string{"tool", "turn-end", "error", "finalize", "background-finalize"} {
		t.Run(path, func(t *testing.T) {
			m := newSlashTestModel(t)
			m.thinkingText, m.streamingText = "thinking", "assistant reply"
			thinkingID, streamingID := m.liveMessageID(true), m.liveMessageID(false)
			switch path {
			case "tool":
				m.handleAgentEvent(agent.Event{Kind: agent.EventToolStart, ToolUseID: "tool", ToolName: "Read"})
			case "turn-end":
				m.handleAgentEvent(agent.Event{Kind: agent.EventTurnEnd})
			case "error":
				m.handleAgentEvent(agent.Event{Kind: agent.EventError, Err: errors.New("synthetic failure")})
			default:
				m.turnBackgrounded = path == "background-finalize"
				m.finalizeTurn(nil)
			}
			foundThinking, foundStreaming := false, false
			for _, message := range m.messages {
				foundThinking = foundThinking || message.ID == thinkingID && message.Role == "thinking"
				foundStreaming = foundStreaming || message.ID == streamingID && message.Role == "assistant"
			}
			if !foundThinking || !foundStreaming || m.thinkingMessageID != "" || m.streamingMessageID != "" {
				t.Fatalf("flush lost or retained a live identity: thinking=%v assistant=%v", foundThinking, foundStreaming)
			}
			if m.liveMessageID(false) == streamingID || m.liveMessageID(true) == thinkingID {
				t.Fatal("next stream reused a completed message identity")
			}
		})
	}
}

func TestScrollAnchor_AppendingLiveReplyDoesNotRelayoutOldCache(t *testing.T) {
	cache := &streamingRenderCache{}
	old := &inProgressStreamingItem{id: "stream", text: strings.Repeat("observed line\n", 40), styleKey: "style", cache: cache}
	l := list.NewList(old)
	l.SetSize(80, 5)
	l.ScrollBy(10)
	next := *old
	next.text += "new line\n"
	cache.render(next.text, 80, streamingRenderStyleKey(), func() string { return "already laid out next revision" })
	want := *cache
	l.SetItemsKeepScroll(&next)
	if *cache != want {
		t.Fatal("anchor restoration rendered old/new text through the shared one-entry cache")
	}
}
