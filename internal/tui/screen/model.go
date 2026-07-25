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
	Recent      bool   // true when this entry is in the user's recent list
}

// ModelScreen is the /model arrow-nav picker with fuzzy search.
// Layout:
//
//	[/model]
//
//	  Pick a model — current: claude-opus-4-7
//
//	  ▸ claude-opus-4-7        most capable, best for hard tasks  · anthropic
//	    claude-sonnet-4-6      fast + smart, balanced              · anthropic
//	    claude-haiku-4-5       cheapest, instant                   · anthropic
//	    MiniMax-M2.7           open-weight, 192k context           · minimax
//	    ...
//
//	  Type to search · ↑/↓ to choose · Enter to apply · Esc to cancel
type ModelScreen struct {
	choices  []ModelChoice // full list
	filtered []int         // indexes into choices after filter
	current  string        // existing model — highlighted as starting cursor
	cursor   int           // index into filtered
	filter   string        // current search text
	width    int
	height   int
	done     bool
	applied  string // empty if cancelled, else chosen ModelChoice.ID
}

// NewModelScreen builds the picker. `current` is the currently-active
// model (used to seed the cursor on the matching entry). If no choice
// matches `current`, the cursor starts at 0.
func NewModelScreen(current string, choices []ModelChoice) *ModelScreen {
	s := &ModelScreen{choices: choices, current: current}
	s.refilter()
	// Seed cursor on current model.
	for i, idx := range s.filtered {
		if choices[idx].ID == current {
			s.cursor = i
			break
		}
	}
	return s
}

// Applied returns the chosen model ID, or empty if the user cancelled.
func (s *ModelScreen) Applied() string { return s.applied }

func (s *ModelScreen) Init() tea.Cmd { return nil }

func (s *ModelScreen) Resize(width, height int) {
	s.width = width
	s.height = height
}

func (s *ModelScreen) Done() bool { return s.done }

// refilter rebuilds the filtered index list from the current filter text.
func (s *ModelScreen) refilter() {
	s.filtered = s.filtered[:0]
	for i, c := range s.choices {
		if s.filter == "" || fuzzyMatchModel(c, s.filter) {
			s.filtered = append(s.filtered, i)
		}
	}
	if s.cursor >= len(s.filtered) {
		s.cursor = max(0, len(s.filtered)-1)
	}
}

// fuzzyMatchModel checks if a ModelChoice matches the filter text.
// It matches against ID, Label, Description, and Provider.
func fuzzyMatchModel(c ModelChoice, pattern string) bool {
	if pattern == "" {
		return true
	}
	pattern = strings.ToLower(pattern)
	for _, field := range []string{c.ID, c.Label, c.Description, c.Provider} {
		if fuzzyMatchString(strings.ToLower(field), pattern) {
			return true
		}
	}
	return false
}

// fuzzyMatchString is a simple fuzzy matcher: substring match first,
// then character-by-character subsequence match.
func fuzzyMatchString(str, pattern string) bool {
	if pattern == "" {
		return true
	}
	if strings.Contains(str, pattern) {
		return true
	}
	si := 0
	for _, c := range pattern {
		found := false
		for si < len(str) {
			if rune(str[si]) == c {
				si++
				found = true
				break
			}
			si++
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *ModelScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		switch m.String() {
		case "esc", "ctrl+c":
			s.done = true
			return s, nil
		case "enter":
			if s.cursor >= 0 && s.cursor < len(s.filtered) {
				s.applied = s.choices[s.filtered[s.cursor]].ID
			}
			s.done = true
			return s, nil
		case "up", "k":
			if n := len(s.filtered); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			return s, nil
		case "down", "j":
			if n := len(s.filtered); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			return s, nil
		case "home", "g":
			s.cursor = 0
			return s, nil
		case "end", "G":
			s.cursor = len(s.filtered) - 1
			return s, nil
		case "backspace":
			if len(s.filter) > 0 {
				s.filter = s.filter[:len(s.filter)-1]
				s.refilter()
			}
			return s, nil
		default:
			// Any printable character goes into the filter.
			if len(m.String()) == 1 && m.String() >= " " {
				s.filter += m.String()
				s.refilter()
			}
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			if n := len(s.filtered); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
		case tea.MouseWheelDown:
			if n := len(s.filtered); n > 0 {
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
	modelFilterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c")).Bold(true)
	modelRecentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c")).Bold(true)
)

func (s *ModelScreen) View() string {
	var out strings.Builder

	// Header stripe.
	out.WriteString(infoHeaderStripe.Render("/model"))
	out.WriteString("\n\n")

	// Sub-header showing current model + filter.
	out.WriteString("  ")
	out.WriteString(modelTitleStyle.Render("Pick a model"))
	if s.current != "" {
		out.WriteString(modelHeaderHint.Render(" · current: "))
		out.WriteString(modelCurrentTag.Render(s.current))
	}
	if s.filter != "" {
		out.WriteString(modelHeaderHint.Render(" · search: "))
		out.WriteString(modelFilterStyle.Render(s.filter))
	}
	out.WriteString("\n\n")

	// Find the longest ID for column alignment.
	idW := 0
	for _, idx := range s.filtered {
		c := s.choices[idx]
		if l := lipgloss.Width(c.ID); l > idW {
			idW = l
		}
	}

	// Render filtered choices.
	if len(s.filtered) == 0 {
		out.WriteString("    ")
		out.WriteString(modelDescStyle.Render("No models match your search."))
		out.WriteString("\n")
	} else {
		for i, idx := range s.filtered {
			c := s.choices[idx]
			// Cursor arrow.
			if i == s.cursor {
				out.WriteString("  ")
				out.WriteString(modelArrowStyle.Render("▸ "))
				out.WriteString(modelIDActive.Render(c.ID))
			} else {
				out.WriteString("    ")
				out.WriteString(modelIDInactive.Render(c.ID))
			}
			// Recent tag.
			if c.Recent {
				out.WriteString(modelRecentStyle.Render(" [recent]"))
			}
			// Pad to column boundary.
			pad := idW - lipgloss.Width(c.ID) + 2
			if c.Recent {
				pad -= 9 // account for " [recent]" tag (space + 8 chars)
			}
			if pad > 0 {
				out.WriteString(strings.Repeat(" ", pad))
			}
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
	}

	// Footer hint.
	out.WriteString("\n")
	if s.filter == "" {
		out.WriteString(modelFooterStyle.Render("  Type to search · ↑/↓ k/j to choose · Enter to apply · Esc to cancel"))
	} else {
		out.WriteString(modelFooterStyle.Render("  Backspace to clear · ↑/↓ k/j to choose · Enter to apply · Esc to cancel"))
	}

	return out.String()
}
