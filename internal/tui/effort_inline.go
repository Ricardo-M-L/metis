package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func isBareEffortCommand(text string) bool {
	args, ok := effortCommandArgs(text)
	return ok && strings.TrimSpace(args) == ""
}

func effortCommandArgs(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	name, args, _ := cut(text[1:], " ")
	return args, strings.EqualFold(name, "effort")
}

// handleLocalEffortCommand keeps /effort out of the steering channel even
// while a turn is running. Bare /effort opens the inline chooser; explicit
// levels apply immediately to the shared Loop and affect the next provider
// request/iteration.
func (m *Model) handleLocalEffortCommand(text string) bool {
	args, ok := effortCommandArgs(text)
	if !ok {
		return false
	}
	m.showPalette = false
	m.palFilter = ""
	m.stickyBottom = true
	if strings.TrimSpace(args) == "" {
		m.input.SetValue("/effort")
		m.openInlineEffortPicker()
		return true
	}

	m.input.Reset()
	if m.cmds == nil {
		return true
	}
	cmd := m.cmds.Get("effort")
	if cmd == nil {
		return true
	}
	if output := cmd.Handler(m.asREPL(), args); output != "" {
		m.messages = append(m.messages, Message{
			Role:      classifyREPLOutput(output),
			Content:   output,
			Timestamp: time.Now(),
		})
	}
	return true
}

func (m *Model) openInlineEffortPicker() {
	current := ""
	if m.loop != nil {
		current = string(m.loop.EffortValue())
	}
	picker := screen.NewEffortScreen(current)
	picker.Resize(m.width, m.height)
	m.effortPicker = picker
	m.showPalette = false
	m.palFilter = ""
	m.stickyBottom = true
}

func (m *Model) handleEffortPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	updated, cmd := m.effortPicker.Update(msg)
	picker, ok := updated.(*screen.EffortScreen)
	if !ok {
		m.effortPicker = nil
		m.input.Reset()
		return m, cmd
	}
	m.effortPicker = picker
	if !picker.Done() {
		return m, cmd
	}

	m.effortPicker = nil
	m.input.Reset()
	if picker.Applied() == "" {
		// Esc is a quiet cancel for the inline chooser. Appending a
		// "dialog dismissed" transcript row would turn temporary chrome into
		// permanent conversation noise.
		return m, cmd
	}
	extra := m.applyScreenResult(picker)
	if cmd != nil && extra != nil {
		return m, tea.Batch(cmd, extra)
	}
	if extra != nil {
		return m, extra
	}
	return m, cmd
}
