package main

// SetNativeTheme is the native half of the theme bridge used by the Web UI
// iframe. Wails' generic runtime theme helpers are no-ops on macOS, so the
// Desktop shell delegates to the platform implementation instead.
func (a *App) SetNativeTheme(theme string) error {
	return applyNativeTheme(theme)
}
