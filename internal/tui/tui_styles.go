package tui

// tui_styles.go — color palette + derived lipgloss styles. Concrete
// colors live on the active *themes.Theme (internal/themes); the
// styleXxx vars below get re-init'd by initStyles() each time the
// theme changes via the themes.OnSwitch callback registered in init().
// Adding a new theme = adding a Theme in internal/themes; no
// per-callsite plumbing here.

import (
	"image/color"

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/themes"
)

var (
	// Color shortcuts — populated by initStyles() from currentTheme.
	// v2: lipgloss.Color became a function; the equivalent type is
	// image/color.Color.
	bgSecondary   color.Color
	textPrimary   color.Color
	textSecondary color.Color
	textMuted     color.Color
	accentBlue    color.Color
	accentGreen   color.Color
	accentOrange  color.Color
	accentRed     color.Color
	accentCyan    color.Color

	// Pre-built styles. Renderers call these directly.
	styleText     lipgloss.Style
	styleDim      lipgloss.Style
	styleMuted    lipgloss.Style
	styleAccent   lipgloss.Style
	styleUser     lipgloss.Style
	styleAsst     lipgloss.Style
	styleToolName lipgloss.Style
	styleErr      lipgloss.Style
	styleSuccess  lipgloss.Style // green ✓ for success role
	styleWarn     lipgloss.Style // orange ⚠ for warning role
	styleSelected lipgloss.Style
)

// initStyles binds the package-level style vars to whatever
// themes.Current() points at. Called once during package init and
// again on every /theme switch via the themes.OnSwitch hook.
func initStyles() {
	t := themes.Current()
	bgSecondary = t.BgSecondary
	textPrimary = t.TextPrimary
	textSecondary = t.TextSecondary
	textMuted = t.TextMuted
	accentBlue = t.AccentBlue
	accentGreen = t.AccentGreen
	accentOrange = t.AccentOrange
	accentRed = t.AccentRed
	accentCyan = t.AccentCyan

	styleText = lipgloss.NewStyle().Foreground(textPrimary)
	styleDim = lipgloss.NewStyle().Foreground(textSecondary)
	styleMuted = lipgloss.NewStyle().Foreground(textMuted)
	styleAccent = lipgloss.NewStyle().Foreground(accentBlue)
	styleUser = lipgloss.NewStyle().Foreground(accentCyan).Bold(true)
	styleAsst = lipgloss.NewStyle().Foreground(accentGreen)
	styleToolName = lipgloss.NewStyle().Foreground(accentOrange).Bold(true)
	styleErr = lipgloss.NewStyle().Foreground(accentRed).Bold(true)
	styleSuccess = lipgloss.NewStyle().Foreground(accentGreen).Bold(true)
	styleWarn = lipgloss.NewStyle().Foreground(accentOrange).Bold(true)
	styleSelected = lipgloss.NewStyle().Foreground(accentBlue).Bold(true).Background(bgSecondary)
}

func init() {
	themes.OnSwitch(initStyles)
	initStyles()
}
