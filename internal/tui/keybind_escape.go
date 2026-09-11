package tui

import tea "charm.land/bubbletea/v2"

// dismissEscapePanel handles only Esc, before task/OAuth cancellation. Reuse
// each panel's dismissal path so closing permission/AskUser prompts also
// releases their waiting tool safely. Full-window screens already own input
// in Update; this covers the inline panels handled by the main key dispatcher.
func (m *Model) dismissEscapePanel(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.overlays.Active() {
		if cmd, consumed := m.overlays.Update(msg); consumed {
			return m, cmd, true
		}
	}
	switch {
	case m.effortPicker != nil:
		model, cmd := m.handleEffortPickerKey(msg)
		return model, cmd, true
	case m.permActive:
		return m.handlePermKey(msg) // Esc denies this request; it never grants it.
	case m.askUserActive:
		return m.handleAskUserKey(msg) // Send dismissal, not just hide the prompt.
	case m.paletteVisible():
		model, cmd := m.handlePaletteKey(msg)
		return model, cmd, true
	case m.showSearch:
		m.closeTranscriptSearch()
		return m, nil, true
	case m.showHistory:
		model, cmd := m.handleHistoryKey(msg)
		return model, cmd, true
	case m.atActive && len(m.atMatched) > 0:
		m.atActive = false
		return m, nil, true
	case m.showTaskPanel:
		m.showTaskPanel = false
		return m, nil, true
	default:
		return m, nil, false
	}
}
