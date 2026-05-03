package screen

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// PermRule is one rule entry in the /permissions widget. Caller (tui pkg)
// converts permission.Rule into this shape so the screen package stays
// import-clean.
type PermRule struct {
	Verb   string // "allow" / "deny" / "ask"
	Match  string // tool name pattern (e.g. "Bash", "Read", "*")
	Source string // where the rule came from (config file path / "session")
}

// PermissionsScreen renders the active permission state as an interactive
// modal. Mode selection is editable (←→ cycles); rule list is read-only
// (↑↓ scrolls). Mirrors the dual-section layout claude-code uses for
// /permissions.
//
// Layout:
//
//	[/permissions]
//
//	  Mode:  ◀  auto  ▶          (interactive — Enter to apply mode)
//
//	  Rules (3):                   (read-only — use /allow to add)
//	    allow  Bash      session
//	    allow  Read      ~/.metis/config.toml
//	    deny   WebFetch  session
//
//	  ←/→ change mode  ·  Enter apply mode  ·  Esc close
type PermissionsScreen struct {
	modes       []string
	modeCursor  int
	initialMode string

	rules       []PermRule
	rulesScroll int

	width   int
	height  int
	done    bool
	applied string // chosen mode name, empty if cancelled
}

// NewPermissionsScreen builds the widget. `currentMode` is the active
// permission mode; rules is a snapshot of the gate's rules.
func NewPermissionsScreen(currentMode string, rules []PermRule) *PermissionsScreen {
	modes := []string{"ask", "auto", "bypass", "plan", "deny"}
	cur := 1 // auto default
	for i, m := range modes {
		if m == currentMode {
			cur = i
			break
		}
	}
	return &PermissionsScreen{
		modes:       modes,
		modeCursor:  cur,
		initialMode: currentMode,
		rules:       rules,
	}
}

// Applied returns the chosen mode (Enter), or empty if cancelled (Esc).
func (s *PermissionsScreen) Applied() string { return s.applied }

func (s *PermissionsScreen) Init() tea.Cmd { return nil }

func (s *PermissionsScreen) Resize(width, height int) {
	s.width = width
	s.height = height
}

func (s *PermissionsScreen) Done() bool { return s.done }

// rulesViewportHeight reserves rows for the chrome (header + mode row +
// rules header + footer ≈ 8).
func (s *PermissionsScreen) rulesViewportHeight() int {
	h := s.height - 8
	if h < 3 {
		h = 3
	}
	return h
}

func (s *PermissionsScreen) clampScroll() {
	maxScroll := len(s.rules) - s.rulesViewportHeight()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if s.rulesScroll > maxScroll {
		s.rulesScroll = maxScroll
	}
	if s.rulesScroll < 0 {
		s.rulesScroll = 0
	}
}

func (s *PermissionsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		// v2: collapsed two switches into one .String() match.
		switch m.String() {
		case "esc", "ctrl+c", "q":
			s.done = true
			return s, nil
		case "enter":
			s.applied = s.modes[s.modeCursor]
			s.done = true
			return s, nil
		case "left", "h":
			if n := len(s.modes); n > 0 {
				s.modeCursor = (s.modeCursor - 1 + n) % n
			}
			return s, nil
		case "right", "l":
			if n := len(s.modes); n > 0 {
				s.modeCursor = (s.modeCursor + 1) % n
			}
			return s, nil
		case "up", "k":
			s.rulesScroll--
			s.clampScroll()
			return s, nil
		case "down", "j":
			s.rulesScroll++
			s.clampScroll()
			return s, nil
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			s.rulesScroll--
			s.clampScroll()
		case tea.MouseWheelDown:
			s.rulesScroll++
			s.clampScroll()
		}
	}
	return s, nil
}

var (
	permModeLabel    = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	permModeArrow    = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	permModeCurrent  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true).Padding(0, 1).Background(lipgloss.Color("#5a4a78"))
	permRuleHeading  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	permVerbAllow    = lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b")).Bold(true)
	permVerbDeny     = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555")).Bold(true)
	permVerbAsk      = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c")).Bold(true)
	permRuleMatch    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	permRuleSource   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)
	permFooterStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	permEmptyHintFmt = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)
)

func (s *PermissionsScreen) View() string {
	var out strings.Builder

	// Header stripe.
	out.WriteString(infoHeaderStripe.Render("/permissions"))
	out.WriteString("\n\n")

	// Mode row — interactive cycle.
	out.WriteString("  ")
	out.WriteString(permModeLabel.Render("Mode: "))
	out.WriteString(permModeArrow.Render("◀ "))
	out.WriteString(permModeCurrent.Render(s.modes[s.modeCursor]))
	out.WriteString(permModeArrow.Render(" ▶"))
	if s.modes[s.modeCursor] == s.initialMode {
		out.WriteString("    ")
		out.WriteString(permRuleSource.Render("(unchanged)"))
	}
	out.WriteString("\n\n")

	// Rules section.
	if len(s.rules) == 0 {
		out.WriteString("  ")
		out.WriteString(permRuleHeading.Render("Rules (0)"))
		out.WriteString("\n\n")
		out.WriteString("  ")
		out.WriteString(permEmptyHintFmt.Render("(no explicit rules — falling back to mode default)"))
		out.WriteString("\n")
	} else {
		out.WriteString("  ")
		out.WriteString(permRuleHeading.Render("Rules (" + itoa(len(s.rules)) + ")"))
		out.WriteString("\n\n")

		// Compute column widths.
		matchW := 0
		for _, r := range s.rules {
			match := r.Match
			if match == "" {
				match = "*"
			}
			if w := lipgloss.Width(match); w > matchW {
				matchW = w
			}
		}

		bh := s.rulesViewportHeight()
		end := s.rulesScroll + bh
		if end > len(s.rules) {
			end = len(s.rules)
		}
		for i := s.rulesScroll; i < end; i++ {
			r := s.rules[i]
			match := r.Match
			if match == "" {
				match = "*"
			}
			source := r.Source
			if source == "" {
				source = "session"
			}
			out.WriteString("    ")
			// Verb in colored bold.
			switch r.Verb {
			case "allow":
				out.WriteString(permVerbAllow.Render("allow "))
			case "deny":
				out.WriteString(permVerbDeny.Render("deny  "))
			default:
				out.WriteString(permVerbAsk.Render("ask   "))
			}
			out.WriteString(permRuleMatch.Render(match))
			out.WriteString(strings.Repeat(" ", matchW-lipgloss.Width(match)+2))
			out.WriteString(permRuleSource.Render(source))
			out.WriteString("\n")
		}
	}

	// Footer hint.
	out.WriteString("\n")
	hint := "← / →  change mode  ·  Enter apply  ·  Esc close"
	if len(s.rules) > s.rulesViewportHeight() {
		hint = "← / →  mode  ·  ↑/↓  scroll rules  ·  Enter apply  ·  Esc close"
	}
	out.WriteString(permFooterStyle.Render("  " + hint))

	return out.String()
}
