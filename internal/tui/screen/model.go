package screen

import (
	"fmt"
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
	choices         []ModelChoice // full list
	filtered        []int         // indexes into choices after filter
	current         string        // existing model — highlighted as starting cursor
	currentProvider string        // active profile; disambiguates duplicate model IDs
	title           string        // optional purpose-specific title
	cursor          int           // index into filtered
	filter          string        // current search text
	width           int
	height          int
	done            bool
	applied         string      // empty if cancelled, else chosen ModelChoice.ID
	appliedChoice   ModelChoice // includes provider profile for a real rebuild
	hasApplied      bool
}

// NewModelScreen builds the picker. `current` is the currently-active
// model (used to seed the cursor on the matching entry). If no choice
// matches `current`, the cursor starts at 0.
func NewModelScreen(current string, choices []ModelChoice) *ModelScreen {
	s := &ModelScreen{choices: choices, current: current, title: "Pick a model"}
	s.refilter()
	s.seedCurrentCursor()
	return s
}

// Applied returns the chosen model ID, or empty if the user cancelled.
func (s *ModelScreen) Applied() string { return s.applied }

// AppliedChoice returns the committed model together with its provider
// profile. Applied() is retained for existing callers, but an ID alone is not
// enough when two configured providers expose different models/transports.
func (s *ModelScreen) AppliedChoice() (ModelChoice, bool) {
	return s.appliedChoice, s.hasApplied
}

// SetTitle customizes the picker purpose (for example, image recovery).
func (s *ModelScreen) SetTitle(title string) {
	if strings.TrimSpace(title) != "" {
		s.title = title
	}
}

// SetCurrentProvider disambiguates identical model IDs exposed by multiple
// profiles. A bare /model followed immediately by Enter must keep the active
// provider, not select whichever built-in entry happened to sort first.
func (s *ModelScreen) SetCurrentProvider(provider string) {
	s.currentProvider = provider
	s.seedCurrentCursor()
}

func (s *ModelScreen) seedCurrentCursor() {
	fallback := -1
	for i, idx := range s.filtered {
		choice := s.choices[idx]
		if choice.ID != s.current {
			continue
		}
		if fallback < 0 {
			fallback = i
		}
		if s.currentProvider != "" && strings.EqualFold(choice.Provider, s.currentProvider) {
			s.cursor = i
			return
		}
	}
	if fallback >= 0 {
		s.cursor = fallback
	}
}

func (s *ModelScreen) Init() tea.Cmd { return nil }

func (s *ModelScreen) Resize(width, height int) {
	s.width = width
	s.height = height
}

func (s *ModelScreen) Done() bool { return s.done }

// visibleRange returns a cursor-centred window that fits within the resized
// terminal. Header/subheader/footer consume six rows; before the first resize
// height is zero and tests/callers retain the legacy all-items view.
func (s *ModelScreen) visibleRange() (start, end int) {
	n := len(s.filtered)
	if n == 0 {
		return 0, 0
	}
	maxVisible := n
	if s.height > 0 {
		maxVisible = s.height - 6
		if maxVisible < 1 {
			maxVisible = 1
		}
		if maxVisible > n {
			maxVisible = n
		}
	}
	start = s.cursor - maxVisible/2
	if start < 0 {
		start = 0
	}
	if start+maxVisible > n {
		start = n - maxVisible
	}
	return start, start + maxVisible
}

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
				s.appliedChoice = s.choices[s.filtered[s.cursor]]
				s.applied = s.appliedChoice.ID
				s.hasApplied = true
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
	out.WriteString(modelTitleStyle.Render(s.title))
	if s.current != "" {
		out.WriteString(modelHeaderHint.Render(" · current: "))
		out.WriteString(modelCurrentTag.Render(s.current))
	}
	if s.filter != "" {
		out.WriteString(modelHeaderHint.Render(" · search: "))
		out.WriteString(modelFilterStyle.Render(s.filter))
	}
	out.WriteString("\n\n")

	visibleStart, visibleEnd := s.visibleRange()

	// Find the longest visible ID for column alignment.
	idW := 0
	for _, idx := range s.filtered[visibleStart:visibleEnd] {
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
		for i := visibleStart; i < visibleEnd; i++ {
			idx := s.filtered[i]
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
	if len(s.filtered) > 0 && visibleEnd-visibleStart < len(s.filtered) {
		out.WriteString(modelFooterStyle.Render(" · "))
		out.WriteString(modelHeaderHint.Render(
			fmt.Sprintf("%d/%d", s.cursor+1, len(s.filtered)),
		))
	}

	return out.String()
}
