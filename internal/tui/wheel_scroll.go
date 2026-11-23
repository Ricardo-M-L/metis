package tui

import tea "charm.land/bubbletea/v2"

func (m *Model) scrollTranscriptWheel(button tea.MouseButton, count int) {
	if m.chatList == nil || count <= 0 {
		return
	}
	step := m.chatList.MouseWheelDelta()
	switch button {
	case tea.MouseWheelUp:
		// Detach even when wheel quantization has not moved the first row.
		m.stickyBottom = false
		step = -step
	case tea.MouseWheelDown:
	default:
		return
	}

	lines := step * count
	if quantum := perfConfig().ScrollQuantum; quantum > 0 {
		if (step < 0) != (m.wheelAccum < 0) {
			m.wheelAccum = 0
		}
		m.wheelAccum += lines
		if m.wheelAccum > -quantum && m.wheelAccum < quantum {
			return
		}
		lines, m.wheelAccum = m.wheelAccum, 0
	}
	m.chatList.ScrollBy(lines)
	if button == tea.MouseWheelDown && m.chatList.AtBottom() {
		m.stickyBottom = true
	}
}

func (m *Model) updateActiveScreen(msg tea.Msg) tea.Cmd {
	batch, ok := msg.(tea.MouseWheelBatchMsg)
	if !ok {
		var cmd tea.Cmd
		m.activeScreen, cmd = m.activeScreen.Update(msg)
		return cmd
	}
	// Picker screens still receive their original input contract. Apply the
	// updates together, then let Bubble Tea render the resulting screen once.
	var cmds []tea.Cmd
	for range batch.Count {
		var cmd tea.Cmd
		m.activeScreen, cmd = m.activeScreen.Update(batch.MouseWheelMsg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if m.activeScreen.Done() {
			break
		}
	}
	return tea.Sequence(cmds...)
}
