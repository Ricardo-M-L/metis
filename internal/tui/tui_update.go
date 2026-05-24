package tui

// tui_update.go — bubbletea Update() loop, the async turn driver, and
// the session persistence tail. Update is the single entry point for
// every key/window/tick event; it dispatches into handleKey,
// handleAgentEvent, or the active screen overlay.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/notify"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tui/overlay"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func (m *Model) Init() tea.Cmd {
	// No initial command - let View() render first frame with banner
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Drain agent events
	select {
	case ev, ok := <-m.eventCh:
		if ok {
			m.handleAgentEvent(ev)
		}
	default:
	}

	// Active full-window screen (e.g. /history) takes over input + view.
	// We still let WindowSizeMsg reach the main chat so the underlying
	// layout is correct when the screen closes; everything else routes
	// only to the screen.
	if m.activeScreen != nil {
		if ws, ok := msg.(tea.WindowSizeMsg); ok {
			m.width = ws.Width
			m.height = ws.Height
			m.activeScreen.Resize(ws.Width, ws.Height)
			return m, nil
		}
		var cmd tea.Cmd
		m.activeScreen, cmd = m.activeScreen.Update(msg)
		if m.activeScreen.Done() {
			// Phase C widgets that "report a result" on dismissal
			// are caught here. Type-assert + apply before clearing
			// the activeScreen reference. Each new widget that needs
			// to commit user-chosen state adds a case here.
			//
			// applyScreenResult may set m.activeScreen to a NEW screen
			// (PickerScreen → DetailScreen drill-down). Snapshot the
			// done screen first, run apply, then only clear if apply
			// didn't replace it.
			doneScreen := m.activeScreen
			extraCmd := m.applyScreenResult(doneScreen)
			if m.activeScreen == doneScreen {
				// apply didn't open a follow-up — clear as usual.
				m.activeScreen = nil
			}
			if extraCmd != nil {
				if cmd != nil {
					return m, tea.Batch(cmd, extraCmd)
				}
				return m, extraCmd
			}
		}
		return m, cmd
	}

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// 2026-05-23: publish the chat-surface width to the package
		// level so render_tool.go's renderEditDiff (which has no
		// width parameter — toolEventItem doesn't carry one) can
		// pad its add/del rows to full row width. Without padding,
		// the colored background stops at content end, leaving a
		// "half-row" diff (image #57 user feedback). claude-code /
		// Cursor render full-row background.
		setLastKnownChatWidth(msg.Width)
		// Reserve rows for the always-visible chrome:
		//   header (1) + separator (1) + spinner (2) + input (2)
		//   + status bar (2) + hints (1) + safety (1) ≈ 10
		const chrome = 10
		vpHeight := msg.Height - chrome
		if vpHeight < 5 {
			vpHeight = 5
		}
		// Reserve 2 cols on the right for the scrollbar overlay so
		// the bar always sits inside the visible width — previously
		// it appended past terminal-width and disappeared off-screen
		// (image #51 the user noticed claude-code has one and metis
		// doesn't).
		vpWidth := msg.Width - 2
		if vpWidth < 40 {
			vpWidth = msg.Width
		}
		m.chatList.SetSize(vpWidth, vpHeight)
		// Compute textarea width consistent with renderInputLine's
		// box width math — subtract 8 from terminal width to land on
		// the inner content area. Capped at 100-4 so wide terminals
		// don't render an absurdly long input row.
		w := msg.Width - 8
		if w > 100-4 {
			w = 100 - 4
		}
		if w < 20 {
			w = 20
		}
		m.input.SetWidth(w)
		// Width is part of every messageKey, so a resize means every
		// existing entry is keyed under the old width. Drop them so the
		// map doesn't accumulate stale entries across resizes — running
		// counters survive on purpose (hit-rate stays comparable).
		m.renderCache.InvalidateAll()

	case tea.KeyPressMsg:
		// v2: KeyMsg is interface; KeyPressMsg is the concrete event
		// type for press (vs KeyReleaseMsg). handleKey takes tea.KeyMsg
		// which both implement, so passing through is fine.
		// Mark interaction so SendNotification's 6s recent-key guard
		// suppresses banners while the user is actively typing.
		notify.MarkUserInteraction()
		return m.handleKey(msg)

	case tea.FocusMsg:
		// Terminal regained focus (user switched back to this tab /
		// window). Snap the chat list to the bottom + re-arm
		// stickyBottom so subsequent streaming output stays glued to
		// the latest message — matches claude-code's "switch back, see
		// the newest" behaviour (user screenshot 37, 2026-05-17:
		// claude code 回到当前会话最底层展示, metis 却不会).
		//
		// Guards:
		//   - copy mode: alt-screen is off, native scrollback is in
		//     use; forcing ScrollToBottom would fight the user.
		//   - permission prompt / activeScreen / overlays: chat list
		//     is not the focus target so a re-snap is just noise.
		if !m.copyMode && !m.permActive && m.activeScreen == nil &&
			!m.showHistory && !m.showTaskPanel {
			m.chatList.ScrollToBottom()
			m.stickyBottom = true
		}
		return m, nil

	case tea.BlurMsg:
		// Lost focus — no state change needed. We don't want to drop
		// stickyBottom here: the user might pop back in seconds and
		// expect to see the latest content (the focus-in branch above
		// re-snaps regardless). Keeping this case explicit so a future
		// reader doesn't think Blur was forgotten.
		return m, nil

	case tea.PasteMsg:
		// Bracketed paste: terminal wraps cmd+V'd content with
		// \x1b[200~ … \x1b[201~ and bubbletea v2 reports it as a
		// single PasteMsg (instead of streaming each char as a
		// KeyPressMsg). Without this case the content is silently
		// dropped (image #20 user report 2026-05-07: "输入框用
		// command+v 粘贴不进去").
		//
		// Skip during permission prompt / copy mode / overlays — the
		// keyboard is owned by something other than the textarea then,
		// and pasting into a permission dialog is meaningless.
		// (turnActive is NOT a guard: users routinely queue the next
		// prompt while the agent is mid-stream.)
		if m.permActive || m.copyMode || m.activeScreen != nil {
			pasteDebug("blocked: perm=%v copy=%v screen=%v len=%d",
				m.permActive, m.copyMode, m.activeScreen != nil, len(msg.Content))
			return m, nil
		}
		text := msg.Content
		if text == "" {
			// Cmd+V / Ctrl+V with an IMAGE on the clipboard: macOS &
			// Linux terminals send the bracketed-paste wrapper with
			// empty body because the clipboard payload isn't text.
			// Earlier metis silently dropped the event; the user would
			// hit Cmd+V on a screenshot, see nothing happen, and
			// re-report image-paste as broken — even though the
			// Ctrl+V-bound branch DOES handle images. Mirror that
			// branch here so Cmd+V works the same.
			pasteDebug("empty PasteMsg — probing image clipboard")
			ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
			defer cancel()
			content, _ := readClipboard(ctx)
			if content != nil && (content.Mime == "image/png" || content.Mime == "image/jpeg" || content.Mime == "image/png-base64") {
				path, err := saveClipboardImage(content.Data, content.Mime)
				if err == nil {
					if m.imagePaste == nil {
						m.imagePaste = map[int]string{}
					}
					m.imageCounter++
					m.imagePaste[m.imageCounter] = path
					m.input.InsertString(fmt.Sprintf("[Image #%d] ", m.imageCounter))
					kb := len(content.Data) / 1024
					m.messages = append(m.messages, Message{
						Role:      "info",
						Content:   fmt.Sprintf("(image #%d cached: %s, %d KiB)", m.imageCounter, path, kb),
						Timestamp: time.Now(),
					})
				} else {
					m.messages = append(m.messages, Message{
						Role:      "error",
						Content:   fmt.Sprintf("paste failed: %v", err),
						Timestamp: time.Now(),
					})
				}
			}
			return m, nil
		}
		pasteDebug("ok: %d bytes", len(text))
		// Big paste guardrail: terminal-emulator paste of a 50KB log
		// file would shove tens of thousands of chars into the
		// textarea and freeze its rendering. Cap at 100KB; anything
		// over gets truncated with a visible info row so the user
		// sees what happened.
		const maxPasteBytes = 100 * 1024
		original := len(text)
		if original > maxPasteBytes {
			text = text[:maxPasteBytes]
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   fmt.Sprintf("(paste truncated to %d KiB; original was %d KiB)", maxPasteBytes/1024, original/1024),
				Timestamp: time.Now(),
			})
		}
		m.input.InsertString(text)
		// Trigger at-mention completion in case the pasted content
		// ends with `@partial` — same path as a normal keypress would
		// take. updateAtMention is idempotent on no-match.
		m.updateAtMention()
		return m, nil

	case externalEditorDoneMsg:
		// Phase D #41 — Ctrl+G round-trip. Editor exited; merge the
		// saved buffer back into the textarea (or surface the error).
		m.applyExternalEditorResult(msg)
		return m, nil

	case tea.MouseClickMsg:
		// Mouse-down event (despite the name "Click", in bubbletea v2
		// MouseClickMsg fires on button press, not full press+release).
		// We use it to start a selection or, on double/triple click,
		// snap to a word / line. The actual copy happens on
		// MouseReleaseMsg below — that way single-click-no-drag and
		// drag-to-select share one outbound copy path.
		if msg.Button != tea.MouseLeft ||
			m.permActive || m.copyMode || m.activeScreen != nil ||
			m.showHistory || m.showTaskPanel {
			return m, nil
		}
		localY := msg.Y - chatStartY
		if localY < 0 {
			return m, nil
		}
		if localY >= m.chatList.Height() {
			// 2026-05-24 second pass (image 67 feedback): the previous
			// fix appended a "copied N chars" info row to chat on
			// every click — five clicks left five lines of confirmation
			// noise in the transcript. Switched to SILENT copy
			// (same pattern as copySelectionAndReport's claude-code-
			// parity rationale: copy is a low-ceremony action, the
			// clipboard write IS the success signal).
			plain := plainStickyStripText(m)
			if plain != "" {
				writeClipboard(plain)
			}
			return m, nil
		}
		// Click-count tracking — same algorithm as crush's chat.go.
		// Within doubleClickThreshold + clickPosTolerance the counter
		// climbs; outside, it resets to 1 (start of a new sequence).
		now := time.Now()
		if now.Sub(m.lastClickTime) <= doubleClickThreshold &&
			abs(msg.X-m.lastClickX) <= clickPosTolerance &&
			abs(msg.Y-m.lastClickY) <= clickPosTolerance {
			m.clickCount++
		} else {
			m.clickCount = 1
		}
		m.lastClickTime = now
		m.lastClickX = msg.X
		m.lastClickY = msg.Y

		m.chatList.HandleMouseDown(msg.X, localY, m.clickCount)
		// Double / triple click finalize their selection inside
		// HandleMouseDown (selectWord / selectLine). Copy on the
		// spot — the user's intent is unambiguous, no need to wait
		// for the release.
		if m.clickCount >= 2 && m.chatList.HasSelection() {
			m.copySelectionAndReport()
			// Clear so the next single click doesn't inherit the
			// triple-click span as a "drag from last selection."
			// Highlight stays drawn until the next click.
		}
		return m, nil

	case tea.MouseMotionMsg:
		// MouseModeCellMotion delivers motion events ONLY while a
		// button is held (drag-while-pressed). We treat any drag as
		// selection-extending; if no anchor is set HandleMouseDrag is
		// a no-op so spurious motion (e.g. during a click that didn't
		// land on the chat surface) is harmless.
		if m.permActive || m.copyMode || m.activeScreen != nil ||
			m.showHistory || m.showTaskPanel {
			return m, nil
		}
		localY := msg.Y - chatStartY
		// Don't clamp here — HandleMouseDrag clamps internally to the
		// visible item range so a drag past the bottom edge doesn't
		// lose the lower half of the selection.
		m.chatList.HandleMouseDrag(msg.X, localY)
		return m, nil

	case tea.MouseReleaseMsg:
		// Button-up: finalize the selection. Three outcomes:
		//  1. Drag produced a non-empty selection → copy SelectedText.
		//  2. Single click with no drag → fall back to "copy whole
		//     row" (the original click-to-copy behavior).
		//  3. Click on chrome (header / below list) → no-op.
		if msg.Button != tea.MouseLeft ||
			m.permActive || m.copyMode || m.activeScreen != nil ||
			m.showHistory || m.showTaskPanel {
			return m, nil
		}
		hadDrag := m.chatList.HandleMouseUp()
		if hadDrag {
			m.copySelectionAndReport()
			return m, nil
		}
		// No drag → single-click row-copy fallback. Suppress when
		// a multi-click in flight already copied (clickCount >= 2).
		if m.clickCount >= 2 {
			return m, nil
		}
		localY := msg.Y - chatStartY
		if localY < 0 || localY >= m.chatList.Height() {
			return m, nil
		}
		idx, itemY := m.chatList.ItemAtY(localY)
		if idx < 0 {
			return m, nil
		}
		// OSC 8 hyperlink under the click? Open it in the system
		// browser instead of copying the row. Mirrors iTerm2 / kitty
		// "cmd+click on link" behavior, but unmodified — feels natural
		// in a chat surface where most "interesting" rows have one
		// canonical link (a URL the assistant shared, a file:// path
		// from a tool result). The row-copy fallback is one row down
		// the priority chain so power users still get it.
		if url := m.chatList.URLAtPoint(idx, itemY, msg.X); url != "" {
			if err := openURL(url); err == nil {
				m.messages = append(m.messages, Message{
					Role:      "info",
					Content:   fmt.Sprintf("(opened %s)", url),
					Timestamp: time.Now(),
				})
			} else {
				m.messages = append(m.messages, Message{
					Role:      "info",
					Content:   fmt.Sprintf("(could not open %s: %v)", url, err),
					Timestamp: time.Now(),
				})
			}
			return m, nil
		}
		raw := m.chatList.ItemContent(idx)
		plain := strings.TrimSpace(ansi.Strip(raw))
		if plain == "" {
			return m, nil
		}
		// Silent copy — same reasoning as copySelectionAndReport: the
		// "(copied N chars · OSC 52 → Apple_Terminal) <preview>" info
		// rows polluted the transcript when the user did a flurry of
		// cmd+C / row-clicks (image #20 2026-05-07). The clipboard +
		// fallback file still receive the payload.
		writeClipboard(plain)
		return m, nil

	case tea.MouseWheelMsg:
		// v2 split MouseMsg into Click/Release/Wheel/Motion subtypes.
		// Wheel arrives as MouseWheelMsg with direction in .Button,
		// renamed from MouseButtonWheelUp/Down to MouseWheelUp/Down.
		//
		// Optional wheel quantization (claude-code SCROLL_QUANTUM):
		// when ScrollQuantum > 0, accumulate the wheel direction into
		// m.wheelAccum and only call list.ScrollBy when |accum| >= Q.
		// A trackpad emits dozens of wheel events per gesture, but
		// only the cumulative scroll matters visually. Quantization
		// reduces list-level churn for free; the user still feels
		// "smooth" because the underlying terminal scroll is line-
		// granular regardless.
		var dir int
		switch msg.Button {
		case tea.MouseWheelUp:
			dir = -m.chatList.MouseWheelDelta()
			// Wheel-up = "I want to read history" — break sticky-bottom
			// so streaming output won't yank the viewport back down on
			// the next tick. claude-code parity (isSticky → false).
			m.stickyBottom = false
		case tea.MouseWheelDown:
			dir = m.chatList.MouseWheelDelta()
		default:
			return m, nil
		}
		q := perfConfig().ScrollQuantum
		if q <= 0 {
			m.chatList.ScrollBy(dir)
			// Re-stick if a wheel-down landed us at the floor.
			if msg.Button == tea.MouseWheelDown && m.chatList.AtBottom() {
				m.stickyBottom = true
			}
			return m, nil
		}
		// Reset accumulator on direction reversal so a flick-up after
		// a scroll-down doesn't have to walk back through the buffered
		// down-direction delta.
		if (dir < 0) != (m.wheelAccum < 0) {
			m.wheelAccum = 0
		}
		m.wheelAccum += dir
		if m.wheelAccum <= -q || m.wheelAccum >= q {
			m.chatList.ScrollBy(m.wheelAccum)
			m.wheelAccum = 0
			if msg.Button == tea.MouseWheelDown && m.chatList.AtBottom() {
				m.stickyBottom = true
			}
		}
		return m, nil

	case spinnerTick:
		// Spinner glyph index is now time-gated (renderSpinnerStatus
		// computes it from elapsed via spinnerStepMs), so we no longer
		// touch m.spinnerFrame here. The tick still drives the
		// token-counter animation + event drain below.
		m.totalTokens.Animate()

		// Mi3 (2026-05-18) — prune sub-agent pills whose terminal
		// ✓/✗ tail has been on screen long enough. handleAgentEvent
		// now stamps FinishedAt instead of yanking the pill the
		// instant the result lands, so the user can read the final
		// state. After subAgentLingerDuration we drop it from the
		// chip row.
		if len(m.subAgents) > 0 {
			cutoff := time.Now().Add(-subAgentLingerDuration)
			kept := m.subAgents[:0]
			for _, sa := range m.subAgents {
				if !sa.FinishedAt.IsZero() && sa.FinishedAt.Before(cutoff) {
					continue
				}
				kept = append(kept, sa)
			}
			m.subAgents = kept
		}

		// Permission prompt auto-deny: if the user hasn't answered within
		// permissionTimeout, decide for them so the agent loop unblocks.
		// Without this, leaving the TUI on a prompt overnight (or while
		// a remote viewer is closed) leaves the LLM waiting on a buffered
		// channel send — observable as a stuck spinner with no progress.
		if m.permActive && !m.permStartedAt.IsZero() &&
			time.Since(m.permStartedAt) >= permissionTimeout {
			m.executePermission("n")
		}

		// Drain any agent events that landed since the last tick. The
		// previous code only popped one event per tick, which made bursty
		// streams (text_delta + tool_use back-to-back) lag visibly.
		for drained := false; !drained; {
			select {
			case ev, ok := <-m.eventCh:
				if ok {
					m.handleAgentEvent(ev)
				} else {
					drained = true
				}
			default:
				drained = true
			}
		}

		// Check turn completion BEFORE deciding to continue ticking — the
		// old code returned tickCmd while turnActive was still true, so the
		// doneCh branch never ran and the spinner kept spinning forever
		// after an error.
		select {
		case err, ok := <-m.doneCh:
			if ok {
				m.finalizeTurn(err)
			}
		default:
		}

		if m.turnActive || m.spinnerActive {
			return m, tickCmd
		}
		// Queue drain — finalizeTurn loaded the next prompt into
		// m.input and flagged queuePending. We're in a fresh tick now
		// (turnActive cleared), so it's safe to fire handleSubmit
		// without re-entering the just-finalized turn.
		if m.queuePending {
			m.queuePending = false
			return m.handleSubmit()
		}
		return m, nil

	case tick:
		return m, tickCmd

	case overlay.BtwAnswerMsg:
		// Route the result to whichever BtwOverlay is currently on the
		// stack. If the user already dismissed it (Esc), Get returns nil
		// and the answer is silently dropped — same as claude-code's
		// behavior when the user closes the modal mid-flight.
		if o := m.overlays.Get("btw"); o != nil {
			if btw, ok := o.(*overlay.BtwOverlay); ok {
				btw.Apply(msg)
			}
		}
		return m, nil
	}
	return m, nil
}

// finalizeTurn collects the post-stream cleanup that runs once doneCh
// fires: stop the spinner, flush any buffered streaming text, append
// the thought-summary + recap rows, log the turn for /lessons, and
// persist the tail to disk.
func (m *Model) finalizeTurn(err error) {
	// Phase F Ctrl+B (2026-05-12) — capture whether the turn was
	// running in background mode BEFORE we reset the flag below, so
	// the notification + result-banner logic can force-fire even on
	// short turns (the user backgrounded explicitly, threshold gate
	// off).
	wasBackgrounded := m.turnBackgrounded
	m.turnBackgrounded = false
	m.backgroundedAt = time.Time{}
	m.turnActive = false
	m.spinnerActive = false
	// Turn complete — clear the per-turn cancel func now that we're
	// on the main thread. Previously runTurnAsync did this from
	// inside the goroutine, which raced against handleSubmit on the
	// next turn's setup. See runTurnAsync's header comment.
	m.turnCancel = nil
	// Snap displayed-token counter to truth so the final number doesn't
	// take an extra second to converge after the spinner stops ticking.
	m.totalTokens.Snap()
	// Flush thinking trace before flushing reply text so the order in
	// the transcript matches the order events arrived in.
	if m.thinkingText != "" {
		m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
		m.thinkingText = ""
	}
	if m.streamingText != "" {
		// Phase F Ctrl+B (2026-05-12) — when the flushed reply came
		// in while backgrounded, prefix with a chip so the user can
		// distinguish "this scrolled past while I was looking away"
		// from a normal foreground reply. Content is identical
		// otherwise — the prefix is just a visual cue.
		content := m.streamingText
		if wasBackgrounded {
			content = "[bg] " + content
		}
		m.messages = append(m.messages, Message{Role: "assistant", Content: content, Timestamp: time.Now()})
		m.streamingText = ""
	}
	// Append claude-code-style turn-end thought summary
	// ("✻ Cogitated for 1m 32s") so the user can see at a glance how
	// long each turn ran. Skipped on errors — the error row already
	// carries failure context, and stacking a summary below it reads
	// as celebratory.
	if err == nil && !m.spinnerStartedAt.IsZero() {
		if d := time.Since(m.spinnerStartedAt); d >= time.Second {
			m.messages = append(m.messages, Message{
				Role:      "thought-summary",
				Content:   fmt.Sprintf("%s for %s", pickTurnDoneVerb(), formatTurnDuration(d)),
				Timestamp: time.Now(),
			})
		}
		// Desktop notification on long turns. Channel matrix
		// (notify.go) picks the right OSC sequence per terminal.
		// Threshold gates out quick replies (<30s) where the user
		// is still watching the screen and a banner would be noise.
		// Mirrors claude-code's notification queue minimum-duration
		// heuristic + recent-interaction guard.
		//
		// Phase F Ctrl+B (2026-05-12) — bypass the threshold when
		// the user explicitly backgrounded the turn. They asked
		// not to look at the screen, so we notify regardless of
		// duration.
		if d := time.Since(m.spinnerStartedAt); d >= notify.NotifyMinDuration || wasBackgrounded {
			msg := fmt.Sprintf("turn finished in %s", formatTurnDuration(d))
			if wasBackgrounded {
				msg = "backgrounded " + msg
			}
			notify.SendNotification("metis", msg)
			// Fire the user's [[hooks.notification]] chain so
			// they can shell out to terminal-notifier / ntfy /
			// whatever for a richer macOS-native banner. Best-
			// effort: nil-check both Loop and Hooks so headless
			// tests that build a bare Model don't blow up.
			if m.loop != nil && m.loop.Hooks != nil {
				m.loop.Hooks.EmitNotification(
					context.Background(),
					pubhook.Context{Model: m.loop.Model},
					&pubhook.Notification{
						Level:   "info",
						Message: msg,
					},
				)
			}
		}
	}
	// Clear the OSC 9;4 progress bar from keybind_submit.go regardless
	// of duration / error — leaving it stuck would mislead a glance at
	// the dock. Indicate failure with the Error variant first so the
	// dock briefly shows red before clearing.
	if err != nil {
		notify.SendProgress(notify.ProgressError, 0)
	}
	notify.SendProgress(notify.ProgressClear, 0)
	// Structural recap below the thought-summary, when this turn did
	// enough work to be worth summarizing (≥2 tool calls).
	recap := ""
	if err == nil {
		recap = buildTurnRecap(m.toolEvents)
		if recap != "" {
			m.messages = append(m.messages, Message{
				Role:      "recap",
				Content:   recap,
				Timestamp: time.Now(),
			})
		}
	}
	// Continuous-learning log — append a structural record so /lessons
	// can surface it later. Cheap (1 JSON line append, no LLM); failures
	// are silent so a flaky disk never breaks the chat.
	if !m.spinnerStartedAt.IsZero() {
		var prompt string
		var tools []string
		var files []string
		seenTool := map[string]bool{}
		for i := len(m.messages) - 1; i >= 0 && prompt == ""; i-- {
			if m.messages[i].Role == "user" {
				prompt = m.messages[i].Content
			}
		}
		hadErrors := false
		for _, te := range m.toolEvents {
			if te.Kind != "result" {
				continue
			}
			if !seenTool[te.ToolName] {
				seenTool[te.ToolName] = true
				tools = append(tools, te.ToolName)
			}
			if te.IsError {
				hadErrors = true
			}
			if path := stringField(te.Input, "path", "file_path"); path != "" {
				files = append(files, basename(path))
			}
		}
		_ = runtime.AppendLearned(runtime.LearnedRecord{
			Timestamp:  time.Now(),
			SessionID:  m.sessionID,
			Prompt:     truncate(prompt, 200),
			ToolUsed:   tools,
			FilesTouch: files,
			Duration:   formatTurnDuration(time.Since(m.spinnerStartedAt)),
			Tokens:     m.totalTokens.Total(),
			HadErrors:  hadErrors,
			Recap:      recap,
		})
	}
	m.persistTail()
	if err != nil {
		// finalizeTurn fires when the loop's done channel returns. Most
		// of the time that error has ALREADY been surfaced via the
		// EventError path (tui_events.go), with formatProviderError
		// applied. Re-emitting here in raw "error: " format created
		// duplicate rows — one prettified ("API Error: ..."), one raw
		// JSON dump — which the user reported in image #9.
		//
		// Fix: route through formatProviderError too, AND dedupe
		// against the last error row so we don't double-print the
		// same failure under two formats.
		errMsg, _ := formatProviderError(err.Error())
		formatted := "API Error: " + errMsg
		alreadyShown := false
		for i := len(m.messages) - 1; i >= 0; i-- {
			r := m.messages[i].Role
			if r == "error-hint" {
				continue
			}
			if r == "error" {
				// Same content (or same content with `(×N)` suffix)
				// means EventError already surfaced this; skip the
				// redundant finalizeTurn echo.
				if strings.HasPrefix(m.messages[i].Content, formatted) {
					alreadyShown = true
				}
			}
			break
		}
		if !alreadyShown {
			m.messages = append(m.messages, Message{Role: "error", Content: formatted, Timestamp: time.Now()})
		}
	}
	// Queued messages — claude-code parity. After the turn finishes,
	// drain the highest-priority batch (priority-aware) and submit
	// it as the next user turn. Multi-item batches join with blank
	// lines so the model sees one merged message instead of N
	// round-trips (claude-code messageQueueManager `dequeueAllMatching`
	// parity). We only run on success: a turn that errored out
	// leaves the queue intact so the user can retry or wipe via
	// Ctrl+C.
	if err == nil && len(m.queuedPrompts) > 0 {
		nextText, batchN := m.drainNextQueuedBatch()
		if batchN > 0 {
			var notice string
			if batchN == 1 {
				notice = fmt.Sprintf("(dequeued · %d remaining)", len(m.queuedPrompts))
			} else {
				notice = fmt.Sprintf("(dequeued ×%d merged · %d remaining)", batchN, len(m.queuedPrompts))
			}
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   notice,
				Timestamp: time.Now(),
			})
			m.input.SetValue(nextText)
			// Defer the actual submit one tick — finalizeTurn is on
			// the fast path, and submitInput needs a fresh frame to
			// read the updated input value.
			m.queuePending = true
		}
	}
}

func (m *Model) persistTail() {
	if m.session == nil || m.sessionID == "" {
		return
	}
	hist := m.loop.History()
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == llm.RoleUser && len(hist[i].Content) > 0 && hist[i].Content[0].Type == "text" {
			for j := i + 1; j < len(hist); j++ {
				_ = m.session.AppendMessage(m.sessionID, hist[j])
			}
			return
		}
	}
}

// runTurnAsync runs one agent turn in a goroutine. It is a FREE
// FUNCTION (not a method) — all Model state it needs is passed in
// by value at the call site. This rules out the data race fixed on
// 2026-05-20:
//
//   pre-fix (method on *Model):
//     - goroutine read m.ctx, wrote m.turnCancel, read m.loop, used
//       m.eventCh and m.doneCh — all while the main bubbletea
//       Update goroutine could legitimately mutate those same
//       fields (test pressEnter ran Update which queued another
//       handleSubmit BEFORE the previous turn's runTurnAsync had
//       drained). go test -race caught it in
//       TestChatSubmit_RuneByRuneSubmission /
//       TestChatSubmit_RuneByRuneMultibyte /
//       TestSlashE2E_BatchRewritesPrompt.
//
//   post-fix (free function):
//     - caller snapshots m.ctx → derives turnCtx, stores cancel
//       into m.turnCancel BEFORE the `go` statement (single
//       writer on main thread)
//     - caller snapshots m.loop, m.eventCh, m.doneCh
//     - goroutine touches no Model state — only the passed-in
//       values + the events channel forwarding loop
//     - turnCancel cleanup moves to finalizeTurn (main thread,
//       reached via doneCh handler) so we never write to Model
//       state from inside the goroutine
//
// The eventCh / doneCh channels themselves are concurrent-safe
// (Go's channel semantics) so writing to them from the goroutine
// is fine — what we removed was the m.field READS that the race
// detector flagged.
func runTurnAsync(
	turnCtx context.Context,
	cancel context.CancelFunc,
	loop *agent.Loop,
	eventCh chan<- agent.Event,
	doneCh chan<- error,
) {
	defer cancel()
	events := make(chan agent.Event, eventBufferSize())
	done := make(chan error, 1)
	go func() { done <- loop.Run(turnCtx, events); close(events) }()
	for ev := range events {
		eventCh <- ev
	}
	doneCh <- <-done
}
