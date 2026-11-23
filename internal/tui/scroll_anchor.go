package tui

import (
	"reflect"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

func (m *Model) liveMessageID(thinking bool) string {
	id := &m.streamingMessageID
	if thinking {
		id = &m.thinkingMessageID
	}
	if *id == "" {
		*id = m.nextID()
	}
	return *id
}

func (m *Model) flushThinkingText() {
	if m.thinkingText == "" {
		return
	}
	content := strings.TrimSpace(m.thinkingText)
	m.captureReadingLiveRevision("thinking", content)
	m.messages = append(m.messages, Message{ID: m.liveMessageID(true), Role: "thinking", Content: content, Timestamp: time.Now()})
	m.thinkingText, m.thinkingMessageID = "", ""
}

func (m *Model) flushStreamingText(content string) {
	if m.streamingText == "" {
		return
	}
	m.captureReadingLiveRevision("assistant", content)
	m.messages = append(m.messages, Message{ID: m.liveMessageID(false), Role: "assistant", Content: content, Timestamp: time.Now()})
	m.streamingText, m.streamingMessageID = "", ""
}

type readingLiveRevision struct {
	id, role, content string
	item              list.Item
}

// Keep one observed live layout while a reader is inside it. Final Markdown
// can merge soft line breaks, and completed thinking shows a different window,
// so a rendered row number alone cannot preserve the visible source text.
func (m *Model) captureReadingLiveRevision(role, content string) {
	if m.stickyBottom || m.chatList == nil {
		return
	}
	start, _ := m.chatList.VisibleItemIndices()
	var id string
	var snapshot list.Item
	switch live := m.chatList.ItemAt(start).(type) {
	case *inProgressStreamingItem:
		if role != "assistant" || live.id == "" || live.id != m.streamingMessageID || live.backgrounded || content != m.streamingText {
			return
		}
		revision := *live
		cache := streamingRenderCache{}
		if live.cache != nil {
			cache = *live.cache
		}
		revision.cache = &cache
		id, snapshot = live.id, &revision
	case *inProgressThinkingItem:
		if role != "thinking" || live.id == "" || live.id != m.thinkingMessageID {
			return
		}
		revision := *live
		id, snapshot = live.id, &revision
	default:
		return
	}
	m.readingLiveRevision = &readingLiveRevision{id: id, role: role, content: content, item: snapshot}
}

func (i *inProgressStreamingItem) ScrollAnchorID() string { return messageScrollAnchor(i.id) }
func (i *inProgressStreamingItem) ContainsScrollAnchor(id string) bool {
	return id != "" && id == messageScrollAnchor(i.id)
}

func (i *inProgressStreamingItem) SameScrollAnchorLayout(other list.Item) bool {
	previous, ok := other.(*inProgressStreamingItem)
	return ok && i.id != "" && i.id == previous.id && i.styleKey == previous.styleKey &&
		i.backgrounded == previous.backgrounded && strings.HasPrefix(i.text, previous.text)
}

func (i *inProgressThinkingItem) ScrollAnchorID() string { return messageScrollAnchor(i.id) }
func (i *inProgressThinkingItem) ContainsScrollAnchor(id string) bool {
	return id != "" && id == messageScrollAnchor(i.id)
}

func messageScrollAnchor(id string) string {
	if id == "" {
		return ""
	}
	return "message:" + id
}

func toolScrollAnchor(id string) string {
	if id == "" {
		return ""
	}
	return "tool:" + id
}

func (i *messageItem) ScrollAnchorID() string { return messageScrollAnchor(i.msg.ID) }
func (i *messageItem) ContainsScrollAnchor(id string) bool {
	return id != "" && id == messageScrollAnchor(i.msg.ID)
}

func (i *toolEventItem) ScrollAnchorID() string { return toolScrollAnchor(i.te.ID) }
func (i *toolEventItem) ContainsScrollAnchor(id string) bool {
	return id != "" && id == toolScrollAnchor(i.te.ID)
}

func (i *compactToolEventItem) ScrollAnchorID() string { return toolScrollAnchor(i.te.ID) }
func (i *compactToolEventItem) ContainsScrollAnchor(id string) bool {
	return id != "" && id == toolScrollAnchor(i.te.ID)
}

func groupScrollAnchor(events []ToolEvent) string {
	for _, event := range events {
		if event.ID != "" {
			return toolScrollAnchor(event.ID)
		}
	}
	return ""
}

func groupContainsScrollAnchor(events []ToolEvent, anchor string) bool {
	id, ok := strings.CutPrefix(anchor, "tool:")
	if !ok || id == "" {
		return false
	}
	for _, event := range events {
		if event.ID == id {
			return true
		}
	}
	return false
}

func (i *explorationGroupItem) ScrollAnchorID() string { return groupScrollAnchor(i.events) }
func (i *explorationGroupItem) ContainsScrollAnchor(id string) bool {
	return groupContainsScrollAnchor(i.events, id)
}

func (i *recoveredErrorGroupItem) ScrollAnchorID() string { return groupScrollAnchor(i.events) }
func (i *recoveredErrorGroupItem) ContainsScrollAnchor(id string) bool {
	return groupContainsScrollAnchor(i.events, id)
}

func sameToolScrollEvents(a, b []ToolEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for index, left := range a {
		right := b[index]
		if left.ID != right.ID || left.Kind != right.Kind || left.ToolName != right.ToolName ||
			left.Output != right.Output || left.IsError != right.IsError || left.Duration != right.Duration ||
			left.SubAgentParentID != right.SubAgentParentID || !left.StartTime.Equal(right.StartTime) ||
			!reflect.DeepEqual(left.Input, right.Input) {
			return false
		}
	}
	return true
}

func (i *explorationGroupItem) SameScrollAnchorLayout(other list.Item) bool {
	previous, ok := other.(*explorationGroupItem)
	return ok && i.expand == previous.expand && i.navigation == previous.navigation &&
		i.subParent == previous.subParent && sameToolScrollEvents(i.events, previous.events)
}

func (i *recoveredErrorGroupItem) SameScrollAnchorLayout(other list.Item) bool {
	previous, ok := other.(*recoveredErrorGroupItem)
	return ok && i.expand == previous.expand && i.navigation == previous.navigation &&
		i.subParent == previous.subParent && i.partialCount == previous.partialCount &&
		i.retriedCount == previous.retriedCount && sameToolScrollEvents(i.events, previous.events)
}

type toolScrollSpan struct {
	id          string
	start, rows int
}

// Use the same member renderers and navigation decoration as the group. Only
// the first member receives group controls; hint removal applies to all of
// them. Retaining trailing newlines is essential for the inter-member gaps.
func toolScrollSpans(events []ToolEvent, navigation toolNavigationVisual, width int, render func(ToolEvent) string) []toolScrollSpan {
	spans := make([]toolScrollSpan, 0, len(events))
	start := 0
	for index, event := range events {
		body := render(event)
		if index == 0 {
			body = navigation.render(strings.TrimLeft(body, "\n"), width)
		} else if navigation.enabled {
			body = toolNavigationHints(body)
		}
		rows := strings.Count(body, "\n")
		if index == len(events)-1 {
			rows = strings.Count(strings.TrimRight(body, "\n"), "\n") + 1
		}
		spans = append(spans, toolScrollSpan{id: toolScrollAnchor(event.ID), start: start, rows: max(1, rows)})
		start += rows
	}
	return spans
}

func toolGroupAnchorAt(events []ToolEvent, expanded bool, line int, spans func() []toolScrollSpan) (string, int) {
	if !expanded {
		return groupScrollAnchor(events), max(0, line)
	}
	var anchor string
	memberLine := max(0, line)
	for _, span := range spans() {
		if span.start > line {
			break
		}
		if span.id != "" {
			anchor, memberLine = span.id, max(0, min(line-span.start, span.rows-1))
		}
	}
	return anchor, memberLine
}

func toolGroupAnchorLine(events []ToolEvent, expanded bool, id string, line int, spans func() []toolScrollSpan) (int, bool) {
	if !expanded {
		// The former output is now hidden; the List clamps this row to the
		// summary itself, rather than carrying it into the next message.
		return max(0, line), groupContainsScrollAnchor(events, id)
	}
	for _, span := range spans() {
		if span.id == id {
			return span.start + max(0, min(line, span.rows-1)), true
		}
	}
	return 0, false
}

func (i *explorationGroupItem) scrollSpans(width int) []toolScrollSpan {
	return toolScrollSpans(i.events, i.navigation, width, func(event ToolEvent) string {
		return renderToolEvent(event, true)
	})
}

func (i *explorationGroupItem) ScrollAnchorAt(line, width int) (string, int) {
	return toolGroupAnchorAt(i.events, i.expand, line, func() []toolScrollSpan { return i.scrollSpans(width) })
}

func (i *explorationGroupItem) ScrollAnchorLine(id string, line, width int) (int, bool) {
	return toolGroupAnchorLine(i.events, i.expand, id, line, func() []toolScrollSpan { return i.scrollSpans(width) })
}

func (i *recoveredErrorGroupItem) scrollSpans(width int) []toolScrollSpan {
	return toolScrollSpans(i.events, i.navigation, width, func(event ToolEvent) string {
		return renderToolEventAtWidth(event, true, width)
	})
}

func (i *recoveredErrorGroupItem) ScrollAnchorAt(line, width int) (string, int) {
	return toolGroupAnchorAt(i.events, i.expand, line, func() []toolScrollSpan { return i.scrollSpans(width) })
}

func (i *recoveredErrorGroupItem) ScrollAnchorLine(id string, line, width int) (int, bool) {
	return toolGroupAnchorLine(i.events, i.expand, id, line, func() []toolScrollSpan { return i.scrollSpans(width) })
}
