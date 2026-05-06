package tui

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// notify.go — desktop notifications via terminal escape codes.
//
// Two formats are supported, the second is the broader-compatibility
// fallback:
//
//   1. OSC 9 (iTerm2 / WezTerm / Alacritty): \x1b]9;<text>\x07
//      The simplest, most widely-supported form. Renders as a
//      Notification Center / D-Bus banner.
//
//   2. OSC 777 with cmd=notify (kitty / urxvt / older terminals):
//      \x1b]777;notify;<title>;<text>\x07. We don't emit this by
//      default because most users are on iTerm2 / Alacritty.
//
// Notifications fire only when:
//   - a turn took longer than notifyMinDuration (default 30s), AND
//   - the user has been idle long enough that they probably context-
//     switched (we approximate via "TUI was unfocused for >5s")
//
// Both thresholds are conservative — random short turns shouldn't
// spam the user, and quick replies they're staring at don't need a
// banner. Mirrors claude-code's notification queue priority logic.

// NotifyMinDuration is the floor below which Notify is a no-op.
// Override via METIS_NOTIFY_MIN_SECONDS env var.
var NotifyMinDuration = 30 * time.Second

// notifyDest is the io.Writer notifications go to. stderr by default
// so the OSC sequence stays out of stdout pipelines (`metis run | jq`
// doesn't gain a stray notification when run-mode never triggers
// these, but defensive separation is cheap).
//
// Tests swap this to a bytes.Buffer to inspect the emitted bytes.
var (
	notifyMu   sync.Mutex
	notifyDest io.Writer = os.Stderr
)

// SetNotifyDest swaps the destination writer. Test-only.
func SetNotifyDest(w io.Writer) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	notifyDest = w
}

// Notify sends a desktop notification with the given title + message.
// Title is included in the OSC9 payload as "title: message" — most
// terminals don't honour a separate title field via OSC9.
//
// Caller is responsible for the duration check; callsites typically
// guard with `if elapsed >= tui.NotifyMinDuration`.
func Notify(title, message string) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if notifyDest == nil {
		return
	}
	body := message
	if title != "" {
		body = title + ": " + message
	}
	// OSC 9 ; <body> BEL — iTerm2 / WezTerm / Alacritty compatible.
	// We don't try alternative dispatch (osascript, notify-send) here:
	// metis stays terminal-pure; users who want richer notifications
	// run a hook (`postToolUse: terminal-notifier ...`) — see F14.
	fmt.Fprintf(notifyDest, "\x1b]9;%s\x07", escapeOSCText(body))
}

// escapeOSCText strips characters that would prematurely terminate
// the OSC sequence (BEL / ST). Non-printable controls get spaced out;
// we keep the message readable rather than try to encode them.
func escapeOSCText(s string) string {
	if s == "" {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r == '\x07', r == '\x1b':
			out = append(out, ' ')
		case r < 0x20 && r != '\n':
			out = append(out, ' ')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
