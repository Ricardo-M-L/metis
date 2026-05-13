package themes

// themes.go centralizes the chat-surface color palette behind a Theme
// struct so the rest of the renderer never names a #hex literal. Three
// themes ship today:
//
//   - dark            (default — what previous metis releases shipped)
//   - light           (for daytime / projector-friendly terminals)
//   - dark-daltonized (red-green color-blind safe; replaces add-green
//                      with cyan and del-red with bright magenta so
//                      diff direction stays distinguishable)
//
// claude-code has a similar set (theme.ts) plus dark-ansi / dark-base16;
// we cover the common cases and leave the rest as follow-up.

import (
	"image/color"
	"os"

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/term"
)

// Theme bundles all the chat-surface colors. Every renderer reads from
// this struct via the package-level `currentTheme` so a /theme switch
// or METIS_THEME env var only requires us to swap the pointer and
// re-init the derived lipgloss.Style vars.
type Theme struct {
	Name string

	// Backgrounds
	BgSecondary color.Color // panel highlight

	// Text tiers (4 layers of tonality)
	TextPrimary   color.Color // body text
	TextSecondary color.Color // dimmed metadata
	TextMuted     color.Color // labels, separators, gutters

	// Accents
	AccentBlue   color.Color // primary (links, selection)
	AccentGreen  color.Color // assistant, success
	AccentOrange color.Color // tool, warning
	AccentRed    color.Color // error, deny
	AccentCyan   color.Color // user input

	// Diff bg/fg pairs (separately tuned because the bg+fg combo needs
	// to stay legible — picking these by hand beats deriving from the
	// accent colors).
	DiffAddBg color.Color
	DiffAddFg color.Color
	DiffDelBg color.Color
	DiffDelFg color.Color
}

// darkTheme is metis's default — what every previous release shipped.
// Tuned for dark terminals (iTerm2, default macOS Terminal.app dark,
// WezTerm dark variants).
var darkTheme = Theme{
	Name:        "dark",
	BgSecondary: lipgloss.Color("#16213e"),
	TextPrimary: lipgloss.Color("#e8e8e8"),
	// Two-tier muted ladder. Earlier metis had Muted at #606060 which
	// rendered near-invisible on most dark terminals — image #19/#20
	// user report. Bumped to #787878 (≈ claude-code's "inactive" tier
	// at #999) so "+N more lines" / "(ctrl+o to expand)" hints stay
	// legible without competing with primary text. Secondary stays
	// at #a0a0a0 for "dim but informational" rows (tool summary line).
	TextSecondary: lipgloss.Color("#a0a0a0"),
	TextMuted:     lipgloss.Color("#787878"),
	AccentBlue:    lipgloss.Color("#64b5f6"),
	AccentGreen:   lipgloss.Color("#81c784"),
	AccentOrange:  lipgloss.Color("#ffb74d"),
	AccentRed:     lipgloss.Color("#e57373"),
	AccentCyan:    lipgloss.Color("#4dd0e1"),
	DiffAddBg:     lipgloss.Color("#1e3a1e"),
	DiffAddFg:     lipgloss.Color("#a3e8a3"),
	DiffDelBg:     lipgloss.Color("#3a1e1e"),
	DiffDelFg:     lipgloss.Color("#e8a3a3"),
}

// lightTheme inverts the tonality for light terminals — primary text
// becomes near-black, dim becomes mid-grey. Accents are darker to keep
// contrast against a white background.
var lightTheme = Theme{
	Name:          "light",
	BgSecondary:   lipgloss.Color("#e6e6f0"),
	TextPrimary:   lipgloss.Color("#1a1a1a"),
	TextSecondary: lipgloss.Color("#555555"),
	TextMuted:     lipgloss.Color("#909090"),
	AccentBlue:    lipgloss.Color("#1976d2"),
	AccentGreen:   lipgloss.Color("#388e3c"),
	AccentOrange:  lipgloss.Color("#e65100"),
	AccentRed:     lipgloss.Color("#c62828"),
	AccentCyan:    lipgloss.Color("#0097a7"),
	DiffAddBg:     lipgloss.Color("#d4f0d4"),
	DiffAddFg:     lipgloss.Color("#1b5e20"),
	DiffDelBg:     lipgloss.Color("#f5d0d0"),
	DiffDelFg:     lipgloss.Color("#b71c1c"),
}

// darkDaltonizedTheme replaces the green/red diff signals with
// cyan/magenta so red-green color-blind users (~8% of men) can
// still tell add from delete by hue, not just position.
// Other accents remain similar to the dark theme.
var darkDaltonizedTheme = Theme{
	Name:          "dark-daltonized",
	BgSecondary:   lipgloss.Color("#16213e"),
	TextPrimary:   lipgloss.Color("#e8e8e8"),
	TextSecondary: lipgloss.Color("#a0a0a0"),
	TextMuted:     lipgloss.Color("#787878"),
	AccentBlue:    lipgloss.Color("#64b5f6"),
	AccentGreen:   lipgloss.Color("#4dd0e1"), // cyan stand-in for assistant/success
	AccentOrange:  lipgloss.Color("#ffb74d"),
	AccentRed:     lipgloss.Color("#e91e63"), // magenta stand-in for error
	AccentCyan:    lipgloss.Color("#80deea"),
	DiffAddBg:     lipgloss.Color("#1a3a3e"),
	DiffAddFg:     lipgloss.Color("#80deea"),
	DiffDelBg:     lipgloss.Color("#3a1a2e"),
	DiffDelFg:     lipgloss.Color("#f48fb1"),
}

var allThemes = map[string]*Theme{
	"dark":            &darkTheme,
	"light":           &lightTheme,
	"dark-daltonized": &darkDaltonizedTheme,
}

// currentTheme is the active palette. Resolved at init from this
// precedence (highest wins):
//
//  1. METIS_THEME env var       — explicit user pick
//  2. OSC 11 background probe   — auto-detect light vs dark terminal
//  3. darkTheme                 — safe fallback
//
// The OSC 11 probe (added 2026-05-09 / SUMMARY #31) sends a
// 9-byte query to /dev/tty and waits ≤200ms; non-responding
// contexts fall through silently. Mirrors opencode's terminal-aware
// theme switch.
//
// Callers reference currentTheme indirectly via the styleX vars in
// tui.go which get re-init'd on /theme switch via initStyles().
var currentTheme = func() *Theme {
	if t, ok := allThemes[os.Getenv("METIS_THEME")]; ok {
		return t
	}
	// Skip the probe in headless / CI / non-interactive contexts —
	// no TTY, no answer, but the 200ms wait still adds startup
	// latency we can avoid. NO_COLOR is also a strong "user has
	// non-default expectations" signal where probing is rude.
	if os.Getenv("CI") == "" && os.Getenv("NO_COLOR") == "" {
		if isLight, ok := term.DetectTerminalBackground(); ok && isLight {
			if t, ok := allThemes["light"]; ok {
				return t
			}
		}
	}
	return &darkTheme
}()

// Current returns the active palette pointer. Callers that need to
// read a theme field (renderers, tests) should always go through this
// rather than caching the pointer, since /theme switches the underlying
// var. Pointer-stability across reads is NOT guaranteed.
func Current() *Theme { return currentTheme }

// onSwitch is invoked after every successful SwitchTheme /
// ApplyProviderTint so consumers (notably tui's tui_styles.go) can
// rebuild derived lipgloss.Style vars bound to the previous palette.
// Nil-safe: nothing fires until tui registers via OnSwitch().
var onSwitch func()

// OnSwitch registers a callback to fire after every successful theme
// change. Single-slot — last writer wins. tui_styles.go::init() owns
// this slot in the canonical wiring; tests that swap callbacks must
// restore the previous one.
func OnSwitch(fn func()) { onSwitch = fn }

func fireSwitch() {
	if onSwitch != nil {
		onSwitch()
	}
}

// SwitchTheme swaps the active theme by name and re-fires the
// registered OnSwitch callback. /theme command in commands.go calls
// this. Returns the resolved theme name (or empty string if name
// unknown).
//
// THREAD SAFETY: must be called from the main bubbletea Update
// goroutine. View() reads currentTheme + the styleX vars without
// locking, so a tea.Cmd background call would race the renderer.
func SwitchTheme(name string) string {
	t, ok := allThemes[name]
	if !ok {
		return ""
	}
	currentTheme = t
	fireSwitch()
	return t.Name
}

// ThemeNames lists the available theme names — used by /theme tab
// completion and the no-arg help message.
func ThemeNames() []string {
	out := make([]string, 0, len(allThemes))
	for k := range allThemes {
		out = append(out, k)
	}
	return out
}
