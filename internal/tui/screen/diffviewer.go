package screen

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type DiffFile struct {
	Path   string
	Status string // "A" (added), "M" (modified), "D" (deleted), "R" (renamed)
	Hunks  []DiffHunk
}

type DiffHunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []DiffLine
}

type DiffLine struct {
	Type    string // "+", "-", " "
	Content string
	OldNum  int
	NewNum  int
}

// DiffSource is one selectable snapshot in the interactive diff viewer.
// Working tree is normally first; later entries represent individual turns.
type DiffSource struct {
	Label    string
	Subtitle string
	Files    []DiffFile
	Error    string
}

type DiffViewerScreen struct {
	sources  []DiffSource
	source   int
	files    []DiffFile
	cursor   int // file cursor
	scroll   int // line scroll within current file
	fileView int // 0 = file list, 1 = diff view
	width    int
	height   int
	done     bool
	// word diff highlight
	wordDiff bool
}

func NewDiffViewerScreen(files []DiffFile) *DiffViewerScreen {
	return NewDiffViewerScreenWithSources([]DiffSource{{Label: "Working tree", Files: files}})
}

func NewDiffViewerScreenWithSources(sources []DiffSource) *DiffViewerScreen {
	if len(sources) == 0 {
		sources = []DiffSource{{Label: "Working tree"}}
	}
	copySources := append([]DiffSource(nil), sources...)
	return &DiffViewerScreen{
		sources:  copySources,
		files:    copySources[0].Files,
		wordDiff: true,
	}
}

func (s *DiffViewerScreen) Init() tea.Cmd { return nil }

func (s *DiffViewerScreen) Resize(width, height int) {
	s.width = width
	s.height = height
}

func (s *DiffViewerScreen) Done() bool { return s.done }

func (s *DiffViewerScreen) totalDiffLines() int {
	if s.cursor < 0 || s.cursor >= len(s.files) {
		return 0
	}
	n := 0
	for _, hunk := range s.files[s.cursor].Hunks {
		n += len(hunk.Lines) + 1 // +1 for hunk header
	}
	return n
}

func (s *DiffViewerScreen) clampScroll() {
	max := s.totalDiffLines() - s.bodyHeight()
	if max < 0 {
		max = 0
	}
	if s.scroll > max {
		s.scroll = max
	}
	if s.scroll < 0 {
		s.scroll = 0
	}
}

func (s *DiffViewerScreen) scrollToFileCursor() {
	bh := s.bodyHeight()
	if s.cursor < s.scroll {
		s.scroll = s.cursor
	}
	if s.cursor >= s.scroll+bh {
		s.scroll = s.cursor - bh + 1
	}
	maxScroll := len(s.files) - bh
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

const diffMaxBody = 22

func (s *DiffViewerScreen) bodyHeight() int {
	h := s.height - 5
	if h < 3 {
		h = 3
	}
	if h > diffMaxBody {
		h = diffMaxBody
	}
	return h
}

func (s *DiffViewerScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.Resize(m.Width, m.Height)
		return s, nil
	case tea.KeyPressMsg:
		switch m.String() {
		case "esc", "ctrl+c", "q":
			if s.fileView == 1 {
				s.fileView = 0
				s.scroll = 0
				return s, nil
			}
			s.done = true
			return s, nil
		case "enter":
			if s.fileView == 0 {
				if s.cursor >= 0 && s.cursor < len(s.files) && len(s.files[s.cursor].Hunks) > 0 {
					s.fileView = 1
					s.scroll = 0
				}
				return s, nil
			}
		case "w":
			s.wordDiff = !s.wordDiff
			return s, nil
		case "left", "[", "shift+tab":
			s.changeSource(-1)
			return s, nil
		case "right", "]", "tab":
			s.changeSource(1)
			return s, nil
		case "up", "k":
			if s.fileView == 0 {
				if n := len(s.files); n > 0 {
					s.cursor = (s.cursor - 1 + n) % n
				}
				s.scrollToFileCursor()
			} else {
				s.scroll--
				s.clampScroll()
			}
			return s, nil
		case "down", "j":
			if s.fileView == 0 {
				if n := len(s.files); n > 0 {
					s.cursor = (s.cursor + 1) % n
				}
				s.scrollToFileCursor()
			} else {
				s.scroll++
				s.clampScroll()
			}
			return s, nil
		case "pgup":
			if s.fileView == 1 {
				s.scroll -= s.bodyHeight() / 2
				s.clampScroll()
			}
			return s, nil
		case "pgdown":
			if s.fileView == 1 {
				s.scroll += s.bodyHeight() / 2
				s.clampScroll()
			}
			return s, nil
		case "home", "g":
			if s.fileView == 0 {
				s.cursor = 0
				s.scrollToFileCursor()
			} else {
				s.scroll = 0
			}
			return s, nil
		case "end", "G":
			if s.fileView == 0 {
				s.cursor = len(s.files) - 1
				s.scrollToFileCursor()
			} else {
				s.scroll = s.totalDiffLines()
				s.clampScroll()
			}
			return s, nil
		case "n":
			// Next file.
			if s.fileView == 1 && s.cursor < len(s.files)-1 {
				s.cursor++
				s.scroll = 0
				return s, nil
			}
		case "p":
			// Previous file.
			if s.fileView == 1 && s.cursor > 0 {
				s.cursor--
				s.scroll = 0
				return s, nil
			}
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			if s.fileView == 0 {
				if n := len(s.files); n > 0 {
					s.cursor = (s.cursor - 1 + n) % n
				}
				s.scrollToFileCursor()
			} else {
				s.scroll--
				s.clampScroll()
			}
		case tea.MouseWheelDown:
			if s.fileView == 0 {
				if n := len(s.files); n > 0 {
					s.cursor = (s.cursor + 1) % n
				}
				s.scrollToFileCursor()
			} else {
				s.scroll++
				s.clampScroll()
			}
		}
	}
	return s, nil
}

func (s *DiffViewerScreen) changeSource(delta int) {
	if len(s.sources) < 2 {
		return
	}
	s.source = (s.source + delta + len(s.sources)) % len(s.sources)
	s.files = s.sources[s.source].Files
	s.cursor = 0
	s.scroll = 0
	s.fileView = 0
}

var (
	diffTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	diffCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	diffFileActive  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2")).Bold(true)
	diffFileIdle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	diffStatusA     = lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b")).Bold(true)
	diffStatusM     = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c")).Bold(true)
	diffStatusD     = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555")).Bold(true)
	diffStatusR     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Bold(true)
	diffHunkHeader  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)
	diffAddBg       = lipgloss.NewStyle().Background(lipgloss.Color("#213A2B"))
	diffDelBg       = lipgloss.NewStyle().Background(lipgloss.Color("#4A221D"))
	diffAddNum      = lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b"))
	diffDelNum      = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555"))
	diffContextNum  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	diffFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	diffCountStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd"))
)

func (s *DiffViewerScreen) statusStyle(status string) lipgloss.Style {
	switch status {
	case "A":
		return diffStatusA
	case "M":
		return diffStatusM
	case "D":
		return diffStatusD
	case "R":
		return diffStatusR
	default:
		return diffFileIdle
	}
}

func (s *DiffViewerScreen) flattenDiff() []string {
	if s.cursor < 0 || s.cursor >= len(s.files) {
		return nil
	}
	f := s.files[s.cursor]
	var out []string
	for _, hunk := range f.Hunks {
		header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", hunk.OldStart, hunk.OldCount, hunk.NewStart, hunk.NewCount)
		out = append(out, diffHunkHeader.Render(header))
		for _, line := range hunk.Lines {
			var b strings.Builder
			content := safeDiffDisplayText(line.Content, s.diffContentWidth())
			switch line.Type {
			case "+":
				b.WriteString(diffAddNum.Render(fmt.Sprintf("%4d ", line.NewNum)))
				b.WriteString(diffAddBg.Render("+" + content))
			case "-":
				b.WriteString(diffDelNum.Render(fmt.Sprintf("%4d ", line.OldNum)))
				b.WriteString(diffDelBg.Render("-" + content))
			default:
				b.WriteString(diffContextNum.Render(fmt.Sprintf("%4d ", line.NewNum)))
				b.WriteString(" " + content)
			}
			out = append(out, b.String())
		}
	}
	return out
}

func (s *DiffViewerScreen) View() string {
	var out strings.Builder

	out.WriteString(infoHeaderStripe.Render("diff"))
	if len(s.sources) > 0 {
		out.WriteString("  ")
		for index, source := range s.sources {
			if index > 0 {
				out.WriteString("  ")
			}
			label := " " + safeDiffDisplayText(source.Label, 24) + " "
			if index == s.source {
				out.WriteString(diffTitleStyle.Render("[" + label + "]"))
			} else {
				out.WriteString(diffFooterStyle.Render(label))
			}
		}
	}
	totalAdd, totalDel := 0, 0
	for _, f := range s.files {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Type == "+" {
					totalAdd++
				}
				if l.Type == "-" {
					totalDel++
				}
			}
		}
	}
	out.WriteString("  ")
	out.WriteString(diffTitleStyle.Render(fmt.Sprintf("%d files", len(s.files))))
	if totalAdd > 0 {
		out.WriteString("  ")
		out.WriteString(diffStatusA.Render(fmt.Sprintf("+%d", totalAdd)))
	}
	if totalDel > 0 {
		out.WriteString("  ")
		out.WriteString(diffStatusD.Render(fmt.Sprintf("-%d", totalDel)))
	}
	out.WriteString("\n")
	if s.source >= 0 && s.source < len(s.sources) && s.sources[s.source].Subtitle != "" {
		out.WriteString("  ")
		out.WriteString(diffFooterStyle.Render(safeDiffDisplayText(s.sources[s.source].Subtitle, s.diffContentWidth())))
		out.WriteString("\n")
	}
	out.WriteString("\n")
	if len(s.files) == 0 {
		out.WriteString("  ")
		if s.source >= 0 && s.source < len(s.sources) && s.sources[s.source].Error != "" {
			out.WriteString(diffStatusD.Render("Unable to load working tree changes: "))
			out.WriteString(diffFooterStyle.Render(safeDiffDisplayText(s.sources[s.source].Error, s.diffContentWidth())))
		} else if s.source == 0 {
			out.WriteString(diffFooterStyle.Render("Working tree clean — no uncommitted changes."))
		} else {
			out.WriteString(diffFooterStyle.Render("No reconstructable file changes for this turn."))
		}
		out.WriteString("\n")
	}

	if s.fileView == 0 {
		// File list.
		bh := s.bodyHeight()
		end := s.scroll + bh
		if end > len(s.files) {
			end = len(s.files)
		}
		for i := s.scroll; i < end; i++ {
			f := s.files[i]
			path := safeDiffDisplayText(f.Path, s.diffPathWidth())
			var line strings.Builder
			if i == s.cursor {
				line.WriteString(diffCursorStyle.Render("▸ "))
				line.WriteString(s.statusStyle(f.Status).Render("[" + f.Status + "] "))
				line.WriteString(diffFileActive.Render(path))
			} else {
				line.WriteString("  ")
				line.WriteString(s.statusStyle(f.Status).Render("[" + f.Status + "] "))
				line.WriteString(diffFileIdle.Render(path))
			}
			// File stats.
			adds, dels := 0, 0
			for _, h := range f.Hunks {
				for _, l := range h.Lines {
					if l.Type == "+" {
						adds++
					}
					if l.Type == "-" {
						dels++
					}
				}
			}
			if adds > 0 || dels > 0 {
				line.WriteString("  ")
				if adds > 0 {
					line.WriteString(diffStatusA.Render(fmt.Sprintf("+%d", adds)))
				}
				if dels > 0 {
					line.WriteString("  ")
					line.WriteString(diffStatusD.Render(fmt.Sprintf("-%d", dels)))
				}
			}
			out.WriteString("  ")
			out.WriteString(line.String())
			out.WriteString("\n")
		}
		for i := end - s.scroll; i < bh; i++ {
			out.WriteString("\n")
		}
	} else {
		// Diff view.
		diff := s.flattenDiff()
		bh := s.bodyHeight()
		end := s.scroll + bh
		if end > len(diff) {
			end = len(diff)
		}
		visible := diff[s.scroll:end]
		if s.scroll > 0 && len(visible) > 0 {
			visible[0] = diffFooterStyle.Render("↑ " + itoa(s.scroll) + " lines above")
		}
		if end < len(diff) && len(visible) > 0 {
			visible[len(visible)-1] = diffFooterStyle.Render("↓ " + itoa(len(diff)-end) + " lines below")
		}
		for _, line := range visible {
			out.WriteString("  ")
			out.WriteString(line)
			out.WriteString("\n")
		}
		for i := len(visible); i < bh; i++ {
			out.WriteString("\n")
		}
	}

	// Footer.
	out.WriteString("\n")
	if s.fileView == 0 {
		hints := []string{"←/→ or Tab source", "↑/↓ select", "Enter view diff", "Esc close"}
		out.WriteString(diffFooterStyle.Render("  " + strings.Join(hints, "  ·  ")))
	} else {
		hints := []string{"↑/↓ scroll", "n/p next/prev file", "Esc back", "q close"}
		if s.wordDiff {
			hints = append(hints[:2], hints[2:]...)
		}
		out.WriteString(diffFooterStyle.Render("  " + strings.Join(hints, "  ·  ")))
	}

	return out.String()
}

func (s *DiffViewerScreen) diffPathWidth() int {
	width := s.width - 24 // cursor, status, indentation, and file stats
	if width <= 0 {
		width = 56
	}
	if width > 120 {
		width = 120
	}
	return width
}

func (s *DiffViewerScreen) diffContentWidth() int {
	width := s.width - 10 // outer indent, line number, and diff marker
	if width <= 0 {
		width = 70
	}
	if width > 200 {
		width = 200
	}
	return width
}

// safeDiffDisplayText is the terminal boundary for repository-controlled file
// names and contents. It leaves ordinary Unicode intact, makes all control
// runes visible (so CSI/OSC/BEL cannot execute), and bounds the rendered cell
// width without rewriting the underlying DiffFile/DiffLine values.
func safeDiffDisplayText(raw string, maxWidth int) string {
	var b strings.Builder
	for _, r := range raw {
		switch r {
		case '\t':
			b.WriteString("⇥")
		case '\n':
			b.WriteString("⏎")
		case '\r':
			b.WriteString("␍")
		case '\x1b':
			b.WriteString(`\x1b`)
		default:
			if unicode.IsControl(r) {
				if r <= 0xff {
					fmt.Fprintf(&b, `\x%02x`, r)
				} else {
					fmt.Fprintf(&b, `\u{%x}`, r)
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	clean := b.String()
	if maxWidth <= 0 || lipgloss.Width(clean) <= maxWidth {
		return clean
	}
	var clipped strings.Builder
	width := 0
	for _, r := range clean {
		runeWidth := lipgloss.Width(string(r))
		if width+runeWidth+1 > maxWidth {
			break
		}
		clipped.WriteRune(r)
		width += runeWidth
	}
	clipped.WriteRune('…')
	return clipped.String()
}
