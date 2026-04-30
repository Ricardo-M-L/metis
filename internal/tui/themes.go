package tui

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
	"os"

	"github.com/charmbracelet/lipgloss"
)

// Theme bundles all the chat-surface colors. Every renderer reads from
// this struct via the package-level `currentTheme` so a /theme switch
// or METIS_THEME env var only requires us to swap the pointer and
// re-init the derived lipgloss.Style vars.
type Theme struct {
	Name string

	// Backgrounds
	BgSecondary lipgloss.Color // panel highlight

	// Text tiers (4 layers of tonality)
	TextPrimary   lipgloss.Color // body text
	TextSecondary lipgloss.Color // dimmed metadata
	TextMuted     lipgloss.Color // labels, separators, gutters

	// Accents
	AccentBlue   lipgloss.Color // primary (links, selection)
	AccentGreen  lipgloss.Color // assistant, success
	AccentOrange lipgloss.Color // tool, warning
	AccentRed    lipgloss.Color // error, deny
	AccentCyan   lipgloss.Color // user input

	// Diff bg/fg pairs (separately tuned because the bg+fg combo needs
	// to stay legible — picking these by hand beats deriving from the
	// accent colors).
	DiffAddBg lipgloss.Color
	DiffAddFg lipgloss.Color
	DiffDelBg lipgloss.Color
	DiffDelFg lipgloss.Color
}

// darkTheme is metis's default — what every previous release shipped.
// Tuned for dark terminals (iTerm2, default macOS Terminal.app dark,
// WezTerm dark variants).
var darkTheme = Theme{
	Name:          "dark",
	BgSecondary:   "#16213e",
	TextPrimary:   "#e8e8e8",
	TextSecondary: "#a0a0a0",
	TextMuted:     "#606060",
	AccentBlue:    "#64b5f6",
	AccentGreen:   "#81c784",
	AccentOrange:  "#ffb74d",
	AccentRed:     "#e57373",
	AccentCyan:    "#4dd0e1",
	DiffAddBg:     "#1e3a1e",
	DiffAddFg:     "#a3e8a3",
	DiffDelBg:     "#3a1e1e",
	DiffDelFg:     "#e8a3a3",
}

// lightTheme inverts the tonality for light terminals — primary text
// becomes near-black, dim becomes mid-grey. Accents are darker to keep
// contrast against a white background.
var lightTheme = Theme{
	Name:          "light",
	BgSecondary:   "#e6e6f0",
	TextPrimary:   "#1a1a1a",
	TextSecondary: "#555555",
	TextMuted:     "#909090",
	AccentBlue:    "#1976d2",
	AccentGreen:   "#388e3c",
	AccentOrange:  "#e65100",
	AccentRed:     "#c62828",
	AccentCyan:    "#0097a7",
	DiffAddBg:     "#d4f0d4",
	DiffAddFg:     "#1b5e20",
	DiffDelBg:     "#f5d0d0",
	DiffDelFg:     "#b71c1c",
}

// darkDaltonizedTheme replaces the green/red diff signals with
// cyan/magenta so red-green color-blind users (~8% of men) can
// still tell add from delete by hue, not just position.
// Other accents remain similar to the dark theme.
var darkDaltonizedTheme = Theme{
	Name:          "dark-daltonized",
	BgSecondary:   "#16213e",
	TextPrimary:   "#e8e8e8",
	TextSecondary: "#a0a0a0",
	TextMuted:     "#606060",
	AccentBlue:    "#64b5f6",
	AccentGreen:   "#4dd0e1", // cyan stand-in for assistant/success
	AccentOrange:  "#ffb74d",
	AccentRed:     "#e91e63", // magenta stand-in for error
	AccentCyan:    "#80deea",
	DiffAddBg:     "#1a3a3e",
	DiffAddFg:     "#80deea",
	DiffDelBg:     "#3a1a2e",
	DiffDelFg:     "#f48fb1",
}

var allThemes = map[string]*Theme{
	"dark":            &darkTheme,
	"light":           &lightTheme,
	"dark-daltonized": &darkDaltonizedTheme,
}

// currentTheme is the active palette. Resolved at init from
// METIS_THEME (env) with darkTheme as fallback. Callers reference
// it indirectly via the styleX vars in tui.go which get re-init'd
// on /theme switch via initStyles().
var currentTheme = func() *Theme {
	if t, ok := allThemes[os.Getenv("METIS_THEME")]; ok {
		return t
	}
	return &darkTheme
}()

// SwitchTheme swaps the active theme by name and re-initializes the
// derived style vars. /theme command in commands.go calls this.
// Returns the resolved theme name (or empty string if name unknown).
func SwitchTheme(name string) string {
	t, ok := allThemes[name]
	if !ok {
		return ""
	}
	currentTheme = t
	initStyles()
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
