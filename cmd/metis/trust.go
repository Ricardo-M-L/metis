package main

// trust.go implements claude-code's "trust this folder?" first-run
// safety check. metis can read/write/exec in the cwd; the prompt
// makes the user blink before letting an unknown directory through.
// Trusted dirs are persisted in ~/.metis/trusted-dirs.json so we
// only ask once per directory.
//
// The prompt uses bubbletea so the user picks with ↑↓+Enter (claude-code
// style) instead of typing "1" + Enter — matches the rest of the chat
// surface and avoids the boxed-in `Choose [1/2]: _` line that read as
// 1990s shell-script.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	worktreepkg "github.com/Ricardo-M-L/metis/internal/worktree"
)

// prepareChatWorkspace resolves --worktree before asking for trust, so the
// confirmation applies to the directory from which runtime config is loaded.
// New and reused worktrees both need their own confirmation; neither inherits
// the source checkout's trust. A nil callback skips prompting, not the policy.
func prepareChatWorkspace(flags *cliFlags, confirmTrust func() error) (*worktreepkg.Info, error) {
	var info *worktreepkg.Info
	if flags.worktree != "" || flags.worktreeOn {
		var err error
		info, err = worktreepkg.Spawn(flags.worktree)
		if err != nil {
			return nil, err
		}
		if err := os.Chdir(info.Path); err != nil {
			return nil, fmt.Errorf("chdir to worktree %s: %w", info.Path, err)
		}
		fmt.Fprintf(os.Stderr, "(worktree: %s on branch %s)\n", info.Path, info.Branch)
	}
	if confirmTrust != nil {
		if err := confirmTrust(); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// ensureTrusted prompts the user to confirm the cwd before launching
// the chat surface. Returns nil to proceed, error to abort. Skipped
// when:
//   - METIS_NO_TRUST_PROMPT=1 (CI / scripted)
//   - cwd is already in trusted-dirs.json
//   - stdin isn't a terminal (can't ask)
func ensureTrusted() error {
	if os.Getenv("METIS_NO_TRUST_PROMPT") == "1" {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil // best-effort: don't block on getwd failure
	}
	if isTrustedDir(cwd) {
		return nil
	}

	m := trustModel{cwd: cwd, choices: []string{"Yes, I trust this folder", "No, exit"}}
	prog := tea.NewProgram(&m)
	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("trust prompt: %w", err)
	}
	if !m.confirmed {
		return fmt.Errorf("aborted: directory not trusted")
	}
	return addTrustedDir(cwd)
}

// trustModel is a minimal bubbletea program for the one-shot trust
// prompt. We keep it inline (rather than in internal/tui) because it
// runs BEFORE the main chat surface initializes — pulling it through
// the chat-surface package would tangle startup ordering.
type trustModel struct {
	cwd       string
	choices   []string
	cursor    int
	confirmed bool
	cancelled bool
}

func (trustModel) Init() tea.Cmd {
	// Same first-frame blank-screen workaround as the chat surface (see
	// internal/tui Model.Init): bubbletea v2.0.6's startup GetSize can
	// return 0x0 under tmux before the pty size is negotiated, blanking
	// the first frame. Re-request the size shortly after start so the
	// trust prompt actually paints instead of looking hung.
	return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg {
		return tea.RequestWindowSize()
	})
}

func (m *trustModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// v2: KeyMsg is interface; switch on .String() for both named keys
	// and printable runes (replaces v1 .Type and .KeyRunes paths).
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "up", "shift+tab":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "tab":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}
		case "enter":
			m.confirmed = m.cursor == 0
			return m, tea.Quit
		case "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case "1", "y", "Y":
			m.confirmed = true
			return m, tea.Quit
		case "2", "n", "N":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *trustModel) View() tea.View {
	var (
		header   = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffd54f")).Bold(true)
		path     = lipgloss.NewStyle().Bold(true)
		body     = lipgloss.NewStyle().Foreground(lipgloss.Color("#cfd8dc"))
		muted    = lipgloss.NewStyle().Foreground(lipgloss.Color("#90a4ae"))
		cursor   = lipgloss.NewStyle().Foreground(lipgloss.Color("#82aaff")).Bold(true)
		selected = lipgloss.NewStyle().Foreground(lipgloss.Color("#82aaff"))
	)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#82aaff")).
		Padding(0, 1)

	var b []string
	b = append(b, "")
	b = append(b, header.Render("Accessing workspace:"))
	b = append(b, "")
	b = append(b, path.Render(m.cwd))
	b = append(b, "")
	b = append(b, body.Render("Quick safety check: is this a project you created or one you trust?"))
	b = append(b, body.Render("(Like your own code, a well-known open-source project, or work from"))
	b = append(b, body.Render("your team.) If not, take a moment to review what's in this folder first."))
	b = append(b, "")
	b = append(b, body.Render("Metis will be able to read, edit, and run shell commands here."))
	b = append(b, "")

	// Boxed selection — matches claude-code's bordered prompt with a
	// '>' marker on the cursor row.
	var lines []string
	for i, c := range m.choices {
		if i == m.cursor {
			lines = append(lines, cursor.Render("❯ ")+selected.Render(fmt.Sprintf("%d. %s", i+1, c)))
		} else {
			lines = append(lines, "  "+muted.Render(fmt.Sprintf("%d. ", i+1))+body.Render(c))
		}
	}
	b = append(b, box.Render(joinLines(lines)))
	b = append(b, "")
	b = append(b, muted.Render("↑↓ select · Enter to confirm · Esc to cancel"))
	b = append(b, "")
	return tea.NewView(joinLines(b))
}

// joinLines is a tiny helper that avoids the strings import (the rest
// of the file doesn't need it). Keeps the dep surface minimal so this
// pre-TUI prompt has a fast cold start.
func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		out += l
		if i < len(lines)-1 {
			out += "\n"
		}
	}
	return out
}

// trustedDirsPath returns the on-disk path for the persisted list.
// Honors METIS_HOME for portable installs.
func trustedDirsPath() string {
	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".metis")
	}
	return filepath.Join(home, "trusted-dirs.json")
}

func loadTrustedDirs() ([]string, error) {
	path := trustedDirsPath()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dirs []string
	if err := json.Unmarshal(data, &dirs); err != nil {
		return nil, err
	}
	return dirs, nil
}

func isTrustedDir(cwd string) bool {
	dirs, err := loadTrustedDirs()
	if err != nil {
		return false
	}
	for _, d := range dirs {
		if d == cwd {
			return true
		}
	}
	return false
}

func addTrustedDir(cwd string) error {
	dirs, _ := loadTrustedDirs()
	dirs = append(dirs, cwd)
	data, err := json.MarshalIndent(dirs, "", "  ")
	if err != nil {
		return err
	}
	path := trustedDirsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
