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
	"fmt"
	"strings"
	"time"

	xansi "github.com/charmbracelet/x/ansi"

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

// explorationGroupItem is Claude Code's collapsed Read/Search cluster. The
// underlying tools remain one event each (and still execute/audit normally),
// but consecutive successful Read/Grep/Glob/LS rows render as one compact
// count summary. Ctrl+O uses the same item to reveal every original row.
//
// 2026-07-27 grouping tweak (user feedback "一堆省略号"): the previous
// groupableExplorationEvent filter rejected any event with a non-empty
// SubAgentParentID, so a fan-out of 5 sub-agents × 3 greps each produced
// 15 un-grouped rows of "⏺ grep …" + "(ctrl+O to expand)" — visually a
// wall of ellipses. We now track SubAgentParentID on the group and let
// same-parent sub-agent events cluster together; the rendered block is
// indented when the whole group came from a child agent, so the
// transcript still reads as a tree.
type explorationGroupItem struct {
	events []ToolEvent
	expand bool
	// subParent is the shared SubAgentParentID across all events ("" for
	// top-level groups). Stamped at flush time so Render can decide
	// whether to indent without re-deriving it.
	subParent string
}

func (i *explorationGroupItem) Render(width int) string {
	_ = width
	if i.expand {
		var out strings.Builder
		for _, te := range i.events {
			out.WriteString(renderToolEvent(te, true))
		}
		return out.String()
	}
	reads, searches, listings := 0, 0, 0
	for _, te := range i.events {
		switch strings.TrimPrefix(te.ToolName, "sub: ") {
		case "Read":
			reads++
		case "Grep":
			searches++
		case "Glob":
			searches++
		case "LS":
			listings++
		}
	}
	parts := make([]string, 0, 3)
	if reads > 0 {
		parts = append(parts, fmt.Sprintf("Read %d %s", reads, pluralN(reads, "file", "files")))
	}
	if searches > 0 {
		parts = append(parts, fmt.Sprintf("Searched %d %s", searches, pluralN(searches, "pattern", "patterns")))
	}
	if listings > 0 {
		parts = append(parts, fmt.Sprintf("Listed %d %s", listings, pluralN(listings, "directory", "directories")))
	}
	// Sub-agent groups render INDENTED under their parent Agent row,
	// mirroring the per-tool sub-agent indentation in render_tool.go's
	// isSub branch. Keeps the tree visual: top-level "explored" is
	// flush-left, sub-agent groups sit at +4.
	isSub := i.subParent != ""
	leadIndent, resultIndent := "  ", "    "
	if isSub {
		leadIndent, resultIndent = "      ", "        "
	}
	var out strings.Builder
	out.WriteString(styleSuccess.Render(leadIndent + glyphBullet + " "))
	out.WriteString(styleToolName.Render("explored"))
	out.WriteString("\n")
	out.WriteString(styleDim.Render(resultIndent + glyphTreeLeaf + "  "))
	out.WriteString(styleAccent.Render("✓ "))
	out.WriteString(strings.Join(parts, " · "))
	out.WriteString(styleMuted.Render(" (ctrl+O to expand)"))
	out.WriteString("\n\n")
	return out.String()
}

func pluralN(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// groupableExplorationEvent reports whether te can join an exploration
// cluster. Errors and in-flight "start" rows are excluded — they need
// their own visual treatment. Sub-agent events ARE included now (they
// were excluded pre-2026-07-27, causing the un-grouped "wall of dots");
// buildChatItems' flushExploration additionally keys clusters by
// SubAgentParentID so events from different children never mix.
func groupableExplorationEvent(te ToolEvent) bool {
	if te.Kind == "start" || te.IsError {
		return false
	}
	switch strings.TrimPrefix(te.ToolName, "sub: ") {
	case "Read", "Grep", "Glob", "LS":
		return true
	default:
		return false
	}
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
	text    string
	expand  bool
	width   int           // captured for thinkingHintFits gate; safe to ignore Render arg
	elapsed time.Duration // time since thinking started; 0 = omit from header
}

// thinkingLiveWindow is the number of trailing thinking lines shown
// during streaming. Matches DeepSeek-TUI's collapsed reasoning card
// (3-4 content lines) — long enough to show the current thread,
// short enough that 60+ token/s streams don't drown the viewport.
const thinkingLiveWindow = 4

func (it *inProgressThinkingItem) Render(width int) string {
	if it.text == "" {
		return ""
	}
	var sb strings.Builder
	// Header: "✻ thinking … live · 3.2s". Mirrors DeepSeek-TUI's
	// "… reasoning live|done · 12.3s" pattern. Elapsed is optional —
	// callers that haven't been taught about the field pass 0 and
	// the header omits it.
	sb.WriteString(styleAccent.Render("  " + glyphAsterisk + " "))
	sb.WriteString(styleAccent.Render("thinking"))
	sb.WriteString(styleMuted.Render(" … live"))
	if it.elapsed > 0 {
		sb.WriteString(styleMuted.Render(" · " + formatElapsed(it.elapsed)))
	}
	sb.WriteString("\n")

	thinkStyle := styleDim.Italic(true)
	bodyW := width - 4
	if bodyW < 20 {
		bodyW = 20
	}
	wrapped := xansi.Wrap(it.text, bodyW, " /-_.")
	lines := strings.Split(wrapped, "\n")
	// Sliding window: keep only the LAST thinkingLiveWindow lines.
	// A leading "…" row hints that earlier lines exist; the full
	// text is preserved on the historical thinking message that
	// lands after streaming ends (renderMessage::case "thinking").
	truncated := false
	if len(lines) > thinkingLiveWindow {
		lines = lines[len(lines)-thinkingLiveWindow:]
		truncated = true
	}
	railStyle := styleDim
	if truncated {
		sb.WriteString(railStyle.Render("  ╎ "))
		sb.WriteString(thinkStyle.Render("…"))
		sb.WriteString("\n")
	}
	for i, ln := range lines {
		sb.WriteString(railStyle.Render("  ╎ "))
		sb.WriteString(thinkStyle.Render(ln))
		// Trailing cursor on the LAST line — DeepSeek-TUI's "▎"
		// glyph, signals "stream is still appending".
		if i == len(lines)-1 {
			sb.WriteString(styleAccent.Render(" ▎"))
		}
		sb.WriteString("\n")
	}
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
	if it.text == "" || it.backgrounded {
		return ""
	}
	// Streaming text arrives before the final assistant message goes through
	// glamour, so a provider may give us one very long paragraph with no
	// newlines. Wrap it here using the same width budget as historical
	// assistant rows. Otherwise the list counts it as one row while the
	// terminal paints/truncates it at the right edge, which desynchronizes
	// frame geometry during long, frequently repainted conversations.
	bodyW := width - 4
	if bodyW < 20 {
		bodyW = 20
	}
	wrapped := xansi.Wrap(it.text, bodyW, " /-_.")

	var sb strings.Builder
	sb.WriteString(styleAsst.Render("  " + glyphBullet + " "))
	lines := strings.Split(wrapped, "\n")
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
	var explorationRun []ToolEvent
	flushExploration := func() {
		if len(explorationRun) == 0 {
			return
		}
		if len(explorationRun) == 1 {
			// A1 (2026-08-02): expand only when this event's ID matches
			// expandedToolID. The legacy m.expandToolOutputs global
			// toggle has been removed (see tui.go) — the old OR-fallback
			// branch was dead code in production but a foot-gun in tests.
			out = append(out, &toolEventItem{te: explorationRun[0], expand: explorationRun[0].ID == m.expandedToolID, cache: m.renderCache})
		} else {
			events := append([]ToolEvent(nil), explorationRun...)
			// explorationGroupItem has no per-event expand plumbing —
			// grouped events always render collapsed. Ctrl+O against a
			// grouped event currently no-ops; per-event expansion inside
			// a group is a follow-up.
			out = append(out, &explorationGroupItem{
				events:    events,
				expand:    false,
				subParent: events[0].SubAgentParentID,
			})
		}
		explorationRun = explorationRun[:0]
	}
	for _, it := range merged {
		switch {
		case it.msg != nil:
			flushExploration()
			if hideThinking && (it.msg.Role == "thinking" || it.msg.Role == "redacted_thinking") {
				continue
			}
			// A1 (2026-08-02): thinking rows honour /thinking show only.
			// The legacy expandToolOutputs global toggle is gone; ctrl+O
			// against a thinking row no longer expands it (only tool
			// events participate in one-at-a-time expansion).
			expand := forceExpandThinking && it.msg.Role == "thinking"
			out = append(out, &messageItem{msg: *it.msg, expand: expand, cache: m.renderCache})
		case it.te != nil:
			// Cluster boundary: a groupable event joins the current run
			// only if its SubAgentParentID MATCHES the run's existing
			// parent. Prevents 5 parallel sub-agents' grep calls from
			// collapsing into one giant "Searched 15 patterns" blob
			// that loses track of which child produced what.
			if groupableExplorationEvent(*it.te) {
				if len(explorationRun) > 0 &&
					explorationRun[0].SubAgentParentID != it.te.SubAgentParentID {
					flushExploration()
				}
				explorationRun = append(explorationRun, *it.te)
				continue
			}
			flushExploration()
			// A1: expand only when this event's ID matches expandedToolID.
			out = append(out, &toolEventItem{te: *it.te, expand: it.te.ID == m.expandedToolID, cache: m.renderCache})
		}
	}
	flushExploration()
	if m.thinkingText != "" && !hideThinking {
		// Elapsed: time since the first streaming delta arrived. Falls
		// back to 0 when firstStreamAt is unset (e.g. thinking started
		// before any text — rare but possible) so the header just omits
		// the duration rather than showing a bogus value.
		var elapsed time.Duration
		if !m.firstStreamAt.IsZero() {
			elapsed = time.Since(m.firstStreamAt)
		}
		out = append(out, &inProgressThinkingItem{
			text:    m.thinkingText,
			expand:  forceExpandThinking,
			width:   m.width,
			elapsed: elapsed,
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
