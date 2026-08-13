package screen

// rewind.go implements Claude Code's two-stage checkpoint dialog:
// first choose a user-message boundary, then choose whether to restore code,
// conversation, both, or summarize the selected suffix. The screen owns only
// interaction state; internal/tui applies the selected operation.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type RewindAction uint8

const (
	RewindActionCancel RewindAction = iota
	RewindActionBoth
	RewindActionConversation
	RewindActionCode
	RewindActionSummary
)

type RewindEntry struct {
	Turn              int
	Prompt            string
	HasCodeCheckpoint bool
	LatestEdit        bool
}

type rewindStage uint8

const (
	rewindStagePoint rewindStage = iota
	rewindStageAction
)

type RewindScreen struct {
	entries       []RewindEntry
	stage         rewindStage
	pointCursor   int
	actionCursor  int
	scroll        int
	width         int
	height        int
	done          bool
	selectedTurn  int
	selectedEntry int
	action        RewindAction
}

func NewRewindScreen(entries []RewindEntry) *RewindScreen {
	return &RewindScreen{
		entries:       append([]RewindEntry(nil), entries...),
		selectedEntry: -1,
	}
}

func (s *RewindScreen) Init() tea.Cmd { return nil }

func (s *RewindScreen) Done() bool { return s.done }

func (s *RewindScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.scrollToCursor()
}

func (s *RewindScreen) SelectedTurn() int { return s.selectedTurn }

func (s *RewindScreen) Action() RewindAction { return s.action }

const rewindMaxBody = 18

func (s *RewindScreen) bodyHeight() int {
	h := s.height - 7
	if h < 3 {
		h = 3
	}
	if h > rewindMaxBody {
		h = rewindMaxBody
	}
	return h
}

func (s *RewindScreen) scrollToCursor() {
	if s.stage != rewindStagePoint {
		return
	}
	body := s.bodyHeight()
	if s.pointCursor < s.scroll {
		s.scroll = s.pointCursor
	}
	if s.pointCursor >= s.scroll+body {
		s.scroll = s.pointCursor - body + 1
	}
	maxScroll := len(s.entries) - body
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

func (s *RewindScreen) move(delta int) {
	if s.stage == rewindStageAction {
		const actionCount = 5
		s.actionCursor = (s.actionCursor + delta + actionCount) % actionCount
		return
	}
	if count := len(s.entries); count > 0 {
		s.pointCursor = (s.pointCursor + delta + count) % count
	}
	s.scrollToCursor()
}

func (s *RewindScreen) selectCurrentPoint() {
	if s.pointCursor < 0 || s.pointCursor >= len(s.entries) {
		return
	}
	s.selectedEntry = s.pointCursor
	s.selectedTurn = s.entries[s.pointCursor].Turn
	s.stage = rewindStageAction
	s.actionCursor = 0
}

func (s *RewindScreen) quickLatestEdit() {
	for index, entry := range s.entries {
		if !entry.LatestEdit {
			continue
		}
		s.selectedEntry = index
		s.selectedTurn = entry.Turn
		s.action = RewindActionBoth
		s.done = true
		return
	}
}

func (s *RewindScreen) selectCurrentAction() {
	actions := [...]RewindAction{
		RewindActionBoth,
		RewindActionConversation,
		RewindActionCode,
		RewindActionSummary,
		RewindActionCancel,
	}
	if s.actionCursor < 0 || s.actionCursor >= len(actions) {
		return
	}
	s.action = actions[s.actionCursor]
	s.done = true
}

func (s *RewindScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch event := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(event.Width, event.Height)
		return s, nil
	case tea.KeyPressMsg:
		switch event.String() {
		case "esc", "ctrl+c":
			if s.stage == rewindStageAction {
				s.stage = rewindStagePoint
				s.actionCursor = 0
				s.action = RewindActionCancel
				return s, nil
			}
			s.action = RewindActionCancel
			s.selectedTurn = 0
			s.done = true
			return s, nil
		case "q":
			s.action = RewindActionCancel
			s.selectedTurn = 0
			s.done = true
			return s, nil
		case "enter":
			if s.stage == rewindStagePoint {
				s.selectCurrentPoint()
			} else {
				s.selectCurrentAction()
			}
			return s, nil
		case "l":
			if s.stage == rewindStagePoint {
				s.quickLatestEdit()
				return s, nil
			}
		case "up", "k":
			s.move(-1)
			return s, nil
		case "down", "j":
			s.move(1)
			return s, nil
		case "pgup":
			if s.stage == rewindStagePoint {
				s.pointCursor -= s.bodyHeight() / 2
				if s.pointCursor < 0 {
					s.pointCursor = 0
				}
				s.scrollToCursor()
			}
			return s, nil
		case "pgdown":
			if s.stage == rewindStagePoint {
				s.pointCursor += s.bodyHeight() / 2
				if s.pointCursor >= len(s.entries) {
					s.pointCursor = len(s.entries) - 1
				}
				s.scrollToCursor()
			}
			return s, nil
		case "home", "g":
			if s.stage == rewindStagePoint {
				s.pointCursor = 0
				s.scrollToCursor()
			}
			return s, nil
		case "end", "G":
			if s.stage == rewindStagePoint {
				s.pointCursor = len(s.entries) - 1
				s.scrollToCursor()
			}
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch event.Button {
		case tea.MouseWheelUp:
			s.move(-1)
		case tea.MouseWheelDown:
			s.move(1)
		}
	}
	return s, nil
}

var (
	rewindTitleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	rewindCursorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	rewindActiveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	rewindIdleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	rewindDetailStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	rewindCodeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b"))
	rewindSummaryStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd"))
	rewindWarnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c"))
	rewindFooterStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	rewindEmptyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)
)

func (s *RewindScreen) View() string {
	if s.stage == rewindStageAction {
		return s.viewActions()
	}
	return s.viewPoints()
}

func (s *RewindScreen) viewPoints() string {
	var out strings.Builder
	out.WriteString(infoHeaderStripe.Render("rewind"))
	out.WriteString("  ")
	out.WriteString(rewindTitleStyle.Render("Select a checkpoint"))
	out.WriteString("\n\n")
	out.WriteString(rewindDetailStyle.Render("  Choose the prompt to return to. The selected prompt will become editable again."))
	out.WriteString("\n\n")

	if len(s.entries) == 0 {
		out.WriteString("  ")
		out.WriteString(rewindEmptyStyle.Render("No user-message checkpoints in this conversation."))
		out.WriteString("\n\n")
		out.WriteString(rewindFooterStyle.Render("  Esc close"))
		return out.String()
	}

	body := s.bodyHeight()
	end := s.scroll + body
	if end > len(s.entries) {
		end = len(s.entries)
	}
	for index := s.scroll; index < end; index++ {
		entry := s.entries[index]
		marker := "  "
		style := rewindIdleStyle
		if index == s.pointCursor {
			marker = rewindCursorStyle.Render("▸ ")
			style = rewindActiveStyle
		}
		promptWidth := s.width - 30
		if promptWidth < 24 {
			promptWidth = 24
		}
		prompt := rewindPromptPreview(entry.Prompt, promptWidth)
		out.WriteString("  ")
		out.WriteString(marker)
		out.WriteString(style.Render(fmt.Sprintf("Turn %d", entry.Turn)))
		out.WriteString("  ")
		out.WriteString(style.Render(prompt))
		if entry.HasCodeCheckpoint {
			out.WriteString("  ")
			out.WriteString(rewindCodeStyle.Render("code"))
		}
		if entry.LatestEdit {
			out.WriteString("  ")
			out.WriteString(rewindWarnStyle.Render("latest edit"))
		}
		out.WriteString("\n")
	}
	for index := end - s.scroll; index < body; index++ {
		out.WriteString("\n")
	}
	out.WriteString("\n")
	hint := "↑/↓  k/j select  ·  Enter actions  ·  l quick restore last edit  ·  Esc cancel"
	if len(s.entries) > body {
		hint = "↑/↓ select  ·  PgUp/PgDn jump  ·  Enter actions  ·  l quick last edit  ·  Esc cancel"
	}
	out.WriteString(rewindFooterStyle.Render("  " + hint))
	return out.String()
}

type rewindActionRow struct {
	name        string
	description string
}

var rewindActionRows = []rewindActionRow{
	{"Restore code and conversation", "Revert files and remove this prompt and everything after it."},
	{"Restore conversation", "Keep current files; rewind chat and put this prompt back in the composer."},
	{"Restore code", "Revert files while keeping the full conversation."},
	{"Summarize from here", "Keep files; replace this prompt and later messages with an AI summary."},
	{"Cancel", "Return to chat without changing anything."},
}

func (s *RewindScreen) viewActions() string {
	var out strings.Builder
	out.WriteString(infoHeaderStripe.Render("rewind"))
	out.WriteString("  ")
	out.WriteString(rewindTitleStyle.Render("Choose an action"))
	out.WriteString("\n\n")
	if s.selectedEntry >= 0 && s.selectedEntry < len(s.entries) {
		entry := s.entries[s.selectedEntry]
		out.WriteString(rewindDetailStyle.Render(fmt.Sprintf("  Turn %d  %s", entry.Turn, rewindPromptPreview(entry.Prompt, 72))))
		out.WriteString("\n\n")
	}
	for index, row := range rewindActionRows {
		marker := "  "
		style := rewindIdleStyle
		if index == s.actionCursor {
			marker = rewindCursorStyle.Render("▸ ")
			style = rewindActiveStyle
		}
		out.WriteString("  ")
		out.WriteString(marker)
		out.WriteString(style.Render(row.name))
		out.WriteString("\n      ")
		if index == 3 {
			out.WriteString(rewindSummaryStyle.Render(row.description))
		} else {
			out.WriteString(rewindDetailStyle.Render(row.description))
		}
		out.WriteString("\n")
	}
	out.WriteString("\n")
	out.WriteString(rewindFooterStyle.Render("  ↑/↓  k/j select  ·  Enter confirm  ·  Esc back  ·  q cancel"))
	return out.String()
}

func rewindPromptPreview(prompt string, maxWidth int) string {
	// Prompt text is untrusted terminal input. Lipgloss styles text but does
	// not neutralize embedded CSI/OSC sequences, so strip every C0/C1 control
	// before rendering. Preserve ordinary Unicode and turn whitespace controls
	// into spaces for a readable one-line preview; the original entry.Prompt is
	// untouched and is still what /rewind returns to the composer.
	prompt = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, prompt)
	prompt = strings.Join(strings.Fields(prompt), " ")
	if prompt == "" || lipgloss.Width(prompt) <= maxWidth {
		return prompt
	}
	var out strings.Builder
	width := 0
	for len(prompt) > 0 {
		r, size := utf8.DecodeRuneInString(prompt)
		if width+lipgloss.Width(string(r))+1 > maxWidth {
			break
		}
		out.WriteRune(r)
		width += lipgloss.Width(string(r))
		prompt = prompt[size:]
	}
	return strings.TrimSpace(out.String()) + "…"
}
