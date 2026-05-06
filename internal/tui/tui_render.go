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

	tea "charm.land/bubbletea/v2"
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
// status bar) into the final tea.View struct.
//
// v2: View returns tea.View (not string). AltScreen + MouseMode are
// declared here as View fields rather than program options — that's
// the bubbletea v2 model where the View describes its own terminal
// requirements each frame.
func (m *Model) View() tea.View {
	// RecordView fires on every View() invocation regardless of which
	// branch returns. defer ensures the early-return paths (copyMode,
	// activeScreen, empty timeline) still tick the counter so the
	// periodic stats log measures real frame frequency, not just main-
	// path frequency.
	defer m.renderCache.RecordView()

	// chatView wraps a content string with the metis-standard view
	// flags. AltScreen on for the full-screen TUI surface; mouse mode
	// is CellMotion (button-event tracking — clicks + wheel + drag-
	// while-button-pressed, NO hover). bubbletea v2.0.6 reliably
	// consumes the wheel/click reports at this level; the leakage
	// problem we hit earlier was specifically AllMotion (drag/hover
	// reports leaking as `^[[<0;col;rowM` text). Without CellMotion
	// the user's trackpad scroll inside the TUI does nothing —
	// feedback 2026-05-05.
	//
	// Copy mode below still works: it returns AltScreen=false, which
	// drops the terminal back to its native buffer where the user
	// can mouse-select scrollback as before.
	chatView := func(content string) tea.View {
		v := tea.NewView(content)
		v.AltScreen = true
		v.MouseMode = tea.MouseModeCellMotion
		return v
	}
	// attachCursor sets v.Cursor based on the textarea's current
	// position. Caller passes the absolute Y row at which
	// renderInputLine BEGAN writing; the rest is computed against
	// the textarea's internal cursor (which knows its own column).
	//
	// Returns the v unchanged when the input shouldn't show a cursor
	// (overlay / copy mode / permission prompt has the keyboard).
	//
	// Mirrors claude-code: native terminal cursor at the textarea
	// position rather than a fake inverse-block character.
	attachCursor := func(v tea.View, inputStartRow int) tea.View {
		if m.activeScreen != nil || m.copyMode || m.permActive || m.showHistory {
			return v
		}
		cur := m.input.Cursor()
		if cur == nil {
			return v
		}
		// renderInputLine layout (see render_chrome.go:renderInputLine):
		//   "\n"               ← 1 line gap
		//   divider "  ────…"  ← 1 line
		//   body[0]            ← textarea row 0 lands here
		//   body[1..n]
		//   divider
		//   "\n"
		// So absolute textarea body Y = inputStartRow + 2.
		cur.Position.Y += inputStartRow + 2
		// renderInputLine prefixes each body line with "  " (2 spaces).
		cur.Position.X += 2
		cur.Shape = tea.CursorBlock
		cur.Blink = true
		v.Cursor = cur
		return v
	}

	// Copy mode: alt-screen is exited so the user can mouse-select
	// from native scrollback. Return empty + AltScreen=false so the
	// terminal drops back to the inline buffer for selection.
	if m.copyMode {
		v := tea.NewView("")
		v.AltScreen = false
		return v
	}
	// Active full-window overlay short-circuits everything else. The
	// chat surface state is preserved so closing the screen returns
	// the user to their exact scroll position. Screen interface still
	// uses View() string (not v2 tea.View) — wrap its output here.
	if m.activeScreen != nil {
		return chatView(m.activeScreen.View())
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
		inputStartRow := strings.Count(s.String(), "\n")
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
		return attachCursor(chatView(s.String()), inputStartRow)
	}

	var s strings.Builder

	// Lazy-fill IDs for any Message / ToolEvent that reached the slices
	// without one. Cheap (linear scan, < 1µs at 100 entries) and
	// invisible to the cache layer: cache keys ignore ID. See
	// (m *Model).ensureIDs in tui.go for the why.
	m.ensureIDs()

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

	// Build the chat surface via the virtualized list package. Items
	// are constructed every frame from the chronological merge of
	// m.messages + m.toolEvents (see chat_items.go::buildChatItems);
	// the list package only renders items intersecting the visible
	// window, dropping per-frame alloc from ~5.6 MB (full transcript
	// concat) to ~150 KB at 1200-item scale.
	w := m.width
	if w <= 0 {
		w = 80
	}

	// Auto-scroll: snapshot AtBottom BEFORE swapping items so newly-
	// arrived content while the user was at the bottom auto-follows.
	// SetItemsKeepScroll preserves offsetIdx/offsetLine so a user
	// PgUp'd into history stays put across spinner ticks.
	wasAtBottom := m.chatList.AtBottom()
	m.chatList.SetItemsKeepScroll(m.buildChatItems()...)

	// Dynamic chat-surface height — size to actual content height
	// (capped at terminal-minus-chrome) so a fresh chat with 2
	// messages doesn't pad with blank rows pushing the input box to
	// the terminal bottom. Must run AFTER SetItems so TotalLineCount
	// reflects the just-installed items, not the previous frame's.
	maxVpHeight := m.height - 10
	if maxVpHeight < 5 {
		maxVpHeight = 5
	}
	totalLines := m.chatList.TotalLineCount()
	desiredVp := totalLines
	if desiredVp > maxVpHeight {
		desiredVp = maxVpHeight
	}
	if desiredVp < 1 {
		desiredVp = 1
	}
	m.chatList.SetSize(w-2, desiredVp) // -2 for the scrollbar gutter
	if wasAtBottom {
		m.chatList.ScrollToBottom()
	}

	// Render the visible window only. ListView is < 5 KB even for
	// 1200-item sessions because virtualization caps output at
	// `desiredVp` lines.
	//
	// Scrollbar is intentionally OFF: user feedback 2026-05-05 said
	// the deep-blue `█` thumb was visually loud. claude-code doesn't
	// paint a scrollbar either; it relies on the terminal's own
	// scrollback (Ctrl+S copy mode here exits alt-screen so users
	// who want to scroll back to history can mouse-select). If we
	// re-enable the gutter later, it should be one of `│┃▎` in dim
	// rather than a full block in accent colour.
	listView := m.chatList.Render()
	s.WriteString(listView)
	s.WriteString("\n")

	// Live-streaming extended-thinking + assistant text rendered in
	// the "stream tail" — distinct from the chat list because their
	// content mutates every spinner tick and would invalidate the
	// list's cache keys repeatedly. They live BELOW the list so the
	// user sees: [history items] / [stream-in-progress] / [chrome].
	var tail strings.Builder
	if m.thinkingText != "" {
		tail.WriteString(styleMuted.Render("  " + glyphAsterisk + " "))
		thinkStyle := styleMuted.Italic(true)
		thinkLines := strings.Split(m.thinkingText, "\n")
		if len(thinkLines) > 0 {
			tail.WriteString(thinkStyle.Render(thinkLines[0]))
			for _, ln := range thinkLines[1:] {
				tail.WriteString("\n  ")
				tail.WriteString(thinkStyle.Render(ln))
			}
		}
		tail.WriteString("\n\n")
	}
	if m.streamingText != "" {
		tail.WriteString(styleAsst.Render("  " + glyphBullet + " "))
		streamLines := strings.Split(m.streamingText, "\n")
		if len(streamLines) > 0 {
			tail.WriteString(styleText.Render(streamLines[0]))
			for _, ln := range streamLines[1:] {
				tail.WriteString("\n  ")
				tail.WriteString(styleText.Render(ln))
			}
		}
		tail.WriteString("\n\n")
	}
	if tail.Len() > 0 {
		s.WriteString(tail.String())
	}

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

	inputStartRow := strings.Count(s.String(), "\n")
	s.WriteString(renderInputLine(m))
	// hints (mode indicator) — claude-code parity: glued to the input.
	s.WriteString(renderHints(m))

	// Slash command palette renders BELOW the hints (claude-code
	// pattern). Palette is suggestion overlay, hints is permanent.
	if m.showPalette {
		s.WriteString(renderPalette(m))
	}
	if m.showSearch {
		s.WriteString(renderTranscriptSearch(m))
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

	return attachCursor(chatView(s.String()), inputStartRow)
}
