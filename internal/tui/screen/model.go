package screen

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ModelChoice is one entry in the /model picker. Caller (tui pkg) supplies
// the candidate list; the screen package stays import-free.
type ModelChoice struct {
	ID          string // exact model identifier (e.g. "claude-opus-4-7")
	Label       string // optional display name (defaults to ID when empty)
	Description string // one-line subtitle shown in muted style
	Provider    string // optional, surfaces in the right column
}

// ModelScreen is the /model arrow-nav picker mirroring claude-code's
// model selector. Layout:
//
//   [/model]
//
//     Pick a model — current: claude-opus-4-7
//
//     ▸ claude-opus-4-7        most capable, best for hard tasks  · anthropic
//       claude-sonnet-4-6      fast + smart, balanced              · anthropic
//       claude-haiku-4-5       cheapest, instant                   · anthropic
//       MiniMax-M2.7           open-weight, 192k context           · minimax
//       ...
//
//     ↑/↓ to choose  ·  Enter to apply  ·  Esc to cancel
type ModelScreen struct {
	choices []ModelChoice
	current string // existing model — highlighted as starting cursor
	cursor  int
	width   int
	height  int
	done    bool
	applied string // empty if cancelled, else chosen ModelChoice.ID
}

// NewModelScreen builds the picker. `current` is the currently-active
// model (used to seed the cursor on the matching entry). If no choice
// matches `current`, the cursor starts at 0.
func NewModelScreen(current string, choices []ModelChoice) *ModelScreen {
	cur := 0
	for i, c := range choices {
		if c.ID == current {
			cur = i
			break
		}
	}
	return &ModelScreen{choices: choices, current: current, cursor: cur}
}

// Applied returns the chosen model ID, or empty if the user cancelled.
func (s *ModelScreen) Applied() string { return s.applied }

func (s *ModelScreen) Init() tea.Cmd { return nil }

func (s *ModelScreen) Resize(width, height int) {
	s.width = width
	s.height = height
}

func (s *ModelScreen) Done() bool { return s.done }

func (s *ModelScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		// v2: collapsed two switches into one .String() match.
		switch m.String() {
		case "esc", "ctrl+c", "q":
			s.done = true
			return s, nil
		case "enter":
			if s.cursor >= 0 && s.cursor < len(s.choices) {
				s.applied = s.choices[s.cursor].ID
			}
			s.done = true
			return s, nil
		case "up", "k":
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			return s, nil
		case "down", "j":
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			return s, nil
		case "home", "g":
			s.cursor = 0
			return s, nil
		case "end", "G":
			s.cursor = len(s.choices) - 1
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
		case tea.MouseWheelDown:
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
		}
	}
	return s, nil
}

var (
	modelTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	modelHeaderHint  = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	modelCurrentTag  = lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b")).Bold(true)
	modelArrowStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	modelIDActive    = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	modelIDInactive  = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	modelDescStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	modelProvStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Italic(true)
	modelFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
)

func (s *ModelScreen) View() string {
	var out strings.Builder

	// Header stripe.
	out.WriteString(infoHeaderStripe.Render("/model"))
	out.WriteString("\n\n")

	// Sub-header showing current model.
	out.WriteString("  ")
	out.WriteString(modelTitleStyle.Render("Pick a model"))
	if s.current != "" {
		out.WriteString(modelHeaderHint.Render(" · current: "))
		out.WriteString(modelCurrentTag.Render(s.current))
	}
	out.WriteString("\n\n")

	// Find the longest ID for column alignment.
	idW := 0
	for _, c := range s.choices {
		if l := lipgloss.Width(c.ID); l > idW {
			idW = l
		}
	}
	for i, c := range s.choices {
		// Cursor arrow.
		if i == s.cursor {
			out.WriteString("  ")
			out.WriteString(modelArrowStyle.Render("▸ "))
			out.WriteString(modelIDActive.Render(c.ID))
		} else {
			out.WriteString("    ")
			out.WriteString(modelIDInactive.Render(c.ID))
		}
		// Pad to column boundary.
		out.WriteString(strings.Repeat(" ", idW-lipgloss.Width(c.ID)+2))
		// Description.
		if c.Description != "" {
			out.WriteString(modelDescStyle.Render(c.Description))
		}
		// Provider tag at the right.
		if c.Provider != "" {
			out.WriteString(modelDescStyle.Render("  · "))
			out.WriteString(modelProvStyle.Render(c.Provider))
		}
		out.WriteString("\n")
	}

	// Footer hint.
	out.WriteString("\n")
	out.WriteString(modelFooterStyle.Render("  ↑/↓  k/j  to choose  ·  Enter to apply  ·  Esc to cancel"))

	return out.String()
}
