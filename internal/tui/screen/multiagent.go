package screen

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type AgentTask struct {
	ID          string
	Name        string
	Status      string // "running", "completed", "failed", "pending"
	StartedAt   time.Time
	FinishedAt  time.Time
	ToolsCount  int
	LastTool    string
	Error       string
	ParentID    string // non-empty for sub-agents
	ChildrenIDs []string
}

type MultiAgentScreen struct {
	agents   []AgentTask
	source   func() []AgentTask
	cursor   int
	scroll   int
	width    int
	height   int
	done     bool
	selected string
}

func NewMultiAgentScreen(agents []AgentTask) *MultiAgentScreen {
	return &MultiAgentScreen{agents: append([]AgentTask(nil), agents...)}
}

// NewLiveMultiAgentScreen creates a read-only view backed by a snapshot
// callback. The callback is evaluated on Update/View, allowing an open modal
// to reflect agent progress as the parent TUI drains events.
func NewLiveMultiAgentScreen(source func() []AgentTask) *MultiAgentScreen {
	s := NewMultiAgentScreen(nil)
	s.source = source
	s.refresh()
	return s
}

func (s *MultiAgentScreen) refresh() {
	if s.source == nil {
		return
	}
	s.agents = append(s.agents[:0], s.source()...)
	if len(s.agents) == 0 {
		s.cursor = 0
		s.scroll = 0
		return
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
	if s.cursor >= len(s.agents) {
		s.cursor = len(s.agents) - 1
	}
	s.scrollToCursor()
}

func (s *MultiAgentScreen) Selected() string { return s.selected }

func (s *MultiAgentScreen) Init() tea.Cmd { return nil }

func (s *MultiAgentScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.scrollToCursor()
}

func (s *MultiAgentScreen) Done() bool { return s.done }

const agentMaxBody = 18

func (s *MultiAgentScreen) bodyHeight() int {
	h := s.height - 5
	if h < 3 {
		h = 3
	}
	if h > agentMaxBody {
		h = agentMaxBody
	}
	return h
}

func (s *MultiAgentScreen) scrollToCursor() {
	bh := s.bodyHeight()
	if s.cursor < s.scroll {
		s.scroll = s.cursor
	}
	if s.cursor >= s.scroll+bh {
		s.scroll = s.cursor - bh + 1
	}
	s.clampScroll()
}

func (s *MultiAgentScreen) clampScroll() {
	max := len(s.agents) - s.bodyHeight()
	if max < 0 {
		max = 0
	}
	if s.scroll > max {
		s.scroll = max
	}
	if s.scroll < 0 {
		s.scroll = 0
	}
}

func (s *MultiAgentScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	s.refresh()
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		switch m.String() {
		case "esc", "ctrl+c", "q":
			s.done = true
			return s, nil
		case "enter":
			if s.cursor >= 0 && s.cursor < len(s.agents) {
				s.selected = s.agents[s.cursor].ID
			}
			s.done = true
			return s, nil
		case "up", "k":
			if n := len(s.agents); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			s.scrollToCursor()
			return s, nil
		case "down", "j":
			if n := len(s.agents); n > 0 {
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
			if s.cursor >= len(s.agents) {
				s.cursor = len(s.agents) - 1
			}
			s.scrollToCursor()
			return s, nil
		case "home", "g":
			s.cursor = 0
			s.scrollToCursor()
			return s, nil
		case "end", "G":
			s.cursor = len(s.agents) - 1
			s.scrollToCursor()
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			if n := len(s.agents); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			s.scrollToCursor()
		case tea.MouseWheelDown:
			if n := len(s.agents); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			s.scrollToCursor()
		}
	}
	return s, nil
}

var (
	agentTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	agentCursorMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	agentNameActive  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	agentNameIdle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	agentStatusRun   = lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b")).Bold(true)
	agentStatusDone  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd"))
	agentStatusFail  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555")).Bold(true)
	agentStatusPend  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c"))
	agentToolStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	agentTimeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Italic(true)
	agentErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555"))
	agentFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	agentIndentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#44475a"))
)

func (s *MultiAgentScreen) statusGlyph(status string) lipgloss.Style {
	switch status {
	case "running":
		return agentStatusRun
	case "completed":
		return agentStatusDone
	case "failed":
		return agentStatusFail
	default:
		return agentStatusPend
	}
}

func (s *MultiAgentScreen) statusIcon(status string) string {
	switch status {
	case "running":
		return "◆"
	case "completed":
		return "✓"
	case "failed":
		return "✗"
	default:
		return "◇"
	}
}

func (s *MultiAgentScreen) View() string {
	s.refresh()
	var out strings.Builder

	out.WriteString(infoHeaderStripe.Render("/agents"))
	out.WriteString("\n\n")

	running, completed, failed := 0, 0, 0
	for _, a := range s.agents {
		switch a.Status {
		case "running":
			running++
		case "completed":
			completed++
		case "failed":
			failed++
		}
	}
	out.WriteString("  ")
	out.WriteString(agentTitleStyle.Render(fmt.Sprintf("%d agents", len(s.agents))))
	if running > 0 {
		out.WriteString("  ")
		out.WriteString(agentStatusRun.Render(fmt.Sprintf("%d running", running)))
	}
	if completed > 0 {
		out.WriteString("  ")
		out.WriteString(agentStatusDone.Render(fmt.Sprintf("%d done", completed)))
	}
	if failed > 0 {
		out.WriteString("  ")
		out.WriteString(agentStatusFail.Render(fmt.Sprintf("%d failed", failed)))
	}
	out.WriteString("\n\n")

	bh := s.bodyHeight()
	end := s.scroll + bh
	if end > len(s.agents) {
		end = len(s.agents)
	}

	for i := s.scroll; i < end; i++ {
		a := s.agents[i]
		var line strings.Builder

		// Indent for sub-agents.
		if a.ParentID != "" {
			line.WriteString(agentIndentStyle.Render("  └ "))
		}

		if i == s.cursor {
			line.WriteString(agentCursorMark.Render("▸ "))
		} else {
			line.WriteString("  ")
		}

		// Status icon + name.
		st := s.statusGlyph(a.Status)
		line.WriteString(st.Render(s.statusIcon(a.Status) + " "))

		if i == s.cursor {
			line.WriteString(agentNameActive.Render(a.Name))
		} else {
			line.WriteString(agentNameIdle.Render(a.Name))
		}

		// Duration.
		if !a.StartedAt.IsZero() {
			dur := time.Since(a.StartedAt).Truncate(time.Second)
			if !a.FinishedAt.IsZero() {
				dur = a.FinishedAt.Sub(a.StartedAt).Truncate(time.Second)
			}
			line.WriteString("  ")
			line.WriteString(agentTimeStyle.Render(dur.String()))
		}

		// Tools count.
		if a.ToolsCount > 0 {
			line.WriteString("  ")
			line.WriteString(agentToolStyle.Render(fmt.Sprintf("%dt", a.ToolsCount)))
		}

		// Last tool.
		if a.LastTool != "" {
			line.WriteString("  ")
			line.WriteString(agentToolStyle.Render(a.LastTool))
		}

		out.WriteString("  ")
		out.WriteString(line.String())
		out.WriteString("\n")

		// Error line for failed agents.
		if a.Status == "failed" && a.Error != "" {
			errText := a.Error
			if len(errText) > 60 {
				errText = errText[:57] + "..."
			}
			out.WriteString("  ")
			if a.ParentID != "" {
				out.WriteString(agentIndentStyle.Render("    "))
			} else {
				out.WriteString("    ")
			}
			out.WriteString(agentErrorStyle.Render(errText))
			out.WriteString("\n")
		}
	}

	// Pad remaining lines.
	for i := end - s.scroll; i < bh; i++ {
		out.WriteString("\n")
	}

	// Overflow indicators.
	if s.scroll > 0 {
		out.WriteString("  ")
		out.WriteString(agentFooterStyle.Render("↑ " + itoa(s.scroll) + " more above"))
		out.WriteString("\n")
	}
	if end < len(s.agents) {
		out.WriteString("  ")
		out.WriteString(agentFooterStyle.Render("↓ " + itoa(len(s.agents)-end) + " more below"))
		out.WriteString("\n")
	}

	// Footer.
	out.WriteString("\n")
	hints := []string{"↑/↓ select", "Enter select", "Esc close"}
	out.WriteString(agentFooterStyle.Render("  " + strings.Join(hints, "  ·  ")))

	return out.String()
}
