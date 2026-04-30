package screen

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// HistoryScreen renders the agent's transcript as a scrollable, role-colored
// page. Triggered by /history; dismissed by Esc / q / Ctrl-C.
//
// The screen is read-only: it never edits the transcript or sends messages.
// Implementation note: we render the entire history into a string once at
// open time and feed it to a viewport. That keeps Update cheap (just
// scrolling) and lets re-opens with the same history avoid the rebuild cost
// — but if the conversation gets really long (>~5k messages) this should
// switch to lazy rendering. Today the cap is small enough that eager wins.
type HistoryScreen struct {
	vp       viewport.Model
	rendered string
	done     bool
	// Header label printed above the scroll region. Caller can override
	// before opening (e.g. with the session id).
	Title string
}

// NewHistoryScreen builds a screen ready to View(). messages is the snapshot
// to render; mutating it after construction has no effect.
func NewHistoryScreen(messages []llm.Message, width, height int) *HistoryScreen {
	body := renderHistory(messages, width)
	vp := viewport.New(width, contentHeight(height))
	vp.SetContent(body)
	return &HistoryScreen{
		vp:       vp,
		rendered: body,
		Title:    "session history",
	}
}

// contentHeight reserves rows for the title bar (1) and the keybind hint (1).
// Falls back to 10 when the parent gives us a bogus size.
func contentHeight(h int) int {
	if h <= 4 {
		return 10
	}
	return h - 2
}

func (s *HistoryScreen) Init() tea.Cmd { return nil }

func (s *HistoryScreen) Resize(width, height int) {
	s.vp.Width = width
	s.vp.Height = contentHeight(height)
	// Re-wrap so long lines fit the new width.
	// Re-rendering on every resize is acceptable — the user only resizes
	// occasionally and the alternative (cached + measured) isn't worth it.
	if s.rendered != "" {
		s.vp.SetContent(s.rendered)
	}
}

func (s *HistoryScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyMsg:
		switch m.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			s.done = true
			return s, nil
		}
		switch m.String() {
		case "q":
			s.done = true
			return s, nil
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *HistoryScreen) Done() bool { return s.done }

// Style palette is local to the screen so tui.go's globals don't need to be
// exported. Colors mirror the chat surface: user is cyan-ish blue, assistant
// is muted green, tool-related entries are gray.
var (
	histTitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#64b5f6")).Bold(true)
	histRoleUser   = lipgloss.NewStyle().Foreground(lipgloss.Color("#4dd0e1")).Bold(true)
	histRoleAsst   = lipgloss.NewStyle().Foreground(lipgloss.Color("#81c784")).Bold(true)
	histRoleTool   = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	histText       = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	histDim        = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	histErrTag     = lipgloss.NewStyle().Foreground(lipgloss.Color("#e57373")).Bold(true)
)

func (s *HistoryScreen) View() string {
	var b strings.Builder
	b.WriteString(histTitleStyle.Render("📜 " + s.Title))
	b.WriteString("\n")
	b.WriteString(s.vp.View())
	b.WriteString("\n")
	b.WriteString(histDim.Render("↑/↓/PgUp/PgDn to scroll · Esc / q to close"))
	return b.String()
}

// RenderHistoryBody is the public version of the transcript renderer.
// Returns the role-colored, wrapped body without any chrome (title, hints).
// Useful for non-screen contexts (e.g. the readline REPL fallback prints it
// directly) so the formatting stays consistent across UIs.
func RenderHistoryBody(messages []llm.Message, width int) string {
	return renderHistory(messages, width)
}

// renderHistory turns a transcript into the static body text fed to the
// viewport. Output is line-oriented (no relative positioning) so viewport
// scrolling stays predictable.
//
// Format per turn:
//
//	▸ user
//	  what the user typed
//
//	▸ assistant
//	  the reply, including any inline thinking
//	  → tool: Read({"path":"…"})
//	  ← result: (200 chars truncated)
//
// Tool calls and results render in gray; errors get a red [error] tag
// in front of the tool result so they're easy to spot when scanning.
func renderHistory(messages []llm.Message, width int) string {
	if len(messages) == 0 {
		return histDim.Render("(history is empty — start chatting)")
	}
	var b strings.Builder
	for i, m := range messages {
		switch m.Role {
		case llm.RoleUser:
			renderUserMessage(&b, m, width)
		case llm.RoleAssistant:
			renderAssistantMessage(&b, m, width)
		default:
			// system / tool roles — render minimally.
			b.WriteString(histRoleTool.Render("▸ " + string(m.Role)))
			b.WriteString("\n")
		}
		if i < len(messages)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func renderUserMessage(b *strings.Builder, m llm.Message, width int) {
	hasText := false
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			hasText = true
			break
		}
	}
	if hasText {
		b.WriteString(histRoleUser.Render("▸ user"))
		b.WriteString("\n")
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				b.WriteString(indent(histText.Render(wrap(c.Text, width-4)), "  "))
				b.WriteString("\n")
			}
		}
		return
	}
	// Tool-result-only user message — show as the "← result" line under
	// whatever assistant bundle invoked it. Caller drives ordering.
	for _, c := range m.Content {
		if c.Type == "tool_result" {
			tag := "← result"
			if c.IsError {
				tag = histErrTag.Render("← error") + histRoleTool.Render(":")
			} else {
				tag = histRoleTool.Render("← result:")
			}
			line := tag + " " + truncate(c.ToolResult, 240)
			b.WriteString(indent(line, "  "))
			b.WriteString("\n")
		}
	}
}

func renderAssistantMessage(b *strings.Builder, m llm.Message, width int) {
	b.WriteString(histRoleAsst.Render("▸ assistant"))
	b.WriteString("\n")
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				b.WriteString(indent(histText.Render(wrap(c.Text, width-4)), "  "))
				b.WriteString("\n")
			}
		case "tool_use":
			b.WriteString(indent(
				histRoleTool.Render(fmt.Sprintf("→ tool: %s(%s)", c.ToolName, formatArgs(c.ToolInput))),
				"  ",
			))
			b.WriteString("\n")
		}
	}
}

func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, truncate(fmt.Sprintf("%v", v), 60)))
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// wrap is a deliberately tiny word-wrap. lipgloss has its own wrapping but
// also rewrites trailing whitespace which can make `truncate` boundaries
// look weird; rolling our own keeps the rendering predictable.
func wrap(s string, width int) string {
	if width <= 20 {
		return s
	}
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		for len(line) > width {
			cut := width
			// Try to break at a space within the last 20 chars.
			for i := width - 1; i >= width-20 && i >= 0; i-- {
				if line[i] == ' ' {
					cut = i
					break
				}
			}
			out.WriteString(line[:cut])
			out.WriteByte('\n')
			line = strings.TrimLeft(line[cut:], " ")
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
