package screen

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PickerItem is one entry in the PickerScreen list. Key is the value
// returned by Selected() when the user presses Enter on this row;
// Label/Description/Hint are visual only.
type PickerItem struct {
	Key         string // returned by Selected() — caller routes on this
	Label       string // bold name shown left (defaults to Key when empty)
	Description string // muted detail shown right of Label
	Hint        string // optional trailing tag (e.g. provider, timestamp)
}

// PickerScreen is the generic single-tab list-with-cursor widget. Used
// by /sessions, /skills, /tools, /agents, /memory etc. — anything
// whose payload is "browse + pick one + do something".
//
// Layout:
//
//   [/sessions]  20 saved sessions
//
//     2026-05-02T13:03  · model=MiniMax-M2.7    abcd1234
//   ▸ 2026-05-02T13:00  · model=claude-opus-4-7 efgh5678   ← cursor
//     2026-05-02T12:55  · model=MiniMax-M2.7    ijkl9012
//
//   ↑/↓ select  ·  Enter pick  ·  Esc close
//
// Wraparound nav (claude-code parity), auto-scroll-to-cursor, ↑N/↓N
// overflow indicators.
type PickerScreen struct {
	command  string // "/sessions" — header stripe label
	subtitle string // optional sub-header text
	items    []PickerItem
	cursor   int
	scroll   int
	width    int
	height   int
	done     bool
	selected string
}

// NewPickerScreen constructs a fresh picker. Caller supplies command
// (header label), an optional subtitle, and the list of items.
func NewPickerScreen(command, subtitle string, items []PickerItem) *PickerScreen {
	return &PickerScreen{command: command, subtitle: subtitle, items: items}
}

// Selected returns the picked item's Key, or empty if cancelled.
func (s *PickerScreen) Selected() string { return s.selected }

// Command returns the slash label the picker was opened with (e.g.
// "/sessions"). The tui-package apply step routes on this so a single
// PickerScreen type serves /sessions, /skills, /tools, /agents,
// /memory etc. without needing distinct types.
func (s *PickerScreen) Command() string { return s.command }

func (s *PickerScreen) Init() tea.Cmd { return nil }

func (s *PickerScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.scrollToCursor()
}

func (s *PickerScreen) Done() bool { return s.done }

const pickerMaxBody = 18

func (s *PickerScreen) bodyHeight() int {
	h := s.height - 5
	if h < 3 {
		h = 3
	}
	if h > pickerMaxBody {
		h = pickerMaxBody
	}
	return h
}

func (s *PickerScreen) scrollToCursor() {
	bh := s.bodyHeight()
	if s.cursor < s.scroll {
		s.scroll = s.cursor
	}
	if s.cursor >= s.scroll+bh {
		s.scroll = s.cursor - bh + 1
	}
	s.clampScroll()
}

func (s *PickerScreen) clampScroll() {
	maxScroll := len(s.items) - s.bodyHeight()
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

func (s *PickerScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
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
			if s.cursor >= 0 && s.cursor < len(s.items) {
				s.selected = s.items[s.cursor].Key
			}
			s.done = true
			return s, nil
		case tea.KeyUp:
			if n := len(s.items); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			s.scrollToCursor()
			return s, nil
		case tea.KeyDown:
			if n := len(s.items); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			s.scrollToCursor()
			return s, nil
		case tea.KeyPgUp:
			s.cursor -= s.bodyHeight() / 2
			if s.cursor < 0 {
				s.cursor = 0
			}
			s.scrollToCursor()
			return s, nil
		case tea.KeyPgDown:
			s.cursor += s.bodyHeight() / 2
			if s.cursor >= len(s.items) {
				s.cursor = len(s.items) - 1
			}
			s.scrollToCursor()
			return s, nil
		case tea.KeyHome:
			s.cursor = 0
			s.scrollToCursor()
			return s, nil
		case tea.KeyEnd:
			s.cursor = len(s.items) - 1
			s.scrollToCursor()
			return s, nil
		}
		switch m.String() {
		case "q":
			s.done = true
		case "j":
			if n := len(s.items); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			s.scrollToCursor()
		case "k":
			if n := len(s.items); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			s.scrollToCursor()
		case "g":
			s.cursor = 0
			s.scrollToCursor()
		case "G":
			s.cursor = len(s.items) - 1
			s.scrollToCursor()
		}
	case tea.MouseMsg:
		switch m.Button {
		case tea.MouseButtonWheelUp:
			if n := len(s.items); n > 0 {
				s.cursor = (s.cursor - 1 + n) % n
			}
			s.scrollToCursor()
		case tea.MouseButtonWheelDown:
			if n := len(s.items); n > 0 {
				s.cursor = (s.cursor + 1) % n
			}
			s.scrollToCursor()
		}
	}
	return s, nil
}

var (
	pickerSubtitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	pickerCursorMark    = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	pickerLabelActive   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	pickerLabelIdle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	pickerDescStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	pickerHintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Italic(true)
	pickerFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	pickerEmptyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)
)

func (s *PickerScreen) View() string {
	var out strings.Builder

	out.WriteString(infoHeaderStripe.Render(s.command))
	if s.subtitle != "" {
		out.WriteString("  ")
		out.WriteString(pickerSubtitleStyle.Render(s.subtitle))
	}
	out.WriteString("\n\n")

	if len(s.items) == 0 {
		out.WriteString("  ")
		out.WriteString(pickerEmptyStyle.Render("(empty)"))
		out.WriteString("\n")
		out.WriteString("\n  ")
		out.WriteString(pickerFooterStyle.Render("Esc to close"))
		return out.String()
	}

	// Compute label column width.
	labelW := 0
	for _, it := range s.items {
		l := it.Label
		if l == "" {
			l = it.Key
		}
		if w := lipgloss.Width(l); w > labelW {
			labelW = w
		}
	}
	if labelW > 32 {
		labelW = 32
	}

	bh := s.bodyHeight()
	end := s.scroll + bh
	if end > len(s.items) {
		end = len(s.items)
	}

	// Render visible items.
	rendered := make([]string, end-s.scroll)
	for i := s.scroll; i < end; i++ {
		it := s.items[i]
		label := it.Label
		if label == "" {
			label = it.Key
		}
		if lipgloss.Width(label) > labelW {
			label = label[:labelW-1] + "…"
		}

		var line strings.Builder
		if i == s.cursor {
			line.WriteString(pickerCursorMark.Render("▸ "))
			line.WriteString(pickerLabelActive.Render(label))
		} else {
			line.WriteString("  ")
			line.WriteString(pickerLabelIdle.Render(label))
		}
		pad := labelW - lipgloss.Width(label)
		if pad < 0 {
			pad = 0
		}
		line.WriteString(strings.Repeat(" ", pad+2))
		if it.Description != "" {
			line.WriteString(pickerDescStyle.Render(it.Description))
		}
		if it.Hint != "" {
			line.WriteString("  ")
			line.WriteString(pickerHintStyle.Render(it.Hint))
		}
		rendered[i-s.scroll] = line.String()
	}

	// Overflow indicators top/bottom.
	if s.scroll > 0 && len(rendered) > 0 {
		rendered[0] = pickerFooterStyle.Render("  ↑ " + itoa(s.scroll) + " more above")
	}
	if end < len(s.items) && len(rendered) > 0 {
		rendered[len(rendered)-1] = pickerFooterStyle.Render("  ↓ " + itoa(len(s.items)-end) + " more below")
	}
	for _, line := range rendered {
		out.WriteString("  ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	for i := len(rendered); i < bh; i++ {
		out.WriteString("\n")
	}

	out.WriteString("\n")
	hint := "↑/↓  k/j  select  ·  Enter pick  ·  Esc close"
	if len(s.items) > bh {
		hint = "↑/↓  k/j  select  ·  PgUp/PgDn jump  ·  Enter pick  ·  Esc close"
	}
	out.WriteString(pickerFooterStyle.Render("  " + hint))
	return out.String()
}
