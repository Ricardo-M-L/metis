package screen

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// EffortScreen is an interactive horizontal slider for /effort, mirroring
// the claude-code widget visible in image #6 of the user's TUI feedback.
//
// Layout (centered horizontally inside a header-stripe modal):
//
//	[/effort]                                                       (header)
//
//	     Faster                                           Smarter
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
// active level. Pass the live agent.Loop.EffortValue() string ("off" / "low"
// / "medium" / "high"); unknown values fall back to "medium".
func NewEffortScreen(current string) *EffortScreen {
	levels := []string{"off", "low", "medium", "high"}
	cur := 2 // medium default
	if strings.TrimSpace(current) == "" {
		// llm.EffortDefault is the empty string and the UI's "off" entry
		// means exactly that: do not override the provider default.
		cur = 0
	}
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
// and applies via loop.SetEffort before clearing the screen reference.
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
	case tea.KeyPressMsg:
		// v2: collapsed both switches into one .String() match.
		switch m.String() {
		case "esc", "ctrl+c", "q":
			// Cancel: leave applied empty, parent won't change effort.
			s.done = true
			return s, nil
		case "enter":
			s.applied = s.levels[s.cursor]
			s.done = true
			return s, nil
		case "left", "h":
			if s.cursor > 0 {
				s.cursor--
			}
			return s, nil
		case "right", "l":
			if s.cursor < len(s.levels)-1 {
				s.cursor++
			}
			return s, nil
		case "home":
			s.cursor = 0
			return s, nil
		case "end":
			s.cursor = len(s.levels) - 1
			return s, nil
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
	effortTitleStyle   = lipgloss.NewStyle().Bold(true)
)

func (s *EffortScreen) View() string {
	var out strings.Builder

	// Header stripe — same as BodyScreen for visual consistency.
	out.WriteString(infoHeaderStripe.Render("/effort"))
	out.WriteString("\n\n")
	out.WriteString(s.sliderView())

	return out.String()
}

// InlineView renders the picker as chat chrome instead of a full-window
// Screen. Bare /effort uses this path so the transcript and input stay visible
// while the user chooses a level, matching Claude Code's inline selector.
// View remains available for older/full-screen callers and focused unit tests.
func (s *EffortScreen) InlineView() string {
	var out strings.Builder
	out.WriteString("  ")
	out.WriteString(effortTitleStyle.Render("Effort"))
	out.WriteString("\n\n")
	out.WriteString(s.sliderView())
	out.WriteString("\n")
	return out.String()
}

func (s *EffortScreen) sliderView() string {
	var out strings.Builder

	// Compute layout from the live terminal width. Four 12-cell segments are
	// retained on normal panes; narrow tmux splits reduce each segment and, when
	// necessary, abbreviate labels instead of letting the right-hand levels
	// disappear beyond the final column.
	width := s.width
	if width <= 0 {
		width = 80
	}
	available := width - 3 // two-cell indent + one-cell terminal wrap guard
	if available < 4 {
		available = 4
	}
	cellW := available / len(s.levels)
	if cellW > 12 {
		cellW = 12
	}
	if cellW < 1 {
		cellW = 1
	}
	totalW := cellW * len(s.levels)
	displayLevels := s.levels
	if cellW < len("medium") {
		displayLevels = []string{"off", "lo", "med", "hi"}
	}
	if cellW < 3 {
		displayLevels = []string{"o", "l", "m", "h"}
	}

	// Polar labels match Claude Code's current inline effort dial.
	leftText, rightText := "Faster", "Smarter"
	if totalW < 14 {
		leftText, rightText = "Fast", "Smart"
	}
	if totalW < 10 {
		leftText, rightText = "F", "S"
	}
	leftLbl := effortPolarStyle.Render(leftText)
	rightLbl := effortPolarStyle.Render(rightText)
	gap := totalW - lipgloss.Width(leftLbl) - lipgloss.Width(rightLbl)
	if gap < 1 {
		gap = 1
	}
	out.WriteString("  ")
	out.WriteString(leftLbl)
	out.WriteString(strings.Repeat(" ", gap))
	out.WriteString(rightLbl)
	out.WriteString("\n")

	// Axis line with the active pointer embedded in the track. Keeping the
	// triangle on the same row is both more compact and visually matches the
	// current Claude Code selector.
	pointerPos := cellW*s.cursor + cellW/2
	out.WriteString("  ")
	out.WriteString(effortAxisStyle.Render(strings.Repeat("─", pointerPos)))
	out.WriteString(effortPointerStyle.Render("▲"))
	out.WriteString(effortAxisStyle.Render(strings.Repeat("─", totalW-pointerPos-1)))
	out.WriteString("\n")

	// Level labels row, with the active one highlighted.
	out.WriteString("  ")
	for i, lvl := range displayLevels {
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
	if width < 70 {
		hint = "←/→ choose · Enter apply · Esc cancel"
	}
	if width < 40 {
		hint = "←/→ · Enter · Esc"
	}
	if width < 22 {
		hint = "←→ Enter Esc"
	}
	out.WriteString(infoFooterStyle.Render("  " + hint))

	return out.String()
}
