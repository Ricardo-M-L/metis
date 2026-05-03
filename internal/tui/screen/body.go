package screen

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// BodyScreen is the simpler sibling of InfoScreen: instead of structured
// rows, it takes a pre-rendered (already lipgloss-styled) body string
// and surrounds it with the standard slash-command modal chrome —
// header stripe at top showing the command name, scrollable body in
// the middle, "Esc to close" footer.
//
// Used by the migration of the 12 existing renderXxx() functions
// (/help, /cost, /doctor, /context, /stats, /keybindings, /permissions,
// /hooks, /tools, /skills, /env, /version, /tokens). Those functions
// already produce styled output with their own internal boxes; rather
// than refactor all 12 to return InfoRow lists, BodyScreen accepts the
// existing string and just adds the modal envelope. The double-chrome
// (outer header stripe + inner box) looks intentional rather than
// accidental — comparable to claude-code's modals.
type BodyScreen struct {
	command string
	body    string   // pre-rendered, may contain ANSI styles
	lines   []string // split for scrolling
	scroll  int
	width   int
	height  int
	done    bool
}

// NewBodyScreen builds a BodyScreen ready to View(). command is the
// slash prefix (e.g. "/cost") shown in the header stripe; body is the
// styled output from one of the existing renderXxx() functions.
func NewBodyScreen(command, body string) *BodyScreen {
	return &BodyScreen{
		command: command,
		body:    body,
		lines:   strings.Split(body, "\n"),
	}
}

func (s *BodyScreen) Init() tea.Cmd { return nil }

func (s *BodyScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.clampScroll()
}

func (s *BodyScreen) Done() bool { return s.done }

// bodyHeight reserves rows for the chrome (header stripe + spacer +
// footer hint + spacer ≈ 4).
func (s *BodyScreen) bodyHeight() int {
	h := s.height - 4
	if h < 3 {
		h = 3
	}
	return h
}

func (s *BodyScreen) clampScroll() {
	maxScroll := len(s.lines) - s.bodyHeight()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if s.scroll > maxScroll {
		s.scroll = maxScroll
	}
	if s.scroll < 0 {
		s.scroll = 0
	}
}

func (s *BodyScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		// v2: KeyMsg is an interface; KeyPressMsg is the concrete press
		// event. Match by .String() — handles named keys ("esc", "pgup")
		// and ASCII alike. Two switches collapsed into one.
		switch m.String() {
		case "esc", "ctrl+c", "q":
			s.done = true
			return s, nil
		case "up", "k":
			s.scroll--
			s.clampScroll()
			return s, nil
		case "down", "j":
			s.scroll++
			s.clampScroll()
			return s, nil
		case "pgup":
			s.scroll -= s.bodyHeight() / 2
			s.clampScroll()
			return s, nil
		case "pgdown":
			s.scroll += s.bodyHeight() / 2
			s.clampScroll()
			return s, nil
		case "home", "g":
			s.scroll = 0
			return s, nil
		case "end", "G":
			s.scroll = len(s.lines)
			s.clampScroll()
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			s.scroll--
			s.clampScroll()
		case tea.MouseWheelDown:
			s.scroll++
			s.clampScroll()
		}
	}
	return s, nil
}

func (s *BodyScreen) View() string {
	var out strings.Builder

	// Header stripe — same style as InfoScreen for visual consistency.
	out.WriteString(infoHeaderStripe.Render(s.command))
	out.WriteString("\n\n")

	// Body window: the visible slice of pre-rendered lines.
	bh := s.bodyHeight()
	end := s.scroll + bh
	if end > len(s.lines) {
		end = len(s.lines)
	}
	for i := s.scroll; i < end; i++ {
		out.WriteString(s.lines[i])
		out.WriteString("\n")
	}
	// Pad blank lines so the footer sits at the bottom.
	for i := end - s.scroll; i < bh; i++ {
		out.WriteString("\n")
	}

	// Footer hint.
	out.WriteString("\n")
	hint := "↑/↓ k/j PgUp/PgDn  ·  Esc / q to close"
	if len(s.lines) <= bh {
		hint = "Esc / q to close"
	}
	out.WriteString(infoFooterStyle.Render("  " + hint))

	return out.String()
}
