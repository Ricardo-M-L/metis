package tui

// external_editor.go — Phase D #41. Ctrl+G suspends bubbletea, opens
// the current input draft in $VISUAL / $EDITOR / vi, and folds the
// edited content back into the textarea on exit. Mirrors claude-code's
// "open the textarea in your real editor" affordance + Bash's classic
// `Ctrl+X Ctrl+E` workflow.
//
// Why a temp file (not a pipe): editors expect a file argument; pipes
// give the user a "stdin not a tty" diagnostic and refuse to start.
// The temp file lives under os.TempDir() with a `.md` extension so
// vim/nvim/VSCode pick the right syntax.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/config"
	runtimepkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// externalEditorDoneMsg flows back into Update once the editor exits.
// Carries the new textarea contents (or an error to surface as an
// info row).
type externalEditorDoneMsg struct {
	contents string
	err      error
	tmpPath  string // for cleanup
}

type configEditorDoneMsg struct{ err error }

type planEditorDoneMsg struct {
	path string
	err  error
}

// openExternalEditor returns a tea.Cmd that suspends the program,
// runs the editor, then re-enters with externalEditorDoneMsg. Caller
// (the keybind handler) returns this cmd from Update.
func (m *Model) openExternalEditor() tea.Cmd {
	tmpPath, err := writeDraftToTemp(m.input.Value())
	if err != nil {
		return func() tea.Msg {
			return externalEditorDoneMsg{err: fmt.Errorf("temp file: %w", err)}
		}
	}
	editor := pickEditor()
	cmd := exec.Command(editor, tmpPath)
	// Stdin/Stdout/Stderr default to the bubbletea-managed terminal —
	// ExecProcess restores it for us; we just need to leave them nil.
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		// Read back regardless of editor exit code: vim's `:cq` is a
		// non-zero exit but the file is still saved with the user's
		// edits, and many users expect that path.
		var contents string
		if data, rerr := os.ReadFile(tmpPath); rerr == nil {
			contents = string(data)
		} else if err == nil {
			err = fmt.Errorf("read back: %w", rerr)
		}
		return externalEditorDoneMsg{
			contents: contents,
			err:      err,
			tmpPath:  tmpPath,
		}
	})
}

// openConfigEditor suspends Bubble Tea before handing the terminal to the
// user's editor. Running the editor synchronously inside handleSubmit corrupts
// the alt-screen and used to report success even when the editor failed.
func (m *Model) openConfigEditor() tea.Cmd {
	path := filepath.Join(config.Home(), "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() tea.Msg { return configEditorDoneMsg{err: err} }
	}
	cmd := exec.Command(pickEditor(), path)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return configEditorDoneMsg{err: err} })
}

// openPlanEditor edits the stable session plan rather than a temporary input
// draft. The file remains available to bare `/plan` and future sessions after
// the editor exits.
func (m *Model) openPlanEditor() tea.Cmd {
	path, err := runtimepkg.EnsureCurrentPlan(m.sessionID)
	if err != nil {
		return func() tea.Msg { return planEditorDoneMsg{path: path, err: err} }
	}
	cmd := exec.Command(pickEditor(), path)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return planEditorDoneMsg{path: path, err: err}
	})
}

// writeDraftToTemp writes `body` to a fresh temp file with a .md
// extension. The .md picks up syntax highlighting in most editors;
// users routinely write multi-paragraph prompts so paragraph wrap is
// the right default.
func writeDraftToTemp(body string) (string, error) {
	dir := os.TempDir()
	stamp := time.Now().UnixNano()
	path := filepath.Join(dir, fmt.Sprintf("metis-draft-%d.md", stamp))
	// 0600 — drafts can contain pasted secrets / private context.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// applyExternalEditorResult merges the editor's saved contents back
// into the textarea. Trims a single trailing newline (editors of all
// stripes append one on save) so the textarea cursor lands at end-of-
// content, not on an empty line below it.
func (m *Model) applyExternalEditorResult(msg externalEditorDoneMsg) {
	defer func() {
		// Cleanup is best-effort — leaking a temp file is preferable
		// to losing the user's edit.
		if msg.tmpPath != "" {
			_ = os.Remove(msg.tmpPath)
		}
	}()
	if msg.err != nil {
		m.messages = append(m.messages, Message{
			Role:      "error",
			Content:   "external editor: " + msg.err.Error(),
			Timestamp: time.Now(),
		})
		return
	}
	body := msg.contents
	if n := len(body); n > 0 && body[n-1] == '\n' {
		body = body[:n-1]
	}
	m.input.SetValue(body)
	m.input.CursorEnd()
	// Quiet on success — the editor round-trip is the feedback. A
	// confirmation row would just clutter the chat.
}
