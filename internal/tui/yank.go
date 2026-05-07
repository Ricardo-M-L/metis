package tui

// yank.go — Ctrl+Y copies the last assistant reply (or the user's
// requested target) to the system clipboard.
//
// Why this exists: bubbletea's alt-screen + mouse-cell-motion combo
// makes terminal-native rubber-band selection unreliable (image #10
// user complaint). Ctrl+S exists and is the most powerful escape
// hatch (drops to a plain transcript), but it's heavy. Ctrl+Y gives
// a one-keystroke shortcut for the common case: "I want to grab the
// model's last response."
//
// Transport: OSC 52 escape sequence (`ESC ] 52 ; c ; <base64> BEL`).
// Terminals that support it (iTerm2, kitty, WezTerm, Alacritty,
// recent Terminal.app, Windows Terminal, tmux with set-clipboard)
// stuff the decoded payload into the system clipboard. Terminals
// without OSC 52 silently ignore the escape — so we ALSO write to
// `~/.metis/clipboard.txt` as a fallback the user can `cat` and
// pipe through `pbcopy` themselves.

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// yankLastAssistant grabs the most recent "assistant" / "thinking"
// message body and pushes it to the clipboard. Returns a short status
// line for the transcript so the user gets feedback ("copied 1.3 KB")
// instead of a silent no-op.
//
// Search order (newest first):
//
//  1. assistant (the model's final reply — what users want 95% of the time)
//  2. bash       (`!ls` output — also commonly copied)
//  3. tool result (e.g. Read or Bash output) — last-resort
//
// Skip: thinking trace (transient and dim, rarely worth copying),
// recap / thought-summary (metadata), error (use Ctrl+S for that).
func (m *Model) yankLastAssistant() string {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.Role == "assistant" || msg.Role == "bash" {
			n := len(msg.Content)
			writeClipboard(msg.Content)
			return fmt.Sprintf("(copied %s — %d chars from %s)",
				osc52Status(), n, msg.Role)
		}
	}
	return "(nothing to copy yet — type a message and let the model reply)"
}

// copySelectionAndReport pulls the chat list's current text selection
// (set by mouse drag / double-click / triple-click), pushes it to the
// system clipboard, and appends an info row with byte count + preview.
// No-op when nothing is selected. Mirrors yankLastAssistant's report
// format so users see consistent feedback regardless of which copy
// path fired.
func (m *Model) copySelectionAndReport() {
	text := m.chatList.SelectedText()
	text = trimTrailingNewlines(text)
	if text == "" {
		return
	}
	writeClipboard(text)
	preview := text
	if len(preview) > 60 {
		preview = preview[:60] + "…"
	}
	preview = formatPreviewForLog(preview)
	m.messages = append(m.messages, Message{
		Role:      "info",
		Content:   fmt.Sprintf("(copied %d chars · %s) %s", len(text), osc52Status(), preview),
		Timestamp: time.Now(),
	})
}

// trimTrailingNewlines strips trailing "\n" characters added by
// SelectedText's per-line joiner — a triple-click on the last line of
// an item doesn't include the row's terminating newline as user-
// visible content, so trimming keeps the clipboard payload tight.
func trimTrailingNewlines(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

// formatPreviewForLog replaces newlines with a visible glyph so the
// info row stays one line in the transcript even when the user copied
// a multi-line span.
func formatPreviewForLog(s string) string {
	if s == "" {
		return s
	}
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r == '\n' {
			out = append(out, " \xe2\x8f\x8e "...) // " ⏎ "
			continue
		}
		out = append(out, string(r)...)
	}
	return string(out)
}

// writeClipboard emits the OSC 52 sequence to stdout AND writes a
// fallback file. Best-effort throughout; we never error the chat
// surface for a clipboard hiccup.
func writeClipboard(text string) {
	if text == "" {
		return
	}
	enc := base64.StdEncoding.EncodeToString([]byte(text))
	// `\x1b]52;c;<b64>\x07` — `c` selects the regular clipboard
	// register (vs primary `p`). BEL (\x07) terminator is more
	// portable than ST (\x9c / \x1b\\).
	fmt.Fprintf(os.Stdout, "\x1b]52;c;%s\x07", enc)
	// Fallback file: lets the user `cat ~/.metis/clipboard.txt | pbcopy`
	// when the terminal eats OSC 52. Truncated to 64 KiB so a runaway
	// paste doesn't fill disk.
	if home := config.Home(); home != "" {
		path := filepath.Join(home, "clipboard.txt")
		const maxFallback = 64 * 1024
		body := text
		if len(body) > maxFallback {
			body = body[:maxFallback]
		}
		_ = os.WriteFile(path, []byte(body), 0o600)
	}
}

// osc52Status returns a single-word hint about whether the OSC 52
// escape is likely to land. iTerm2 / kitty / WezTerm / Alacritty /
// modern Terminal.app all support it; tmux passes through if
// `set-clipboard on` is configured. We detect tmux via env and warn,
// otherwise assume it works.
//
// Note: there's no synchronous way to ASK the terminal "did you
// accept that OSC 52?" — DCS responses to clipboard queries are
// terminal-specific and gated. The status string is a heuristic, not
// a guarantee.
func osc52Status() string {
	if os.Getenv("TMUX") != "" {
		return "OSC 52 → tmux (needs `set-clipboard on`)"
	}
	if t := os.Getenv("TERM_PROGRAM"); t != "" {
		return "OSC 52 → " + t
	}
	return "OSC 52"
}

// stamp is unused but reserved — future enhancement could append a
// timestamp + session id to the fallback file so users have a
// natural log of yanks. Keeping the import for that.
var _ = time.Now

// yankFullTranscript copies the entire chat transcript (user + assistant
// + bash output) to the system clipboard via OSC 52. Mirrors claude-
// code's transcript-export workflow — when a user wants to paste a
// whole session into a bug report or doc, Ctrl+Shift+Y is much faster
// than scrolling + drag-selecting in copy mode.
//
// Filters out spinner / status / dim metadata so the export reads
// like a conversation, not a TUI dump. Returns a one-line confirmation
// for the transcript ("copied X chars across N turns").
func (m *Model) yankFullTranscript() string {
	if len(m.messages) == 0 {
		return "(nothing to copy yet)"
	}
	var b []byte
	turns := 0
	for _, msg := range m.messages {
		switch msg.Role {
		case "user":
			b = append(b, "USER: "...)
			b = append(b, msg.Content...)
			b = append(b, '\n', '\n')
			turns++
		case "user-steer":
			b = append(b, "USER (mid-turn): "...)
			b = append(b, msg.Content...)
			b = append(b, '\n', '\n')
		case "assistant":
			b = append(b, "ASSISTANT: "...)
			b = append(b, msg.Content...)
			b = append(b, '\n', '\n')
		case "bash":
			b = append(b, "$ "...)
			b = append(b, msg.Content...)
			b = append(b, '\n', '\n')
		}
	}
	if len(b) == 0 {
		return "(nothing to copy yet)"
	}
	writeClipboard(string(b))
	return fmt.Sprintf("(copied transcript — %d chars across %d turn(s) via %s)",
		len(b), turns, osc52Status())
}
