package screen

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HelpTab is one tab in the /help screen. The caller (the tui package)
// builds the slice of tabs from the live REPLCommandRegistry + skill
// loader so the screen package doesn't need to import tui.
type HelpTab struct {
	Name string    // tab label, e.g. "general"
	Body []HelpRow // rendered top-to-bottom
}

// HelpRow is one line in a tab body. Either:
//   - A header (Heading != "") — bold colored title, NOT selectable.
//   - A command/value pair (Key starts with "/") — selectable; ↑↓ moves
//     the cursor onto it, Enter dispatches the command name.
//   - A free-form note (Heading == "" && Key not "/...") — muted, NOT
//     selectable.
type HelpRow struct {
	Heading string
	Key     string
	Value   string
}

// isSelectable reports whether the row is a command entry the user can
// pick with ↑/↓ + Enter. A row is selectable iff its Key starts with
// "/" — that's the exact contract the tui package's HelpRow producers
// (helpCommandsRows, helpCustomCommandsRows) use.
func (r HelpRow) isSelectable() bool {
	return strings.HasPrefix(r.Key, "/")
}

// HelpScreen is the tabbed /help modal mirroring claude-code's three-tab
// layout from images #7-9 in the user feedback. Beyond layout, it
// matches claude-code's selectable-row behavior: ↑/↓ moves a cursor
// (▸ marker) over command rows, Enter dispatches the highlighted
// command via Selected().
//
//   [/help]
//
//   metis vX.Y.Z   [general]   commands   custom-commands     (tabs row)
//
//     /agents     list sub-agents currently in flight (Agent tool)
//   ▸ /commit     git commit (-m 'message')                      ← cursor
//     /compact    force context compaction now
//
//   ← / →  switch tab  ·  ↑/↓ select  ·  Enter run  ·  Esc close
type HelpScreen struct {
	version string
	tabs    []HelpTab
	active  int // index into tabs
	cursor  int // index into current tab's Body — points at a selectable row
	scroll  int // line offset for viewport
	width   int
	height  int
	done    bool
	// selected captures the command name the user picked (Enter on a
	// selectable row). Empty when the user dismissed via Esc or Enter
	// on a non-selectable row.
	selected string
}

// NewHelpScreen builds a help modal with the given version label and
// pre-rendered tabs. Cursor is placed on the first selectable row of
// the first tab; tabs without any selectable rows leave cursor at 0
// (Enter no-ops).
func NewHelpScreen(version string, tabs []HelpTab) *HelpScreen {
	s := &HelpScreen{version: version, tabs: tabs}
	s.cursor = s.firstSelectable()
	return s
}

// Selected returns the command name (without leading slash) the user
// picked, or empty if Esc/cancel/no-pick.
func (s *HelpScreen) Selected() string { return s.selected }

func (s *HelpScreen) Init() tea.Cmd { return nil }

func (s *HelpScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.scrollToCursor()
}

func (s *HelpScreen) Done() bool { return s.done }

// bodyHeight reserves rows for header (1) + tabs row (1) + spacer (1)
// + footer hint (2) ≈ 5. Capped at helpMaxBody so the modal feels
// modal-sized even on huge terminals — claude-code's help doesn't
// stretch to fill the whole screen, and capping here gives the user a
// reason to actually scroll (without the cap, content fits the
// viewport on any reasonable terminal and ↑↓ silently no-op).
const helpMaxBody = 18

func (s *HelpScreen) bodyHeight() int {
	h := s.height - 5
	if h < 3 {
		h = 3
	}
	if h > helpMaxBody {
		h = helpMaxBody
	}
	return h
}

func (s *HelpScreen) currentTab() *HelpTab {
	if s.active < 0 || s.active >= len(s.tabs) {
		return nil
	}
	return &s.tabs[s.active]
}

func (s *HelpScreen) currentTabLen() int {
	t := s.currentTab()
	if t == nil {
		return 0
	}
	return len(t.Body)
}

// firstSelectable returns the index of the first selectable row in the
// current tab, or 0 if there are none.
func (s *HelpScreen) firstSelectable() int {
	t := s.currentTab()
	if t == nil {
		return 0
	}
	for i, r := range t.Body {
		if r.isSelectable() {
			return i
		}
	}
	return 0
}

// nextSelectable returns the index of the next selectable row at or
// after `from`. Returns the current cursor when none found (no-op).
func (s *HelpScreen) nextSelectable(from int) int {
	t := s.currentTab()
	if t == nil {
		return s.cursor
	}
	for i := from; i < len(t.Body); i++ {
		if t.Body[i].isSelectable() {
			return i
		}
	}
	return s.cursor
}

// prevSelectable returns the index of the previous selectable row at or
// before `from`. Returns the current cursor when none found.
func (s *HelpScreen) prevSelectable(from int) int {
	t := s.currentTab()
	if t == nil {
		return s.cursor
	}
	for i := from; i >= 0; i-- {
		if t.Body[i].isSelectable() {
			return i
		}
	}
	return s.cursor
}

// scrollToCursor adjusts s.scroll so the cursor row is in the viewport.
// Called after every cursor move.
func (s *HelpScreen) scrollToCursor() {
	bh := s.bodyHeight()
	if s.cursor < s.scroll {
		s.scroll = s.cursor
	}
	if s.cursor >= s.scroll+bh {
		s.scroll = s.cursor - bh + 1
	}
	s.clampScroll()
}

func (s *HelpScreen) clampScroll() {
	maxScroll := s.currentTabLen() - s.bodyHeight()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if s.scroll > maxScroll {
		s.scroll = maxScroll
	}
	if s.scroll < 0 {
		s.scroll = 0
	}
}

// switchTab moves to the new tab index (clamped to range), resets the
// cursor to the first selectable row, and resets scroll.
func (s *HelpScreen) switchTab(newActive int) {
	if newActive < 0 || newActive >= len(s.tabs) {
		return
	}
	s.active = newActive
	s.cursor = s.firstSelectable()
	s.scroll = 0
	s.scrollToCursor()
}

func (s *HelpScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyMsg:
		switch m.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			s.done = true
			return s, nil
		case tea.KeyEnter:
			// Dispatch the cursor's command. selected stays empty if
			// the cursor sits on a non-selectable row (eg. /general
			// tab where everything is heading + free-form).
			if t := s.currentTab(); t != nil && s.cursor >= 0 && s.cursor < len(t.Body) {
				row := t.Body[s.cursor]
				if row.isSelectable() {
					s.selected = strings.TrimPrefix(row.Key, "/")
				}
			}
			s.done = true
			return s, nil
		case tea.KeyLeft:
			s.switchTab(s.active - 1)
			return s, nil
		case tea.KeyRight:
			s.switchTab(s.active + 1)
			return s, nil
		case tea.KeyTab:
			if len(s.tabs) > 0 {
				s.switchTab((s.active + 1) % len(s.tabs))
			}
			return s, nil
		case tea.KeyUp:
			s.cursor = s.prevSelectable(s.cursor - 1)
			s.scrollToCursor()
			return s, nil
		case tea.KeyDown:
			s.cursor = s.nextSelectable(s.cursor + 1)
			s.scrollToCursor()
			return s, nil
		case tea.KeyPgUp:
			// Page up: jump cursor by ~half a page worth of selectable rows.
			for i := 0; i < s.bodyHeight()/2; i++ {
				next := s.prevSelectable(s.cursor - 1)
				if next == s.cursor {
					break
				}
				s.cursor = next
			}
			s.scrollToCursor()
			return s, nil
		case tea.KeyPgDown:
			for i := 0; i < s.bodyHeight()/2; i++ {
				next := s.nextSelectable(s.cursor + 1)
				if next == s.cursor {
					break
				}
				s.cursor = next
			}
			s.scrollToCursor()
			return s, nil
		case tea.KeyHome:
			s.cursor = s.firstSelectable()
			s.scrollToCursor()
			return s, nil
		case tea.KeyEnd:
			// Walk forward until the last selectable row.
			t := s.currentTab()
			if t != nil {
				for i := len(t.Body) - 1; i >= 0; i-- {
					if t.Body[i].isSelectable() {
						s.cursor = i
						break
					}
				}
			}
			s.scrollToCursor()
			return s, nil
		}
		switch m.String() {
		case "q":
			s.done = true
		case "h":
			s.switchTab(s.active - 1)
		case "l":
			s.switchTab(s.active + 1)
		case "j":
			s.cursor = s.nextSelectable(s.cursor + 1)
			s.scrollToCursor()
		case "k":
			s.cursor = s.prevSelectable(s.cursor - 1)
			s.scrollToCursor()
		case "g":
			s.cursor = s.firstSelectable()
			s.scrollToCursor()
		case "G":
			t := s.currentTab()
			if t != nil {
				for i := len(t.Body) - 1; i >= 0; i-- {
					if t.Body[i].isSelectable() {
						s.cursor = i
						break
					}
				}
			}
			s.scrollToCursor()
		}
	case tea.MouseMsg:
		switch m.Button {
		case tea.MouseButtonWheelUp:
			s.cursor = s.prevSelectable(s.cursor - 1)
			s.scrollToCursor()
		case tea.MouseButtonWheelDown:
			s.cursor = s.nextSelectable(s.cursor + 1)
			s.scrollToCursor()
		}
	}
	return s, nil
}

var (
	helpVerStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Bold(true)
	helpTabActive   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Background(lipgloss.Color("#5a4a78")).Bold(true).Padding(0, 1)
	helpTabInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0")).Padding(0, 1)
	helpHeading     = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	helpKey         = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd"))
	helpKeyActive   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	helpVal         = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	helpValActive   = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8")).Bold(true)
	helpMuted       = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	helpCursorMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
)

func (s *HelpScreen) View() string {
	var out strings.Builder

	// Header stripe + version next to it.
	out.WriteString(infoHeaderStripe.Render("/help"))
	if s.version != "" {
		out.WriteString("  ")
		out.WriteString(helpVerStyle.Render("metis " + s.version))
	}
	out.WriteString("\n")

	// Tabs row — active in highlight stripe, others muted.
	out.WriteString("  ")
	for i, t := range s.tabs {
		if i == s.active {
			out.WriteString(helpTabActive.Render(t.Name))
		} else {
			out.WriteString(helpTabInactive.Render(t.Name))
		}
	}
	out.WriteString("\n\n")

	// Body — current tab, scrolled by s.scroll lines. Each row is
	// rendered with cursor awareness so the active selectable row
	// gets the ▸ marker + bold styling.
	bh := s.bodyHeight()
	body := s.renderTabBody()
	end := s.scroll + bh
	if end > len(body) {
		end = len(body)
	}
	visible := body[s.scroll:end]
	if s.scroll > 0 && len(visible) > 0 {
		visible[0] = helpMuted.Render("↑ " + itoa(s.scroll) + " more above")
	}
	if end < len(body) && len(visible) > 0 {
		visible[len(visible)-1] = helpMuted.Render("↓ " + itoa(len(body)-end) + " more below")
	}
	for _, line := range visible {
		out.WriteString("  ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	for i := len(visible); i < bh; i++ {
		out.WriteString("\n")
	}

	// Footer hint adapts to capabilities. Selectable tabs (commands /
	// custom-commands) get the "Enter run" hint; tabs without
	// selectable rows (general) only get the close hint.
	canScroll := len(body) > bh
	canSwitchTab := len(s.tabs) > 1
	canPickRow := s.hasAnySelectable()
	parts := []string{}
	if canSwitchTab {
		parts = append(parts, "← / →  switch tab")
	}
	if canPickRow {
		parts = append(parts, "↑/↓  select  ·  Enter run")
	} else if canScroll {
		parts = append(parts, "↑/↓  scroll")
	}
	parts = append(parts, "Esc to close")
	out.WriteString(helpMuted.Render("  " + strings.Join(parts, "  ·  ")))

	return out.String()
}

// hasAnySelectable returns true iff the active tab has at least one
// selectable row (used by the footer hint to decide between "Enter run"
// vs plain "Esc to close").
func (s *HelpScreen) hasAnySelectable() bool {
	t := s.currentTab()
	if t == nil {
		return false
	}
	for _, r := range t.Body {
		if r.isSelectable() {
			return true
		}
	}
	return false
}

// renderTabBody turns the active tab's HelpRow slice into rendered
// strings (one per line). Computes column widths from the actual
// content so keys left-align across the panel. Selectable rows get a
// cursor-aware render (▸ marker + bold for the active row).
func (s *HelpScreen) renderTabBody() []string {
	t := s.currentTab()
	if t == nil {
		return nil
	}
	rows := t.Body
	keyW := 0
	for _, r := range rows {
		if w := lipgloss.Width(r.Key); w > keyW {
			keyW = w
		}
	}
	out := make([]string, 0, len(rows))
	for i, r := range rows {
		switch {
		case r.Heading != "":
			out = append(out, helpHeading.Render(r.Heading))
		case r.Key == "" && r.Value == "":
			out = append(out, "")
		case r.Key == "":
			out = append(out, helpMuted.Render(r.Value))
		default:
			pad := keyW - lipgloss.Width(r.Key)
			if pad < 0 {
				pad = 0
			}
			active := i == s.cursor && r.isSelectable()
			var line strings.Builder
			if active {
				line.WriteString(helpCursorMark.Render("▸ "))
			} else {
				line.WriteString("  ")
			}
			if active {
				line.WriteString(helpKeyActive.Render(r.Key))
			} else {
				line.WriteString(helpKey.Render(r.Key))
			}
			line.WriteString(strings.Repeat(" ", pad+2))
			if active {
				line.WriteString(helpValActive.Render(r.Value))
			} else {
				line.WriteString(helpVal.Render(r.Value))
			}
			out = append(out, line.String())
		}
	}
	return out
}
