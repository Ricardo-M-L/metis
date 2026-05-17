package tui

// keybind_askuser.go — key handling for the AskUser blocking prompt.
//
// Behavioral parallel to keybind_permission.go's handlePermKey: while
// the prompt is up, navigation/decision keys (arrows, enter/space,
// 1-9 numeric shortcuts, esc) are intercepted and the rest fall
// through. Tab toggles focus between option list and freeform input
// (when freeform is enabled); while focus is on freeform, character
// keys go to the textinput and Enter submits the typed answer.
//
// Mirrors claude-code's AskUserQuestion menu UX: number keys are the
// fast-path for users who can read the option list visually; Tab is
// the alternative for "I have a different answer."

import (
	tea "charm.land/bubbletea/v2"
)

// handleAskUserKey processes a key while the AskUser prompt is up.
//
// Returns handled=true ONLY when the key is part of the prompt's
// vocabulary. Other keys fall through to the editor so the user can
// continue composing their next message while the prompt waits — same
// permissive editor policy as handlePermKey for image #18.
func (m *Model) handleAskUserKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	key := msg.String()

	// When focus is on freeform, the option-list arrows / shortcuts
	// don't apply: most printable keys must reach the textinput. Only
	// the universal exits (esc / tab) and the submit key (enter) keep
	// their semantics here.
	if m.askUserFreeformOn {
		switch key {
		case "esc":
			m.askUserActive = false
			m.executeAskUser("")
			return m, nil, true
		case "tab":
			// Tab cycles back to option list when there are options to
			// pick from; otherwise stays in freeform.
			if len(m.askUserOptions) > 0 {
				m.askUserFreeformOn = false
				m.askUserInput.Blur()
			}
			return m, nil, true
		case "enter":
			answer := m.askUserInput.Value()
			m.executeAskUser(answer)
			return m, nil, true
		}
		// Everything else (printable characters, backspace, arrows
		// within the input, etc.) is forwarded to the textinput. We
		// run the update on a synthetic Msg so the input keeps its
		// cursor / value bookkeeping correct.
		var cmd tea.Cmd
		m.askUserInput, cmd = m.askUserInput.Update(msg)
		return m, cmd, true
	}

	// Focus on the option list. The full menu vocabulary applies.
	switch key {
	case "left", "up":
		if m.askUserCursor > 0 {
			m.askUserCursor--
		}
		return m, nil, true
	case "right", "down":
		if m.askUserCursor < len(m.askUserOptions)-1 {
			m.askUserCursor++
		}
		return m, nil, true
	case "enter", "space":
		if len(m.askUserOptions) == 0 {
			// Edge: no options AND freeform turned off by some odd
			// dispatch path. Treat Enter as "dismiss" so the user
			// isn't trapped. ask.go's normalization should prevent
			// this case but defending the UI against it is cheap.
			m.askUserActive = false
			m.executeAskUser("")
			return m, nil, true
		}
		choice := m.askUserOptions[m.askUserCursor]
		m.executeAskUser(choice)
		return m, nil, true
	case "tab":
		if m.askUserAllowFreeform {
			m.askUserFreeformOn = true
			m.askUserInput.Focus()
		}
		return m, nil, true
	case "esc":
		m.askUserActive = false
		m.executeAskUser("")
		return m, nil, true
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		idx := int(key[0] - '1')
		if idx >= 0 && idx < len(m.askUserOptions) {
			m.executeAskUser(m.askUserOptions[idx])
			return m, nil, true
		}
		// Out-of-range numeric — drop, don't fall through (typing
		// a digit while in the menu shouldn't pollute the editor).
		return m, nil, true
	}
	return m, nil, false
}

// executeAskUser sends the chosen answer through the reply channel
// and clears prompt state. The reply channel is buffered size 1, so
// the send never blocks. An empty answer signals dismissal (Esc /
// no-options-Enter); the tool side treats that as IsError so the
// model can choose to retry or abandon.
func (m *Model) executeAskUser(answer string) {
	if m.askUserReply != nil {
		m.askUserReply <- answer
		m.askUserReply = nil
	}
	m.askUserActive = false
	m.askUserQuestion = ""
	m.askUserOptions = nil
	m.askUserAllowFreeform = false
	m.askUserCursor = 0
	m.askUserFreeformOn = false
	// Leaving askUserInput's stored value behind would surface it on
	// the next AskUser dispatch as pre-filled text. Reset it.
	m.askUserInput.SetValue("")
}
