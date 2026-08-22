package tui

// widgetHint returns the palette-row description override for slash
// commands that open an interactive widget (Phase C series). Returning
// non-empty replaces the registered Description so the user sees what
// will happen when they press Enter:
//
//	/effort       → "→ slider: off / low / medium / high"
//	/help         → "→ tabs: general / commands / custom-commands"
//	/model        → "→ picker: choose model from curated list"
//	/theme        → "→ cycle: dark / light / dark-daltonized / nord / solarized-dark"
//	/permissions  → "→ editor: cycle mode + view rules"
//
// Empty return means "use the registered Description" — the default
// behavior for non-widget commands.
func widgetHint(cmdName string) string {
	switch cmdName {
	case "effort":
		return "→ slider: off / low / medium / high"
	case "help", "h", "?":
		return "→ tabs: general / commands / custom-commands"
	case "model", "m":
		return "→ picker: choose from curated model list"
	case "theme":
		return "→ cycle: dark / light / dark-daltonized / nord / solarized-dark + live swatches"
	case "permissions", "perms":
		return "→ editor: cycle mode + view active rules"
	}
	return ""
}
