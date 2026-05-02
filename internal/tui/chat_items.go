package tui

// chat_items.go — adapters that let internal/tui/list/List render the
// chat surface.
//
// The list package defines `Item.Render(width int) string`; this file
// wraps the existing `tui.Message` and `tui.ToolEvent` value types into
// list-compatible items WITHOUT touching the underlying renderMessage /
// renderToolEvent functions (which means visual_dump_test.go and the
// existing render_message_test/render_tool_test coverage stays valid).
//
// Each adapter reuses the per-message cache (`renderCache.GetMessage` /
// `renderCache.GetTool`) so the cache+virtualization combination
// achieves: (a) viewport-only items rendered, (b) those items hit the
// renderCache so glamour cost is paid once per content change.

import (
	"time"

	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

// messageItem adapts a Message into list.Item.
//
// We carry the Message by value, not pointer: the list package walks
// items by index for both Render and AtBottom, and a stable-by-value
// snapshot avoids any cross-frame mutation surprises (the renderCache
// keys on (role, content, width), so a stale snapshot is harmless —
// the next View() rebuild creates a fresh messageItem reflecting the
// updated Message anyway).
type messageItem struct {
	msg   Message
	cache *renderCache
}

// Render implements list.Item. Cache-aware: hit returns the stored
// string verbatim; miss runs renderMessage, instruments cost via
// RecordRender (slow-render log + rolling avg), then stores the result.
func (i *messageItem) Render(width int) string {
	if i.cache != nil {
		if cached, ok := i.cache.GetMessage(i.msg, width); ok {
			return cached
		}
	}
	t0 := time.Now()
	rendered := renderMessage(i.msg, width)
	if i.cache != nil {
		i.cache.RecordRender(i.msg.Role, len(i.msg.Content), time.Since(t0))
		i.cache.PutMessage(i.msg, width, rendered)
	}
	return rendered
}

// toolEventItem adapts a ToolEvent into list.Item.
//
// Like messageItem, carries the ToolEvent by value. expand is captured
// at item-construction time (in buildChatItems) — if the user toggles
// Ctrl+O between frames, buildChatItems is called again with the new
// flag and rebuilds the items with the new expand baked in.
type toolEventItem struct {
	te     ToolEvent
	expand bool
	cache  *renderCache
}

// Render implements list.Item. The width parameter is ignored because
// renderToolEvent doesn't take a width — its lipgloss styling uses
// terminal default. Reserved for future width-aware tool rendering.
func (i *toolEventItem) Render(width int) string {
	_ = width
	if i.cache != nil {
		if cached, ok := i.cache.GetTool(i.te, i.expand, width); ok {
			return cached
		}
	}
	t0 := time.Now()
	rendered := renderToolEvent(i.te, i.expand)
	if i.cache != nil {
		i.cache.RecordRender("tool:"+i.te.ToolName, len(i.te.Output), time.Since(t0))
		i.cache.PutTool(i.te, i.expand, width, rendered)
	}
	return rendered
}

// buildChatItems composes a chronologically-ordered []list.Item from the
// Model's messages and toolEvents. Same merge logic as `m.timeline()`
// (sort by Timestamp / StartTime, stable order on ties), but produces
// list-compatible items so the chat list can virtualize the render.
//
// Streaming text (m.streamingText / m.thinkingText) is intentionally
// NOT included — those are rendered by View() in a separate "stream
// tail" section so they can update every spinner tick without
// invalidating the cached items above them in the transcript.
func (m *Model) buildChatItems() []list.Item {
	merged := m.timeline()
	out := make([]list.Item, 0, len(merged))
	for _, it := range merged {
		switch {
		case it.msg != nil:
			out = append(out, &messageItem{msg: *it.msg, cache: m.renderCache})
		case it.te != nil:
			out = append(out, &toolEventItem{te: *it.te, expand: m.expandToolOutputs, cache: m.renderCache})
		}
	}
	return out
}
