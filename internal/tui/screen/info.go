package screen

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// InfoRow is one line in an info screen — same shape as the inline
// renderInfoBox uses, duplicated here to avoid a screen→tui import
// cycle. Empty Key = full-width free text or section header; otherwise
// rendered as `key: value · hint` with keys right-padded so values
// left-align.
type InfoRow struct {
	Key   string
	Value string
	Hint  string
}

// InfoScreen is a full-window overlay for information-dense slash
// command output (/help, /cost, /tokens, /doctor, /context, /stats,
// /keybindings, /permissions, /hooks, /tools, /skills, /env, /version).
//
// Visual model mirrors claude-code's command modals: a header bar at
// the top showing the invoked command name (e.g. "/cost") in a highlight
// stripe, the rendered body in a scrollable middle region, and a footer
// hint line ("Esc to close") at the bottom.
//
// Distinct from HistoryScreen (which is special-cased for transcript
// rendering with virtualized list); InfoScreen takes pre-shaped rows
// and renders them itself, no list package needed.
type InfoScreen struct {
	command string // e.g. "/cost" — shows in the header stripe
	title   string // e.g. "Session Cost · MiniMax-M2.7" — body title
	rows    []InfoRow
	scroll  int // line offset
	width   int
	height  int
	done    bool
}

// NewInfoScreen builds the screen ready to View(). The command argument
// is what the user typed (with the leading "/"); title is the body
// header line; rows is the content.
func NewInfoScreen(command, title string, rows []InfoRow) *InfoScreen {
	return &InfoScreen{
		command: command,
		title:   title,
		rows:    rows,
	}
}

func (s *InfoScreen) Init() tea.Cmd { return nil }

func (s *InfoScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.clampScroll()
}

func (s *InfoScreen) Done() bool { return s.done }

// bodyHeight = total height minus chrome (header 2 rows, footer 2 rows).
func (s *InfoScreen) bodyHeight() int {
	h := s.height - 4
	if h < 3 {
		h = 3
	}
	return h
}

func (s *InfoScreen) totalLines() int {
	// Each row renders to 1 line in our table layout.
	return len(s.rows)
}

func (s *InfoScreen) clampScroll() {
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

func (s *InfoScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		// v2: collapsed two switches (.Type + .String()) into one
		// .String() match.
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

// Style palette — distinct from HistoryScreen so the two visually
// distinguish. Header uses a magenta highlight stripe (claude-code
// uses a similar style for slash command modals).
var (
	infoHeaderStripe = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#f8f8f2")).
				Background(lipgloss.Color("#5a4a78")).
				Bold(true).
				Padding(0, 1)
	infoTitleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#bd93f9")).
			Bold(true)
	infoKeyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	infoValueStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	infoHintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)
	infoSectStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Bold(true)
	infoFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
)

// renderRow turns one InfoRow into a single styled line.
//   - Empty Key + empty Value → blank spacer.
//   - Empty Key with Value starting with "──" → section divider (e.g.
//     "── most recent API call ──"), styled in cyan.
//   - Empty Key with normal Value → muted free-form line (footnotes).
//   - Key + Value → "key:  value  · hint" with keys right-padded to keyW.
func renderRow(row InfoRow, keyW int) string {
	switch {
	case row.Key == "" && row.Value == "":
		return ""
	case row.Key == "" && strings.HasPrefix(strings.TrimSpace(row.Value), "──"):
		return infoSectStyle.Render(row.Value)
	case row.Key == "":
		return infoHintStyle.Render(row.Value)
	}
	pad := keyW - lipgloss.Width(row.Key)
	if pad < 0 {
		pad = 0
	}
	out := infoKeyStyle.Render(row.Key+":") + strings.Repeat(" ", pad+1) + infoValueStyle.Render(row.Value)
	if row.Hint != "" {
		out += "  " + infoHintStyle.Render("· "+row.Hint)
	}
	return out
}

func (s *InfoScreen) View() string {
	var out strings.Builder

	// Header: "/cost" stripe + body title on the next line.
	out.WriteString(infoHeaderStripe.Render(s.command))
	out.WriteString("\n")
	if s.title != "" {
		out.WriteString(infoTitleStyle.Render("  " + s.title))
		out.WriteString("\n")
	}

	// Body — table of rows, scrolled by s.scroll lines.
	keyW := 0
	for _, r := range s.rows {
		if w := lipgloss.Width(r.Key); w > keyW {
			keyW = w
		}
	}
	bh := s.bodyHeight()
	end := s.scroll + bh
	if end > len(s.rows) {
		end = len(s.rows)
	}
	for i := s.scroll; i < end; i++ {
		out.WriteString("  ")
		out.WriteString(renderRow(s.rows[i], keyW))
		out.WriteString("\n")
	}
	// Pad remaining body height with blank lines so the footer sits at
	// the bottom regardless of content length.
	for i := end - s.scroll; i < bh; i++ {
		out.WriteString("\n")
	}

	// Footer hint.
	out.WriteString("\n")
	hints := "↑/↓ k/j PgUp/PgDn  ·  Esc / q to close"
	if s.totalLines() <= bh {
		hints = "Esc / q to close"
	}
	out.WriteString(infoFooterStyle.Render("  " + hints))

	return out.String()
}
