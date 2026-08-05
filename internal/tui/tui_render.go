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
		// Last-resort frame invariant: no logical row may reach the
		// terminal's final column. Component renderers normally wrap to their
		// own narrower budgets, but one missed dynamic label/tool result is
		// enough to trigger terminal auto-wrap and make bubbletea's
		// newline-based row accounting drift from the physical screen. Keep
		// one cell free at the right margin on every full-screen frame.
		safeWidth := m.width - 1
		if safeWidth <= 0 {
			safeWidth = 79
		}
		content = clampBlock(content, safeWidth)
		v := tea.NewView(content)
		v.AltScreen = true
		// MouseModeNone: don't capture the mouse at all. The terminal
		// then handles drag-to-select natively, so Cmd+C just works
		// for copying any on-screen text — including the input box,
		// which is what the user actually wants (2026-08-01 user
		// report: "输入框里的文字没法用光标选择").
		//
		// Trade-off: the mouse wheel no longer scrolls chat history
		// (it now scrolls the terminal's native scrollback instead,
		// which is empty in alt-screen mode). Users scroll chat via
		// PgUp/PgDn / arrow keys / the scroll bar.
		v.MouseMode = tea.MouseModeNone
		// Enable xterm focus reporting (DECSET 1004 — `\x1b[?1004h`)
		// so bubbletea v2 dispatches FocusMsg / BlurMsg when the
		// terminal tab gains/loses focus. tui_update.go handles
		// FocusMsg by snapping the chat list back to the bottom,
		// matching claude-code's "switch back to this tab → see the
		// latest" behaviour (user screenshot 37, 2026-05-17).
		v.ReportFocus = true
		// Drive the terminal window/tab title via bubbletea v2's
		// built-in support (tea.View.WindowTitle → ansi.SetWindowTitle
		// OSC 0). The renderer auto-diffs against the previous frame
		// and only emits the escape when the value changes
		// (cursed_renderer.go:372), and clears the title to "" on
		// alt-screen exit (cursed_renderer.go:189-191). Plain "metis"
		// as the baseline so a fresh session still gets a useful tab
		// name; "metis · <title>" once the user has renamed.
		v.WindowTitle = "metis"
		if m.sessionTitle != "" {
			v.WindowTitle = "metis · " + m.sessionTitle
		}
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
		if m.activeScreen != nil || m.copyMode || m.permActive || m.askUserActive || m.showHistory {
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

	// Sticky header banner — Phase-33 redesign (user feedback 2026-05-08).
	// Same brand strip claude-code keeps pinned at the top of every
	// session: ✻ metis · model · mode · cwd. Replaces the ad-hoc
	// "metis · model · mode + ──── separator" so cwd stays visible
	// during long agent runs (the previous strip dropped cwd as soon
	// as the welcome banner scrolled off; users couldn't tell which
	// directory the current turn was operating against).
	s.WriteString(m.renderHeaderBanner())

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

	// Auto-scroll combines a sticky-bottom *flag* (m.stickyBottom) with
	// a pre-rebuild AtBottom() snapshot. claude-code's useVirtualScroll
	// mixes the same two signals: an isSticky boolean carried across
	// frames AND the live list position. Sampling BEFORE the rebuild
	// means we're reading the user's last-observed state, not the
	// transient position the rebuild leaves us in.
	//
	// Why both:
	//   - Flag alone fails when external code (or tests) calls
	//     ScrollToTop directly without going through our key handler:
	//     the flag stays true and the next tick yanks the user back.
	//   - Snapshot alone fails during fast streaming — each tick
	//     appends lines so the natural list position is "almost at
	//     bottom but not quite", AtBottom() returns false, and the
	//     stream piles up off-screen even though the user is following.
	//
	// Combined: snap iff the user appears at-bottom AND hasn't
	// gestured away. wheel-up/PgUp/Home flip the flag false; submit
	// and ScrollToBottom flip it true. (See keybind_main.go +
	// tui_update.go for flag-flip points.)
	wasAtBottom := m.chatList.AtBottom()
	m.chatList.SetItemsKeepScroll(m.buildChatItems()...)

	// Two-phase render: build the chrome BELOW the chat list first so
	// we know exactly how many rows it occupies, then size the chat
	// viewport to leave room. The previous code reserved a hardcoded
	// 10 rows, which was wrong when permission prompt / palette /
	// search / multi-line input pushed chrome past that — alt-screen
	// silently clipped the bottom (e.g. permission options 3-4 became
	// invisible, image #2 user report 2026-05-07).
	//
	// Phase 1a: permission prompt lives ABOVE the input — it owns the
	// keyboard while it's up, so sticking it on screen is required
	// (you can't scroll away from a decision the agent is waiting on).
	// Everything else that USED to live here — the streaming reply,
	// the thinking summary, the spinner status row — has been moved
	// into buildChatItems so the chat list virtualizes them together
	// with the rest of the transcript (image #12 + #16 user feedback
	// 2026-05-15: they visually matched transcript rows but stuck on
	// screen as the user scrolled).
	var upper strings.Builder
	if m.permActive {
		upper.WriteString(renderPermission(m))
	}
	if m.askUserActive {
		upper.WriteString(renderAskUser(m))
	}

	// Phase 1b: input + hints + palette/search/taskPanel + statusBar
	// + overlays. Input renders BEFORE we count chrome height because
	// renderInputLine sets m.input.SetHeight; counting against a stale
	// height understates input rows when the user is mid-typing a
	// multi-line prompt.
	var lower strings.Builder
	// Queue indication: previously a sticky pill above the input box,
	// but that anchored to the bottom and didn't follow the chat list
	// when the user scrolled. Now the enqueue notice goes through
	// chatList (see keybind_submit.go ~L147 — info-role message added
	// alongside the queuedPrompts append), and a compact `◷ N queued`
	// chip sits in the status bar so the count is always visible
	// without blocking the message stream.
	lower.WriteString(renderInputLine(m))
	lower.WriteString(renderHints(m))
	// Queued-prompts preview (claude-code PromptInputQueuedCommands
	// parity, 2026-05-20). Visible only when the user has typed
	// mid-turn; rendered as faint one-line rows below renderHints so
	// the user sees their input was captured. Without this band the
	// only feedback was the status-bar `◷ N queued` chip, which
	// users (image #1 feedback 2026-05-20) consistently missed —
	// they thought Enter had silently dropped their message.
	lower.WriteString(renderQueuedPreview(m))
	if m.showPalette {
		lower.WriteString(renderPalette(m))
	}
	if m.showSearch {
		lower.WriteString(renderTranscriptSearch(m))
	}
	// 2026-05-24: stripOffsetInLower records newlines in `lower` BEFORE
	// the strip is appended. The actual Y in the final View is computed
	// post-chatList-sizing (see "strip Y calculation" block after Phase 2)
	// because chatList.Height() isn't known until then.
	stripOffsetInLower := -1
	if m.showTaskPanel {
		lower.WriteString(renderTaskPanel(m))
		m.stripStartY = -1
		m.stripPlainLines = nil
	} else {
		// Sticky live todo strip — always-on compact view of the
		// model's current focus + lookahead, when the Ctrl+T overlay
		// isn't already showing the full list. Empty when the session
		// has no todos. Image #1 user request 2026-05-17.
		stripOffsetInLower = strings.Count(lower.String(), "\n")
		lower.WriteString(renderStickyTaskStrip(m))
	}
	lower.WriteString(renderStatusBar(m))
	for _, ov := range m.overlays.View(m.width, m.height) {
		lower.WriteString("\n")
		lower.WriteString(ov)
		lower.WriteString("\n")
	}

	// Phase 2: size the chat viewport against the actual chrome height.
	// chrome occupies upperLines + lowerLines + 1 line for the "\n"
	// written between listView and upper. headerLines = 2 (brand line
	// + separator) was already written to s above.
	const headerLines = 2
	const listSeparatorLines = 1
	upperLines := strings.Count(upper.String(), "\n")
	lowerLines := strings.Count(lower.String(), "\n")
	maxVpHeight := m.height - headerLines - listSeparatorLines - upperLines - lowerLines
	if maxVpHeight < 3 {
		// Floor at 3 so the chat surface never collapses entirely; if
		// the terminal is genuinely too short, the alt-screen will
		// still clip something but at least the user sees recent chat.
		maxVpHeight = 3
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
	if m.stickyBottom && wasAtBottom {
		m.chatList.ScrollToBottom()
	}

	// 2026-05-24: now that chatList is sized, compute the strip's actual
	// Y in the final View output. Layout: header(headerLines) + listView
	// (desiredVp lines) + 1 separator + upperLines + lower-pre-strip
	// (stripOffsetInLower lines). Used by MouseClickMsg / MouseMotionMsg
	// to derive `lineIdx := msg.Y - m.stripStartY` for in-strip drag
	// selection (image 67 user feedback).
	if stripOffsetInLower >= 0 {
		m.stripStartY = headerLines + desiredVp + listSeparatorLines + upperLines + stripOffsetInLower
	}

	// Phase 3: stitch [header][listView][upper][lower] together.
	// inputStartRow points at the row where renderInputLine begins, so
	// attachCursor can position the terminal cursor correctly.
	listView := m.chatList.Render()
	s.WriteString(listView)
	s.WriteString("\n")
	s.WriteString(upper.String())
	inputStartRow := strings.Count(s.String(), "\n")
	s.WriteString(lower.String())

	// Reset ANSI styles to prevent stacking
	s.WriteString("\033[0m")

	return attachCursor(chatView(s.String()), inputStartRow)
}
