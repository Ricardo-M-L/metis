package tui

import "github.com/Ricardo-M-L/metis/internal/tui/screen"

// buildThemeChoices snapshots each registered theme into a ThemeChoice
// for the /theme picker widget. Swatches are sampled from the live
// palette (text + accents) so the user previews the actual colors
// before committing.
func buildThemeChoices() []screen.ThemeChoice {
	out := make([]screen.ThemeChoice, 0, len(allThemes))
	for _, name := range ThemeNames() {
		t := allThemes[name]
		out = append(out, screen.ThemeChoice{
			Name: t.Name,
			Swatches: []string{
				string(t.AccentCyan),  // user
				string(t.AccentBlue),  // accent
				string(t.AccentGreen), // success
				string(t.AccentOrange), // warn
				string(t.AccentRed),   // error
				string(t.TextMuted),   // muted gutter
			},
		})
	}
	return out
}
