package tui

// keybind_session.go — session-level helpers triggered from key events:
// Ctrl+P session picker, Ctrl+S copy mode, /undo summary, asREPL bridge
// to the REPL command surface.

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) handleSessionPick() (tea.Model, tea.Cmd) {
	if m.session == nil {
		m.messages = append(m.messages, Message{Role: "info", Content: "(no session store)", Timestamp: time.Now()})
		return m, nil
	}
	sessions, err := m.session.List(10)
	if err != nil {
		m.messages = append(m.messages, Message{Role: "error", Content: "error: " + err.Error(), Timestamp: time.Now()})
		return m, nil
	}
	if len(sessions) == 0 {
		m.messages = append(m.messages, Message{Role: "info", Content: "(no saved sessions)", Timestamp: time.Now()})
		return m, nil
	}
	var b strings.Builder
	b.WriteString("recent sessions:\n")
	for _, s := range sessions {
		prefix := "  "
		if s.ID == m.sessionID {
			prefix = "▶ "
		}
		b.WriteString(fmt.Sprintf("%s%s  %s\n", prefix, s.ID[:8], s.CreatedAt.Format("01-02 15:04")))
	}
	m.messages = append(m.messages, Message{Role: "info", Content: b.String(), Timestamp: time.Now()})
	return m, nil
}

// toggleCopyMode flips the alt-screen on/off so the user can natively
// select and copy chat content. Entering: print the current transcript
// to stdout (visible in scrollback) then exit alt-screen. Exiting:
// re-enter alt-screen, the chat redraws fresh.
//
// v2: tea.EnterAltScreen / tea.ExitAltScreen Cmds are gone. Alt-screen
// state is declared via tea.View.AltScreen each frame; View() reads
// m.copyMode and returns AltScreen=false when set, so toggling the
// flag is sufficient — the next View() call reflects the change.
func (m *Model) toggleCopyMode() (tea.Model, tea.Cmd) {
	if m.copyMode {
		m.copyMode = false
		return m, nil
	}
	m.copyMode = true
	// Dump a plain transcript so the user has SOMETHING to select
	// after we leave alt-screen — otherwise the post-alt screen
	// shows nothing related to metis. Strip ANSI for clean copy.
	fmt.Println(m.buildPlainTranscript())
	fmt.Println("[copy mode — Ctrl+S to return to chat]")
	return m, nil
}

// buildPlainTranscript renders messages + tool events as a single
// text blob suitable for terminal scrollback (no ANSI escapes, no
// rich rendering). Used by toggleCopyMode.
func (m *Model) buildPlainTranscript() string {
	var b strings.Builder
	for _, msg := range m.messages {
		switch msg.Role {
		case "user":
			b.WriteString("\n> " + msg.Content + "\n")
		case "assistant":
			b.WriteString("\n● " + msg.Content + "\n")
		case "thought-summary":
			b.WriteString("✻ " + msg.Content + "\n")
		case "recap":
			b.WriteString("※ recap: " + msg.Content + "\n")
		case "compaction":
			b.WriteString("✻ Conversation compacted (" + msg.Content + ")\n")
		case "error":
			b.WriteString("✗ " + msg.Content + "\n")
		case "info":
			b.WriteString(msg.Content + "\n")
		}
	}
	for _, te := range m.toolEvents {
		b.WriteString(fmt.Sprintf("⏺ %s(%s)\n", te.ToolName, toolArgsPreview(te.ToolName, te.Input)))
		if te.Kind == "result" {
			b.WriteString(fmt.Sprintf("  ⎿ %s\n", summarizeToolResult(te)))
		}
	}
	return b.String()
}

// asREPL packages the chat-surface state into the REPL command struct
// so slash-handlers (cmdHelp, cmdGitStatus, etc.) can read shared
// fields without each one duplicating the bridge.
func (m *Model) asREPL() *REPL {
	return &REPL{
		Loop:        m.loop,
		Gate:        m.gate,
		Slash:       m.slash,
		Session:     m.session,
		SessionID:   m.sessionID,
		model:       m.model,
		skillDir:    m.skillDir,
		cmds:        m.cmds,
		totalTokens: m.totalTokens,
	}
}

// summarizeHistory extracts a text summary from conversation history
// for daily notes (saved on /new before the session is reset).
func (m *Model) summarizeHistory() string {
	history := m.loop.History()
	if len(history) == 0 {
		return ""
	}
	var lines []string
	for _, msg := range history {
		role := string(msg.Role)
		var text string
		for _, block := range msg.Content {
			if block.Type == "text" {
				text += block.Text
			}
		}
		if text != "" {
			if len(text) > 300 {
				text = text[:300] + "..."
			}
			lines = append(lines, role+": "+text)
		}
	}
	result := strings.Join(lines, "\n")
	if len(result) > 2000 {
		result = result[:2000] + "\n...(truncated)"
	}
	return result
}
