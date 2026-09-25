package tui

// render_welcome.go — fresh-session banner. Its small brand mark remains the
// first transcript item, so it scrolls away with the conversation.

import (
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/version"
)

// Terminal icon derived from the blue M, orbit, and star in the existing
// metis-desktop/build/appicon.png. The white app-tile background is omitted;
// the mark is sampled onto a 20x16 dot grid and encoded as 10x4 Braille cells.
var metisIconLines = []string{
	"  ⣀   ⠴⠆  ",
	"  ⣿⣷⣄⣠⣴⣿⠐⣦",
	"⢠⠖⣿⡟⠿⠿⢻⣿⠖⠁",
	"⠻⠶⠿⠷⠖⠛⠙⠿  ",
}

var metisIconPalette = []string{
	"#08A9F7", "#0797F8", "#0786F9", "#0872FA", "#0B5EFA",
	"#1751FA", "#2B48FA", "#433FFA", "#5937F9", "#6830F7",
}

func renderMetisIcon() string {
	styles := make([]lipgloss.Style, len(metisIconPalette))
	for i, hex := range metisIconPalette {
		styles[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Bold(true)
	}
	var rows []string
	for _, raw := range metisIconLines {
		var row strings.Builder
		for col, dot := range []rune(raw) {
			if dot == ' ' {
				row.WriteRune(dot)
			} else {
				row.WriteString(styles[col].Render(string(dot)))
			}
		}
		rows = append(rows, row.String())
	}
	return strings.Join(rows, "\n")
}

// renderWelcomeBanner paints the fresh-session card plus its one-time start
// hint. Active chat inserts renderWelcomeBannerCard as the first transcript
// item, keeping the visual identity stable without retaining a stale
// "Type a message to start" instruction after the user has already started.
func (m *Model) renderWelcomeBanner() string {
	return m.renderWelcomeBannerCard() + "\n" + m.renderWelcomeHint() + "\n"
}

func (m *Model) renderWelcomeBannerCard() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(accentBlue).
		Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(textMuted)
	valueStyle := lipgloss.NewStyle().Foreground(textPrimary)

	icon := renderMetisIcon()

	// Title row carries the version inline (claude-code parity: the
	// banner is the discoverable surface for "what version am I on?",
	// no need to also stash it in the bottom status bar).
	titleRow := lipgloss.JoinHorizontal(lipgloss.Bottom,
		titleStyle.Render("metis"),
		labelStyle.Render(" v"+version.Short()),
	)

	// Build the right-hand column body — title+version, tagline, model
	// row, cwd row. The icon goes in the left column. lipgloss.JoinHorizontal
	// lines them up; the JoinVertical inside the right column handles
	// the row stack.
	modelRow := lipgloss.JoinHorizontal(lipgloss.Left,
		labelStyle.Render("model: "),
		valueStyle.Render(effectiveModelID(m)),
	)
	if m != nil && m.gate != nil {
		modelRow += labelStyle.Render("  ·  mode: ") +
			valueStyle.Render(string(m.gate.Mode()))
	}
	right := lipgloss.JoinVertical(lipgloss.Left,
		titleRow,
		"",
		modelRow,
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

	return box
}

func (m *Model) renderWelcomeHint() string {
	titleStyle := lipgloss.NewStyle().Foreground(accentBlue).Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(textMuted)
	return labelStyle.Render("  Type a message to start  ·  ") +
		titleStyle.Render("/help") +
		labelStyle.Render(" for commands  ·  ") +
		titleStyle.Render("/quit") +
		labelStyle.Render(" to exit")
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
// Long paths are kept readable by the welcome card, which lets lipgloss wrap
// the path inside the bordered box.
func prettifyCwd(p string) string {
	return p
}

// effectiveModelID returns the model id the running Provider actually
// sends on the wire — the trustworthy source for the banner / status
// bar. Falls back to m.model (the user-picked string) only when no
// Provider is bound yet (cold-start before agent loop wiring).
//
// Why this exists: m.model is set by NewModel + /model handlers and
// can drift from Provider.ModelID() when the user changes the model
// string mid-session WITHOUT a Provider rebuild — e.g. picking
// "deepseek-v4-pro" from /model while the live Provider is still the
// MiniMax-Anthropic gateway (user screenshot 35, 2026-05-17). Reading
// from the Provider closes that gap: the banner shows what's actually
// running, even when the user's intent and the wire state disagree.
func effectiveModelID(m *Model) string {
	if m != nil && m.loop != nil && m.loop.Provider != nil {
		if id := m.loop.Provider.ModelID(); id != "" {
			return id
		}
	}
	if m != nil {
		return m.model
	}
	return ""
}
