package term

// multiplexer.go — DCS passthrough wrapping for tmux + GNU screen.
// Inner ESCs must be doubled inside tmux DCS. tmux requires
// `set -g allow-passthrough on` in .tmux.conf for this to work;
// without it, tmux silently drops the whole DCS — same observable
// result as raw OSC (no notification, no clipboard write).
//
// Shared by notify (OSC 9 / 9;4) and yank (OSC 52) — both need the
// same passthrough rules; living here keeps the env-detection logic
// in one place.

import (
	"os"
	"strings"
)

// WrapForMultiplexer wraps an OSC sequence so it reaches the outer
// terminal when running inside tmux ($TMUX) or GNU screen ($STY).
// No-op outside of multiplexers.
func WrapForMultiplexer(seq string) string {
	if os.Getenv("TMUX") != "" {
		escaped := strings.ReplaceAll(seq, "\x1b", "\x1b\x1b")
		return "\x1bPtmux;" + escaped + "\x1b\\"
	}
	if os.Getenv("STY") != "" {
		return "\x1bP" + seq + "\x1b\\"
	}
	return seq
}
