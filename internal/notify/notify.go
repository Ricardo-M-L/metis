package notify

// notify.go — desktop notifications via terminal escape codes.
//
// Mirrors claude-code's services/notifier.ts + ink/useTerminalNotification.ts:
// 5-channel matrix dispatched by $TERM_PROGRAM + an env-var override
// (METIS_NOTIFY_CHANNEL). Emits the right OSC sequence for each terminal
// instead of one-size-fits-all OSC 9 (which kitty / ghostty / Apple
// Terminal silently drop).
//
// Channels:
//   ChannelITerm2          — OSC 9;<text>BEL (iTerm2 / WezTerm / Alacritty)
//   ChannelITerm2WithBell  — OSC 9 + raw BEL on top
//   ChannelKitty           — 3-step OSC 99 protocol with random id
//   ChannelGhostty         — OSC 777;notify;<title>;<body>
//   ChannelBell            — raw BEL (Apple Terminal w/ audible-bell-off)
//   ChannelOff             — emit nothing
//
// Selection (first wins):
//   1. METIS_NOTIFY_CHANNEL env (auto/iterm2/iterm2_with_bell/kitty/
//      ghostty/bell/off)
//   2. $TERM_PROGRAM auto-detection
//   3. KITTY_WINDOW_ID / ALACRITTY_LOG fallback markers
//   4. Apple_Terminal: probe the profile's Bell setting via osascript +
//      defaults; only emit BEL when audible bell is off (otherwise we'd
//      spam the user's speakers)
//   5. nothing recognized → ChannelOff
//
// All OSC sequences (but NOT BEL) are routed through wrapForMultiplexer
// for tmux/screen DCS passthrough. BEL is intentionally raw so tmux's
// bell-action (window flag) still fires.

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/term"
)

// Channel is the notification dispatch target.
type Channel int

const (
	ChannelOff Channel = iota
	ChannelITerm2
	ChannelITerm2WithBell
	ChannelKitty
	ChannelGhostty
	ChannelBell
)

// NotifyMinDuration — turns shorter than this don't fire desktop
// notifications. Override via METIS_NOTIFY_MIN_SECONDS env var (read
// at startup elsewhere if needed).
var NotifyMinDuration = 30 * time.Second

// RecentInteractionThreshold — if the user pressed a key within this
// window, skip the notification (they're at the keyboard, no need to
// pop a banner). 6s mirrors claude-code's
// DEFAULT_INTERACTION_THRESHOLD_MS (hooks/useNotifyAfterTimeout.ts:9).
var RecentInteractionThreshold = 6 * time.Second

var (
	lastInteractionMu sync.Mutex
	// Zero value = "never recorded" — guard returns false until the
	// first MarkUserInteraction call. Init-to-time.Now() would have
	// silently suppressed every notification in the first 6 seconds
	// after process start, including the ones probes / tests fire
	// before bubbletea has even rendered a keystroke.
	lastInteractionAt time.Time
)

// MarkUserInteraction — call from the input loop on any keypress so
// SendNotification can suppress banners while the user is actively at
// the keyboard.
func MarkUserInteraction() {
	lastInteractionMu.Lock()
	defer lastInteractionMu.Unlock()
	lastInteractionAt = time.Now()
}

// ResetInteractionForTest back-dates the recent-interaction marker so
// SendNotification's 6s guard doesn't suppress test emissions. Use
// only from tests — calling this in prod would defeat the suppression.
func ResetInteractionForTest() {
	lastInteractionMu.Lock()
	defer lastInteractionMu.Unlock()
	lastInteractionAt = time.Now().Add(-(RecentInteractionThreshold + time.Second))
}

func hasRecentInteraction() bool {
	lastInteractionMu.Lock()
	defer lastInteractionMu.Unlock()
	if lastInteractionAt.IsZero() {
		return false
	}
	return time.Since(lastInteractionAt) < RecentInteractionThreshold
}

// notifyDest — io.Writer notifications go to. stderr by default so
// the OSC sequence stays out of stdout pipelines.
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

// SendNotification dispatches a desktop notification to whichever
// channel the env / terminal selects. Skips emission entirely when:
//   - the user pressed a key within RecentInteractionThreshold
//   - the selected channel is ChannelOff
//
// Caller is responsible for the duration check (e.g. only notify on
// turns longer than NotifyMinDuration).
func SendNotification(title, message string) {
	if hasRecentInteraction() {
		return
	}
	ch := SelectChannel()
	emit(ch, title, message)
}

// Notify is the legacy entry point — kept for compatibility with
// existing callsites. Routes through SendNotification so the new
// channel-matrix logic applies.
func Notify(title, message string) {
	SendNotification(title, message)
}

// SelectChannel resolves the active notification channel by reading
// METIS_NOTIFY_CHANNEL first, then auto-detecting from $TERM_PROGRAM
// and other markers. Exported so tests / `metis config show` can
// inspect what would fire without actually emitting.
func SelectChannel() Channel {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("METIS_NOTIFY_CHANNEL")))
	switch v {
	case "off", "disabled", "false", "0", "no":
		return ChannelOff
	case "iterm2":
		return ChannelITerm2
	case "iterm2_with_bell", "iterm2-with-bell":
		return ChannelITerm2WithBell
	case "kitty":
		return ChannelKitty
	case "ghostty":
		return ChannelGhostty
	case "bell", "terminal_bell":
		return ChannelBell
	case "", "auto":
		return autoChannel()
	default:
		// Unknown value — fall back to auto rather than error out.
		return autoChannel()
	}
}

func autoChannel() Channel {
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app":
		return ChannelITerm2
	case "WezTerm":
		// WezTerm honors the iTerm2 OSC 9 protocol natively.
		return ChannelITerm2
	case "ghostty":
		return ChannelGhostty
	case "Apple_Terminal":
		// Apple Terminal: BEL only when audible bell is off in the
		// profile (otherwise we'd play a sound, not show a banner).
		// See notify_apple.go (darwin-only build).
		if isAppleTerminalAudibleBellDisabled() {
			return ChannelBell
		}
		return ChannelOff
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return ChannelKitty
	}
	if os.Getenv("ALACRITTY_LOG") != "" {
		// Alacritty is picky about the OSC 9 form but accepts the
		// claude-code-style sequence.
		return ChannelITerm2
	}
	return ChannelOff
}

// SelectChannelName returns a human-friendly label for the active
// channel, used by `metis config show`.
func SelectChannelName() string {
	switch SelectChannel() {
	case ChannelITerm2:
		return "iterm2"
	case ChannelITerm2WithBell:
		return "iterm2_with_bell"
	case ChannelKitty:
		return "kitty"
	case ChannelGhostty:
		return "ghostty"
	case ChannelBell:
		return "bell"
	default:
		return "off"
	}
}

func emit(ch Channel, title, message string) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if notifyDest == nil {
		return
	}
	switch ch {
	case ChannelOff:
		return
	case ChannelITerm2:
		emitITerm2(notifyDest, title, message)
	case ChannelITerm2WithBell:
		emitITerm2(notifyDest, title, message)
		emitBell(notifyDest)
	case ChannelKitty:
		emitKitty(notifyDest, title, message)
	case ChannelGhostty:
		emitGhostty(notifyDest, title, message)
	case ChannelBell:
		emitBell(notifyDest)
	}
}

// emitITerm2 — OSC 9;<title: body>BEL. iTerm2 / WezTerm / Alacritty.
// The "\n\n" prefix matches claude-code's spacing — iTerm2 uses it as a
// visible separator in the rendered banner.
func emitITerm2(w io.Writer, title, message string) {
	body := message
	if title != "" {
		body = title + ": " + message
	}
	seq := "\x1b]9;\n\n" + escapeOSCText(body) + "\x07"
	fmt.Fprint(w, wrapForMultiplexer(seq))
}

// emitKitty — 3-step OSC 99 sequence. id= ties the parts together; d=0
// on the first part means "more parts coming," d=1 on the focus part
// closes the announcement. a=focus tells kitty the notification action
// is "focus the terminal window when clicked."
//
// Kitty terminator is ST (ESC \), not BEL, because BEL inside a kitty
// notification payload would prematurely terminate.
func emitKitty(w io.Writer, title, message string) {
	id := generateKittyID()
	titleSeq := fmt.Sprintf("\x1b]99;i=%d:d=0:p=title;%s\x1b\\",
		id, escapeOSCText(title))
	bodySeq := fmt.Sprintf("\x1b]99;i=%d:p=body;%s\x1b\\",
		id, escapeOSCText(message))
	focusSeq := fmt.Sprintf("\x1b]99;i=%d:d=1:a=focus;\x1b\\", id)
	fmt.Fprint(w, wrapForMultiplexer(titleSeq))
	fmt.Fprint(w, wrapForMultiplexer(bodySeq))
	fmt.Fprint(w, wrapForMultiplexer(focusSeq))
}

// emitGhostty — OSC 777;notify;<title>;<body>BEL. Ghostty's
// notification protocol is distinct from kitty's despite OSC 99 ≈ 777
// numerically. Ghostty also accepts plain OSC 9 but the OSC 777 form
// gives a proper title.
func emitGhostty(w io.Writer, title, message string) {
	seq := fmt.Sprintf("\x1b]777;notify;%s;%s\x07",
		escapeOSCText(title), escapeOSCText(message))
	fmt.Fprint(w, wrapForMultiplexer(seq))
}

// emitBell — raw \x07. Deliberately NOT wrapped in DCS passthrough:
// inside tmux, raw BEL triggers tmux's bell-action (window flag in the
// status line). Wrapped BEL is opaque DCS payload that tmux never
// inspects, killing the bell-action fallback. Mirrors claude-code's
// ink/useTerminalNotification.ts:67 comment.
func emitBell(w io.Writer) {
	fmt.Fprint(w, "\x07")
}

// wrapForMultiplexer is a thin alias for term.WrapForMultiplexer kept
// so call-sites and tests in this package don't pile on `term.` noise.
// All the actual logic (tmux DCS passthrough, ESC doubling, screen
// envelope) lives in internal/term/multiplexer.go.
func wrapForMultiplexer(seq string) string {
	return term.WrapForMultiplexer(seq)
}

// generateKittyID returns a small random int. Kitty uses this to group
// the 3 sequences (title / body / focus) of a single notification —
// a fixed value would cause overlapping notifications to collide.
//
// Range 1..9999 matches claude-code; collision rate is negligible for
// notifications fired seconds apart.
func generateKittyID() int {
	return rand.Intn(9999) + 1
}

// escapeOSCText strips characters that would prematurely terminate
// the OSC sequence (BEL / ESC). Non-printable controls except newline
// get spaced out so the message stays readable.
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

// ─────────────────────────────────────────────────────────────────────
// Progress reporting (OSC 9;4) — iTerm2 3.6.6+ / Ghostty 1.2.0+ /
// ConEmu / WezTerm. Renders as a Dock-icon progress bar (macOS) or a
// taskbar fill (Windows). Useful for long-running multi-step turns
// where the user wants to glance at the dock to see "20%".
// ─────────────────────────────────────────────────────────────────────

// ProgressState is the current task state for OSC 9;4 reporting.
type ProgressState int

const (
	ProgressClear         ProgressState = iota // hide the bar
	ProgressRunning                            // 0..100%
	ProgressIndeterminate                      // animated unknown-progress
	ProgressError                              // red bar with pct
)

// SendProgress emits an OSC 9;4 sequence. No-op on unsupported
// terminals. pct is clamped to 0..100 (ignored for Indeterminate /
// Clear).
func SendProgress(state ProgressState, pct int) {
	if !progressSupported() {
		return
	}
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if notifyDest == nil {
		return
	}
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	var seq string
	switch state {
	case ProgressClear:
		seq = "\x1b]9;4;0;\x07"
	case ProgressRunning:
		seq = fmt.Sprintf("\x1b]9;4;1;%d\x07", pct)
	case ProgressError:
		seq = fmt.Sprintf("\x1b]9;4;2;%d\x07", pct)
	case ProgressIndeterminate:
		seq = "\x1b]9;4;3;\x07"
	default:
		return
	}
	fmt.Fprint(notifyDest, wrapForMultiplexer(seq))
}

// progressSupported — terminals that honor OSC 9;4. Routes to the
// centralized SupportsProgressBar in capabilities.go which adds
// version gating (iTerm2 ≥ 3.6.6, Ghostty ≥ 1.2.0) and SSH/Apple
// Terminal exclusions. Older versions silently drop the sequence
// but Apple Terminal misrenders it, so the gate matters there.
func progressSupported() bool {
	// WezTerm honors OSC 9;4 unconditionally; capabilities.go's
	// SupportsProgressBar doesn't list it because Anthropic's
	// reference doesn't, but metis tested it before so keep allowing.
	if os.Getenv("TERM_PROGRAM") == "WezTerm" {
		return true
	}
	return term.SupportsProgressBar()
}
