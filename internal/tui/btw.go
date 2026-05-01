package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// btwAnswerMsg is delivered to Update when a /btw side-question call
// returns. Result is a single assistant text reply (or err).
type btwAnswerMsg struct {
	answer string
	err    error
}

// startBtwQuery fires a single-turn LLM call (no tools, no history
// write) and routes the answer to a transient overlay modal. The main
// turn — if any — is unaffected: this returns a tea.Cmd that runs in a
// separate goroutine and posts btwAnswerMsg back to the program loop.
//
// `question` is the user-typed text after `/btw `. We strip surrounding
// whitespace; an empty question is filtered earlier by the slash
// command.
func (m *Model) startBtwQuery(question string) tea.Cmd {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil
	}
	if m.ext.BtwAsk == nil {
		// No backend wired — surface a friendly error directly.
		m.btwActive = true
		m.btwQuestion = question
		m.btwErr = "/btw is not wired in this build"
		return nil
	}
	m.btwActive = true
	m.btwQuestion = question
	m.btwAnswer = ""
	m.btwErr = ""
	m.btwLoading = true
	ask := m.ext.BtwAsk
	ctx := m.ctx
	return func() tea.Msg {
		// Capped context so a stuck call doesn't keep the spinner up
		// forever. The wired BtwAsk should also enforce its own timeout.
		ans, err := ask(ctx, question)
		return btwAnswerMsg{answer: ans, err: err}
	}
}

// handleBtwAnswer delivers the side-question result into the modal.
// Called from Update on btwAnswerMsg.
func (m *Model) handleBtwAnswer(msg btwAnswerMsg) {
	m.btwLoading = false
	if msg.err != nil {
		m.btwErr = msg.err.Error()
		return
	}
	m.btwAnswer = msg.answer
}

// dismissBtw closes the side-question modal. Called on Esc.
func (m *Model) dismissBtw() {
	m.btwActive = false
	m.btwQuestion = ""
	m.btwAnswer = ""
	m.btwErr = ""
	m.btwLoading = false
}

// renderBtwOverlay returns a lipgloss-rendered box. Caller composes
// this onto the main view (typically via lipgloss.Place at center).
// Empty string when btwActive is false.
func (m *Model) renderBtwOverlay() string {
	if !m.btwActive {
		return ""
	}
	w := m.width - 8
	if w < 30 {
		w = 30
	}
	if w > 80 {
		w = 80
	}
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	bodyStyle := lipgloss.NewStyle().Width(w - 4)
	hintStyle := lipgloss.NewStyle().Faint(true)

	var b strings.Builder
	b.WriteString(titleStyle.Render("/btw — side question"))
	b.WriteString("\n\n")
	b.WriteString(bodyStyle.Render(strings.TrimSpace(m.btwQuestion)))
	b.WriteString("\n\n")
	switch {
	case m.btwErr != "":
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("error: " + m.btwErr))
	case m.btwLoading:
		b.WriteString(lipgloss.NewStyle().Faint(true).Render("· thinking…"))
	default:
		b.WriteString(bodyStyle.Render(strings.TrimSpace(m.btwAnswer)))
	}
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render("(esc to dismiss)"))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("13")).
		Padding(1, 2).
		Width(w).
		Render(b.String())
}

var _ = context.Background // keep context import even before background usage
