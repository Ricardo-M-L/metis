package screen

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ThemeChoice is one entry in the /theme picker. Caller (tui pkg) maps
// each registered Theme to swatches sampled from the palette.
type ThemeChoice struct {
	Name string // theme identifier (e.g. "dark")
	// Swatches are 5–6 hex colors sampled from the theme palette
	// (background, fg, accent, success, warn, error). Rendered as
	// solid blocks so the user sees the actual colors before applying.
	Swatches []string
}

// ThemeScreen is the /theme cycle widget. Layout:
//
//   [/theme]
//
//   Theme:  ◀  dark-daltonized  ▶
//
//             ▮▮ ▮▮ ▮▮ ▮▮ ▮▮ ▮▮     ← live swatches
//
//   ←/→ to cycle  ·  Enter to apply  ·  Esc to cancel
type ThemeScreen struct {
	choices []ThemeChoice
	cursor  int
	initial string
	width   int
	height  int
	done    bool
	applied string
}

// NewThemeScreen builds the picker. `current` is the currently-active
// theme; the cursor is seeded on the matching entry, falling back to
// index 0.
func NewThemeScreen(current string, choices []ThemeChoice) *ThemeScreen {
	cur := 0
	for i, c := range choices {
		if c.Name == current {
			cur = i
			break
		}
	}
	return &ThemeScreen{choices: choices, cursor: cur, initial: current}
}

// Applied returns the chosen theme name, or empty if cancelled.
func (s *ThemeScreen) Applied() string { return s.applied }

func (s *ThemeScreen) Init() tea.Cmd { return nil }

func (s *ThemeScreen) Resize(width, height int) {
	s.width = width
	s.height = height
}

func (s *ThemeScreen) Done() bool { return s.done }

func (s *ThemeScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyMsg:
		switch m.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			s.done = true
			return s, nil
		case tea.KeyEnter:
			if s.cursor >= 0 && s.cursor < len(s.choices) {
				s.applied = s.choices[s.cursor].Name
			}
			s.done = true
			return s, nil
		case tea.KeyLeft:
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			return s, nil
		case tea.KeyRight:
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			return s, nil
		}
		switch m.String() {
		case "h":
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
		case "l":
			if n := len(s.choices); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
		case "q":
			s.done = true
		}
	}
	return s, nil
}

var (
	themeArrowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	themeNameStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true).Padding(0, 1).Background(lipgloss.Color("#5a4a78"))
	themeLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	themeFootStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
)

func (s *ThemeScreen) View() string {
	var out strings.Builder

	// Header stripe.
	out.WriteString(infoHeaderStripe.Render("/theme"))
	out.WriteString("\n\n")

	// Theme: ◀ name ▶ row.
	out.WriteString("  ")
	out.WriteString(themeLabelStyle.Render("Theme: "))
	out.WriteString(themeArrowStyle.Render("◀ "))
	if s.cursor >= 0 && s.cursor < len(s.choices) {
		out.WriteString(themeNameStyle.Render(s.choices[s.cursor].Name))
	}
	out.WriteString(themeArrowStyle.Render(" ▶"))
	out.WriteString("\n\n")

	// Swatch row — render each swatch as 4 cells of background color
	// so they look like color chips rather than thin slivers.
	if s.cursor >= 0 && s.cursor < len(s.choices) {
		out.WriteString("           ")
		for _, hex := range s.choices[s.cursor].Swatches {
			swatch := lipgloss.NewStyle().Background(lipgloss.Color(hex)).Render("    ")
			out.WriteString(swatch)
			out.WriteString(" ")
		}
		out.WriteString("\n")
	}

	// Page indicator (1/3) so the user knows the cycle range.
	if len(s.choices) > 1 {
		out.WriteString("\n  ")
		out.WriteString(themeLabelStyle.Render(positionString(s.cursor, len(s.choices))))
		out.WriteString("\n")
	}

	// Footer hint.
	out.WriteString("\n")
	out.WriteString(themeFootStyle.Render("  ← / →  h/l  to cycle  ·  Enter to apply  ·  Esc to cancel"))

	return out.String()
}

// positionString returns "(2/3)" style indicator.
func positionString(cur, total int) string {
	if total <= 0 {
		return ""
	}
	return "(" + itoa(cur+1) + "/" + itoa(total) + ")"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
