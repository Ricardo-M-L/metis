package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/promptsuggest"
)

const defaultInputPlaceholder = "type a message · /commands · alt+enter newline"

type promptSuggestionState struct {
	generation uint64
	pending    bool
	text       string
	sessionID  string
	model      string
	provider   string
	cancel     context.CancelFunc
}

type promptSuggestionResultMsg struct {
	generation uint64
	text       string
	err        error
}

func (m *Model) promptSuggestionEligible() bool {
	return m.cfg != nil && m.cfg.UI.PromptSuggestions && m.loop != nil && m.sessionID != "" &&
		!m.turnActive && !m.spinnerActive && !m.shellMode && !m.copyMode && !m.queuePending && len(m.queuedPrompts) == 0 &&
		!m.permActive && !m.askUserActive && !m.showPalette && m.activeScreen == nil && m.effortPicker == nil &&
		(m.overlays == nil || m.overlays.Top() == nil) &&
		m.input.Value() == "" && len(m.imagePaste) == 0 && (m.ctx == nil || m.ctx.Err() == nil)
}

func (m *Model) queuePromptSuggestion() {
	m.clearPromptSuggestion()
	if !m.promptSuggestionEligible() {
		return
	}
	m.promptSuggestion.generation++
	m.promptSuggestion.pending = true
	m.promptSuggestion.sessionID = m.sessionID
	m.promptSuggestion.model = m.model
	m.promptSuggestion.provider = m.providerName
}

func (m *Model) startPromptSuggestion() tea.Cmd {
	if !m.promptSuggestion.pending {
		return nil
	}
	m.promptSuggestion.pending = false
	if !m.promptSuggestionEligible() {
		m.clearPromptSuggestion()
		return nil
	}
	p, model, _ := m.loop.ProviderModelSnapshot()
	exchange, ok := promptsuggest.LatestExchange(m.loop.History())
	if !ok || p == nil {
		return nil
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	m.promptSuggestion.cancel = cancel
	generation := m.promptSuggestion.generation
	return func() tea.Msg {
		defer cancel()
		text, err := promptsuggest.Generate(ctx, p, model, exchange)
		return promptSuggestionResultMsg{generation: generation, text: text, err: err}
	}
}

func (m *Model) applyPromptSuggestion(result promptSuggestionResultMsg) {
	if result.generation != m.promptSuggestion.generation {
		return
	}
	if !m.promptSuggestionCurrent() || result.err != nil || result.text == "" {
		m.clearPromptSuggestion()
		return
	}
	m.promptSuggestion.cancel = nil
	m.promptSuggestion.text = result.text
	m.input.Placeholder = result.text
}

func (m *Model) promptSuggestionCurrent() bool {
	return m.promptSuggestionEligible() && m.promptSuggestion.sessionID == m.sessionID &&
		m.promptSuggestion.model == m.model && m.promptSuggestion.provider == m.providerName
}

func (m *Model) clearPromptSuggestion() {
	s := &m.promptSuggestion
	if !s.pending && s.cancel == nil && s.text == "" {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.generation++
	s.cancel, s.pending, s.text = nil, false, ""
	m.input.Placeholder = defaultInputPlaceholder
}

func (m *Model) reconcilePromptSuggestion() {
	if (m.promptSuggestion.pending || m.promptSuggestion.cancel != nil || m.promptSuggestion.text != "") && !m.promptSuggestionCurrent() {
		m.clearPromptSuggestion()
	}
}

func (m *Model) acceptPromptSuggestion() bool {
	if m.promptSuggestion.text == "" || !m.promptSuggestionCurrent() {
		return false
	}
	text := m.promptSuggestion.text
	m.clearPromptSuggestion()
	m.input.SetValue(text)
	m.input.CursorEnd()
	return true
}
