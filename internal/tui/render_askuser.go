package tui

// render_askuser.go — UI for the AskUser tool's blocking question
// prompt. Layout mirrors renderPermission (label-style header in
// accent color → body text → numbered options → footer hints) so the
// two prompts feel like siblings of the same modal family.
//
// What it draws:
//
//	  ❔ AskUser
//	    <question body, wrapped to width>
//
//	    Pick one:
//	  ❯ 1. <option 1>
//	    2. <option 2>
//	    3. <option 3>
//	    [type your own answer]
//	      › <textinput>
//
//	    ↑↓ select · enter confirm · 1-9 shortcut · tab freeform · esc dismiss
//
// The freeform input row only appears when the tool was dispatched
// with allow_freeform=true (or when no options were given, in which
// case ask.go's normalization auto-enables it).

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
)

// newAskUserInput builds the textinput.Model used for freeform answers.
// Mirrors auth_wizard's textinput configuration so the prompt glyph
// and char-limit match the rest of the TUI.
func newAskUserInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.CharLimit = 512
	ti.Placeholder = "type your own answer"
	return ti
}

// askUserOptionWidth caps each rendered option line at this many chars
// (after the "N. " prefix). Options the model emits longer than this
// get truncated with an ellipsis — the tool documents the limit too.
const askUserOptionWidth = 88

func renderAskUser(m *Model) string {
	var s strings.Builder
	headerStyle := lipgloss.NewStyle().Foreground(accentOrange).Bold(true)

	s.WriteString("\n")
	s.WriteString("  ")
	s.WriteString(headerStyle.Render("❔ AskUser"))
	s.WriteString("\n")

	if m.askUserQuestion != "" {
		// Wrap the question to the TUI width minus the 4-space indent.
		// lipgloss.NewStyle.Width handles the wrap; using styleText
		// keeps the foreground color consistent with permission body.
		wrapWidth := m.width - 4
		if wrapWidth < 20 {
			wrapWidth = 20
		}
		wrapped := lipgloss.NewStyle().Width(wrapWidth).Render(m.askUserQuestion)
		for _, line := range strings.Split(wrapped, "\n") {
			s.WriteString("    ")
			s.WriteString(styleText.Render(line))
			s.WriteString("\n")
		}
	}
	s.WriteString("\n")

	if len(m.askUserOptions) > 0 {
		s.WriteString("  ")
		s.WriteString(styleText.Render("Pick one:"))
		s.WriteString("\n")
		for i, opt := range m.askUserOptions {
			optTrimmed := truncateOption(opt, askUserOptionWidth)
			num := fmt.Sprintf("%d. ", i+1)
			s.WriteString("  ")
			// Only highlight when focus is on the option list (not the
			// freeform input). When the user has tabbed into freeform,
			// the option highlight disappears so it's obvious the
			// keystrokes are going to the input field, not the list.
			selected := i == m.askUserCursor && !m.askUserFreeformOn
			if selected {
				s.WriteString(styleAccent.Bold(true).Render("❯ "))
				s.WriteString(styleAccent.Bold(true).Render(num + optTrimmed))
			} else {
				s.WriteString("  ")
				s.WriteString(styleMuted.Render(num))
				s.WriteString(styleText.Render(optTrimmed))
			}
			s.WriteString("\n")
		}
	}

	if m.askUserAllowFreeform {
		s.WriteString("\n")
		s.WriteString("  ")
		if m.askUserFreeformOn {
			s.WriteString(styleAccent.Bold(true).Render("❯ "))
			s.WriteString(styleAccent.Bold(true).Render("Type your own answer:"))
		} else {
			s.WriteString("  ")
			s.WriteString(styleMuted.Render("Type your own answer:"))
		}
		s.WriteString("\n")
		s.WriteString("    ")
		s.WriteString(m.askUserInput.View())
		s.WriteString("\n")
	}

	s.WriteString("\n")
	if m.askUserAllowFreeform && len(m.askUserOptions) > 0 {
		s.WriteString(styleMuted.Render("  ↑↓ select · enter confirm · 1-9 shortcut · tab freeform · esc dismiss"))
	} else if m.askUserAllowFreeform {
		s.WriteString(styleMuted.Render("  type answer · enter submit · esc dismiss"))
	} else {
		s.WriteString(styleMuted.Render("  ↑↓ select · enter confirm · 1-9 shortcut · esc dismiss"))
	}
	s.WriteString("\n")
	return s.String()
}

// truncateOption — option rows are single-line; oversize text gets an
// ellipsis so the prompt stays visually compact. Rune-safe so CJK
// option labels truncate at character boundaries, not byte boundaries.
func truncateOption(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:maxRunes-1]) + "…"
}
