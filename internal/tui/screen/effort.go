package screen

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// EffortScreen is an interactive horizontal slider for /effort, mirroring
// the claude-code widget visible in image #6 of the user's TUI feedback.
//
// Layout (centered horizontally inside a header-stripe modal):
//
//	[/effort]                                                       (header)
//
//	     Speed                                       Intelligence
//	  ────────────────────────────────────────────────────────────────
//	    off       low      medium      high
//	                          ▲
//	                  (selected marker)
//
//	  ← / →  to choose  ·  Enter to apply  ·  Esc to cancel       (footer)
//
// metis exposes 4 effort levels at the LLM layer (off / low / medium /
// high — see pkg/llm/effort.go). claude-code's 5-step slider (low /
// medium / high / xhigh / max) maps to a strict superset; the "xhigh /
// max" tail isn't wired into the agent loop yet, so we render only the
// supported 4. Phase later if those land in the provider abstraction.
type EffortScreen struct {
	levels  []string // ordered list of selectable effort labels
	cursor  int      // index into levels
	initial string   // remembered for cancel-restore (Esc reverts visual)
	width   int
	height  int
	done    bool

	// applied is the level the user committed (Enter). Empty means
	// the user cancelled (Esc) — the parent should NOT update the
	// loop's effort.
	applied string
}

// NewEffortScreen builds the screen with `current` highlighted as the
// active level. Pass the live agent.Loop.Effort string ("off" / "low"
// / "medium" / "high"); unknown values fall back to "medium".
func NewEffortScreen(current string) *EffortScreen {
	levels := []string{"off", "low", "medium", "high"}
	cur := 2 // medium default
	for i, l := range levels {
		if l == strings.ToLower(strings.TrimSpace(current)) {
			cur = i
			break
		}
	}
	return &EffortScreen{
		levels:  levels,
		cursor:  cur,
		initial: levels[cur],
	}
}

// Applied returns the effort the user picked, or empty if they cancelled.
// Caller (handleSubmit) reads this in the activeScreen.Done() branch
// and applies to loop.Effort before clearing the screen reference.
func (s *EffortScreen) Applied() string { return s.applied }

func (s *EffortScreen) Init() tea.Cmd { return nil }

func (s *EffortScreen) Resize(width, height int) {
	s.width = width
	s.height = height
}

func (s *EffortScreen) Done() bool { return s.done }

func (s *EffortScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyMsg:
		switch m.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			// Cancel: leave applied empty, parent won't change effort.
			s.done = true
			return s, nil
		case tea.KeyEnter:
			s.applied = s.levels[s.cursor]
			s.done = true
			return s, nil
		case tea.KeyLeft:
			if s.cursor > 0 {
				s.cursor--
			}
			return s, nil
		case tea.KeyRight:
			if s.cursor < len(s.levels)-1 {
				s.cursor++
			}
			return s, nil
		case tea.KeyHome:
			s.cursor = 0
			return s, nil
		case tea.KeyEnd:
			s.cursor = len(s.levels) - 1
			return s, nil
		}
		switch m.String() {
		case "h":
			if s.cursor > 0 {
				s.cursor--
			}
		case "l":
			if s.cursor < len(s.levels)-1 {
				s.cursor++
			}
		case "q":
			s.done = true
		}
	}
	return s, nil
}

// Local style palette — colors mirror the TUI theme but are local to
// the screen so the package stays self-contained.
var (
	effortAxisStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	effortLabelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	effortActiveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	effortPolarStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Italic(true)
	effortPointerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
)

func (s *EffortScreen) View() string {
	var out strings.Builder

	// Header stripe — same as BodyScreen for visual consistency.
	out.WriteString(infoHeaderStripe.Render("/effort"))
	out.WriteString("\n\n")

	// Compute layout: 4 levels + per-level cell width. We allocate a
	// fixed cell width so the slider looks the same regardless of
	// terminal width past a reasonable minimum.
	const cellW = 12
	totalW := cellW * len(s.levels)

	// Polar labels: "Speed" left, "Intelligence" right.
	leftLbl := effortPolarStyle.Render("Speed")
	rightLbl := effortPolarStyle.Render("Intelligence")
	gap := totalW - lipgloss.Width(leftLbl) - lipgloss.Width(rightLbl)
	if gap < 1 {
		gap = 1
	}
	out.WriteString("  ")
	out.WriteString(leftLbl)
	out.WriteString(strings.Repeat(" ", gap))
	out.WriteString(rightLbl)
	out.WriteString("\n")

	// Axis line.
	out.WriteString("  ")
	out.WriteString(effortAxisStyle.Render(strings.Repeat("─", totalW)))
	out.WriteString("\n")

	// Pointer ▲ row — placed under the active cell.
	pointerLine := strings.Repeat(" ", cellW*s.cursor+(cellW/2))
	out.WriteString("  ")
	out.WriteString(pointerLine)
	out.WriteString(effortPointerStyle.Render("▲"))
	out.WriteString("\n")

	// Level labels row, with the active one highlighted.
	out.WriteString("  ")
	for i, lvl := range s.levels {
		// Center each label inside its cell.
		pad := (cellW - len(lvl)) / 2
		left := strings.Repeat(" ", pad)
		right := strings.Repeat(" ", cellW-pad-len(lvl))
		if i == s.cursor {
			out.WriteString(left)
			out.WriteString(effortActiveStyle.Render(lvl))
			out.WriteString(right)
		} else {
			out.WriteString(left)
			out.WriteString(effortLabelStyle.Render(lvl))
			out.WriteString(right)
		}
	}
	out.WriteString("\n\n")

	// Footer hint.
	hint := "← / →  h/l  to choose  ·  Enter to apply  ·  Esc to cancel"
	out.WriteString(infoFooterStyle.Render("  " + hint))

	return out.String()
}
