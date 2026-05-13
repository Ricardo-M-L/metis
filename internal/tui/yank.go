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
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/term"
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
// (set by mouse drag / double-click / triple-click) and pushes it to
// the system clipboard. SILENT — no info row appended.
//
// Why silent: the previous version logged "(copied N chars · OSC 52
// → Apple_Terminal) <preview>" into the transcript, which polluted
// the chat (image #20 2026-05-07: ten of these grey rows after the
// user's cmd+C session). Claude Code's parity is also silent — copy
// is a low-ceremony action, the model output is the message, the
// clipboard is the side effect. If the user wants confirmation they
// can paste somewhere; the OSC 52 escape + ~/.metis/clipboard.txt
// fallback both still fire.
func (m *Model) copySelectionAndReport() {
	text := m.chatList.SelectedText()
	text = trimTrailingNewlines(text)
	if text == "" {
		return
	}
	writeClipboard(text)
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
//
// Terminator + tmux handling matches claude-code-sourcemap's
// `restored-src/src/ink/termio/osc.ts::setClipboard`:
//
//   - Pick BEL (`\x07`) by default, ST (`\x1b\\`) for Kitty (which
//     silently drops BEL-terminated OSCs).
//   - When inside tmux ($TMUX set) or screen ($STY set), wrap the
//     OSC 52 in a DCS passthrough envelope `\x1bPtmux;\x1b<inner>\x1b\\`
//     and DOUBLE every ESC byte inside the payload — tmux's DCS
//     parser uses ESC as a delimiter so a literal ESC in the inner
//     payload would terminate the wrapper early and corrupt downstream
//     bytes. opencode's simpler version skipped this step and silently
//     truncated payloads containing ESC.
func writeClipboard(text string) {
	if text == "" {
		return
	}
	enc := base64.StdEncoding.EncodeToString([]byte(text))

	// Path 1 — OSC 52 escape sequence. Works in iTerm2, kitty, WezTerm,
	// Alacritty, and tmux with `set-clipboard on`. Critically does NOT
	// work in macOS Terminal.app (the user's 2026-05-11 image #3 report:
	// "command+c 没成功") — that's why we ALSO run path 2.
	terminator := "\x07"
	if term.PreferSTTerminator() {
		terminator = "\x1b\\"
	}
	bare := "\x1b]52;c;" + enc + terminator

	// wrapForMultiplexer (notify.go) handles tmux DCS passthrough +
	// ESC doubling + screen DCS — same envelope OSC 9;4 progress
	// uses, kept in one place.
	fmt.Fprint(os.Stdout, term.WrapForMultiplexer(bare))

	// Path 2 — Native OS clipboard tool (pbcopy / xclip / wl-copy /
	// clip.exe). Bypasses OSC 52 entirely so the user's clipboard
	// gets the payload regardless of terminal capabilities. Best-
	// effort: missing tool / sandbox / locked clipboard all fall
	// through silently. Both paths run; whichever one lands first
	// wins (subsequent identical writes are idempotent).
	writeClipboardNative(text)

	// Path 3 — Fallback file: lets the user `cat ~/.metis/clipboard.txt
	// | pbcopy` when both paths above are blocked (rare). Truncated
	// to 64 KiB so a runaway paste doesn't fill disk.
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

// writeClipboardNative shells out to the OS clipboard tool to put
// text directly into the system clipboard, bypassing the terminal.
// Mirrors readClipboard's strategy in clipboard.go but in the
// write direction.
//
// Per-OS:
//   - macOS:   pbcopy (always present)
//   - Linux:   wl-copy (Wayland) → xclip → xsel
//   - Windows: clip.exe
//
// Each invocation is capped at 1.5s so a hung clipboard daemon
// can't block the chat surface. nil tools just return; the OSC 52
// + fallback file paths still ran.
func writeClipboardNative(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		runClipWrite(ctx, text, "pbcopy")
	case "linux":
		// Try Wayland first (newer distros), then X11 fallbacks.
		if runClipWrite(ctx, text, "wl-copy") {
			return
		}
		if runClipWrite(ctx, text, "xclip", "-selection", "clipboard") {
			return
		}
		runClipWrite(ctx, text, "xsel", "--clipboard", "--input")
	case "windows":
		runClipWrite(ctx, text, "clip.exe")
	}
}

// runClipWrite spawns `name args...` and pipes text into stdin.
// Returns true when the tool was found and the write succeeded —
// caller uses the return only to decide whether to try a fallback.
// Missing tool / spawn error / non-zero exit all return false
// without surfacing the error (best-effort path).
func runClipWrite(ctx context.Context, text, name string, args ...string) bool {
	if _, err := exec.LookPath(name); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, name, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	_, _ = stdin.Write([]byte(text))
	_ = stdin.Close()
	return cmd.Wait() == nil
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
