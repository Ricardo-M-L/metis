package screen

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

// HistoryScreen renders the agent's transcript as a scrollable, role-colored
// page. Triggered by /history; dismissed by Esc / q / Ctrl-C.
//
// The screen is read-only: it never edits the transcript or sends messages.
//
// Implementation note: we wrap each Message into a historyItem and feed
// the slice to the same internal/tui/list package the chat surface uses,
// so HistoryScreen gets virtualization for free — long sessions
// (thousands of messages) only render the visible window each frame.
// Per-item rendering is cached by width so resize is the only path that
// actually re-runs renderUserMessage / renderAssistantMessage.
type HistoryScreen struct {
	l    *list.List
	done bool
	// Header label printed above the scroll region. Caller can override
	// before opening (e.g. with the session id).
	Title string
}

// historyItem wraps an llm.Message into list.Item for the virtualized
// list. Each item caches its rendered string by width so spinner ticks
// (which hit Render multiple times via List.AtBottom + Render +
// TotalLineCount) stay cheap.
type historyItem struct {
	msg          llm.Message
	cachedWidth  int
	cachedOutput string
}

// emptyItem renders a placeholder when the transcript is empty.
type emptyItem struct{}

func (e *emptyItem) Render(width int) string {
	return histDim.Render("(history is empty — start chatting)")
}

func (i *historyItem) Render(width int) string {
	if i.cachedWidth == width && i.cachedOutput != "" {
		return i.cachedOutput
	}
	var b strings.Builder
	switch i.msg.Role {
	case llm.RoleUser:
		renderUserMessage(&b, i.msg, width)
	case llm.RoleAssistant:
		renderAssistantMessage(&b, i.msg, width)
	default:
		// system / tool roles — render minimally.
		b.WriteString(histRoleTool.Render("▸ " + string(i.msg.Role)))
		b.WriteString("\n")
	}
	i.cachedWidth = width
	// Trim trailing newlines so list's height calculation doesn't
	// double-count blank rows; list inserts gap rows between items
	// itself when SetGap > 0.
	i.cachedOutput = strings.TrimRight(b.String(), "\n")
	return i.cachedOutput
}

// NewHistoryScreen builds a screen ready to View(). messages is the snapshot
// to render; mutating it after construction has no effect.
func NewHistoryScreen(messages []llm.Message, width, height int) *HistoryScreen {
	l := list.NewList()
	l.SetSize(width, contentHeight(height))
	// Single-row gap between turns — same visual rhythm the previous
	// renderHistory produced via "\n" separators.
	l.SetGap(1)

	if len(messages) == 0 {
		l.SetItems(&emptyItem{})
	} else {
		items := make([]list.Item, len(messages))
		for i := range messages {
			items[i] = &historyItem{msg: messages[i]}
		}
		l.SetItems(items...)
	}
	return &HistoryScreen{
		l:     l,
		Title: "session history",
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
	s.l.SetSize(width, contentHeight(height))
	// historyItem's per-width cache invalidates implicitly: the next
	// Render(width) call sees a fresh width and re-runs renderXxxMessage.
}

func (s *HistoryScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		// v2: KeyMsg is interface; KeyPressMsg concrete. Match by
		// .String() — covers named keys ("esc", "pgup") and ASCII alike.
		switch m.String() {
		case "esc", "ctrl+c", "q":
			s.done = true
			return s, nil
		case "up", "k":
			s.l.ScrollBy(-1)
			return s, nil
		case "down", "j":
			s.l.ScrollBy(1)
			return s, nil
		case "pgup":
			s.l.ScrollBy(-s.l.Height() / 2)
			return s, nil
		case "pgdown":
			s.l.ScrollBy(s.l.Height() / 2)
			return s, nil
		case "home", "g":
			s.l.ScrollToTop()
			return s, nil
		case "end", "G":
			s.l.ScrollToBottom()
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			s.l.ScrollBy(-1)
		case tea.MouseWheelDown:
			s.l.ScrollBy(1)
		}
	}
	return s, nil
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
	b.WriteString(s.l.Render())
	b.WriteString("\n")
	b.WriteString(histDim.Render("↑/↓ k/j PgUp/PgDn Home/End g/G to scroll · Esc / q to close"))
	return b.String()
}

// RenderHistoryBody is the public version of the transcript renderer.
// Returns the role-colored, wrapped body without any chrome (title, hints).
// Useful for non-screen contexts (e.g. the readline REPL fallback prints it
// directly) so the formatting stays consistent across UIs.
func RenderHistoryBody(messages []llm.Message, width int) string {
	return renderHistory(messages, width)
}

// renderHistory turns a transcript into a static body string. Used by the
// non-TUI fallback (RenderHistoryBody) where we don't have a list/screen
// to drive virtualized render. The HistoryScreen path goes through
// historyItem.Render directly and skips this function.
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
		if c.Type == "text" && transcript.VisibleUserText(c.Text) != "" {
			hasText = true
			break
		}
	}
	if hasText {
		b.WriteString(histRoleUser.Render("▸ user"))
		b.WriteString("\n")
		for _, c := range m.Content {
			if c.Type == "text" {
				visible := transcript.VisibleUserText(c.Text)
				if visible == "" {
					continue
				}
				b.WriteString(indent(histText.Render(wrap(visible, width-4)), "  "))
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
		case "thinking":
			if c.Text != "" {
				b.WriteString(indent(histDim.Render("✻ thinking"), "  "))
				b.WriteString("\n")
				b.WriteString(indent(histDim.Render(wrap(c.Text, width-6)), "    "))
				b.WriteString("\n")
			}
		case "redacted_thinking":
			// The encrypted payload is persisted for provider continuity but
			// must never be printed into the transcript screen.
			b.WriteString(indent(histDim.Render("🔒 thinking redacted by provider"), "  "))
			b.WriteString("\n")
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
