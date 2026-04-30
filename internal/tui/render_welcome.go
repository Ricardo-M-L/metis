package tui

// render_welcome.go — empty-transcript welcome banner. Shown until the
// user sends their first message; after that we drop straight into the
// chat surface. Same package as the rest of tui so the renderer can
// read Model state without exporting fields.

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderWelcomeBanner paints the fresh-session greeting. The earlier
// pixel-block "METIS" wordmark rendered ambiguously (the `█▀` for S and
// `█▀▀` for E read as Γ and C in many fonts — the user reported it as
// "MCTIS"), so we now use a clean stylized title plus a tagline. Same
// visual weight, none of the cross-font fragility.
func (m *Model) renderWelcomeBanner() string {
	var s strings.Builder

	titleStyle := lipgloss.NewStyle().
		Foreground(accentBlue).
		Bold(true).
		PaddingLeft(2)
	tagStyle := lipgloss.NewStyle().
		Foreground(textMuted).
		Italic(true).
		PaddingLeft(2)
	labelStyle := lipgloss.NewStyle().Foreground(textMuted)
	valueStyle := lipgloss.NewStyle().Foreground(textPrimary)
	accent := lipgloss.NewStyle().Foreground(accentBlue)

	// 1) Header — "metis" in bold accent + 2-line tagline. Same brand
	// presence as the old block art, half the rows, zero font risk.
	s.WriteString("\n")
	s.WriteString(titleStyle.Render("✻ metis"))
	s.WriteString("\n")
	s.WriteString(tagStyle.Render("local-first agent CLI · cunning intelligence"))
	s.WriteString("\n\n")

	// 2) Session info row — model + mode + session id.
	// claude-code's first frame shows the same triple so the user can
	// confirm "yes I'm pointed at the right model" without typing /info.
	s.WriteString(labelStyle.Render("  model: "))
	s.WriteString(valueStyle.Render(m.model))
	s.WriteString(labelStyle.Render("  ·  mode: "))
	s.WriteString(accent.Render(string(m.gate.Mode())))
	if m.sessionID != "" {
		s.WriteString(labelStyle.Render("  ·  session: "))
		// First 8 chars of the UUID — the leading hex group is enough
		// to disambiguate any two sessions you'd be looking at in the
		// /sessions list. No trailing "…" because the truncation is
		// the natural shape of a UUID prefix, not a cropping cue.
		sid := m.sessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		s.WriteString(valueStyle.Render(sid))
	}
	s.WriteString("\n\n")

	// 4) Quick start hint — single muted row, no extra scaffolding.
	s.WriteString(labelStyle.Render("  Type a message to start  ·  "))
	s.WriteString(accent.Render("/help"))
	s.WriteString(labelStyle.Render(" for commands  ·  "))
	s.WriteString(accent.Render("/quit"))
	s.WriteString(labelStyle.Render(" to exit"))
	s.WriteString("\n\n")

	return s.String()
}
