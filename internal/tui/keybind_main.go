package tui

// keybind_main.go — top-level handleKey dispatcher. Routes key events
// to the right sub-handler (palette / permission / submit / vim) and
// owns the special-key shortcuts (Ctrl-C double-tap, Ctrl-D quit,
// Ctrl-S copy mode, Ctrl-V clipboard paste, etc.).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// exitFunc is the process-termination hook used by the Ctrl-C double-
// tap belt-and-braces path. Default is os.Exit, but tests override it
// to a no-op so the 800 ms-scheduled goroutine doesn't kill the test
// binary mid-suite. Caught 2026-05-20: TestCtrlC_DoubleTapDuringTurnQuits
// triggered the path, the goroutine fired ~800 ms later during a
// downstream test, and os.Exit(0) marked the whole `go test
// ./internal/tui/...` run as FAIL despite every individual test
// passing.
var exitFunc = os.Exit

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.permActive {
		// Permission prompt consumes ONLY navigation/decision keys
		// (arrows, enter/space, esc). Any other key falls through to
		// the main dispatch below so the user can keep composing the
		// next message while the permission popup waits for an answer
		// — image #18 user feedback. claude-code/crush/opencode all
		// block the editor entirely here; metis is intentionally more
		// permissive because the user reported the lock as a friction
		// point during heavy multi-tool turns.
		if model, cmd, handled := m.handlePermKey(msg); handled {
			return model, cmd
		}
	}
	if m.askUserActive {
		// AskUser prompt — same partial-intercept policy as
		// permission. Navigation / selection / dismiss keys (arrows,
		// enter, 1-9, esc, tab) and the freeform input's keystrokes
		// are consumed here; everything else falls through so the user
		// can still compose the next message in the background.
		if model, cmd, handled := m.handleAskUserKey(msg); handled {
			return model, cmd
		}
	}
	// Palette is a hovering suggestion layer rather than a modal dialog —
	// it intercepts only navigation keys (Up/Down/Tab/Esc) and lets every
	// other key fall through to the main input handler so the user keeps
	// typing into inputBuffer naturally. claude-code does the same.
	if m.showPalette {
		// v2: KeyMsg is interface, .Type gone. Match by .String().
		switch msg.String() {
		case "up", "down", "tab", "esc":
			return m.handlePaletteKey(msg)
		}
		// fall through for typed characters / Enter / Backspace
	}

	// History-search overlay claims ALL keys while open — typing
	// narrows the filter, ↑↓ selects, Enter copies into input, Esc
	// cancels. We don't fall through to the editor while it's up.
	if m.showSearch {
		// Transcript-search overlay (Ctrl+F). Esc closes; Enter jumps
		// to the current hit; Ctrl+N/P walk hits; chars filter.
		if kp, ok := msg.(tea.KeyPressMsg); ok {
			cmd, _ := m.handleSearchKey(kp)
			return m, cmd
		}
	}
	if m.showHistory {
		return m.handleHistoryKey(msg)
	}

	// At-mention dropdown intercepts ONLY navigation + selection keys.
	// Typed runes still go to the textarea so the user keeps composing
	// `@filt` and the dropdown narrows live. Tab / Enter accept the
	// highlighted match; Esc dismisses.
	if m.atActive && len(m.atMatched) > 0 {
		switch msg.String() {
		case "up":
			if m.atCursor > 0 {
				m.atCursor--
			}
			return m, nil
		case "down":
			if m.atCursor < len(m.atMatched)-1 {
				m.atCursor++
			}
			return m, nil
		case "tab":
			m.acceptAtMention()
			return m, nil
		case "esc":
			m.atActive = false
			return m, nil
			// Enter is intentionally NOT consumed — user typing prompt
			// + Enter to submit shouldn't have to also dismiss the
			// dropdown. The submit handler picks the typed `@xxx` as-
			// is; if user wanted the highlighted one, they hit Tab.
		}
	}

	// Special keys we handle ourselves (Ctrl-C, Enter, Shift+Tab, etc.).
	// Anything not matched falls through to bubbles/textinput.
	//
	// v2: KeyMsg is an interface; switch on .String() with named keys
	// like "ctrl+c", "alt+enter", "pgup". This subsumes both the v1
	// `switch msg.Type` and per-case Alt-modifier checks (msg.Alt is
	// gone; alt-modified keys arrive as e.g. "alt+enter" directly).
	switch msg.String() {
	case "ctrl+c":
		// Single-press exit when idle (no turn running). The user
		// flagged the previous double-tap-to-exit as "still won't
		// quit" — once you're at a quiet prompt with no active LLM
		// call, Ctrl-C should behave like the standard CLI shortcut
		// and just leave. The input may contain garbage from OSC
		// leaks; we don't care, we exit either way.
		//
		// Same hard-exit safety net as the mid-turn second-press
		// path: if bubbletea's clean shutdown doesn't return within
		// 800ms (e.g. an MCP child still alive on its socket),
		// os.Exit so the user doesn't have to `pkill metis`. Run
		// resetTerminal first so mouse-tracking / kitty-keyboard
		// sequences are disabled before the process disappears —
		// otherwise the next mouse motion in the shell echoes raw
		// `<col;row;buttonM` SGR reports (image bug 2026-05-15).
		if !m.turnActive {
			savedTermios := m.savedTermios
			go func() {
				time.Sleep(800 * time.Millisecond)
				resetTerminal(savedTermios)
				exitFunc(0)
			}()
			return m, tea.Quit
		}
		// During an active turn, Ctrl-C cancels the turn (so the
		// session stays alive) — same as claude-code. A second
		// Ctrl-C *during the same turn* exits, which is rare in
		// practice but the right escape hatch when cancellation
		// itself hangs.
		if !m.lastCtrlC.IsZero() && time.Since(m.lastCtrlC) < ctrlCQuitWindow {
			// Defense in depth — bubbletea's clean shutdown sometimes
			// blocks behind a wedged in-flight goroutine (LLM stream
			// stuck in a kernel read, MCP child process not honouring
			// SIGTERM, etc.). User reported needing to `pkill metis` to
			// recover (image #2 follow-up 2026-05-10). Belt-and-braces
			// here:
			//   1. Cancel the turn context one more time (idempotent
			//      if already cancelled). This is the polite path —
			//      pending network reads return ctx.Canceled and their
			//      goroutines exit.
			//   2. Schedule a hard os.Exit(0) deadline of 800ms. If
			//      the polite path hasn't killed the process by then,
			//      something is stuck below user-space — exit anyway
			//      so the user gets their shell back without `pkill`.
			//      800ms because tea.Quit + alt-screen restore + defer
			//      cleanup typically takes <200ms; 800ms gives 4× safety
			//      margin while still feeling instant to the user.
			//      Critically: resetTerminal runs before os.Exit so
			//      mouse-tracking / alt-screen / kitty-keyboard
			//      sequences are flushed; otherwise the next mouse
			//      motion in the shell echoes raw SGR mouse reports
			//      because os.Exit skips RunTUI's deferred resetTerminal.
			if m.turnCancel != nil {
				m.turnCancel()
				m.turnCancel = nil
			}
			savedTermios := m.savedTermios
			go func() {
				time.Sleep(800 * time.Millisecond)
				resetTerminal(savedTermios)
				exitFunc(0)
			}()
			return m, tea.Quit
		}
		m.lastCtrlC = time.Now()
		if m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
		}
		// Drop any queued prompts when the user cancels — claude-code
		// behavior. A user hitting Ctrl+C is saying "stop everything";
		// silently letting the queue drain after the cancellation
		// would be surprising. Visible message tells them what we did.
		queueCleared := len(m.queuedPrompts)
		m.queuedPrompts = nil
		m.queuePending = false
		msg2 := "interrupted · Ctrl-C again to exit"
		if queueCleared > 0 {
			msg2 = fmt.Sprintf("interrupted · queue cleared (%d dropped) · Ctrl-C again to exit", queueCleared)
		}
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   msg2,
			Timestamp: time.Now(),
		})
		return m, nil

	case "ctrl+d":
		return m, tea.Quit

	case "ctrl+l":
		m.messages = append(m.messages, Message{Role: "info", Content: "models: claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5 — use /model <name> to switch", Timestamp: time.Now()})
		return m, nil

	case "ctrl+x":
		// Phase F #62 — shell-mode toggle. Flips `m.shellMode`; the
		// submit handler (keybind_submit.go) checks the flag and, when
		// on, dispatches the input to bash via the runGitCmd-shaped
		// helper instead of the agent loop. Idempotent toggle: second
		// Ctrl+X returns to agent mode.
		m.shellMode = !m.shellMode
		state := "off"
		if m.shellMode {
			state = "on — next input runs as `bash -c <input>`"
		}
		m.messages = append(m.messages, Message{
			Role: "info", Content: "shell mode: " + state, Timestamp: time.Now(),
		})
		return m, nil

	case "ctrl+b":
		// Phase F (2026-05-12) — background the current turn.
		// Toggle: pressing again foregrounds. Only valid mid-turn;
		// at idle it's a no-op with an info hint so the user learns
		// what the binding does without confusion. While
		// backgrounded:
		//   - the turn KEEPS running (no ctx cancel)
		//   - streaming text + thinking stop mirroring to visible chat
		//     (still accumulates so finalizeTurn flushes the full reply
		//     atomically when the turn ends)
		//   - the spinner shrinks to a one-line "bg Xs" status chip
		//   - any newly-typed prompts queue via the existing
		//     queuedPrompts FIFO and auto-dispatch when the bg turn ends
		// finalizeTurn fires a desktop notification on bg-turn end
		// regardless of duration (the user backgrounded explicitly,
		// they want to know).
		if !m.turnActive {
			m.messages = append(m.messages, Message{
				Role: "info",
				Content: "Ctrl+B backgrounds the current turn — press while a turn is running, " +
					"then keep typing or look away. You'll be notified when it finishes.",
				Timestamp: time.Now(),
			})
			return m, nil
		}
		m.turnBackgrounded = !m.turnBackgrounded
		if m.turnBackgrounded {
			m.backgroundedAt = time.Now()
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "✻ turn moved to background — output suppressed until done · Ctrl+B to foreground · Ctrl+C cancels",
				Timestamp: time.Now(),
			})
		} else {
			m.backgroundedAt = time.Time{}
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "✻ turn back in foreground — streaming output resumes",
				Timestamp: time.Now(),
			})
		}
		return m, nil

	case "ctrl+g":
		// External editor — Phase D #41. Drops the current input into
		// a temp file, suspends bubbletea, spawns $EDITOR, then reads
		// the file back into the textarea on exit. Ergonomics match
		// Bash's `Ctrl+X Ctrl+E` (we use Ctrl+G because Ctrl+X is
		// already a pending-action lead-in for vim/copy modes).
		// $EDITOR resolution mirrors /mcp edit + /skills edit.
		return m, m.openExternalEditor()

	case "ctrl+p":
		return m.handleSessionPick()

	case "ctrl+t":
		// Toggle the task panel — claude-code's Ctrl+T affordance.
		m.showTaskPanel = !m.showTaskPanel
		return m, nil

	case "ctrl+s":
		// Toggle copy mode — exit alt-screen briefly so user can
		// mouse-select-and-copy chat content from native scrollback.
		return m.toggleCopyMode()

	case "ctrl+r":
		// Bash-style history search. Lazy-loads ~/.metis/history.jsonl
		// on first open. While the overlay is up, all keys route to
		// handleHistoryKey above so they don't leak into the editor.
		m.openHistorySearch()
		return m, nil

	case "ctrl+f":
		// In-transcript full-text search (F10). Distinct from Ctrl+R
		// (cross-session prompt history). Opens a small overlay
		// above the input with /query/ + n/p navigation. Esc closes.
		m.openTranscriptSearch()
		return m, nil

	case "ctrl+y":
		// Yank the last assistant reply to the system clipboard via
		// OSC 52. Vim's `y` muscle memory for "copy this." Useful in
		// alt-screen mode where rubber-band selection is unreliable
		// (mouse cell motion intercepts drag events). Falls back to
		// writing ~/.metis/clipboard.txt for terminals without OSC 52.
		status := m.yankLastAssistant()
		m.messages = append(m.messages, Message{
			Role: "info", Content: status, Timestamp: time.Now(),
		})
		return m, nil

	case "ctrl+shift+y":
		// Yank the FULL transcript (every user/assistant/bash row) to
		// the clipboard. Useful when the user wants to dump a whole
		// session into a bug report, blog post, or PR description.
		// Filtered: thinking traces, info rows, status / metadata
		// stay out of the export so it reads as a conversation.
		status := m.yankFullTranscript()
		m.messages = append(m.messages, Message{
			Role: "info", Content: status, Timestamp: time.Now(),
		})
		return m, nil

	case "ctrl+v":
		// Read the system clipboard. Image content is saved to
		// ~/.metis/cache/ and shown as `[Image #N]` (claude-code's
		// pasted-image placeholder); the actual path is resolved at
		// submit time via expandPastedImages. Text content goes in
		// at cursor. Falls through silently on empty clipboard / no
		// reader installed (xclip etc.).
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
		defer cancel()
		content, _ := readClipboard(ctx)
		if content == nil {
			return m, nil
		}
		if content.Mime == "image/png" || content.Mime == "image/jpeg" || content.Mime == "image/png-base64" {
			path, err := saveClipboardImage(content.Data, content.Mime)
			if err == nil {
				if m.imagePaste == nil {
					m.imagePaste = map[int]string{}
				}
				m.imageCounter++
				m.imagePaste[m.imageCounter] = path
				m.input.InsertString(fmt.Sprintf("[Image #%d] ", m.imageCounter))
				// Match claude-code: the [Image #N] chip in the input
				// editor is the only on-screen artifact. The path/size
				// info that used to land in chat history was redundant
				// (the side table m.imagePaste records the path for
				// submit-time resolution via expandPastedImages); kept
				// adding a gray row per paste that the user flagged
				// as noise.
			} else {
				m.messages = append(m.messages, Message{
					Role:      "error",
					Content:   fmt.Sprintf("paste failed: %v", err),
					Timestamp: time.Now(),
				})
			}
		} else if content.Mime == "text/plain" {
			m.input.InsertString(string(content.Data))
		}
		return m, nil

	case "ctrl+o":
		// Toggle global "expand truncated output" — claude-code's ctrl+o.
		m.expandToolOutputs = !m.expandToolOutputs
		state := "off"
		if m.expandToolOutputs {
			state = "on"
		}
		newContent := "expand tool output: " + state
		// REPLACE the trailing info message instead of appending a new
		// one when the previous message is also an `expand tool
		// output: …` toggle. Otherwise rapid ctrl+O presses pile up
		// "off / on / off / on / off" rows in the transcript.
		// Feedback 2026-05-05.
		if n := len(m.messages); n > 0 &&
			m.messages[n-1].Role == "info" &&
			strings.HasPrefix(m.messages[n-1].Content, "expand tool output:") {
			m.messages[n-1].Content = newContent
			m.messages[n-1].Timestamp = time.Now()
		} else {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   newContent,
				Timestamp: time.Now(),
			})
		}
		return m, nil

	case "esc":
		// Overlay stack takes priority — if any overlay's Update
		// consumes the Esc, we stop here. Otherwise fall through to
		// the existing palette / vim / double-tap-clear handlers.
		if m.overlays.Active() {
			cmd, consumed := m.overlays.Update(msg)
			if consumed {
				return m, cmd
			}
		}
		// Vim mode hijack: ESC in INSERT mode goes to NORMAL.
		if vimModeState == vimInsert {
			vimModeState = vimNormal
			return m, nil
		}
		// 2026-05-23: when a turn is in flight, ESC cancels it —
		// matches claude-code behavior and what the spinner's hint
		// "press esc to interrupt" promises. Pre-fix ESC only ever
		// touched the input box; the user with a hung deepseek
		// stream (image #59 — 19m44s stuck spinner) hit ESC ESC
		// expecting to bail out and got nothing. Now the first ESC
		// during a live turn delivers the cancellation; subsequent
		// double-tap-clear logic only fires when no turn is running.
		if m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
			// Drop queued prompts so the cancellation actually stops
			// everything (parity with Ctrl+C handler above).
			queueCleared := len(m.queuedPrompts)
			m.queuedPrompts = nil
			m.queuePending = false
			msg2 := "interrupted (esc)"
			if queueCleared > 0 {
				msg2 = fmt.Sprintf("interrupted (esc) · dropped %d queued", queueCleared)
			}
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   msg2,
				Timestamp: time.Now(),
			})
			m.lastEsc = time.Time{}
			return m, nil
		}
		// Single ESC: dismiss palette / pending state, leave typed
		// input alone. Double-tap (within doubleEscWindow): clear the
		// input completely. Mirrors claude-code's "double tap esc to
		// clear input" hint.
		const doubleEscWindow = 500 * time.Millisecond
		if !m.lastEsc.IsZero() && time.Since(m.lastEsc) < doubleEscWindow {
			m.input.Reset()
			m.showPalette = false
			m.palFilter = ""
			m.lastEsc = time.Time{}
			return m, nil
		}
		m.lastEsc = time.Now()
		if m.showPalette {
			m.showPalette = false
			m.palFilter = ""
		}
		return m, nil

	case "alt+enter":
		// Alt+Enter inserts a literal newline so the user can compose
		// multi-line prompts. v2: alt-modified keys arrive as
		// "alt+enter" directly via String() (msg.Alt field is gone).
		m.input.InsertRune('\n')
		return m, nil

	case "alt+y":
		// Yank the current input box content to the system clipboard.
		// Complements Ctrl+Y (yank last assistant reply). Addresses
		// user screenshot 34 / 2026-05-16: "蓝色框起来的输入框的内容不
		// 能复制, 鼠标选中文字没显示选中的阴影" — bubbletea's mouse
		// cell-motion mode eats drag events so terminal-native
		// selection can't reach the input region. A one-keystroke
		// "copy whatever I'm typing" gives them the same outcome
		// without disabling mouse capture or switching modes.
		v := m.input.Value()
		if v == "" {
			m.messages = append(m.messages, Message{
				Role: "info", Content: "(input box is empty — nothing to copy)",
				Timestamp: time.Now(),
			})
			return m, nil
		}
		writeClipboard(v)
		m.messages = append(m.messages, Message{
			Role: "info",
			Content: fmt.Sprintf("(copied %d chars from input — %s)",
				len(v), osc52Status()),
			Timestamp: time.Now(),
		})
		return m, nil

	case "enter":
		return m.handleSubmit()

	case "ctrl+j":
		// Ctrl+J — alternate "newline" keybind for terminals that don't
		// distinguish Alt+Enter from plain Enter.
		m.input.InsertRune('\n')
		return m, nil

	case "tab":
		m.doAutocomplete()
		return m, nil

	case "shift+tab":
		m.cyclePermissionMode()
		return m, nil

	case "pgup":
		// User scrolled away from the live tail — claude-code's
		// useVirtualScroll flips isSticky false on every wheel-up /
		// PgUp. Once unstuck, View() stops auto-snapping to bottom on
		// the next streaming tick so the user can read older history.
		m.stickyBottom = false
		m.chatList.ScrollBy(-m.chatList.Height() / 2)
		return m, nil

	case "pgdown":
		m.chatList.ScrollBy(m.chatList.Height() / 2)
		// Re-stick if PgDn carried us back to the bottom. Same gesture
		// as wheel-down hitting the floor — the user signalled "I'm
		// caught up, follow the stream again."
		if m.chatList.AtBottom() {
			m.stickyBottom = true
		}
		return m, nil

	case "home":
		// Ctrl+Home / Home — jump to top of transcript. Bubble's
		// textarea also binds Home but only when the input has content;
		// when empty (the common scroll-back case) we route to chatList.
		if strings.TrimSpace(m.input.Value()) == "" {
			m.stickyBottom = false
			m.chatList.ScrollToTop()
			return m, nil
		}
	case "end":
		if strings.TrimSpace(m.input.Value()) == "" {
			m.stickyBottom = true
			m.chatList.ScrollToBottom()
			return m, nil
		}
	}

	// Mouse wheel scrolling. Bubbletea routes WheelUp/WheelDown via
	// MouseMsg, not KeyMsg — handled here because MouseMsg arrives in
	// the same Update loop. Three-line scroll matches claude-code's
	// React/Ink behavior where each wheel detent steps the viewport
	// by 3 lines.

	// Vim NORMAL mode: intercept all keypresses BEFORE the textarea
	// sees them. ESC was already handled above.
	if vimModeState == vimNormal {
		handled, cmd := m.handleVimNormalKey(msg)
		if handled {
			return m, cmd
		}
	}

	// Direct history navigation (T7): when the input is empty or its
	// content was loaded from history, ↑/↓ walks through prior prompts
	// instead of moving the textarea cursor between rows. Saves a
	// Ctrl+R round-trip for the common "what did I type last" case.
	//
	// Single-line non-empty input (the user typed but hasn't wrapped to
	// a second visual row): ↑ = CursorStart, ↓ = CursorEnd. The default
	// textarea behaviour was a no-op there (no row above / below to
	// land on), which read as "arrows are broken" — image+video user
	// report 2026-05-07: "输入向上箭头他还是不能跳到这个输入框最开始
	// 的地方". macOS text-field convention is Cmd+↑/↓ for jump-to-edge,
	// but with no Cmd modifier reaching the TUI we surface the same
	// behaviour on plain ↑/↓ for this single-row case.
	//
	// Once the cursor IS at col 0 (line 0), ↑ then hands off to
	// directHistoryUp via the expanded directHistoryEligible — the
	// second-press history load matches claude-code behaviour. User
	// report 2026-05-16 (screenshot 32): "把光标放到最开始的位置然
	// 后按向上 向下箭头就能切换这个会话的历史 query, claude code 可以".
	switch msg.String() {
	case "up":
		if m.directHistoryEligible() && m.directHistoryUp() {
			return m, nil
		}
		if m.input.LineCount() <= 1 && m.input.Value() != "" {
			m.input.CursorStart()
			return m, nil
		}
	case "down":
		if m.histDirectIdx >= 0 && m.directHistoryDown() {
			return m, nil
		}
		if m.input.LineCount() <= 1 && m.input.Value() != "" {
			m.input.CursorEnd()
			return m, nil
		}
	}

	// Any other key during nav-mode means the user is editing — exit
	// nav so the next ↑ starts fresh from the (just-mutated) draft.
	if m.histDirectIdx >= 0 {
		m.resetDirectHistoryNav()
	}

	// While a turn is in flight, let the user keep typing the next prompt
	// — claude-code parity. Submit (Enter) is still blocked by
	// handleSubmit's `if m.turnActive` guard which surfaces a
	// "(turn still running ...)" hint instead of double-spawning a turn.
	// The previous "drop every key except ←→Home/End" behavior was
	// user-hostile: a 2-minute generation locked the keyboard so the
	// user couldn't queue follow-up prompts or even fix typos in the
	// current input.

	// All other keys go to bubbles/textinput.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	// After every keystroke, re-scan the buffer for an `@xxx` token at
	// the cursor and rebuild the at-mention dropdown. Cheap (cached
	// file index, ~5k entries max) and gives the user live feedback as
	// they type — without this, @-completion needed a blind guess +
	// Tab to confirm the first match.
	m.updateAtMention()

	// Defensively scrub terminal escape responses that bubbletea's
	// parser sometimes lets leak into the textarea — iTerm2's OSC 11
	// background-color reply (`]11;rgb:158e/193a/1e75\`) and SGR mouse
	// events (`<66;80;12M`) can both arrive char-by-char, the leading
	// ESC bytes get consumed but the body of the sequence ends up
	// typed into the input. The user's "11;rgb:158e/193a/1e75\<66;80;12M"
	// screenshot was exactly this: OSC body + mouse event concatenated
	// after each turn boundary.
	//
	// Strategy: scan the input, strip any matching escape-body pattern,
	// and only re-set the textarea if we changed the value. Doing it
	// this way preserves any user-typed text that happened to share a
	// prefix with an escape sequence.
	if v := m.input.Value(); v != "" {
		if cleaned := scrubEscapeLeaks(v); cleaned != v {
			m.input.SetValue(cleaned)
			m.input.CursorEnd()
			return m, cmd
		}
	}

	// Slash command shortcuts: a complete "/quit" or "/clear" submitted
	// via typing alone (no Enter) still takes effect.
	//
	// Mid-turn protection (Task #87): destructive shortcuts skip the
	// live-fire path when a turn is active, falling through to
	// handleSubmit's MidTurnDestructive branch which surfaces the
	// "press Esc to cancel first" hint. Without this guard, /clear
	// typed during a running turn would silently wipe state without
	// the user pressing Enter — exactly the kind of accidental
	// destruction the mid-turn refusal exists to prevent.
	val := m.input.Value()
	if !m.turnActive {
		if val == "/quit" || val == "/exit" {
			return m, tea.Quit
		}
		if val == "/clear" || val == "/reset" {
			m.Reload(ReloadOpts{})
			return m, cmd
		}
	}

	// claude-code-style live palette: opens as soon as the user types
	// the literal forward slash "/" (U+002F), filter follows the rest
	// of the buffer, closes when "/" is gone.
	//
	// We check rune-by-rune (val[0] == '/') instead of HasPrefix so
	// that visually similar prefixes — backslash "\\" (U+005C),
	// fullwidth solidus "／" (U+FF0F), division slash "∕" (U+2215),
	// fraction slash "⁄" (U+2044) — DON'T trigger the palette. The
	// user reported on 2026-05-01 that backslash also opened commands;
	// that turned out to be a vncdotool keysym mis-mapping rather than
	// a metis bug, but defense-in-depth here makes the contract
	// explicit: only literal U+002F counts.
	// Slash-command palette gating. Naive `val[0] == '/'` mis-fires on
	// pasted absolute paths (`/Users/...`) — the palette pops up "no
	// match for /Users/..." which is glaring noise. Real slash
	// commands are `/<name>[ args]` where <name> is alphanum + underscore
	// only; paths contain at least one more `/` before the first space.
	// Discriminate on that: head-token (everything before first space)
	// must NOT contain a second `/` to count as a slash command.
	if len(val) > 0 && val[0] == '/' && !looksLikeSlashPath(val) {
		m.showPalette = true
		m.palFilter = val[1:]
		m.matchCommands()
	} else if m.showPalette {
		m.showPalette = false
		m.palFilter = ""
	}

	return m, cmd
}
