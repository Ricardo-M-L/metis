package themes

import "github.com/Ricardo-M-L/metis/internal/tui/screen"

// themeSwatchHex enumerates each theme's swatch colors as hex strings.
//
// v2: the Theme struct now stores color.Color interfaces (not the v1
// `type Color string` alias), so a `string(t.AccentCyan)` cast no
// longer works. Recovering the hex from the interface requires a type
// assertion to lipgloss.RGBColor and a Sprintf — at which point a
// hardcoded lookup keyed by theme name is shorter, faster, and keeps
// the swatch source-of-truth in one obvious place. If a theme adds
// or renames an accent, both this map and themes.go need a touch.
var themeSwatchHex = map[string][]string{
	"dark": {
		"#4dd0e1", // user / cyan
		"#64b5f6", // accent / blue
		"#81c784", // success / green
		"#ffb74d", // warn / orange
		"#e57373", // error / red
		"#606060", // muted gutter
	},
	"light": {
		"#0097a7",
		"#1976d2",
		"#388e3c",
		"#e65100",
		"#c62828",
		"#909090",
	},
	"dark-daltonized": {
		"#80deea",
		"#64b5f6",
		"#4dd0e1", // green stand-in (cyan)
		"#ffb74d",
		"#e91e63", // red stand-in (magenta)
		"#606060",
	},
	"nord": {
		"#88c0d0", // user / cyan
		"#81a1c1", // accent / blue
		"#a3be8c", // success / green
		"#d08770", // warn / orange
		"#bf616a", // error / red
		"#868f9c", // muted gutter
	},
	"solarized-dark": {
		"#2aa198", // user / cyan
		"#268bd2", // accent / blue
		"#859900", // success / green
		"#cb4b16", // warn / orange
		"#dc322f", // error / red
		"#657b83", // muted gutter
	},
}

// BuildThemeChoices snapshots each registered theme into a ThemeChoice
// for the /theme picker widget. Swatches are sampled from
// themeSwatchHex so the user previews the actual colors before
// committing.
func BuildThemeChoices() []screen.ThemeChoice {
	out := make([]screen.ThemeChoice, 0, len(allThemes))
	for _, name := range ThemeNames() {
		t := allThemes[name]
		out = append(out, screen.ThemeChoice{
			Name:     t.Name,
			Swatches: themeSwatchHex[name],
		})
	}
	return out
}
