package term

// osc_progress.go — OSC 8 hyperlink emitter (companion to OSC 9;4
// progress in notify.go::SendProgress; the progress side already
// exists, this file adds hyperlink support which previously didn't).
//
// OSC 8 spec:
//
//	ESC ] 8 ; <params> ; <url> BEL <text> ESC ] 8 ; ; BEL
//
// params is a key=value:key=value list; we use `id=<sha8(url)>` so
// terminals that re-render wrapped text (long URLs that line-wrap
// across panes / status bars) treat multiple visible chunks as one
// logical link. Without `id=`, hover/copy treats each line as a
// separate URL — common rendering bug in tmux + iTerm2.
//
// Mirrors claude-code-sourcemap `restored-src/src/ink/termio/osc.ts`
// `link()` and `osc8Id()`.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Hyperlink wraps text in an OSC 8 escape pointing at url. When the
// terminal doesn't support OSC 8 (per SupportsHyperlink), falls back
// to plain `text (url)` — the URL is still copy-able from the bare
// rendering, just not click-able.
//
// NOT wrapped in tmux DCS: OSC 8 is consumed by the inner cell layer
// and tmux propagates it natively (unlike OSC 9 / 52 which need DCS
// passthrough). Adding DCS here would actually break iTerm2 + tmux.
func Hyperlink(text, url string) string {
	if url == "" {
		return text
	}
	if !SupportsHyperlink() {
		return fmt.Sprintf("%s (%s)", text, url)
	}
	terminator := "\x07"
	if PreferSTTerminator() {
		terminator = "\x1b\\"
	}
	id := osc8ID(url)
	return fmt.Sprintf("\x1b]8;id=%s;%s%s%s\x1b]8;;%s",
		id, url, terminator, text, terminator)
}

// osc8ID computes a stable 8-char hex id for an URL so wrapped
// renderings of the same hyperlink re-use the same id (lets the
// terminal merge them into one click target).
func osc8ID(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:4])
}
