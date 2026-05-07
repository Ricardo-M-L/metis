package list

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Item represents a single item in the lazy-loaded list.
type Item interface {
	// Render returns the string representation of the item for the given
	// width.
	Render(width int) string
}

// RawRenderable represents an item that can provide a raw rendering
// without additional styling.
type RawRenderable interface {
	// RawRender returns the raw rendered string without any additional
	// styling.
	RawRender(width int) string
}

// Focusable represents an item that can be aware of focus state changes.
type Focusable interface {
	// SetFocused sets the focus state of the item.
	SetFocused(focused bool)
}

// Highlightable represents an item that can highlight a portion of its content.
// Defined for interface compatibility with crush's list package; metis chat
// surface does not currently implement range-highlight (would require
// porting highlight.go's ultraviolet cell-buffer logic).
type Highlightable interface {
	// SetHighlight highlights the content from the given start to end
	// positions. Use -1 for no highlight.
	SetHighlight(startLine, startCol, endLine, endCol int)
	// Highlight returns the current highlight positions within the item.
	Highlight() (startLine, startCol, endLine, endCol int)
}

// MouseClickable represents an item that can handle mouse click events.
// metis does not yet route per-item click events to list items; reserved
// for future click-to-expand / click-to-copy on transcript rows.
type MouseClickable interface {
	// HandleMouseClick processes a mouse click event at the given coordinates.
	// It returns true if the event was handled, false otherwise.
	HandleMouseClick(btn ansi.MouseButton, x, y int) bool
}

// NoSelectable is an opt-out marker. Items returning IsNoSelect() == true
// are excluded from text selection: drag-select skips them, double / triple-
// click ignores them, SelectedText() returns nothing for that row, and
// applySelectionToLine renders the row plain (no highlight even if the
// drag spanned across it). Use for separators, spinners, role headers,
// timestamps, ephemeral status lines — anything the user wouldn't want
// to copy-paste into another tool.
//
// Mirrors Claude Code's `noSelect` cell attribute (cell.ts:30) without
// requiring a per-cell flag — metis selects at item granularity, which
// is coarser but maps cleanly onto our row model where chrome and chat
// content already live in different items.
type NoSelectable interface {
	IsNoSelect() bool
}

// SpacerItem is a spacer item that adds vertical space in the list.
type SpacerItem struct {
	Height int
}

// IsNoSelect implements NoSelectable for SpacerItem — pure-whitespace
// rows have nothing meaningful to copy.
func (s *SpacerItem) IsNoSelect() bool { return true }

// NewSpacerItem creates a new [SpacerItem] with the specified height.
func NewSpacerItem(height int) *SpacerItem {
	return &SpacerItem{
		Height: max(0, height-1),
	}
}

// Render implements the Item interface for [SpacerItem].
func (s *SpacerItem) Render(width int) string {
	return strings.Repeat("\n", s.Height)
}
