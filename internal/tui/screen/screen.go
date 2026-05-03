// Package screen holds full-window TUI overlays — components that take over
// the screen until the user dismisses them, distinct from the always-on chat
// surface. Each screen is a self-contained bubbletea sub-model so the
// monolithic tui.go doesn't grow another switch case for each one.
//
// Design parallels openclaude's `src/screens/` directory: the parent Model
// owns lifecycle (open/close), the screen owns its own state, scroll, and
// keybindings. Communication is one-way — screens signal "I'm done" via
// Done(), the parent then drops the reference and returns to chat.
//
// Screens MUST NOT import internal/tui to keep the dependency direction
// clean (tui imports screen, never the reverse).
package screen

import tea "charm.land/bubbletea/v2"

// Screen is the contract every overlay implements. The parent Model holds a
// concrete value and forwards Update/View calls until Done() returns true.
type Screen interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Screen, tea.Cmd)
	View() string
	// Done reports that the user has dismissed the screen and the parent
	// should drop its reference. The screen's last View() before Done() is
	// the final frame — the parent shouldn't render after that.
	Done() bool
	// Resize lets the parent push window dimension updates without going
	// through the bubbletea WindowSizeMsg cycle (e.g. on initial open).
	Resize(width, height int)
}
