package tui

// render_overlay.go — overlays painted on top of the chat surface:
// slash-command palette, permission prompt, task-list panel, and the
// vertical scrollbar. None of these own state — they read Model fields
// and render strings.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

// renderScrollbar overlays a vertical scrollbar on the right edge of
// the viewport view. Track is dim, thumb is bright. claude-code shows
// a visible bar with a multi-cell thumb proportional to viewport
// coverage — we mirror that so very long transcripts produce a short
// thumb (lots to scroll) and short transcripts a tall one.
//
// Implementation: split viewport lines, append the appropriate glyph
// to each. Lines past viewport.Height are left untouched. Caller has
// reserved width for us via the WindowSizeMsg handler.
func renderScrollbar(view string, vp *viewport.Model) string {
	lines := strings.Split(view, "\n")
	if len(lines) == 0 {
		return view
	}
	height := vp.Height
	if height <= 0 {
		return view
	}
	// Thumb height proportional to visible/total ratio, min 1, max
	// height. Without this every scrollbar shows a 1-cell thumb
	// regardless of context size — looks like a stuck dot.
	total := vp.TotalLineCount()
	thumbH := height
	if total > 0 {
		thumbH = int(float64(height) * float64(height) / float64(total))
	}
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > height {
		thumbH = height
	}
	pct := vp.ScrollPercent()
	thumbStart := int(pct * float64(height-thumbH))
	if thumbStart < 0 {
		thumbStart = 0
	}
	if thumbStart+thumbH > height {
		thumbStart = height - thumbH
	}
	// Track stays a thin line for context; thumb uses a full block so
	// it stands out without claiming an extra column. `┃` (heavy vert
	// bar) + same 1-col width made the thumb basically invisible on
	// most terminals (image #62 the user complained the scrollbar was
	// "very thin"). `█` is the clearest 1-col indicator that doesn't
	// require an extra column reservation in the layout math.
	track := styleMuted.Render("│")
	thumb := styleAccent.Render("█")
	for i := 0; i < len(lines) && i < height; i++ {
		glyph := track
		if i >= thumbStart && i < thumbStart+thumbH {
			glyph = thumb
		}
		// Single-space gutter then glyph — keeps content from
		// touching the bar. Caller reserved width=msg.Width-2 so
		// this always lands inside the terminal.
		lines[i] = lines[i] + " " + glyph
	}
	return strings.Join(lines, "\n")
}

// renderTaskPanel draws the Ctrl+T todo overlay. Status icons match
// claude-code's task UI:
//
//	▢  pending
//	◐  in_progress
//	▣  completed
//
// Empty session reads "(no todos yet)". Long titles get truncated.
func renderTaskPanel(m *Model) string {
	tasks := tasksFullList(m.sessionID)
	var s strings.Builder
	s.WriteString(styleMuted.Render("  ┌─ tasks ") + styleMuted.Render(strings.Repeat("─", 36)))
	s.WriteString("\n")
	if len(tasks) == 0 {
		s.WriteString(styleMuted.Render("  │  (no todos yet — TodoWrite tool populates this)"))
		s.WriteString("\n")
		s.WriteString(styleMuted.Render("  └" + strings.Repeat("─", 44)))
		s.WriteString("\n")
		return s.String()
	}
	for _, t := range tasks {
		var icon string
		var col lipgloss.Style
		switch t.Status {
		case "pending":
			icon = "▢"
			col = styleMuted
		case "in_progress":
			icon = "◐"
			col = styleAccent
		case "completed":
			icon = "▣"
			col = lipgloss.NewStyle().Foreground(accentGreen)
		default:
			icon = "?"
			col = styleMuted
		}
		s.WriteString(styleMuted.Render("  │ "))
		s.WriteString(col.Render(icon + " "))
		content := t.Content
		if len(content) > 40 {
			content = content[:39] + "…"
		}
		s.WriteString(styleText.Render(content))
		s.WriteString("\n")
	}
	s.WriteString(styleMuted.Render("  └" + strings.Repeat("─", 44)))
	s.WriteString("\n")
	return s.String()
}

// renderPalette draws the live slash-command suggestion box. Inspired by
// claude-code's "/" popup: a small bordered list of matches that filters
// as the user types, with the highlighted row marked by a "▸". We cap
// at paletteMaxRows so a long match list doesn't fill the terminal.
const paletteMaxRows = 8

func renderPalette(m *Model) string {
	var s strings.Builder
	s.WriteString(styleMuted.Render("  ┌─ slash commands ") + styleMuted.Render("─────────────────────────") + "\n")

	if len(m.palMatched) == 0 {
		s.WriteString(styleMuted.Render("  │ ") + styleErr.Render("no match for /"+m.palFilter) + "\n")
		s.WriteString(styleMuted.Render("  └────────────────────────────────────────────") + "\n")
		return s.String()
	}

	rows := m.palMatched
	start := 0
	if len(rows) > paletteMaxRows {
		if m.palCursor >= paletteMaxRows {
			start = m.palCursor - paletteMaxRows + 1
		}
		end := start + paletteMaxRows
		if end > len(rows) {
			end = len(rows)
			start = end - paletteMaxRows
		}
		rows = rows[start:end]
	}

	for i, cmd := range rows {
		idx := start + i
		marker := "  "
		if idx == m.palCursor {
			marker = styleAccent.Render("▸ ")
		}
		s.WriteString(styleMuted.Render("  │ "))
		s.WriteString(marker)
		name := fmt.Sprintf("/%-10s ", cmd.Name)
		if idx == m.palCursor {
			s.WriteString(styleSelected.Render(name))
		} else {
			s.WriteString(styleText.Render(name))
		}
		desc := cmd.Description
		if len(desc) > 40 {
			desc = desc[:40] + "…"
		}
		s.WriteString(styleMuted.Render(desc))
		s.WriteString("\n")
	}
	if len(m.palMatched) > paletteMaxRows {
		s.WriteString(styleMuted.Render(fmt.Sprintf("  │ … %d more (↑↓ to scroll)\n",
			len(m.palMatched)-paletteMaxRows)))
	}
	s.WriteString(styleMuted.Render("  └────────────────────────────────────────────") + "\n")
	return s.String()
}

// historySearchMaxRows caps the visible candidate list so the overlay
// doesn't push the input off-screen on small terminals.
const historySearchMaxRows = 10

// renderHistorySearch paints the Ctrl+R fuzzy-history overlay. Layout
// mirrors renderPalette so muscle memory carries: bordered top, "▸"
// highlight, "↑↓ select · enter use · esc cancel" footer hint.
//
// The query line ("(reverse-i-search) `<filter>`:") is borrowed from
// bash readline's Ctrl+R prompt — most terminal users will pattern-
// match it instantly.
func renderHistorySearch(m *Model) string {
	var s strings.Builder
	s.WriteString(styleMuted.Render("  ┌─ reverse-i-search ") + styleMuted.Render("───────────────────────") + "\n")
	s.WriteString(styleMuted.Render("  │ ") + styleAccent.Render("`"+m.histFilter+"`:") + "\n")

	if len(m.histMatched) == 0 {
		hint := "(no history yet — submit prompts to populate)"
		if m.histFilter != "" {
			hint = "(no match for `" + m.histFilter + "`)"
		}
		s.WriteString(styleMuted.Render("  │ ") + styleErr.Render(hint) + "\n")
		s.WriteString(styleMuted.Render("  └────────────────────────────────────────────") + "\n")
		return s.String()
	}

	rows := m.histMatched
	start := 0
	if len(rows) > historySearchMaxRows {
		if m.histCursor >= historySearchMaxRows {
			start = m.histCursor - historySearchMaxRows + 1
		}
		end := start + historySearchMaxRows
		if end > len(rows) {
			end = len(rows)
			start = end - historySearchMaxRows
		}
		rows = rows[start:end]
	}

	for i, prompt := range rows {
		idx := start + i
		marker := "  "
		if idx == m.histCursor {
			marker = styleAccent.Render("▸ ")
		}
		s.WriteString(styleMuted.Render("  │ "))
		s.WriteString(marker)
		// Truncate long prompts to keep one-row-per-entry — multi-line
		// pastes (`<<<...>>>`) compress to "first 60 chars …".
		preview := strings.ReplaceAll(prompt, "\n", " ")
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		if idx == m.histCursor {
			s.WriteString(styleSelected.Render(preview))
		} else {
			s.WriteString(styleText.Render(preview))
		}
		s.WriteString("\n")
	}
	if len(m.histMatched) > historySearchMaxRows {
		s.WriteString(styleMuted.Render(fmt.Sprintf("  │ … %d more (↑↓ to scroll)\n",
			len(m.histMatched)-historySearchMaxRows)))
	}
	s.WriteString(styleMuted.Render("  └─ ↑↓ select · enter use · esc cancel ──────") + "\n")
	return s.String()
}

// renderAtMention paints the live `@xxx` file-completion dropdown. Sits
// above the input box so the user can see candidates as they type.
// Same visual idiom as renderPalette / renderHistorySearch — bordered,
// "▸" highlight, hint footer.
//
// claude-code shows file paths as relative-to-cwd which is what
// matchAtMention already produces; we render them verbatim.
func renderAtMention(m *Model) string {
	if !m.atActive || len(m.atMatched) == 0 {
		return ""
	}
	var s strings.Builder
	header := "  ┌─ @-mention "
	if m.atFilter != "" {
		header += "(" + m.atFilter + ") "
	}
	s.WriteString(styleMuted.Render(header + strings.Repeat("─", 30)))
	s.WriteString("\n")
	for i, path := range m.atMatched {
		marker := "  "
		if i == m.atCursor {
			marker = styleAccent.Render("▸ ")
		}
		s.WriteString(styleMuted.Render("  │ "))
		s.WriteString(marker)
		preview := path
		if len(preview) > 60 {
			preview = "…" + preview[len(preview)-59:]
		}
		if i == m.atCursor {
			s.WriteString(styleSelected.Render(preview))
		} else {
			s.WriteString(styleText.Render(preview))
		}
		s.WriteString("\n")
	}
	s.WriteString(styleMuted.Render("  └─ ↑↓ select · tab use · esc cancel ────────") + "\n")
	return s.String()
}

// renderPermission draws the permission prompt — claude-code's
// flat, no-box style. Hierarchical text:
//
//	<tool name>
//	  <arg preview>
//
//	Allow this action?
//	❯ 1. Yes
//	  2. Yes, always (this session)
//	  3. No
//	  4. Cancel turn
//
//	  ↑↓ select · enter confirm · 1-4 / y a n c shortcuts
//
// Tool name is bold/orange (the permission accent color). Arg preview
// sits dim under it. Numbered options match claude-code's convention.
func renderPermission(m *Model) string {
	var s strings.Builder
	headerStyle := lipgloss.NewStyle().Foreground(accentOrange).Bold(true)

	s.WriteString("\n")
	if m.permTool != "" {
		s.WriteString("  ")
		s.WriteString(headerStyle.Render(m.permTool + " command"))
		s.WriteString("\n")
		if m.permArgs != "" {
			s.WriteString("    ")
			s.WriteString(styleText.Render(m.permArgs))
			s.WriteString("\n")
		}
	} else if m.permQuestion != "" {
		s.WriteString("  ")
		s.WriteString(headerStyle.Render(m.permQuestion))
		s.WriteString("\n")
	}
	s.WriteString("\n")

	s.WriteString("  ")
	if m.permTool != "" {
		s.WriteString(styleText.Render("Allow this " + m.permTool + " command?"))
	} else {
		s.WriteString(styleText.Render("Allow this action?"))
	}
	s.WriteString("\n")

	for i, choice := range m.permChoices {
		s.WriteString("  ")
		num := fmt.Sprintf("%d. ", i+1)
		if i == m.permCursor {
			s.WriteString(styleAccent.Bold(true).Render("❯ "))
			s.WriteString(styleAccent.Bold(true).Render(num + choice.Label))
		} else {
			s.WriteString("  ")
			s.WriteString(styleMuted.Render(num))
			s.WriteString(styleText.Render(choice.Label))
		}
		s.WriteString("\n")
	}
	s.WriteString("\n")
	s.WriteString(styleMuted.Render("  ↑↓ select · enter confirm · y/a/n/c shortcuts · esc deny"))
	s.WriteString("\n")
	return s.String()
}
