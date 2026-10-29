package tui

import (
	"regexp"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

// Selection belongs to the live view, not to the persisted conversation.
func (m *Model) clearToolSelection() {
	m.selectedToolID = ""
	m.expandedToolID = ""
	m.pendingToolScrollID = ""
}

func (m *Model) reconcileToolSelectionSession() {
	if m.selectedToolID != "" && m.toolSelectionSession != m.sessionID {
		m.clearToolSelection()
	}
}

func (m *Model) navigateToolDetails(direction int, toggle bool) {
	m.reconcileToolSelectionSession()
	candidates := m.expandableToolEventIDs()
	if len(candidates) == 0 {
		m.clearToolSelection()
		return
	}
	currentID := m.selectedToolID
	if currentID == "" {
		currentID = m.expandedToolID
	}
	current := -1
	for index, id := range candidates {
		if id == currentID {
			current = index
			break
		}
	}
	// A recovered/exploration group can acquire a different representative
	// when more results arrive. Keep the selected group rather than jumping.
	if current < 0 && currentID != "" {
		for _, item := range m.buildChatItems() {
			if !toolItemContainsID(item, currentID) {
				continue
			}
			for index, id := range candidates {
				if toolItemContainsID(item, id) {
					current = index
					break
				}
			}
			break
		}
	}
	if current < 0 {
		current = len(candidates) - 1
	}
	wasExpanded := m.expandedToolID != ""
	current = max(0, min(len(candidates)-1, current+direction))
	m.selectedToolID = candidates[current]
	m.toolSelectionSession = m.sessionID
	if toggle {
		if wasExpanded {
			m.expandedToolID = ""
		} else {
			m.expandedToolID = m.selectedToolID
		}
	} else if wasExpanded {
		m.expandedToolID = m.selectedToolID
	}
	m.stickyBottom = false
	m.pendingToolScrollID = m.selectedToolID
	if m.chatList != nil {
		m.chatList.SetItemsKeepScroll(m.buildChatSurfaceItems()...)
		m.scrollToSelectedTool()
		// Reapply after View sizes the viewport and rebuilds its items.
		m.pendingToolScrollID = m.selectedToolID
	}
}

func (m *Model) scrollToSelectedTool() {
	if m.pendingToolScrollID == "" || m.chatList == nil {
		return
	}
	for index := 0; index < m.chatList.Len(); index++ {
		if toolItemContainsID(m.chatList.ItemAt(index), m.pendingToolScrollID) {
			// Anchor at the beginning, including the selected marker, even if
			// the expanded output is taller than the whole viewport.
			m.chatList.ScrollToIndex(index)
			break
		}
	}
	m.pendingToolScrollID = ""
}

func toolItemContainsID(item list.Item, id string) bool {
	if id == "" {
		return false
	}
	var events []ToolEvent
	switch typed := item.(type) {
	case *toolEventItem:
		return typed.te.ID == id && typed.te.Kind != "start"
	case *explorationGroupItem:
		events = typed.events
	case *recoveredErrorGroupItem:
		events = typed.events
	}
	for _, event := range events {
		if event.ID == id {
			return true
		}
	}
	return false
}

type toolNavigationVisual struct {
	enabled, selected, expanded bool
}

func (m *Model) decorateToolNavigation(items []list.Item) {
	found := false
	for _, item := range items {
		selected := toolItemContainsID(item, m.selectedToolID)
		found = found || selected
		visual := toolNavigationVisual{enabled: true, selected: selected}
		switch typed := item.(type) {
		case *toolEventItem:
			visual.expanded = typed.expand
			typed.navigation = visual
		case *explorationGroupItem:
			visual.expanded = typed.expand
			typed.navigation = visual
		case *recoveredErrorGroupItem:
			visual.expanded = typed.expand
			typed.navigation = visual
		}
	}
	if m.selectedToolID != "" && !found {
		m.clearToolSelection()
	}
}

var renderedToolShortcut = regexp.MustCompile(` ?\(ctrl\+O to [^)\n]+\)`)

// Rewrite only UI-owned muted spans. Plain command/output text that happens
// to mention the same shortcut is evidence and must remain byte-for-byte.
func toolNavigationHints(rendered string, selected bool) string {
	styled := styleMuted.Render("tool-hint-sentinel")
	prefix, suffix, ok := strings.Cut(styled, "tool-hint-sentinel")
	if !ok || prefix == "" || suffix == "" {
		return rendered
	}
	replacement := " (Alt+↑/↓)"
	if selected {
		replacement = ""
	}
	lines := strings.Split(rendered, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		var rewritten strings.Builder
		remaining, changed := line, false
		for {
			before, rest, found := strings.Cut(remaining, prefix)
			rewritten.WriteString(before)
			if !found {
				break
			}
			span, tail, closed := strings.Cut(rest, suffix)
			if !closed {
				rewritten.WriteString(prefix + rest)
				break
			}
			updated := renderedToolShortcut.ReplaceAllString(span, replacement)
			changed = changed || updated != span
			rewritten.WriteString(prefix + updated + suffix)
			remaining = tail
		}
		updated := rewritten.String()
		if changed && strings.TrimSpace(xansi.Strip(updated)) == "" {
			continue
		}
		out = append(out, updated)
	}
	return strings.Join(out, "\n")
}

func (v toolNavigationVisual) render(body string, width int) string {
	if !v.enabled {
		return body
	}
	body = toolNavigationHints(body, v.selected)
	if !v.selected {
		return body
	}
	action := "(ctrl+O to expand)"
	if v.expanded {
		action = "(ctrl+O to collapse)"
	}
	parts := []string{"> Selected tool", action, "Alt+↑/↓"}
	if width <= 0 {
		width = 80
	}
	var rows []string
	row := ""
	for _, part := range parts {
		if row != "" && xansi.StringWidth(row+" · "+part) <= width {
			row += " · " + part
			continue
		}
		if row != "" {
			rows = append(rows, row)
		}
		row = part
	}
	rows = append(rows, row)
	return styleAccent.Render(strings.Join(rows, "\n")) + "\n" + body
}
