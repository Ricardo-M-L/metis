package tui

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// sessionsPickerItems builds the picker rows for /sessions. Caller
// applies the selected ID via gate-bridge in screen_results.go.
func (m *Model) sessionsPickerItems(limit int) []screen.PickerItem {
	if m.session == nil {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	entries, err := m.session.List(limit)
	if err != nil || len(entries) == 0 {
		return nil
	}
	out := make([]screen.PickerItem, 0, len(entries))
	for _, e := range entries {
		ts := e.CreatedAt.Local().Format("2006-01-02 15:04")
		label := e.Title
		if label == "" {
			label = shortID(e.ID)
		}
		desc := e.Model
		out = append(out, screen.PickerItem{
			Key:         e.ID, // exact ID for resume routing
			Label:       label,
			Description: desc,
			Hint:        ts,
		})
	}
	return out
}

// skillsPickerItems builds the picker rows for /skills.
func (m *Model) skillsPickerItems() []screen.PickerItem {
	loader := skills.NewLoader(m.skillDir, "", nil)
	list, err := loader.List()
	if err != nil || len(list) == 0 {
		return nil
	}
	out := make([]screen.PickerItem, 0, len(list))
	for _, sk := range list {
		desc := sk.Description
		if desc == "" {
			desc = "(no description)"
		}
		out = append(out, screen.PickerItem{
			Key:         sk.Name,
			Label:       sk.Name,
			Description: desc,
		})
	}
	return out
}

// toolsPickerItems builds the picker rows for /tools. Selected tool's
// detailed schema is shown by the apply step.
func (m *Model) toolsPickerItems() []screen.PickerItem {
	if m.loop == nil || m.loop.Registry == nil {
		return nil
	}
	tools := m.loop.Registry.All()
	out := make([]screen.PickerItem, 0, len(tools))
	for _, t := range tools {
		out = append(out, screen.PickerItem{
			Key:         t.Name(),
			Label:       t.Name(),
			Description: truncateForPicker(t.Description(), 80),
		})
	}
	return out
}

// truncateForPicker keeps a description short enough to fit a single
// picker row without wrapping.
func truncateForPicker(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// pickerSubtitle returns the subtitle line shown next to the command
// stripe. Reads "20 saved sessions" / "23 skills loaded" etc.
func pickerSubtitle(noun string, n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s", n, noun)
}
