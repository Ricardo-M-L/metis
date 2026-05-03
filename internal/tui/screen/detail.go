package screen

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// DetailScreen is a generic "single resource detail view" overlay used
// by /skills (skill prompt body), /tools (tool schema), and similar
// follow-up screens that drill into one PickerScreen item.
//
// Layout:
//
//	[/skills · code-review]
//
//	Description
//	  review staged diff for bugs, style, security
//
//	When to use
//	  after `git add`, before `git commit`
//
//	Allowed tools
//	  Read · Edit · Bash · Git
//
//	Body
//	  ┃ <full prompt body, scrollable>
//
//	↑/↓ scroll · Esc back
//
// Caller builds DetailSection list (key/value rows + a free-form Body)
// from the picked resource. Sections render top-to-bottom in order.
type DetailSection struct {
	Heading string   // bold colored title
	Lines   []string // body lines under the heading (rendered muted-text)
}

// DetailScreen is the read-only detail viewer.
type DetailScreen struct {
	command       string // header stripe label, e.g. "/skills"
	subtitle      string // resource name, e.g. "code-review"
	sections      []DetailSection
	scroll        int
	width         int
	height        int
	done          bool
	parentCommand string // slash name (no "/") that opened this detail; Esc re-opens it
}

// NewDetailScreen builds the detail overlay.
func NewDetailScreen(command, subtitle string, sections []DetailSection) *DetailScreen {
	return &DetailScreen{command: command, subtitle: subtitle, sections: sections}
}

// WithParent records the slash command (without "/") that opened this
// detail, so Esc returns to that picker rather than chat. Mirrors
// claude-code's stack semantics. Returns the screen for chaining.
func (s *DetailScreen) WithParent(parent string) *DetailScreen {
	s.parentCommand = parent
	return s
}

// ParentCommand returns the slash command name (without "/") the user
// invoked to open this detail. The tui-level apply step reads this on
// Done() to re-dispatch the parent picker. Empty when the detail
// wasn't opened from a picker — Esc then falls back to chat.
func (s *DetailScreen) ParentCommand() string { return s.parentCommand }

func (s *DetailScreen) Init() tea.Cmd { return nil }

func (s *DetailScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.clampScroll()
}

func (s *DetailScreen) Done() bool { return s.done }

const detailMaxBody = 22

func (s *DetailScreen) bodyHeight() int {
	h := s.height - 4
	if h < 3 {
		h = 3
	}
	if h > detailMaxBody {
		h = detailMaxBody
	}
	return h
}

func (s *DetailScreen) totalLines() int {
	n := 0
	for _, sec := range s.sections {
		if sec.Heading != "" {
			n++
			n++ // blank under heading
		}
		n += len(sec.Lines)
		n++ // separator after section
	}
	return n
}

func (s *DetailScreen) clampScroll() {
	maxScroll := s.totalLines() - s.bodyHeight()
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

func (s *DetailScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
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
			s.scroll = s.totalLines()
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

var (
	detailSubtitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	detailResourceStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	detailHeadingStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Bold(true)
	detailBodyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	detailFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
)

// flatten produces the line-by-line render of all sections, ready to
// slice for the scroll viewport.
func (s *DetailScreen) flatten() []string {
	out := make([]string, 0, s.totalLines())
	for i, sec := range s.sections {
		if sec.Heading != "" {
			out = append(out, detailHeadingStyle.Render(sec.Heading))
			out = append(out, "")
		}
		for _, ln := range sec.Lines {
			out = append(out, detailBodyStyle.Render("  "+ln))
		}
		if i < len(s.sections)-1 {
			out = append(out, "")
		}
	}
	return out
}

func (s *DetailScreen) View() string {
	var out strings.Builder

	out.WriteString(infoHeaderStripe.Render(s.command))
	if s.subtitle != "" {
		out.WriteString("  ")
		out.WriteString(detailSubtitleStyle.Render("· "))
		out.WriteString(detailResourceStyle.Render(s.subtitle))
	}
	out.WriteString("\n\n")

	bh := s.bodyHeight()
	body := s.flatten()
	end := s.scroll + bh
	if end > len(body) {
		end = len(body)
	}
	visible := body[s.scroll:end]
	if s.scroll > 0 && len(visible) > 0 {
		visible[0] = detailFooterStyle.Render("↑ " + itoa(s.scroll) + " more above")
	}
	if end < len(body) && len(visible) > 0 {
		visible[len(visible)-1] = detailFooterStyle.Render("↓ " + itoa(len(body)-end) + " more below")
	}
	for _, line := range visible {
		out.WriteString("  ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	for i := len(visible); i < bh; i++ {
		out.WriteString("\n")
	}

	out.WriteString("\n")
	hint := "Esc / q to back"
	if len(body) > bh {
		hint = "↑/↓ k/j PgUp/PgDn  ·  Esc / q to back"
	}
	out.WriteString(detailFooterStyle.Render("  " + hint))
	return out.String()
}
