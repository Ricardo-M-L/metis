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
	"image/color"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/version"
)

// metisOwlGlyphLines is the metis owl banner — an 18-char × 7-row
// Braille (U+2800-U+28FF) bitmap derived from the user's reference
// image (#29). Generated via tools/img2braille:
//
//	go run ./tools/img2braille \
//	    -in owl.png -w 18 -h 7 -threshold 110 \
//	    -crop 540,140,910,800 -quoted
//
// Each Braille char carries 8 dot positions (2×4) — virtual resolution
// at this banner size is 36×28 pixels. Compact 2.6:1 aspect ratio
// (2026-05-16 user pick after iterating 24×14 → 18×10 → 30×7 →
// settling on this size as "稍宽,特征最清晰"): the silhouette stays
// legible, the wing spread is recognizable, and the banner only eats
// 7 rows so the first chat turn lands above the fold on a standard
// terminal. Hermes-agent's caduceus is 15-row-tall for comparison —
// metis goes half as tall to keep the welcome card from dominating
// first impressions the way hermes's does.
var metisOwlGlyphLines = []string{
	"⠀⠀⢳⣦⣤⣖⡋⠩⡽⠯⡏⢙⣓⣤⣤⣞⠀⠀",
	"⠀⠀⢈⡍⣁⢙⡛⠷⡖⢠⠾⢛⡛⣈⣩⡁⠀⠀",
	"⠀⠀⠈⣇⠁⣠⣅⠀⣈⣈⡀⣨⣥⠘⢹⠃⠀⠀",
	"⣤⢤⣤⣬⡓⠲⠖⠊⠘⡏⠙⠒⠷⢚⣧⣤⡤⣤",
	"⠿⡷⢜⠓⠭⠹⠏⢻⣦⣴⣟⠻⢫⠭⠲⡫⢼⡿",
	"⠀⠈⡱⢫⠟⠞⢀⠩⠛⠟⠌⡀⠰⠙⠝⢯⡁⠀",
	"⠀⠀⠀⠤⠐⠀⣈⢅⢈⡃⣀⣅⠀⠔⠐⠤⠀⠀",
}

// Eye-row index — the row that holds the owl's eye ring. The
// renderer paints this row in cyan (#00D5E5) to echo the "glowing
// iris" detail in image #29. In the 18×7 layout the eye ring sits
// at row 2 (forehead → eye band → cheek), right above the wing-
// spread midline at row 3.
const owlEyeRow = 2

// owlRowColor returns the foreground color for row i of the owl
// banner — colors picked by anatomical part, not a linear silver
// gradient, so the banner reads as a real owl rather than a
// monochrome silhouette.
//
// Palette borrows from both Athena's mythological iconography
// (owl, olive branch, golden helmet) and image #29's metallic
// silver-with-cyan-accent aesthetic. Mapping for the 18×7 layout:
//
//	0  ear tufts + crown   #F2D27A  warm amber-gold (raptor highlights)
//	1  brow / forehead     #E0E8F0  ice silver
//	2  eye band            #00D5E5  cyan iris (Athena's "lit" gaze)
//	3  wing-spread bar     #B8C4DC  silver-blue (midline outline)
//	4  wing feathers       #8090B8  steel-blue
//	5  wing tips           #5C6F8E  dim steel
//	6  talons + olive      #88A056  olive green (Athena's olive branch)
//
// Saturated colors are reserved for the visual "anchor" rows
// (ear tufts, eyes, olive branch) — the surrounding silver/
// steel-blue stays close to neutral so the highlights pop without
// the whole banner reading as a clown wash. Bumped saturation on
// the amber/cyan/olive versus prior passes to defeat the "all
// looks white" perception users got when half the rows used pale
// tints that 256-color terminals quantized to bright_white.
func owlRowColor(i int) color.Color {
	switch i {
	case 0:
		return lipgloss.Color("#F2D27A") // amber-gold ear tufts + crown
	case 1:
		return lipgloss.Color("#E0E8F0") // ice-silver brow
	case owlEyeRow:
		return lipgloss.Color("#00D5E5") // cyan iris
	case 3:
		return lipgloss.Color("#B8C4DC") // silver-blue wing-spread bar
	case 4:
		return lipgloss.Color("#8090B8") // steel-blue wing feathers
	case 5:
		return lipgloss.Color("#5C6F8E") // dim steel wing tips
	default:
		return lipgloss.Color("#88A056") // olive-green talons + branch
	}
}

// renderWelcomeBanner paints the bordered, centered welcome card shown on a
// fresh session. Once the first prompt is submitted, the active-chat path uses
// the compact sticky header instead of repeating this large card in history.
func (m *Model) renderWelcomeBanner() string {
	return m.renderWelcomeBannerCard()
}

func (m *Model) renderWelcomeBannerCard() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(accentBlue).
		Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(textMuted)
	valueStyle := lipgloss.NewStyle().Foreground(textPrimary)

	// Owl glyph painted in per-row color tiers — matches the silver-
	// with-cyan-accent aesthetic of image #29. Hermes uses the same
	// trick on its caduceus banner (per-row Rich [#hex] markup):
	// dividing the silhouette into bands gives a "lit from above"
	// feel that's much more visually rich than a single-color paint
	// at the same character cost. Band mapping lives in owlRowColor
	// next to the glyph data so the palette + layout stay in sync.
	rowStyles := make([]lipgloss.Style, len(metisOwlGlyphLines))
	for i := range rowStyles {
		rowStyles[i] = lipgloss.NewStyle().Foreground(owlRowColor(i)).Bold(true)
	}

	owlRows := make([]string, len(metisOwlGlyphLines))
	for i, raw := range metisOwlGlyphLines {
		owlRows[i] = rowStyles[i].Render(raw)
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
			valueStyle.Render(effectiveModelID(m)),
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
		valueStyle.Render(effectiveModelID(m))
	if mode != "" {
		row += dimStyle.Render(" · ") + valueStyle.Render(mode)
	}
	row += dimStyle.Render(" · ") + valueStyle.Render(cwd)

	// Leave the terminal's final column unused. Writing a printable cell in
	// the rightmost column puts iTerm2/tmux into pending-wrap state; repeated
	// full-screen repaints can then advance one physical row even though the
	// frame still contains only one newline-delimited separator row. The
	// resulting cursor drift leaves fragments of older frames in the middle
	// of long conversations.
	sepWidth := m.width - 1
	if sepWidth <= 0 {
		sepWidth = 59
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
