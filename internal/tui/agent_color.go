package tui

// agent_color.go — per-teammate color palette for sub-agent output
// rendering (G.7, 2026-05-12). The TUI uses this to prefix sub-agent
// lines with a colored `[name]` chip so the user can visually
// distinguish concurrent teammates' streams in the same chat scroll.
//
// Eight curated colors picked from the same palette as the auth
// wizard / status bar — colorblind-aware (no green/red adjacency, no
// pure red anywhere), high contrast on both light and dark themes.
//
// Color assignment is deterministic on the teammate's name (hash mod
// palette size), so the same name comes back to the same color across
// restarts. "general" / "" / "main" all bypass coloring to keep the
// default look untouched.

import (
	"hash/fnv"
	"image/color"

	"charm.land/lipgloss/v2"
)

// agentPalette is the 8-color palette used for sub-agent name chips.
// Picked from material design vibrant — high contrast, distinct hues,
// readable on both dark and light terminal themes.
var agentPalette = []color.Color{
	lipgloss.Color("#64b5f6"), // blue 300
	lipgloss.Color("#81c784"), // green 300
	lipgloss.Color("#ffb74d"), // orange 300
	lipgloss.Color("#ba68c8"), // purple 300
	lipgloss.Color("#4dd0e1"), // cyan 300
	lipgloss.Color("#fff176"), // yellow 300
	lipgloss.Color("#f06292"), // pink 300
	lipgloss.Color("#a1887f"), // brown 300
}

// uncoloredAgentNames is the set of names that should render without
// a color chip. The default "general" agent and the main loop's
// implicit name "main" both render as plain text so the chat doesn't
// look "decorated" by default — only when the user is actually
// running named teammates.
var uncoloredAgentNames = map[string]bool{
	"":        true,
	"general": true,
	"main":    true,
}

// ColorForAgent returns the palette color associated with the given
// teammate name, or nil for names that should render uncolored.
// Stable across runs — same name → same color (FNV-1a hash mod
// palette size).
func ColorForAgent(name string) color.Color {
	if uncoloredAgentNames[name] {
		return nil
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	idx := h.Sum32() % uint32(len(agentPalette))
	return agentPalette[idx]
}

// StyleForAgent returns a lipgloss style suitable for rendering a
// `[name]` chip in the chat scroll. Returns a default empty style
// for names that should render uncolored — caller can use it
// directly without nil-checking.
func StyleForAgent(name string) lipgloss.Style {
	c := ColorForAgent(name)
	if c == nil {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(c).Bold(true)
}
