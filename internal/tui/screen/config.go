package screen

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ConfigSetting is one safe scalar exposed by the /config panel. Values are
// supplied by the parent so this package never imports the runtime config
// package or sees provider credentials.
type ConfigSetting struct {
	Key             string
	Label           string
	Description     string
	Value           string
	EffectiveValue  string
	Options         []string
	LockedReason    string
	RestartRequired bool
}

// ConfigChange is a staged value returned only after the user applies.
type ConfigChange struct {
	Key   string
	Value string
}

type configFocus uint8

const (
	configFocusList configFocus = iota
	configFocusSearch
	configFocusEdit
)

// ConfigScreen is a clean-room settings browser: fuzzy search, arrow-driven
// previews, staged text edits, and an explicit apply/cancel boundary. It never
// writes disk itself; the parent validates and persists the returned changes.
type ConfigScreen struct {
	settings []ConfigSetting
	values   map[string]string
	filtered []int
	cursor   int
	scroll   int
	filter   string
	edit     string
	focus    configFocus
	width    int
	height   int
	done     bool
	applied  bool
	openEdit bool
	err      string
}

func NewConfigScreen(settings []ConfigSetting) *ConfigScreen {
	s := &ConfigScreen{
		settings: append([]ConfigSetting(nil), settings...),
		values:   make(map[string]string, len(settings)),
	}
	for i := range s.settings {
		s.settings[i].Options = append([]string(nil), settings[i].Options...)
		s.values[s.settings[i].Key] = s.settings[i].Value
	}
	s.refilter()
	return s
}

func (s *ConfigScreen) Init() tea.Cmd { return nil }

func (s *ConfigScreen) Resize(width, height int) {
	s.width, s.height = width, height
	s.scrollToCursor()
}

func (s *ConfigScreen) Done() bool { return s.done }

func (s *ConfigScreen) Applied() bool { return s.applied }

func (s *ConfigScreen) OpenEditorRequested() bool { return s.openEdit }

func (s *ConfigScreen) Changes() []ConfigChange {
	if !s.applied {
		return nil
	}
	changes := make([]ConfigChange, 0, len(s.settings))
	for _, setting := range s.settings {
		if setting.LockedReason != "" {
			continue
		}
		if value := s.values[setting.Key]; value != setting.Value {
			changes = append(changes, ConfigChange{Key: setting.Key, Value: value})
		}
	}
	return changes
}

func (s *ConfigScreen) visibleRows() int {
	h := s.height - 10
	if h < 3 {
		h = 3
	}
	if h > 16 {
		h = 16
	}
	return h
}

func (s *ConfigScreen) refilter() {
	s.filtered = s.filtered[:0]
	needle := strings.ToLower(strings.TrimSpace(s.filter))
	for i, setting := range s.settings {
		haystack := strings.ToLower(setting.Key + " " + setting.Label + " " + setting.Description + " " + s.values[setting.Key])
		if needle == "" || strings.Contains(haystack, needle) || fuzzyMatchString(haystack, needle) {
			s.filtered = append(s.filtered, i)
		}
	}
	if len(s.filtered) == 0 {
		s.cursor = 0
		s.scroll = 0
		return
	}
	if s.cursor >= len(s.filtered) {
		s.cursor = len(s.filtered) - 1
	}
	s.scrollToCursor()
}

func (s *ConfigScreen) scrollToCursor() {
	rows := s.visibleRows()
	if s.cursor < s.scroll {
		s.scroll = s.cursor
	}
	if s.cursor >= s.scroll+rows {
		s.scroll = s.cursor - rows + 1
	}
	maxScroll := len(s.filtered) - rows
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

func (s *ConfigScreen) selected() (ConfigSetting, bool) {
	if s.cursor < 0 || s.cursor >= len(s.filtered) {
		return ConfigSetting{}, false
	}
	return s.settings[s.filtered[s.cursor]], true
}

func (s *ConfigScreen) cycle(delta int) {
	setting, ok := s.selected()
	if !ok || len(setting.Options) == 0 {
		return
	}
	if setting.LockedReason != "" {
		s.err = setting.LockedReason
		return
	}
	current := s.values[setting.Key]
	idx := 0
	for i, option := range setting.Options {
		if option == current {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(setting.Options)) % len(setting.Options)
	s.values[setting.Key] = setting.Options[idx]
	s.err = ""
}

func (s *ConfigScreen) startEdit() {
	setting, ok := s.selected()
	if !ok || len(setting.Options) > 0 {
		return
	}
	if setting.LockedReason != "" {
		s.err = setting.LockedReason
		return
	}
	s.edit = s.values[setting.Key]
	s.focus = configFocusEdit
	s.err = ""
}

func (s *ConfigScreen) commitEdit() {
	setting, ok := s.selected()
	if !ok || strings.TrimSpace(s.edit) == "" {
		s.err = "value cannot be empty"
		return
	}
	s.values[setting.Key] = strings.TrimSpace(s.edit)
	s.focus = configFocusList
	s.err = ""
}

func (s *ConfigScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.MouseWheelMsg:
		if s.focus == configFocusList {
			if m.Button == tea.MouseWheelUp {
				s.move(-1)
			} else if m.Button == tea.MouseWheelDown {
				s.move(1)
			}
		}
		return s, nil
	case tea.KeyPressMsg:
		key := m.String()
		switch s.focus {
		case configFocusSearch:
			switch key {
			case "esc":
				s.filter = ""
				s.focus = configFocusList
				s.refilter()
			case "enter":
				s.focus = configFocusList
			case "backspace":
				s.filter = trimLastRune(s.filter)
				s.refilter()
			default:
				if m.Text != "" {
					s.filter += m.Text
					s.refilter()
				}
			}
			return s, nil
		case configFocusEdit:
			switch key {
			case "esc":
				s.edit = ""
				s.focus = configFocusList
				s.err = ""
			case "enter":
				s.commitEdit()
			case "backspace":
				s.edit = trimLastRune(s.edit)
			default:
				if m.Text != "" {
					s.edit += m.Text
				}
			}
			return s, nil
		}

		switch key {
		case "esc", "ctrl+c", "q":
			s.done = true
			return s, nil
		case "/":
			s.focus = configFocusSearch
			return s, nil
		case "enter":
			s.applied = true
			s.done = true
			return s, nil
		case "e":
			s.openEdit = true
			s.done = true
			return s, nil
		case "up", "k":
			s.move(-1)
		case "down", "j":
			s.move(1)
		case "left", "h":
			s.cycle(-1)
		case "right", "l", "space":
			s.cycle(1)
		case "home", "g":
			s.cursor = 0
			s.scrollToCursor()
		case "end", "G":
			if len(s.filtered) > 0 {
				s.cursor = len(s.filtered) - 1
			}
			s.scrollToCursor()
		case "i":
			s.startEdit()
		}
	}
	return s, nil
}

func (s *ConfigScreen) move(delta int) {
	if len(s.filtered) == 0 {
		return
	}
	s.cursor = (s.cursor + delta + len(s.filtered)) % len(s.filtered)
	s.scrollToCursor()
	s.err = ""
}

func trimLastRune(value string) string {
	if value == "" {
		return value
	}
	_, size := utf8.DecodeLastRuneInString(value)
	return value[:len(value)-size]
}

var (
	configTitleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	configMutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#787878"))
	configLabelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	configActiveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	configValueStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd"))
	configChangedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c")).Bold(true)
	configSearchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c")).Bold(true)
	configErrorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555"))
)

func (s *ConfigScreen) View() string {
	var out strings.Builder
	out.WriteString(infoHeaderStripe.Render("/config"))
	out.WriteString("\n\n  ")
	out.WriteString(configTitleStyle.Render("Settings"))
	if s.focus == configFocusSearch || s.filter != "" {
		out.WriteString(configMutedStyle.Render("  search: "))
		out.WriteString(configSearchStyle.Render(s.filter + "▏"))
	} else {
		out.WriteString(configMutedStyle.Render("  / to search"))
	}
	out.WriteString("\n\n")

	rows := s.visibleRows()
	end := s.scroll + rows
	if end > len(s.filtered) {
		end = len(s.filtered)
	}
	if len(s.filtered) == 0 {
		out.WriteString(configMutedStyle.Render("  No settings match this search."))
		out.WriteString("\n")
	}
	for pos := s.scroll; pos < end; pos++ {
		setting := s.settings[s.filtered[pos]]
		value := s.values[setting.Key]
		marker := "  "
		labelStyle := configLabelStyle
		if pos == s.cursor {
			marker = "▸ "
			labelStyle = configActiveStyle
		}
		out.WriteString("  " + marker)
		out.WriteString(labelStyle.Render(setting.Label))
		out.WriteString("  ")
		if value != setting.Value {
			out.WriteString(configChangedStyle.Render(value + "  modified"))
		} else {
			out.WriteString(configValueStyle.Render(value))
		}
		if setting.LockedReason != "" {
			out.WriteString(configMutedStyle.Render("  ·  " + setting.LockedReason))
		} else if setting.RestartRequired {
			out.WriteString(configMutedStyle.Render("  ·  restart required"))
		}
		out.WriteString("\n")
		if pos == s.cursor {
			out.WriteString("      ")
			out.WriteString(configMutedStyle.Render(setting.Description + "  ·  " + setting.Key))
			out.WriteString("\n")
			if setting.RestartRequired && setting.EffectiveValue != "" && value != setting.EffectiveValue {
				out.WriteString("      ")
				out.WriteString(configMutedStyle.Render("running: " + setting.EffectiveValue + "  ·  restart required for configured value"))
				out.WriteString("\n")
			}
			if setting.LockedReason != "" {
				out.WriteString("      ")
				out.WriteString(configMutedStyle.Render("read-only here  ·  " + setting.LockedReason))
				out.WriteString("\n")
			} else if s.focus == configFocusEdit {
				out.WriteString("      ")
				out.WriteString(configSearchStyle.Render("value: " + s.edit + "▏"))
				out.WriteString("\n")
			} else if len(setting.Options) > 0 {
				out.WriteString("      ")
				out.WriteString(configMutedStyle.Render("← / → preview: " + strings.Join(setting.Options, " · ")))
				out.WriteString("\n")
			} else {
				out.WriteString(configMutedStyle.Render("      i to edit value"))
				out.WriteString("\n")
			}
		}
	}
	for i := end - s.scroll; i < rows; i++ {
		out.WriteString("\n")
	}
	if s.err != "" {
		out.WriteString("\n  " + configErrorStyle.Render(s.err))
	}
	out.WriteString("\n\n")
	if s.focus == configFocusEdit {
		out.WriteString(configMutedStyle.Render("  Type value  ·  Enter stage  ·  Esc discard edit"))
	} else if s.focus == configFocusSearch {
		out.WriteString(configMutedStyle.Render("  Type to search  ·  Enter return to list  ·  Esc clear"))
	} else {
		out.WriteString(configMutedStyle.Render("  ↑/↓ select  ·  ←/→ preview  ·  i edit  ·  Enter apply all  ·  e open editor  ·  Esc cancel"))
	}
	return out.String()
}
