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
)

// runBashLocal runs cmd via the user's shell and appends the output to
// the transcript. Synchronous (foreground) — short commands like ls/pwd
// are why this exists; long-running stuff should still go through the
// Bash tool with proper streaming and timeout enforcement.
//
// Honours cfg.Tools.Bash settings (timeout / max_output_bytes / shell)
// so a global denylist applies here too. Bash mode is NOT a backdoor
// around the permission gate — it's a UX shortcut.
func (m *Model) runBashLocal(cmd string) {
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

	// Quick denylist check (mirrors a slice of internal/tools/builtin/bash
	// without dragging that whole package in — we can't easily call the
	// Bash tool from here because it expects to be wired through the
	// agent dispatcher).
	for _, deny := range settings.Denylist {
		if deny != "" && strings.Contains(cmd, deny) {
			m.messages = append(m.messages, Message{
				Role:      "bash-error",
				Content:   fmt.Sprintf("$ %s\n(refused: matches denylist entry %q)", cmd, deny),
				Timestamp: time.Now(),
			})
			return
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
	m.messages = append(m.messages, Message{
		Role: role, Content: body, Timestamp: time.Now(),
	})
}
