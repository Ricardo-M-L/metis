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

	tea "github.com/charmbracelet/bubbletea"
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
		switch msg.Type {
		case tea.KeyUp, tea.KeyDown, tea.KeyTab, tea.KeyEscape:
			return m.handlePaletteKey(msg)
		}
		// fall through for typed characters / Enter / Backspace
	}

	// History-search overlay claims ALL keys while open — typing
	// narrows the filter, ↑↓ selects, Enter copies into input, Esc
	// cancels. We don't fall through to the editor while it's up.
	if m.showHistory {
		return m.handleHistoryKey(msg)
	}

	// At-mention dropdown intercepts ONLY navigation + selection keys.
	// Typed runes still go to the textarea so the user keeps composing
	// `@filt` and the dropdown narrows live. Tab / Enter accept the
	// highlighted match; Esc dismisses.
	if m.atActive && len(m.atMatched) > 0 {
		switch msg.Type {
		case tea.KeyUp:
			if m.atCursor > 0 {
				m.atCursor--
			}
			return m, nil
		case tea.KeyDown:
			if m.atCursor < len(m.atMatched)-1 {
				m.atCursor++
			}
			return m, nil
		case tea.KeyTab:
			m.acceptAtMention()
			return m, nil
		case tea.KeyEscape:
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
	switch msg.Type {
	case tea.KeyCtrlC:
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

	case tea.KeyCtrlD:
		return m, tea.Quit

	case tea.KeyCtrlL:
		m.messages = append(m.messages, Message{Role: "info", Content: "models: claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5 — use /model <name> to switch", Timestamp: time.Now()})
		return m, nil

	case tea.KeyCtrlP:
		return m.handleSessionPick()

	case tea.KeyCtrlT:
		// Toggle the task panel — claude-code's Ctrl+T affordance.
		m.showTaskPanel = !m.showTaskPanel
		return m, nil

	case tea.KeyCtrlS:
		// Toggle copy mode — exit alt-screen briefly so user can
		// mouse-select-and-copy chat content from native scrollback.
		return m.toggleCopyMode()

	case tea.KeyCtrlR:
		// Bash-style history search. Lazy-loads ~/.metis/history.jsonl
		// on first open. While the overlay is up, all keys route to
		// handleHistoryKey above so they don't leak into the editor.
		m.openHistorySearch()
		return m, nil

	case tea.KeyCtrlY:
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

	case tea.KeyCtrlV:
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
			}
		} else if content.Mime == "text/plain" {
			m.input.InsertString(string(content.Data))
		}
		return m, nil

	case tea.KeyCtrlO:
		// Toggle global "expand truncated output" — claude-code's ctrl+o.
		m.expandToolOutputs = !m.expandToolOutputs
		state := "off"
		if m.expandToolOutputs {
			state = "on"
		}
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "expand tool output: " + state,
			Timestamp: time.Now(),
		})
		return m, nil

	case tea.KeyEscape:
		// /btw modal takes priority — dismiss it without disturbing
		// other state.
		if m.btwActive {
			m.dismissBtw()
			return m, nil
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

	case tea.KeyEnter:
		// Alt+Enter inserts a literal newline so the user can compose
		// multi-line prompts. Plain Enter submits.
		if msg.Alt {
			m.input.InsertRune('\n')
			return m, nil
		}
		return m.handleSubmit()

	case tea.KeyCtrlJ:
		// Ctrl+J — alternate "newline" keybind for terminals that don't
		// distinguish Alt+Enter from plain Enter.
		m.input.InsertRune('\n')
		return m, nil

	case tea.KeyTab:
		m.doAutocomplete()
		return m, nil

	case tea.KeyShiftTab:
		m.cyclePermissionMode()
		return m, nil

	case tea.KeyPgUp:
		m.viewport.ScrollUp(m.viewport.Height / 2)
		return m, nil

	case tea.KeyPgDown:
		m.viewport.ScrollDown(m.viewport.Height / 2)
		return m, nil

	case tea.KeyHome:
		// Ctrl+Home / Home — jump to top of transcript. Bubble's
		// textarea also binds Home but only when the input has content;
		// when empty (the common scroll-back case) we route to viewport.
		if strings.TrimSpace(m.input.Value()) == "" {
			m.viewport.GotoTop()
			return m, nil
		}
	case tea.KeyEnd:
		if strings.TrimSpace(m.input.Value()) == "" {
			m.viewport.GotoBottom()
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

	// While a turn is in flight the input is read-only — only allow
	// navigation/selection so the user can still copy from the
	// previously typed line.
	if m.turnActive {
		switch msg.Type {
		case tea.KeyLeft, tea.KeyRight, tea.KeyHome, tea.KeyEnd:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		return m, nil
	}

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
	val := m.input.Value()
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

	// claude-code-style live palette: opens as soon as the user types "/",
	// filter follows the rest of the buffer, closes when "/" is gone.
	if strings.HasPrefix(val, "/") {
		m.showPalette = true
		m.palFilter = strings.TrimPrefix(val, "/")
		m.matchCommands()
	} else if m.showPalette {
		m.showPalette = false
		m.palFilter = ""
	}

	return m, cmd
}
