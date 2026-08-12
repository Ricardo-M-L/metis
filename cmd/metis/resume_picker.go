package main

// cmd_resume_picker.go — `metis -r` / bare `--resume` opens a small
// bubbletea picker over recent sessions. Mirrors claude-code's
// `ResumeConversation` / `LogSelector`: arrow-key navigation, full
// session id + title shown per row, Enter to select, Esc/q to abort.
//
// History note: the picker used to be a printf+Scan numbered prompt
// (cmd_resume_picker.go @ 6f1a05a). That had two problems the user
// reported on 2026-05-13:
//
//   1. Session ids were truncated to 12 chars for display, so users
//      copying the displayed id into `--resume <id>` got ENOENT.
//   2. Numbered input is fiddly for >10 sessions and feels dated next
//      to claude-code's arrow-key UX.
//
// Both problems are fixed by going TUI: the full UUID is rendered
// inline (no truncation surprise), and the user picks by moving a
// cursor. The TUI program is short-lived — it runs to completion
// before the main chat surface boots, same lifecycle as trust.go's
// safety prompt.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/session"
)

var errResumePickerCancelled = errors.New("resume picker cancelled")

// liftBareResume walks `args` and removes any `-r` / `--resume` token
// that's NOT followed by a value. Sets *pick=true when it removes one.
// Multi-form input is handled:
//
//	metis -r            → bare → pick=true
//	metis -r -c         → bare → pick=true (next arg is another flag)
//	metis -r abc-123    → has value → left intact for flag.Parse
//	metis --resume xyz  → has value → left intact
//	metis --resume      → bare → pick=true
//	metis --resume=xyz  → has value (=-form) → left intact
func liftBareResume(args []string, pick *bool) []string {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		isResume := a == "-r" || a == "--resume"
		if isResume {
			next := ""
			if i+1 < len(args) {
				next = args[i+1]
			}
			// Bare when there's no next token, OR when the next token
			// looks like a flag (starts with `-`).
			if next == "" || strings.HasPrefix(next, "-") {
				*pick = true
				i++ // skip the lone -r
				continue
			}
		}
		out = append(out, a)
		i++
	}
	return out
}

// runResumePicker opens a bubbletea selector over recent sessions and
// returns the full session id the user chose. Cancelling is distinct from
// starting fresh: merely opening `metis -r` must never create a new session.
func runResumePicker(store *session.Store) (string, error) {
	const limit = 200
	cwd, _ := os.Getwd()
	entries, err := store.ListResumable(session.ResumeListOptions{Limit: limit, WorkDir: cwd})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "(no resumable sessions for this directory)")
		return "", errResumePickerCancelled
	}

	m := &resumePickerModel{entries: entries, viewSize: 15}
	// Output to stderr so a piped stdin/stdout caller (CI, eval harness)
	// still sees the picker chrome instead of mixing it into structured
	// output. Same trick trust.go uses.
	prog := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	if _, err := prog.Run(); err != nil {
		// Terminal too dumb to host bubbletea (no PTY, tests). Fall
		// back to the legacy numbered prompt so the path still works.
		return runResumePickerFallback(store, entries)
	}
	if m.cancelled || m.cursor < 0 {
		fmt.Fprintln(os.Stderr, "(resume cancelled)")
		return "", errResumePickerCancelled
	}
	return m.entries[m.cursor].ID, nil
}

// resumePickerModel renders a vertical list of session rows with a
// movable cursor. The viewport (`viewSize` rows wide) auto-scrolls to
// keep the cursor visible, so a list of 200 sessions degrades to "scroll
// nicely" rather than "renders past the terminal bottom".
type resumePickerModel struct {
	entries   []session.ListEntry
	cursor    int
	offset    int // first visible row
	viewSize  int // rows shown at once
	width     int // last-known terminal width (for row truncation)
	cancelled bool
}

func (resumePickerModel) Init() tea.Cmd { return nil }

func (m *resumePickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		// Leave 6 rows for chrome (header + footer + padding). Always
		// keep at least 5 visible rows so the picker stays usable in
		// very short windows.
		avail := msg.Height - 6
		if avail < 5 {
			avail = 5
		}
		if avail < m.viewSize {
			m.viewSize = avail
		}
		m.clampScroll()
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k", "shift+tab":
			if m.cursor > 0 {
				m.cursor--
				m.clampScroll()
			}
		case "down", "j", "tab":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
				m.clampScroll()
			}
		case "pgup", "ctrl+u":
			m.cursor -= m.viewSize
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.clampScroll()
		case "pgdown", "ctrl+d":
			m.cursor += m.viewSize
			if m.cursor >= len(m.entries) {
				m.cursor = len(m.entries) - 1
			}
			m.clampScroll()
		case "home", "g":
			m.cursor = 0
			m.clampScroll()
		case "end", "G":
			m.cursor = len(m.entries) - 1
			m.clampScroll()
		case "enter":
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		default:
			// Number shortcut (claude-code parity): 1-9 jump to row N
			// within the current viewport AND select. Two-digit input
			// not supported — for that, scroll then Enter.
			if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= 9 {
				idx := m.offset + n - 1
				if idx < len(m.entries) {
					m.cursor = idx
					return m, tea.Quit
				}
			}
		}
	}
	return m, nil
}

// clampScroll keeps `m.offset` aligned so the cursor is always visible.
// Called after any cursor move OR window-size change.
func (m *resumePickerModel) clampScroll() {
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+m.viewSize {
		m.offset = m.cursor - m.viewSize + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *resumePickerModel) View() tea.View {
	var (
		header   = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffd54f")).Bold(true)
		cursor   = lipgloss.NewStyle().Foreground(lipgloss.Color("#82aaff")).Bold(true)
		selected = lipgloss.NewStyle().Foreground(lipgloss.Color("#82aaff")).Bold(true)
		idCol    = lipgloss.NewStyle().Foreground(lipgloss.Color("#c792ea"))
		titleCol = lipgloss.NewStyle()
		muted    = lipgloss.NewStyle().Foreground(lipgloss.Color("#90a4ae"))
		dim      = lipgloss.NewStyle().Foreground(lipgloss.Color("#5c6370"))
	)

	var b []string
	b = append(b, "")
	b = append(b, header.Render(fmt.Sprintf("Resume session  (%d of %d)", m.cursor+1, len(m.entries))))
	b = append(b, dim.Render("  ─────────────────────────────────────────────────────────"))

	end := m.offset + m.viewSize
	if end > len(m.entries) {
		end = len(m.entries)
	}
	for i := m.offset; i < end; i++ {
		e := m.entries[i]
		title := e.Title
		if title == "" {
			title = "(untitled)"
		}
		date := resumeEntryTime(e).Format("2006-01-02 15:04")
		// Compose the row. Format:
		//   ❯ <full-uuid>  <date>  <title>
		// Truncate the title rather than the id so the user can always
		// copy/paste the full id from the row they have under the
		// cursor.
		row := fmt.Sprintf("%s  %s  %s", idCol.Render(e.ID), muted.Render(date), titleCol.Render(title))
		if i == m.cursor {
			b = append(b, cursor.Render("❯ ")+selected.Render(stripStyles(row)))
		} else {
			b = append(b, "  "+row)
		}
	}
	if m.offset > 0 {
		b = append(b, dim.Render(fmt.Sprintf("  ↑ %d more above", m.offset)))
	}
	if end < len(m.entries) {
		b = append(b, dim.Render(fmt.Sprintf("  ↓ %d more below", len(m.entries)-end)))
	}
	b = append(b, "")
	b = append(b, muted.Render("  ↑↓/jk · PgUp/PgDn · g/G · 1-9 quick-pick · Enter resume · Esc cancel"))
	b = append(b, "")
	return tea.NewView(strings.Join(b, "\n"))
}

// stripStyles strips lipgloss escape sequences from a pre-styled row so
// the row can be re-rendered under the selected style without nested
// escapes (which lipgloss handles, but the nested form sometimes drops
// trailing styles). Cheap regex-free strip — the ANSI form is fixed.
func stripStyles(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// Skip until the final letter of the ANSI sequence.
			j := i + 2
			for j < len(s) && !((s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= 'a' && s[j] <= 'z')) {
				j++
			}
			if j < len(s) {
				i = j
			}
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// runResumePickerFallback is the legacy printf+Scan path used when
// bubbletea can't start (no PTY, harness/CI without a tty). Same
// behaviour the picker had before 2026-05-13: numbered list,
// `enter the number` prompt. Kept tight: 8 rows max so the
// fallback stays terse.
func runResumePickerFallback(store *session.Store, entries []session.ListEntry) (string, error) {
	const limit = 8
	if len(entries) > limit {
		entries = entries[:limit]
	}
	fmt.Fprintln(os.Stderr, "Resume which session?")
	for i, e := range entries {
		title := e.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(os.Stderr, "  %2d. %s  %s  %s\n",
			i+1, e.ID, resumeEntryTime(e).Format("2006-01-02 15:04"), title)
	}
	fmt.Fprint(os.Stderr, "Pick number (or `q` / Enter to cancel): ")
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return "", errResumePickerCancelled
	}
	line = strings.TrimSpace(line)
	if line == "" || line == "q" || line == "Q" || line == "0" {
		return "", errResumePickerCancelled
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(entries) {
		fmt.Fprintf(os.Stderr, "(invalid choice %q — resume cancelled)\n", line)
		return "", errResumePickerCancelled
	}
	return entries[n-1].ID, nil
}

// short12 is preserved as an exported helper for callers that still
// want a compact representation (logs, status bar). Picker rows no
// longer use it.
func short12(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Compile-time sanity that time.Time stays imported (we use it via
// session.ListEntry.CreatedAt above).
var _ = time.Time{}

func resumeEntryTime(entry session.ListEntry) time.Time {
	if !entry.UpdatedAt.IsZero() {
		return entry.UpdatedAt
	}
	return entry.CreatedAt
}
