package tui

// tui_styles.go — color palette + derived lipgloss styles. Concrete
// colors live on currentTheme (themes.go); the styleXxx vars below
// get re-init'd by initStyles() each time the theme changes. Adding a
// new theme = adding a Theme to themes.go; no per-callsite plumbing.

import "github.com/charmbracelet/lipgloss"

var (
	// Color shortcuts — populated by initStyles() from currentTheme.
	bgSecondary   lipgloss.Color
	textPrimary   lipgloss.Color
	textSecondary lipgloss.Color
	textMuted     lipgloss.Color
	accentBlue    lipgloss.Color
	accentGreen   lipgloss.Color
	accentOrange  lipgloss.Color
	accentRed     lipgloss.Color
	accentCyan    lipgloss.Color

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
// currentTheme points at. Called once during package init and again
// on every /theme switch.
func initStyles() {
	t := currentTheme
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
	initStyles()
}
