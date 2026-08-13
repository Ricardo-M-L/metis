package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	updateCheckTimeout   = 10 * time.Second
	updateCheckMaxOutput = 256 << 10
)

type updateCheckResultMsg struct {
	requestID uint64
	body      string
	err       error
}

type updateCheckRunner func(context.Context) (string, error)

// cappedUpdateBuffer bounds both stdout and stderr while preserving the
// io.Writer contract expected by os/exec. Update metadata is remote input, so
// an unexpectedly chatty or compromised endpoint cannot grow the TUI process
// without limit.
type cappedUpdateBuffer struct {
	buf       bytes.Buffer
	remaining int
	truncated bool
}

func (b *cappedUpdateBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.remaining > 0 {
		keep := len(p)
		if keep > b.remaining {
			keep = b.remaining
			b.truncated = true
		}
		_, _ = b.buf.Write(p[:keep])
		b.remaining -= keep
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedUpdateBuffer) String() string {
	body := strings.TrimSpace(b.buf.String())
	if b.truncated {
		body += "\n… (output truncated)"
	}
	return strings.TrimSpace(body)
}

// runMetisUpdateCheck is the only process boundary for `/update`. Callers
// supply a bounded context; output is capped before buffering and exit errors
// are returned instead of being silently discarded.
func runMetisUpdateCheck(ctx context.Context) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot resolve metis path: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, "update", "--check")
	captured := &cappedUpdateBuffer{remaining: updateCheckMaxOutput}
	cmd.Stdout = captured
	cmd.Stderr = captured
	err = cmd.Run()
	body := captured.String()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return body, fmt.Errorf("update check canceled or timed out: %w", ctxErr)
		}
		return body, fmt.Errorf("update check failed: %w", err)
	}
	if body == "" {
		body = "(/update: no output; run `metis update --check` for details)"
	}
	return body, nil
}

func formatUpdateCheckResult(body string, err error) string {
	body = safeTaskTerminalText(strings.TrimSpace(body), 50_000)
	if err == nil {
		return body
	}
	errText := safeTaskTerminalText(err.Error(), 2_000)
	if body == "" {
		return "/update: " + errText
	}
	return "/update: " + errText + "\n" + body
}

func renderUpgrade() string {
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	body, err := runMetisUpdateCheck(ctx)
	return formatUpdateCheckResult(body, err)
}

func (m *Model) startUpdateCheck() tea.Cmd {
	if m.updateCheckPending {
		m.messages = append(m.messages, Message{Role: "info", Content: "(update check already in progress)", Timestamp: time.Now()})
		return nil
	}
	m.updateCheckSeq++
	requestID := m.updateCheckSeq
	m.updateCheckPending = true
	m.messages = append(m.messages, Message{Role: "info", Content: "(checking for Metis updates…)", Timestamp: time.Now()})

	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	runner := m.updateCheckRunner
	if runner == nil {
		runner = runMetisUpdateCheck
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, updateCheckTimeout)
		defer cancel()
		body, err := runner(ctx)
		return updateCheckResultMsg{requestID: requestID, body: body, err: err}
	}
}

func (m *Model) handleUpdateCheckResult(msg updateCheckResultMsg) {
	if !m.updateCheckPending || msg.requestID != m.updateCheckSeq {
		return
	}
	m.updateCheckPending = false
	body := formatUpdateCheckResult(msg.body, msg.err)
	if msg.err != nil {
		role := "error"
		if errors.Is(msg.err, context.Canceled) || errors.Is(msg.err, context.DeadlineExceeded) {
			role = "warning"
		}
		m.messages = append(m.messages, Message{Role: role, Content: body, Timestamp: time.Now()})
		return
	}
	m.openBodyScreen("/update", body)
}
