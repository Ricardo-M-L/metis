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
	"strings"
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
	msg    Message
	expand bool
	cache  *renderCache
}

// Render implements list.Item. Cache-aware: hit returns the stored
// string verbatim; miss runs renderMessage, instruments cost via
// RecordRender (slow-render log + rolling avg), then stores the result.
//
// expand is captured at item-construction time (in buildChatItems) and
// keys into the cache via PutMessage/GetMessage — so toggling Ctrl+O
// invalidates cached folded thinking blocks and forces a re-render.
func (i *messageItem) Render(width int) string {
	if i.cache != nil {
		if cached, ok := i.cache.GetMessage(i.msg, width, i.expand); ok {
			return cached
		}
	}
	t0 := time.Now()
	rendered := renderMessage(i.msg, width, i.expand)
	if i.cache != nil {
		i.cache.RecordRender(i.msg.Role, len(i.msg.Content), time.Since(t0))
		i.cache.PutMessage(i.msg, width, i.expand, rendered)
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

// staticItem is a list.Item whose render is precomputed once per
// buildChatItems call. Used for the welcome banner that we want to
// scroll naturally with the transcript instead of disappearing the
// moment the user types their first message.
//
// Width is intentionally ignored — the welcome banner sizes itself
// against m.width at build time, and the list package re-builds items
// when width changes (terminal resize triggers View() → buildChatItems).
type staticItem struct {
	rendered string
}

func (s *staticItem) Render(width int) string {
	_ = width
	return s.rendered
}

// inProgressThinkingItem renders the live thinking summary for the
// current turn at the tail of the chat list, so it scrolls with the
// transcript instead of staying pinned above the input (image #12 user
// feedback 2026-05-15: the thinking summary visually matched the
// historical thinking rows in the transcript but didn't follow the
// mouse wheel, causing it to "stick" on screen as the user scrolled).
// Not cached — content updates on every spinner tick.
type inProgressThinkingItem struct {
	text   string
	expand bool
	width  int // captured for thinkingHintFits gate; safe to ignore Render arg
}

func (it *inProgressThinkingItem) Render(width int) string {
	if it.text == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(styleAccent.Render("  " + glyphAsterisk + " "))
	thinkStyle := styleDim.Italic(true)
	if !it.expand {
		sb.WriteString(thinkStyle.Render(firstThinkingLine(it.text, width)))
		if thinkingHintFits(width) {
			sb.WriteString(styleMuted.Render("  (ctrl+o to expand)"))
		}
		sb.WriteString("\n")
		return sb.String()
	}
	lines := strings.Split(it.text, "\n")
	if len(lines) > 0 {
		sb.WriteString(thinkStyle.Render(lines[0]))
		for _, ln := range lines[1:] {
			sb.WriteString("\n  ")
			sb.WriteString(thinkStyle.Render(ln))
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// inProgressStreamingItem renders the partial assistant reply for the
// current turn at the tail of the chat list. Same rationale as
// inProgressThinkingItem — keeps the visible streaming text aligned
// with transcript scroll. Suppressed when the turn has been
// backgrounded (Ctrl+B): the bytes still accumulate, we just don't
// paint them until finalizeTurn flushes.
type inProgressStreamingItem struct {
	text         string
	backgrounded bool
}

func (it *inProgressStreamingItem) Render(width int) string {
	_ = width
	if it.text == "" || it.backgrounded {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(styleAsst.Render("  " + glyphBullet + " "))
	lines := strings.Split(it.text, "\n")
	if len(lines) > 0 {
		sb.WriteString(styleText.Render(lines[0]))
		for _, ln := range lines[1:] {
			sb.WriteString("\n  ")
			sb.WriteString(styleText.Render(ln))
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// buildChatItems composes a chronologically-ordered []list.Item from the
// Model's messages and toolEvents. Same merge logic as `m.timeline()`
// (sort by Timestamp / StartTime, stable order on ties), but produces
// list-compatible items so the chat list can virtualize the render.
//
// The welcome banner is prepended as the first item so the brand strip
// (ASCII bot + version + model + cwd) stays at the top of the
// transcript and scrolls with the conversation, instead of being
// replaced by the compact top-of-screen header strip the moment the
// first message arrives. Mirrors claude-code's "banner is the first
// thing in the chat history" pattern (user feedback 2026-05-09).
//
// Streaming text (m.streamingText / m.thinkingText) is appended at the
// tail as live in-progress items so they scroll WITH the transcript
// rather than sticking above the input (image #12 user feedback). The
// items live outside the renderCache because their content updates
// every spinner tick; the rest of the list (historical messages +
// tool events) still hits the cache normally — only the tail is
// re-rendered per frame.
func (m *Model) buildChatItems() []list.Item {
	merged := m.timeline()
	out := make([]list.Item, 0, len(merged)+3)
	out = append(out, &staticItem{rendered: m.renderWelcomeBannerNoHint()})
	// thinkingDisplay = "hide" drops every reasoning row from the
	// transcript and from the live-streaming preview. "show" forces
	// expanded view regardless of ctrl+o state. "auto" (default) keeps
	// the old collapsed-by-default-with-ctrl+o behaviour.
	hideThinking := m.thinkingDisplay == "hide"
	forceExpandThinking := m.thinkingDisplay == "show"
	for _, it := range merged {
		switch {
		case it.msg != nil:
			if hideThinking && (it.msg.Role == "thinking" || it.msg.Role == "redacted_thinking") {
				continue
			}
			expand := m.expandToolOutputs
			if forceExpandThinking && it.msg.Role == "thinking" {
				expand = true
			}
			out = append(out, &messageItem{msg: *it.msg, expand: expand, cache: m.renderCache})
		case it.te != nil:
			out = append(out, &toolEventItem{te: *it.te, expand: m.expandToolOutputs, cache: m.renderCache})
		}
	}
	if m.thinkingText != "" && !hideThinking {
		out = append(out, &inProgressThinkingItem{
			text:   m.thinkingText,
			expand: m.expandToolOutputs || forceExpandThinking,
			width:  m.width,
		})
	}
	if m.streamingText != "" {
		out = append(out, &inProgressStreamingItem{
			text:         m.streamingText,
			backgrounded: m.turnBackgrounded,
		})
	}
	// Spinner status (`* scaffolding (5.5s · ↑ 95k tokens)`) used to
	// live in the `upper` chrome block, but it visually matches the
	// asterisked rows already inside the transcript and the user
	// reported it as "stuck" when scrolling (image #16 feedback). We
	// snapshot the spinner string at this frame's buildChatItems call
	// so it appears at the tail of the chat list and scrolls together
	// with the thinking/streaming items. Permission prompt and active
	// screen overlays stay in `upper` because they require keyboard
	// focus and must remain on screen.
	if m.spinnerActive {
		out = append(out, &staticItem{rendered: renderSpinnerStatus(m)})
	}
	return out
}
