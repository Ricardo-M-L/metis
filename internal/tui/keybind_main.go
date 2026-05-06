package tui

// keybind_main.go — top-level handleKey dispatcher. Routes key events
// to the right sub-handler (palette / permission / submit / vim) and
// owns the special-key shortcuts (Ctrl-C double-tap, Ctrl-D quit,
// Ctrl-S copy mode, Ctrl-V clipboard paste, etc.).

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.permActive {
		return m.handlePermKey(msg)
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
		if !m.turnActive {
			return m, tea.Quit
		}
		// During an active turn, Ctrl-C cancels the turn (so the
		// session stays alive) — same as claude-code. A second
		// Ctrl-C *during the same turn* exits, which is rare in
		// practice but the right escape hatch when cancellation
		// itself hangs.
		if !m.lastCtrlC.IsZero() && time.Since(m.lastCtrlC) < ctrlCQuitWindow {
			return m, tea.Quit
		}
		m.lastCtrlC = time.Now()
		if m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
		}
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "interrupted · Ctrl-C again to exit",
			Timestamp: time.Now(),
		})
		return m, nil

	case "ctrl+d":
		return m, tea.Quit

	case "ctrl+l":
		m.messages = append(m.messages, Message{Role: "info", Content: "models: claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5 — use /model <name> to switch", Timestamp: time.Now()})
		return m, nil

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
				// F19: surface a tiny info row so the user sees the
				// image was actually cached + where. Without this
				// the only feedback is the [Image #N] placeholder
				// in the editor, which doesn't tell the user the
				// MB-size dump on their disk.
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
		m.chatList.ScrollBy(-m.chatList.Height() / 2)
		return m, nil

	case "pgdown":
		m.chatList.ScrollBy(m.chatList.Height() / 2)
		return m, nil

	case "home":
		// Ctrl+Home / Home — jump to top of transcript. Bubble's
		// textarea also binds Home but only when the input has content;
		// when empty (the common scroll-back case) we route to chatList.
		if strings.TrimSpace(m.input.Value()) == "" {
			m.chatList.ScrollToTop()
			return m, nil
		}
	case "end":
		if strings.TrimSpace(m.input.Value()) == "" {
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
			m.loop.Reset()
			m.messages = nil
			m.toolEvents = nil
			m.input.Reset()
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
	if len(val) > 0 && val[0] == '/' {
		m.showPalette = true
		m.palFilter = val[1:]
		m.matchCommands()
	} else if m.showPalette {
		m.showPalette = false
		m.palFilter = ""
	}

	return m, cmd
}
