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
//   - A header (Heading != "") rendered as a bold colored title,
//   - A command/value pair (Key != "") rendered "key   value",
//   - A free-form note (Heading == "" && Key == "") rendered muted.
type HelpRow struct {
	Heading string
	Key     string
	Value   string
}

// HelpScreen is the tabbed /help modal mirroring claude-code's three-tab
// layout from images #7-9 in the user feedback:
//
//   [/help]
//
//   metis vX.Y.Z   [general]   commands   custom-commands     (tabs row)
//
//   ── tab body ──                                            (scrollable)
//
//   ← / →  switch tab  ·  ↑/↓  scroll  ·  Esc to close
//
// Tab content is supplied at construction time so the screen stays
// import-clean (no skills / REPLCommandRegistry dependency leaking
// into the screen package).
type HelpScreen struct {
	version string
	tabs    []HelpTab
	active  int // index into tabs
	scroll  int
	width   int
	height  int
	done    bool
}

// NewHelpScreen builds a help modal with the given version label and
// pre-rendered tabs.
func NewHelpScreen(version string, tabs []HelpTab) *HelpScreen {
	return &HelpScreen{version: version, tabs: tabs}
}

func (s *HelpScreen) Init() tea.Cmd { return nil }

func (s *HelpScreen) Resize(width, height int) {
	s.width = width
	s.height = height
	s.clampScroll()
}

func (s *HelpScreen) Done() bool { return s.done }

// bodyHeight reserves rows for header (1) + tabs row (1) + spacer (1)
// + footer hint (2) ≈ 5. Capped at maxBodyHeight so the modal feels
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

func (s *HelpScreen) currentTabLen() int {
	if s.active < 0 || s.active >= len(s.tabs) {
		return 0
	}
	return len(s.tabs[s.active].Body)
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
		case tea.KeyLeft:
			if s.active > 0 {
				s.active--
				s.scroll = 0
			}
			return s, nil
		case tea.KeyRight:
			if s.active < len(s.tabs)-1 {
				s.active++
				s.scroll = 0
			}
			return s, nil
		case tea.KeyTab:
			s.active = (s.active + 1) % len(s.tabs)
			s.scroll = 0
			return s, nil
		case tea.KeyUp:
			s.scroll--
			s.clampScroll()
			return s, nil
		case tea.KeyDown:
			s.scroll++
			s.clampScroll()
			return s, nil
		case tea.KeyPgUp:
			s.scroll -= s.bodyHeight() / 2
			s.clampScroll()
			return s, nil
		case tea.KeyPgDown:
			s.scroll += s.bodyHeight() / 2
			s.clampScroll()
			return s, nil
		case tea.KeyHome:
			s.scroll = 0
			return s, nil
		case tea.KeyEnd:
			s.scroll = s.currentTabLen()
			s.clampScroll()
			return s, nil
		}
		switch m.String() {
		case "q":
			s.done = true
		case "h":
			if s.active > 0 {
				s.active--
				s.scroll = 0
			}
		case "l":
			if s.active < len(s.tabs)-1 {
				s.active++
				s.scroll = 0
			}
		case "j":
			s.scroll++
			s.clampScroll()
		case "k":
			s.scroll--
			s.clampScroll()
		case "g":
			s.scroll = 0
		case "G":
			s.scroll = s.currentTabLen()
			s.clampScroll()
		}
	case tea.MouseMsg:
		switch m.Button {
		case tea.MouseButtonWheelUp:
			s.scroll--
			s.clampScroll()
		case tea.MouseButtonWheelDown:
			s.scroll++
			s.clampScroll()
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
	helpVal         = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	helpMuted       = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
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

	// Body — current tab, scrolled by s.scroll lines.
	bh := s.bodyHeight()
	body := s.renderTabBody()
	end := s.scroll + bh
	if end > len(body) {
		end = len(body)
	}
	// "↑ N more above" indicator when scroll > 0. Same idea as
	// claude-code's image #9 ("↑ /claude-api"): tells the user there's
	// content above the viewport. Replaces the first body row when
	// shown so we don't lose render budget.
	visible := body[s.scroll:end]
	if s.scroll > 0 && len(visible) > 0 {
		visible[0] = helpMuted.Render("↑ " + itoa(s.scroll) + " more above")
	}
	if end < len(body) && len(visible) > 0 {
		// "↓ N more below" replaces the last visible row when content
		// overflows past the viewport. Mirrors claude-code's "↓ /agents".
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

	// Footer hint adapts to actual capabilities so it doesn't promise
	// scroll when content fits in the viewport.
	canScroll := len(body) > bh
	canSwitchTab := len(s.tabs) > 1
	parts := []string{}
	if canSwitchTab {
		parts = append(parts, "← / →  switch tab")
	}
	if canScroll {
		parts = append(parts, "↑/↓  scroll")
	}
	parts = append(parts, "Esc to close")
	out.WriteString(helpMuted.Render("  " + strings.Join(parts, "  ·  ")))

	return out.String()
}

// renderTabBody turns the active tab's HelpRow slice into rendered
// strings (one per line). Computes column widths from the actual
// content so keys left-align across the panel.
func (s *HelpScreen) renderTabBody() []string {
	if s.active < 0 || s.active >= len(s.tabs) {
		return nil
	}
	rows := s.tabs[s.active].Body
	keyW := 0
	for _, r := range rows {
		if w := lipgloss.Width(r.Key); w > keyW {
			keyW = w
		}
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
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
			out = append(out, helpKey.Render(r.Key)+strings.Repeat(" ", pad+2)+helpVal.Render(r.Value))
		}
	}
	return out
}
