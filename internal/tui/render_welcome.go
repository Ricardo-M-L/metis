package tui

// render_welcome.go — fresh-session banner. Design notes (2026-05-08
// user feedback, images #1 and #4):
//
//   * The icon was missing — claude-code anchors its banner with a
//     pixel-block robot face. We add a 4-row ASCII robot whose shape
//     stays legible across fonts (no glyph-collision class — the
//     earlier METIS wordmark rendered as MCTIS in some terminals).
//   * The session UUID didn't belong on the banner — it's noise the
//     user can find via /session if they need it. Dropped.
//   * cwd was already shown but got scrolled off as soon as the agent
//     ran. The renderer paths now share one banner: the "fresh" frame
//     and the "active chat" frame both call renderWelcomeBanner, with
//     the active-chat path rendering a compact one-row variant via
//     renderHeaderBanner so it stays sticky above the chat list
//     without dominating the screen.

import (
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/version"
)

// metisOwlGlyphLines is the hand-crafted Unicode block-art owl —
// 17 cells wide × 10 rows tall, every char placed deliberately.
// This is the "spread coat-of-arms" Athenian owl design that the
// user (2026-05-09 image #18 feedback) called acceptable as a
// pixel-art rendering. Going finer than this requires either:
//   - Real PNG via iTerm2 inline image protocol — blocked by
//     bubbletea v2's ultraviolet renderer truncating large OSC
//     payloads (would require a fork of the renderer).
//   - Algorithmic image-to-glyph conversion (chafa) — produces
//     noisy fragments at 17×8 / 24×14 cell sizes because the
//     source PNG's gradients/details have no clean mapping.
//
// At this glyph density the owl-ness is carried by deliberate
// silhouette: pointed ear tufts (▟▙), round dome head, V-beak,
// two-layer spread wings, taloned feet. Eyes (●) are repainted
// cyan in renderWelcomeBanner for contrast.
var metisOwlGlyphLines = []string{
	"    ▟▙     ▟▙    ",
	"  ▄█▀▀▀▀▀▀▀▀▀█▄  ",
	" ▐█           █▌ ",
	" ▐█  ●     ●  █▌ ",
	" ▐█    ╲ ╱    █▌ ",
	"  ▀█▄▄▄▄▄▄▄▄▄█▀  ",
	" ▟██▙▄▄▄▄▄▄▄▟██▙ ",
	"▐███▌       ▐███▌",
	" ▀▀▀         ▀▀▀ ",
	"       ▀ ▀       ",
}

// Eye-position cyan accent — row + col indices in metisOwlGlyphLines.
const (
	owlEyeRow  = 3
	owlEyeColL = 5
	owlEyeColR = 11
)

// renderWelcomeBanner paints the bordered, centered welcome card we
// show on a fresh session — and again as the first scrollable item
// once the user starts chatting (so the brand strip doesn't suddenly
// disappear after the first turn). Same package as the rest of tui
// so the renderer can read Model state without exporting fields.
//
// Keeps the trailing "Type a message to start · /help · /quit" hint —
// the active-chat path uses renderWelcomeBannerNoHint so that
// already-engaged users aren't told to start typing.
func (m *Model) renderWelcomeBanner() string {
	return m.renderWelcomeBannerCard(true)
}

// renderWelcomeBannerNoHint is the same card without the
// "Type a message to start" line. Used when the banner appears
// inside an active chat list (where the hint is stale).
func (m *Model) renderWelcomeBannerNoHint() string {
	return m.renderWelcomeBannerCard(false)
}

func (m *Model) renderWelcomeBannerCard(showHint bool) string {
	titleStyle := lipgloss.NewStyle().
		Foreground(accentBlue).
		Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(textMuted)
	valueStyle := lipgloss.NewStyle().Foreground(textPrimary)

	// Owl glyph painted in two passes: silver body + cyan-glowing eyes.
	silverStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#C0C8D8")).Bold(true)
	eyeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00D5E5")).Bold(true)

	owlRows := make([]string, len(metisOwlGlyphLines))
	for i, raw := range metisOwlGlyphLines {
		if i == owlEyeRow {
			rs := []rune(raw)
			before := silverStyle.Render(string(rs[:owlEyeColL]))
			eyeL := eyeStyle.Render(string(rs[owlEyeColL]))
			middle := silverStyle.Render(string(rs[owlEyeColL+1 : owlEyeColR]))
			eyeR := eyeStyle.Render(string(rs[owlEyeColR]))
			after := silverStyle.Render(string(rs[owlEyeColR+1:]))
			owlRows[i] = before + eyeL + middle + eyeR + after
			continue
		}
		owlRows[i] = silverStyle.Render(raw)
	}
	icon := strings.Join(owlRows, "\n")

	// Title row carries the version inline (claude-code parity: the
	// banner is the discoverable surface for "what version am I on?",
	// no need to also stash it in the bottom status bar).
	titleRow := lipgloss.JoinHorizontal(lipgloss.Bottom,
		titleStyle.Render("✻ metis"),
		labelStyle.Render(" v"+version.Short()),
	)

	// Build the right-hand column body — title+version, tagline, model
	// row, cwd row. The icon goes in the left column. lipgloss.JoinHorizontal
	// lines them up; the JoinVertical inside the right column handles
	// the row stack.
	right := lipgloss.JoinVertical(lipgloss.Left,
		titleRow,
		"",
		lipgloss.JoinHorizontal(lipgloss.Left,
			labelStyle.Render("model: "),
			valueStyle.Render(m.model),
			labelStyle.Render("  ·  mode: "),
			valueStyle.Render(string(m.gate.Mode())),
		),
		// cwd row drops the "cwd:" label (claude-code doesn't print one
		// either — the path stands on its own and avoids burning a
		// label-column on a value users already recognize).
		valueStyle.Render(prettifyCwd(currentCwd())),
	)

	body := lipgloss.JoinHorizontal(lipgloss.Top, icon, "  ", right)

	// Outline the whole card. Width is tied to the live terminal width
	// so the banner stretches across the whole screen instead of
	// hugging the left side at content-width (claude-code parity:
	// their welcome card spans the full pane). Fixed overhead is
	// border(2) + padding-LR(4) + left-margin(1) = 7; clamp to a
	// sensible minimum so a tiny terminal still renders something.
	w := m.width
	if w <= 0 {
		w = 80
	}
	boxWidth := w - 7
	if boxWidth < 40 {
		boxWidth = 40
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accentBlue).
		Padding(0, 2).
		Margin(1, 0, 1, 1).
		Width(boxWidth).
		Render(body)

	if !showHint {
		return box + "\n"
	}
	hint := labelStyle.Render("  Type a message to start  ·  ") +
		titleStyle.Render("/help") +
		labelStyle.Render(" for commands  ·  ") +
		titleStyle.Render("/quit") +
		labelStyle.Render(" to exit")

	return box + "\n" + hint + "\n"
}

// renderHeaderBanner is the compact, single-row variant of the welcome
// card used in the active-chat path. Stays at the top above the chat
// list so the user can always see their model + cwd without typing
// /session. claude-code's `claude · model · cwd` ribbon is the
// reference. Width-aware: truncates the cwd from the left when the
// terminal is narrow.
func (m *Model) renderHeaderBanner() string {
	if m == nil {
		return ""
	}
	titleStyle := lipgloss.NewStyle().Foreground(accentBlue).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(textMuted)
	valueStyle := lipgloss.NewStyle().Foreground(textPrimary)

	cwd := prettifyCwd(currentCwd())
	maxCwd := m.width - 30
	if maxCwd < 12 {
		maxCwd = 12
	}
	if len(cwd) > maxCwd {
		// Left-truncate so the trailing path component (where the user
		// usually orients themselves) stays visible.
		cwd = "…" + cwd[len(cwd)-maxCwd+1:]
	}

	mode := ""
	if m.gate != nil {
		mode = string(m.gate.Mode())
	}

	row := titleStyle.Render("✻ metis") +
		dimStyle.Render(" v"+version.Short()) +
		dimStyle.Render(" · ") +
		valueStyle.Render(m.model)
	if mode != "" {
		row += dimStyle.Render(" · ") + valueStyle.Render(mode)
	}
	row += dimStyle.Render(" · ") + valueStyle.Render(cwd)

	// Separator stretches across the full terminal so the header reads
	// as one continuous strip, matching the bottom status bar's width.
	sepWidth := m.width
	if sepWidth <= 0 {
		sepWidth = 60
	}
	return row + "\n" + dimStyle.Render(strings.Repeat("─", sepWidth)) + "\n"
}

// currentCwd is a tiny wrapper so callers don't import os just to
// read the working directory. Returns "" on error so the banner can
// quietly skip the cwd row. Also walks symlinks via EvalSymlinks so
// macOS's `/tmp` → `/private/tmp` etc. show the canonical path users
// would see from `pwd -P` (and that tool error messages reference).
func currentCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		return real
	}
	return wd
}

// prettifyCwd returns the path verbatim. We used to collapse $HOME
// to "~" (claude-code parity) but the lone `~` in the welcome card
// reads as visual noise rather than a path — users couldn't tell at
// a glance which directory metis was operating from. Returning the
// absolute path makes the cwd unambiguous.
//
// Long paths are kept readable by the call sites: the compact top
// header truncates from the left with `…`; the welcome card lets
// lipgloss wrap the path inside the bordered box.
func prettifyCwd(p string) string {
	return p
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
