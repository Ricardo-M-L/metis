package screen

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type ResumeAction int

const (
	ResumeActionResume ResumeAction = iota
	ResumeActionFork
	ResumeActionFresh
)

type SessionEntry struct {
	ID           string
	Title        string
	Model        string
	CreatedAt    time.Time
	Dir          string
	Branch       string
	MessageCount int
}

type ResumeScreen struct {
	sessions []SessionEntry
	cursor   int
	scroll   int
	width    int
	height   int
	done     bool
	action   ResumeAction
	selected string
	// filter for incremental search
	filter    string
	filtered  []int // indices into sessions
	searching bool
}

func NewResumeScreen(sessions []SessionEntry) *ResumeScreen {
	s := &ResumeScreen{sessions: sessions}
	s.filtered = make([]int, len(sessions))
	for i := range sessions {
		s.filtered[i] = i
	}
	return s
}

func (s *ResumeScreen) Action() ResumeAction { return s.action }
func (s *ResumeScreen) Selected() string     { return s.selected }

func (s *ResumeScreen) Init() tea.Cmd { return nil }

func (s *ResumeScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.scrollToCursor()
}

func (s *ResumeScreen) Done() bool { return s.done }

const resumeMaxBody = 18

func (s *ResumeScreen) bodyHeight() int {
	h := s.height - 7
	if h < 3 {
		h = 3
	}
	if h > resumeMaxBody {
		h = resumeMaxBody
	}
	return h
}

func (s *ResumeScreen) scrollToCursor() {
	bh := s.bodyHeight()
	if s.cursor < s.scroll {
		s.scroll = s.cursor
	}
	if s.cursor >= s.scroll+bh {
		s.scroll = s.cursor - bh + 1
	}
	s.clampScroll()
}

func (s *ResumeScreen) clampScroll() {
	maxScroll := len(s.filtered) - s.bodyHeight()
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

func (s *ResumeScreen) applyFilter() {
	s.filtered = s.filtered[:0]
	if s.filter == "" {
		for i := range s.sessions {
			s.filtered = append(s.filtered, i)
		}
	} else {
		lf := strings.ToLower(s.filter)
		for i, sess := range s.sessions {
			haystack := strings.ToLower(sess.Title + " " + sess.ID + " " + sess.Model)
			if strings.Contains(haystack, lf) {
				s.filtered = append(s.filtered, i)
			}
		}
	}
	if s.cursor >= len(s.filtered) {
		s.cursor = len(s.filtered) - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
	s.scroll = 0
	s.scrollToCursor()
}

func (s *ResumeScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		if s.searching {
			switch m.String() {
			case "esc":
				s.searching = false
				s.filter = ""
				s.applyFilter()
				return s, nil
			case "enter":
				s.searching = false
				return s, nil
			case "backspace":
				if len(s.filter) > 0 {
					_, size := utf8.DecodeLastRuneInString(s.filter)
					s.filter = s.filter[:len(s.filter)-size]
					s.applyFilter()
				}
				return s, nil
			default:
				if utf8.RuneCountInString(m.String()) == 1 {
					s.filter += m.String()
					s.applyFilter()
				}
				return s, nil
			}
		}
		switch m.String() {
		case "esc", "ctrl+c":
			s.done = true
			return s, nil
		case "enter":
			if s.cursor >= 0 && s.cursor < len(s.filtered) {
				s.selected = s.sessions[s.filtered[s.cursor]].ID
				s.action = ResumeActionResume
			}
			s.done = true
			return s, nil
		case "f":
			if s.cursor >= 0 && s.cursor < len(s.filtered) {
				s.selected = s.sessions[s.filtered[s.cursor]].ID
				s.action = ResumeActionFork
			}
			s.done = true
			return s, nil
		case "n":
			s.action = ResumeActionFresh
			s.done = true
			return s, nil
		case "/", "ctrl+f":
			s.searching = true
			s.filter = ""
			s.applyFilter()
			return s, nil
		case "up", "k":
			if n := len(s.filtered); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			s.scrollToCursor()
			return s, nil
		case "down", "j":
			if n := len(s.filtered); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			s.scrollToCursor()
			return s, nil
		case "pgup":
			s.cursor -= s.bodyHeight() / 2
			if s.cursor < 0 {
				s.cursor = 0
			}
			s.scrollToCursor()
			return s, nil
		case "pgdown":
			s.cursor += s.bodyHeight() / 2
			if s.cursor >= len(s.filtered) {
				s.cursor = len(s.filtered) - 1
			}
			s.scrollToCursor()
			return s, nil
		case "home", "g":
			s.cursor = 0
			s.scrollToCursor()
			return s, nil
		case "end", "G":
			s.cursor = len(s.filtered) - 1
			s.scrollToCursor()
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			if n := len(s.filtered); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			s.scrollToCursor()
		case tea.MouseWheelDown:
			if n := len(s.filtered); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			s.scrollToCursor()
		}
	}
	return s, nil
}

var (
	resumeTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	resumeCursorMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	resumeLabelActive = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	resumeLabelIdle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	resumeHintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Italic(true)
	resumeDescStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	resumeModelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b"))
	resumeDateStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c"))
	resumeFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	resumeEmptyStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)
	resumeSearchStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c")).Bold(true)
	resumeFilterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Underline(true)
)

func (s *ResumeScreen) View() string {
	var out strings.Builder

	out.WriteString(infoHeaderStripe.Render("resume"))
	out.WriteString("\n\n")

	// Search bar
	if s.searching {
		out.WriteString("  ")
		out.WriteString(resumeSearchStyle.Render("/"))
		out.WriteString(resumeFilterStyle.Render(s.filter))
		out.WriteString(resumeSearchStyle.Render("█"))
		out.WriteString("  ")
		out.WriteString(resumeFooterStyle.Render(fmt.Sprintf("%d matches", len(s.filtered))))
		out.WriteString("\n\n")
	} else if s.filter != "" {
		out.WriteString("  ")
		out.WriteString(resumeFooterStyle.Render("filter: "))
		out.WriteString(resumeFilterStyle.Render(s.filter))
		out.WriteString("  (")
		out.WriteString(resumeFooterStyle.Render(fmt.Sprintf("%d matches", len(s.filtered))))
		out.WriteString(")  ")
		out.WriteString(resumeFooterStyle.Render("/ to search"))
		out.WriteString("\n\n")
	} else {
		out.WriteString("\n")
	}

	if len(s.filtered) == 0 {
		out.WriteString("  ")
		out.WriteString(resumeEmptyStyle.Render("no matching sessions"))
		out.WriteString("\n")
		out.WriteString("\n")
		out.WriteString(resumeFooterStyle.Render("  n start fresh  ·  Esc close  ·  CLI: metis --resume <id>"))
		return out.String()
	}

	// Compute column widths for alignment.
	titleW := 0
	for _, idx := range s.filtered {
		t := s.sessions[idx].Title
		if t == "" {
			t = "(untitled)"
		}
		if w := lipgloss.Width(t); w > titleW {
			titleW = w
		}
	}
	if titleW > 36 {
		titleW = 36
	}

	bh := s.bodyHeight()
	end := s.scroll + bh
	if end > len(s.filtered) {
		end = len(s.filtered)
	}

	rendered := make([]string, 0, bh)
	for i := s.scroll; i < end; i++ {
		sess := s.sessions[s.filtered[i]]
		title := sess.Title
		if title == "" {
			title = "(untitled)"
		}
		if lipgloss.Width(title) > titleW {
			title = title[:titleW-1] + "…"
		}

		var line strings.Builder
		if i == s.cursor {
			line.WriteString(resumeCursorMark.Render("▸ "))
			line.WriteString(resumeLabelActive.Render(title))
		} else {
			line.WriteString("  ")
			line.WriteString(resumeLabelIdle.Render(title))
		}
		pad := titleW - lipgloss.Width(title)
		if pad < 0 {
			pad = 0
		}
		line.WriteString(strings.Repeat(" ", pad+2))

		if sess.Model != "" {
			line.WriteString(resumeModelStyle.Render(sess.Model))
			line.WriteString("  ")
		}

		if !sess.CreatedAt.IsZero() {
			ago := time.Since(sess.CreatedAt).Truncate(time.Minute)
			line.WriteString(resumeDateStyle.Render(ago.String() + " ago"))
			line.WriteString("  ")
		}

		line.WriteString(resumeDescStyle.Render(fmt.Sprintf("%d msgs", sess.MessageCount)))

		if sess.ID != "" {
			short := sess.ID
			if len(short) > 8 {
				short = short[:8]
			}
			line.WriteString("  ")
			line.WriteString(resumeHintStyle.Render(short))
		}

		rendered = append(rendered, line.String())
	}

	// Overflow indicators.
	if s.scroll > 0 && len(rendered) > 0 {
		rendered[0] = resumeFooterStyle.Render("  ↑ " + itoa(s.scroll) + " more above")
	}
	if end < len(s.filtered) && len(rendered) > 0 {
		rendered[len(rendered)-1] = resumeFooterStyle.Render("  ↓ " + itoa(len(s.filtered)-end) + " more below")
	}

	for _, line := range rendered {
		out.WriteString("  ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	for i := len(rendered); i < bh; i++ {
		out.WriteString("\n")
	}

	// Footer hints.
	out.WriteString("\n")
	hints := []string{
		"↑/↓ select",
		"Enter resume",
		"f fork",
		"n fresh",
		"/ search",
		"Esc close",
		"CLI: metis --resume <id>",
	}
	if len(s.filtered) > bh {
		hints = []string{
			"↑/↓ select",
			"PgUp/PgDn jump",
			"Enter resume",
			"f fork",
			"n fresh",
			"/ search",
			"Esc close",
		}
	}
	out.WriteString(resumeFooterStyle.Render("  " + strings.Join(hints, "  ·  ")))

	return out.String()
}
