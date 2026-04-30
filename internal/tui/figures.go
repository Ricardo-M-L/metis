package tui

import "os"

// Glyph constants for the chat surface — modelled on claude-code's
// constants/figures.ts so transcripts read the same.
//
// Why constants and not inline literals: the bullet/leaf hierarchy
// (`⏺ Tool(arg)` + `  ⎿ summary`) is the load-bearing visual pattern
// for tool results. A typo or stylistic drift here breaks scanability
// across N tool renderers.
//
// claude-code falls back to ASCII (`●`) on non-darwin platforms because
// `⏺` (U+23FA, "BLACK CIRCLE FOR RECORD") doesn't always have a glyph in
// Linux/Windows default fonts. metis sticks with `⏺` since most modern
// terminals (iTerm2, WezTerm, Alacritty, Windows Terminal) ship fonts
// with full unicode coverage; if a user reports rendering breakage we
// can add the same darwin-only fallback claude-code does.
const (
	glyphBullet   = "⏺" // assistant message + tool-call leader
	glyphTreeLeaf = "⎿" // sub-line under bullet (tool result, summary)
	glyphAsterisk = "✻" // turn-end thought summary, system events
	glyphRecap    = "※" // reserved: future recap memo line
	glyphPrompt   = "❯" // user input mark
)

// reducedMotionEnabled is captured once at startup. METIS_REDUCED_MOTION=1
// (or POSIX-standard NO_MOTION=1) disables the spinner shimmer, the
// tool-use flash pulse, and slows the tick rate so visually-sensitive
// users / accessibility tooling get a calmer transcript.
//
// claude-code does the same via a settings flag; we read env var so
// users can flip it without touching config files.
var reducedMotionEnabled = os.Getenv("METIS_REDUCED_MOTION") == "1" ||
	os.Getenv("NO_MOTION") == "1"

// osc8Link wraps text with OSC 8 hyperlink escape sequences so the
// terminal makes URL clickable (where supported — iTerm2, WezTerm,
// Alacritty, GNOME Terminal, Windows Terminal). Falls back gracefully:
// terminals that don't speak OSC 8 just render the plain text.
//
// Format: ESC ] 8 ; ; URL ESC \ TEXT ESC ] 8 ; ; ESC \
func osc8Link(text, url string) string {
	if url == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}
