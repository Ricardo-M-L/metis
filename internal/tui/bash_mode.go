package tui

// bash_mode.go — `!ls -la` and friends. Lets the user run shell
// commands inline without going through the LLM.
//
// claude-code calls this "BashMode" and triggers on a `!` prefix or a
// dedicated mode toggle; we use the prefix form because it composes with
// the existing slash-command parser (a leading character switches the
// dispatcher) and doesn't add a third "current mode" state to the Model.
//
// Output renders as a transcript row with role="bash" so users can scroll
// back through their shell history alongside the conversation. The agent
// loop never sees these lines (separate from `m.loop.Messages`), so token
// usage is zero — that's the entire point.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// bashLocalResultMsg carries the finished `!cmd` output back to the Update
// goroutine so it can be appended to the transcript on the main thread.
type bashLocalResultMsg struct{ row Message }

// bashLocalCmd runs cmd via the user's shell ASYNCHRONOUSLY (off the
// bubbletea Update goroutine) and returns its output as a bashLocalResultMsg.
// Synchronous execution here would freeze the entire UI for up to the bash
// timeout (`!sleep 60` hung the TUI). Short commands like ls/pwd are the
// intended use; long-running stuff should go through the Bash tool with
// streaming. Honours cfg.Tools.Bash settings (timeout / max_output_bytes /
// shell / denylist). Bash mode is NOT a permission-gate backdoor — a UX shortcut.
func (m *Model) bashLocalCmd(cmd string) tea.Cmd {
	settings := m.cfg.Tools.Bash
	timeout := time.Duration(settings.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxBytes := settings.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20 // 1 MiB
	}
	shell := settings.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	denylist := append([]string(nil), settings.Denylist...) // snapshot; closure must not touch m

	return func() tea.Msg {
		// Quick denylist check (mirrors a slice of internal/tools/builtin/bash).
		for _, deny := range denylist {
			if deny != "" && strings.Contains(cmd, deny) {
				return bashLocalResultMsg{Message{
					Role:      "bash-error",
					Content:   fmt.Sprintf("$ %s\n(refused: matches denylist entry %q)", cmd, deny),
					Timestamp: time.Now(),
				}}
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		start := time.Now()
		out, err := exec.CommandContext(ctx, shell, "-c", cmd).CombinedOutput()
		elapsed := time.Since(start).Truncate(time.Millisecond)

		if int64(len(out)) > int64(maxBytes) {
			out = append(out[:maxBytes], []byte(fmt.Sprintf("\n... (truncated at %d bytes)\n", maxBytes))...)
		}

		role := "bash"
		body := fmt.Sprintf("$ %s\n%s", cmd, strings.TrimRight(string(out), "\n"))
		if err != nil {
			role = "bash-error"
			body = fmt.Sprintf("%s\n(exit: %v, %s)", body, err, elapsed)
		} else {
			body = fmt.Sprintf("%s\n(%s)", body, elapsed)
		}
		return bashLocalResultMsg{Message{Role: role, Content: body, Timestamp: time.Now()}}
	}
}
