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

// robotIconLines is the 5-row pixel-block robot face. Heavier visual
// weight than the previous line-art version (user feedback 2026-05-08
// image #10: "icon too ugly compared to claude-code"). Uses `▄ █ ▀`
// for the silhouette and `▐ ▌` half-blocks for the body sides — every
// char in this set renders at a stable 1-cell width across iTerm,
// Apple_Terminal, Alacritty, and JetBrains Mono. The eyes (`◉`) and
// mouth (`──`) sit inside the face so the silhouette reads as a robot
// even in monochrome rendering. Width is fixed at 9 cells so the
// JoinHorizontal aligns cleanly with the right-column text.
// robotIconLines (kept var name for diff continuity) is now an Athenian
// owl silhouette — Metis is the Greek titaness of wisdom & cunning,
// mother of Athena, whose tetradrachm coin owl is the canonical
// "wisdom" sigil in Western iconography. The earlier robot face read
// as generic-bot and didn't tie to the project's name. Width is fixed
// at 9 cells so JoinHorizontal still aligns with the right column;
// every char (▄ ▀ █ • ▼ and space) is single-cell across iTerm2,
// Apple_Terminal, Alacritty, Ghostty, kitty, and Windows Terminal.
var robotIconLines = []string{
	"  ▄▀▀▀▄  ",
	" █ ▀ ▀ █ ",
	" █ • • █ ",
	" █  ▼  █ ",
	"  ▀█▄█▀  ",
	"   ▀ ▀   ",
}

// renderWelcomeBanner paints the bordered, centered welcome card we
// show on a fresh session. Same package as the rest of tui so the
// renderer can read Model state without exporting fields.
func (m *Model) renderWelcomeBanner() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(accentBlue).
		Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(textMuted)
	valueStyle := lipgloss.NewStyle().Foreground(textPrimary)
	iconStyle := lipgloss.NewStyle().Foreground(accentBlue)

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

	icon := iconStyle.Render(strings.Join(robotIconLines, "\n"))

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

// prettifyCwd replaces the user's home dir with `~` for shorter
// display. claude-code's banner does this and it makes a real
// difference on macOS where /Users/<long>/Documents/... eats the
// terminal width.
func prettifyCwd(p string) string {
	if p == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		// Match exact-home or home-prefix; never mangle a path that
		// happens to contain $HOME as a substring elsewhere.
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
