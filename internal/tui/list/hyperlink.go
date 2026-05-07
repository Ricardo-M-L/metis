package list

// hyperlink.go — OSC 8 hyperlink scanner. Lets the chat surface
// detect "did the user click on a hyperlink?" so a single click can
// open the URL in the system browser instead of (or in addition to)
// copying the row.
//
// OSC 8 wire format (per VT terminal spec, used by iTerm2, WezTerm,
// kitty, Alacritty, GNOME Terminal, Windows Terminal):
//
//	ESC ]8;<params>;<URL> ST  TEXT  ESC ]8;; ST
//
// where ST = `ESC \` (the proper string terminator) or `BEL` (`\x07`,
// commonly used as a shorter equivalent). We accept both.
//
// metis already EMITS OSC 8 from figures.go::osc8Link (rendered into
// chat rows by render_message.go and render_tool.go). This file is the
// reverse direction: given a click position, recover the URL.

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// URLAtPoint returns the OSC 8 URL at (itemIdx, lineY, col) in the
// list's rendered content, or "" if the click didn't land on a
// hyperlink. col is the **display column** (matches the convention used
// by HandleMouseDown/Drag and ansi.StringWidth — East-Asian wide chars
// count as 2).
//
// Out-of-range indices return "" — the caller treats that the same as
// "no link here."
func (l *List) URLAtPoint(itemIdx, lineY, col int) string {
	if itemIdx < 0 || itemIdx >= len(l.items) {
		return ""
	}
	item := l.getItem(itemIdx)
	lines := strings.Split(item.content, "\n")
	if lineY < 0 || lineY >= len(lines) {
		return ""
	}
	return scanOSC8AtCol(lines[lineY], col)
}

// scanOSC8AtCol walks one rendered line (with embedded SGR + OSC 8
// escapes) and returns the URL of the hyperlink region containing col,
// or "" if col falls outside any link.
//
// State machine:
//   - "outside": consume SGR escapes (no width), advance display col on
//     visible runes.
//   - When we see `\x1b]8;;<URL><ST>`: capture URL, switch to "inside".
//   - "inside": same width accumulation, but if col is reached we
//     return the captured URL.
//   - When we see `\x1b]8;;<ST>` (empty URL): switch back to "outside".
//
// `<ST>` is either `\x1b\\` (proper string terminator) or `\x07` (BEL).
func scanOSC8AtCol(line string, col int) string {
	if col < 0 || line == "" {
		return ""
	}
	currentCol := 0
	activeURL := ""
	i := 0
	for i < len(line) {
		// Look for start of an escape sequence.
		if line[i] == '\x1b' && i+1 < len(line) {
			next := line[i+1]
			switch next {
			case ']':
				// OSC sequence — could be hyperlink (8;) or some other
				// OSC. Find the terminator (ST = ESC\ or BEL).
				end, st := findOSCTerminator(line, i+2)
				if end < 0 {
					return activeURL // malformed, give up
				}
				body := line[i+2 : end]
				i = st
				// "8;<params>;<URL>" — link open or close.
				if strings.HasPrefix(body, "8;") {
					rest := body[2:]
					// Skip params section up to the second `;`.
					semi := strings.Index(rest, ";")
					if semi >= 0 {
						url := rest[semi+1:]
						if url == "" {
							activeURL = "" // link close
						} else {
							activeURL = url // link open
						}
					}
				}
				continue
			case '[':
				// CSI / SGR — find the final byte (ASCII 0x40-0x7E).
				j := i + 2
				for j < len(line) && (line[j] < 0x40 || line[j] > 0x7E) {
					j++
				}
				if j < len(line) {
					j++ // include final byte
				}
				i = j
				continue
			default:
				// Other escape — skip the next byte conservatively.
				i += 2
				continue
			}
		}
		// Visible byte/rune. Decode rune to advance display width.
		r, size := decodeRuneInString(line[i:])
		w := ansi.StringWidth(string(r))
		if currentCol <= col && col < currentCol+w {
			return activeURL
		}
		currentCol += w
		i += size
		if currentCol > col && activeURL == "" {
			// Past the click col without finding a link.
			return ""
		}
	}
	// Click was past the end of the line — no link.
	return ""
}

// findOSCTerminator scans from start (just past `\x1b]`) for either ST
// (`\x1b\\`) or BEL (`\x07`). Returns (end-of-body, end-of-terminator)
// where end-of-body is the index of the first terminator byte and
// end-of-terminator is the index just past it. (-1, -1) if not found.
func findOSCTerminator(s string, start int) (int, int) {
	for j := start; j < len(s); j++ {
		if s[j] == '\x07' {
			return j, j + 1
		}
		if s[j] == '\x1b' && j+1 < len(s) && s[j+1] == '\\' {
			return j, j + 2
		}
	}
	return -1, -1
}

// decodeRuneInString is a tiny shim around utf8.DecodeRuneInString.
// Inlined here to avoid pulling unicode/utf8 just for one callsite.
func decodeRuneInString(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	b := s[0]
	if b < 0x80 {
		return rune(b), 1
	}
	// Multi-byte — defer to the standard library via a single string conv.
	for _, r := range s {
		// First rune is what we want; figure out its byte size.
		return r, len(string(r))
	}
	return 0, 1
}
