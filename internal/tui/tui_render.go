package tui

// tui_render.go is now just the View() entry point and timeline plumbing.
// Per-feature render functions live in render_message.go (transcript rows),
// render_tool.go (tool events + diffs), render_overlay.go (palette /
// permission / task panel / scrollbar), render_chrome.go (input box +
// status bar + spinner + hints), render_welcome.go (empty-session
// banner), render_util.go (shared helpers).

import (
	"sort"
	"strings"
	"time"
)

// timelineItem is a chronologically-orderable wrapper around either a
// Message or a ToolEvent. We keep two separate slices on the Model
// (m.messages, m.toolEvents) for backward-compat with existing read
// paths (sessionStore writes only m.messages), but render them merged.
type timelineItem struct {
	msg *Message
	te  *ToolEvent
	ts  time.Time
}

// sortTimelineByTime sorts a chronological merge of timelineItems
// in-place. Stable so two items with identical Timestamps preserve
// their append order.
func sortTimelineByTime(items []timelineItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].ts.Before(items[j].ts)
	})
}

// timeline builds a chronological merged view of m.messages and
// m.toolEvents for rendering. Both source slices are append-only so a
// simple merge by timestamp produces the chat-surface order
// claude-code shows.
func (m *Model) timeline() []timelineItem {
	out := make([]timelineItem, 0, len(m.messages)+len(m.toolEvents))
	for i := range m.messages {
		out = append(out, timelineItem{msg: &m.messages[i], ts: m.messages[i].Timestamp})
	}
	for i := range m.toolEvents {
		out = append(out, timelineItem{te: &m.toolEvents[i], ts: m.toolEvents[i].StartTime})
	}
	sortTimelineByTime(out)
	return out
}

// View is the bubbletea-required render entry point. It composes the
// per-feature renderers (welcome banner, transcript, overlays,
// status bar) into the final string. Pure presentation: no mutation
// outside the viewport content cache + lastViewportLen.
func (m *Model) View() string {
	// Copy mode: alt-screen is exited so the user can mouse-select
	// from native scrollback. Return empty so we don't re-paint over
	// their selection.
	if m.copyMode {
		return ""
	}
	// Active full-window overlay short-circuits everything else. The
	// chat surface state is preserved so closing the screen returns
	// the user to their exact scroll position.
	if m.activeScreen != nil {
		return m.activeScreen.View()
	}

	// Empty transcript → welcome banner (plus chrome so the user can
	// type immediately). claude-code's pattern: the 3-line logo
	// persists at the top of a fresh session until the user actually
	// types something.
	if len(m.messages) == 0 && !m.spinnerActive {
		m.firstRender = false
		m.showBanner = false
		var s strings.Builder
		s.WriteString(m.renderWelcomeBanner())
		s.WriteString(renderInputLine(m))
		// hints (mode indicator) goes IMMEDIATELY below the input box —
		// claude-code parity: the user's eye is already on the input,
		// the mode reminder belongs adjacent to it. Status bar (with
		// tokens / version on the right) is a separate, lower band.
		s.WriteString(renderHints(m))
		if m.showPalette {
			s.WriteString(renderPalette(m))
		}
		if m.showHistory {
			s.WriteString(renderHistorySearch(m))
		}
		if m.atActive && len(m.atMatched) > 0 {
			s.WriteString(renderAtMention(m))
		}
		s.WriteString(renderStatusBar(m))
		s.WriteString("\033[0m")
		return s.String()
	}

	var s strings.Builder

	// Brand watermark — every-frame model + mode reminder. The brand
	// itself ("metis") gets the accent color so the eye lands on it
	// the way claude-code's "✻ claude-code" sits at the top of every
	// session; the model id and mode follow in dim so they don't
	// compete with the transcript below.
	s.WriteString(styleAccent.Render("  metis"))
	s.WriteString(styleMuted.Render(" · "))
	s.WriteString(styleDim.Render(m.model))
	s.WriteString(styleMuted.Render(" · "))
	s.WriteString(styleDim.Render(string(m.gate.Mode())))
	s.WriteString("\n")
	s.WriteString(styleMuted.Render("  ────────────────────────────────────────────"))
	s.WriteString("\n")

	// Build the scrollable content as a chronologically-interleaved
	// timeline of messages and tool events. claude-code renders
	// strictly by timestamp; this matches that.
	var scroll strings.Builder
	w := m.width
	if w <= 0 {
		w = 80
	}
	for _, item := range m.timeline() {
		if item.msg != nil {
			scroll.WriteString(renderMessage(*item.msg, w))
		} else if item.te != nil {
			scroll.WriteString(renderToolEvent(*item.te, m.expandToolOutputs))
		}
	}
	// Live-streaming extended-thinking trace, rendered above the
	// in-flight reply in dim italic so the user sees the model's
	// reasoning as it arrives instead of an opaque spinner.
	if m.thinkingText != "" {
		scroll.WriteString(styleMuted.Render("  " + glyphAsterisk + " "))
		thinkStyle := styleMuted.Italic(true)
		thinkLines := strings.Split(m.thinkingText, "\n")
		if len(thinkLines) > 0 {
			scroll.WriteString(thinkStyle.Render(thinkLines[0]))
			for _, ln := range thinkLines[1:] {
				scroll.WriteString("\n  ")
				scroll.WriteString(thinkStyle.Render(ln))
			}
		}
		scroll.WriteString("\n\n")
	}
	if m.streamingText != "" {
		scroll.WriteString(styleAsst.Render("  " + glyphBullet + " "))
		streamLines := strings.Split(m.streamingText, "\n")
		if len(streamLines) > 0 {
			scroll.WriteString(styleText.Render(streamLines[0]))
			for _, ln := range streamLines[1:] {
				scroll.WriteString("\n  ")
				scroll.WriteString(styleText.Render(ln))
			}
		}
		scroll.WriteString("\n\n")
	}
	content := scroll.String()

	// Dynamic viewport height — size to actual content (capped at
	// terminal-minus-chrome) so a fresh chat with 2 messages doesn't
	// pad with blank rows pushing the input box to the terminal
	// bottom.
	contentLines := strings.Count(content, "\n")
	maxVpHeight := m.height - 10
	if maxVpHeight < 5 {
		maxVpHeight = 5
	}
	desiredVp := contentLines + 1
	if desiredVp > maxVpHeight {
		desiredVp = maxVpHeight
	}
	if desiredVp < 1 {
		desiredVp = 1
	}
	m.viewport.Height = desiredVp
	// Smart auto-scroll — only follow new content to the bottom when
	// the user was already at the bottom. If they PgUp'd to read
	// older context, leave the viewport alone.
	wasAtBottom := m.viewport.AtBottom()
	m.viewport.SetContent(content)
	if l := len(content); l > m.lastViewportLen {
		if wasAtBottom {
			m.viewport.GotoBottom()
		}
		m.lastViewportLen = l
	}
	// Render viewport with a vertical scrollbar gutter on the right
	// edge — claude-code shows a thin bar so the user can see where
	// they are in the transcript.
	vpView := m.viewport.View()
	if m.viewport.Height > 0 && m.viewport.TotalLineCount() > m.viewport.Height {
		vpView = renderScrollbar(vpView, &m.viewport)
	}
	s.WriteString(vpView)
	s.WriteString("\n")

	// Spinner — mirrors claude-code's `* Verb (Xs · ↓ N.Nk tokens · thought
	// for Ys)` parts pattern.
	if m.spinnerActive {
		s.WriteString(renderSpinnerStatus(m))
	}

	// Permission prompt sits ABOVE the input — modal interrupt the
	// user must answer before continuing.
	if m.permActive {
		s.WriteString(renderPermission(m))
	}

	s.WriteString(renderInputLine(m))
	// hints (mode indicator) — claude-code parity: glued to the input.
	s.WriteString(renderHints(m))

	// Slash command palette renders BELOW the hints (claude-code
	// pattern). Palette is suggestion overlay, hints is permanent.
	if m.showPalette {
		s.WriteString(renderPalette(m))
	}

	// Task panel — Ctrl+T opens a bordered list of todos.
	if m.showTaskPanel {
		s.WriteString(renderTaskPanel(m))
	}

	s.WriteString(renderStatusBar(m))

	// Overlay stack — every active modal/dialog. Each overlay's View()
	// already returns a lipgloss-bordered box, so we just append in
	// stack order. Empty list = no modals visible.
	for _, ov := range m.overlays.View(m.width, m.height) {
		s.WriteString("\n")
		s.WriteString(ov)
		s.WriteString("\n")
	}

	// Reset ANSI styles to prevent stacking
	s.WriteString("\033[0m")

	return s.String()
}
