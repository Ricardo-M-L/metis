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
func toolNavigationHints(rendered string) string {
	styled := styleMuted.Render("tool-hint-sentinel")
	prefix, suffix, ok := strings.Cut(styled, "tool-hint-sentinel")
	if !ok || prefix == "" || suffix == "" {
		return rendered
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
			updated := renderedToolShortcut.ReplaceAllString(span, "")
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
	body = toolNavigationHints(body)
	if !v.selected {
		return body
	}
	if width <= 0 {
		width = 80
	}
	rows := strings.Split(body, "\n")
	if len(rows) == 0 || strings.TrimSpace(xansi.Strip(rows[0])) == "" {
		return body
	}
	// Consume the existing two-cell leader padding. The success/error glyph,
	// its colour, the tool title, and nested-tool indentation keep their slots.
	header := rows[0]
	plain := xansi.Strip(header)
	padding := len(plain) - len(strings.TrimLeft(plain, " "))
	if padding >= 2 {
		markerAt := padding - 2
		rows[0] = xansi.Cut(header, 0, markerAt) + styleAccent.Render("› ") +
			xansi.Cut(header, padding, xansi.StringWidth(header))
	}

	// Prefer the existing result/elapsed row. An expanded command may place
	// that row far below the header; in that case keep the shortcut reachable
	// beside the header instead of behind many screens of command text.
	auxiliary := -1
	for index := 1; index < len(rows) && index <= 2; index++ {
		if toolNavigationAuxiliaryRow(rows[index]) {
			// A failed summary can continue over several physical rows. Keep
			// that explanation contiguous instead of guessing where its text
			// ends from indentation, which real output can also contain.
			if strings.Contains(xansi.Strip(rows[index]), "✗ ") {
				break
			}
			auxiliary = index
			break
		}
	}
	insertAt, auxiliaryText := 1, ""
	if auxiliary >= 0 {
		insertAt, auxiliaryText = auxiliary, rows[auxiliary]
	}
	controls := toolNavigationControlRows(auxiliaryText, width, padding+5, v.expanded)
	end := insertAt
	if auxiliary >= 0 {
		end++
	}
	out := make([]string, 0, len(rows)+len(controls))
	out = append(out, rows[:insertAt]...)
	out = append(out, controls...)
	out = append(out, rows[end:]...)
	return strings.Join(out, "\n")
}

// The renderer owns this dim tree prefix. Do not identify auxiliary rows from
// plain text alone: actual tool output can contain the same glyph or wording.
func toolNavigationAuxiliaryRow(row string) bool {
	styled := styleDim.Render("tool-aux-sentinel")
	prefix, suffix, ok := strings.Cut(styled, "tool-aux-sentinel")
	if !ok || prefix == "" || suffix == "" {
		return false
	}
	row = strings.TrimLeft(row, " ") // outer indentation of a sub-agent block
	span, ok := strings.CutPrefix(row, prefix)
	if !ok {
		return false
	}
	span, _, closed := strings.Cut(span, suffix)
	return closed && strings.HasPrefix(strings.TrimLeft(span, " "), glyphTreeLeaf)
}

func toolNavigationControlRows(auxiliary string, width, indent int, expanded bool) []string {
	action := "ctrl+O expand"
	if expanded {
		action = "ctrl+O collapse"
	}
	parts := []string{action, "Alt+↑/↓"}
	indent = min(indent, max(0, width-xansi.StringWidth(action)))
	padding := strings.Repeat(" ", indent)
	var rows []string
	row := auxiliary
	for _, part := range parts {
		if row != "" && xansi.StringWidth(row)+3+xansi.StringWidth(part) <= width {
			row += styleMuted.Render(" · " + part)
			continue
		}
		if row != "" {
			rows = append(rows, row)
		}
		// Hardwrap matters only below the supported 24-column narrow view.
		// Never clip a key name or consume any of the original output bytes.
		wrapped := xansi.Hardwrap(part, max(1, width-indent), true)
		row = styleMuted.Render(padding + strings.ReplaceAll(wrapped, "\n", "\n"+padding))
	}
	if row != "" {
		rows = append(rows, row)
	}
	return rows
}
