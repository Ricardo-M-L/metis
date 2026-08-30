package tui

// keybind_session.go — session-level helpers triggered from key events:
// Ctrl+P session picker, Ctrl+S copy mode, /undo summary, asREPL bridge
// to the REPL command surface.

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

const copyModeHint = "[copy mode — mouse-select/copy · Ctrl+S to return to chat]"

func (m *Model) handleSessionPick() (tea.Model, tea.Cmd) {
	if m.session == nil {
		m.messages = append(m.messages, Message{Role: "info", Content: "(no session store)", Timestamp: time.Now()})
		return m, nil
	}
	cwd, _ := os.Getwd()
	sessions, err := m.session.ListResumable(session.ResumeListOptions{Limit: 10, WorkDir: cwd})
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
		b.WriteString(fmt.Sprintf("%s%s  %s\n", prefix, s.ID[:8], sessionEntryTime(s).Format("01-02 15:04")))
	}
	m.messages = append(m.messages, Message{Role: "info", Content: b.String(), Timestamp: time.Now()})
	return m, nil
}

// toggleCopyMode flips the alt-screen on/off so the user can natively
// select and copy chat content. Entering schedules the current transcript
// through Bubble Tea's managed print path; the renderer first applies the
// pending AltScreen=false View, then inserts the transcript into native
// scrollback. Exiting re-enters alt-screen and redraws the chat.
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
	// Never write directly with fmt.Println here. Update runs while the
	// terminal is still in alt-screen; direct output would therefore be
	// clipped to the physical viewport before View() can request the exit.
	// tea.Println is ordered by our Bubble Tea renderer patch: pending View
	// state is flushed first, so the transcript reaches native scrollback
	// only after DECRST 1049 has restored the normal screen.
	transcript := strings.TrimRight(m.buildPlainTranscript(), "\n")
	if transcript == "" {
		return m, tea.Println(copyModeHint)
	}
	return m, tea.Println(transcript + "\n" + copyModeHint)
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
	// Surface the in-progress input box content so Ctrl+S copy mode
	// covers the blue-frame area too (user screenshot 34, 2026-05-16:
	// "蓝色框起来的输入框的内容不能复制, 鼠标选中文字没显示选中的阴影").
	// Without this the alt-screen exit drops a transcript but the
	// user's draft prompt is invisible in scrollback — they'd have to
	// re-type or Alt+Y to grab it via clipboard.
	if v := m.input.Value(); v != "" {
		b.WriteString("\n> " + v + "\n")
	}
	return b.String()
}

// asREPL packages the chat-surface state into the REPL command struct
// so slash-handlers (cmdHelp, cmdGitStatus, etc.) can read shared
// fields without each one duplicating the bridge.
func (m *Model) asREPL() *REPL {
	r := &REPL{
		ctx:          m.ctx,
		Loop:         m.loop,
		Gate:         m.gate,
		Slash:        m.slash,
		Session:      m.session,
		SessionID:    m.sessionID,
		model:        m.model,
		providerName: m.providerName,
		cfg:          m.cfg,
		skillDir:     m.skillDir,
		cmds:         m.cmds,
		totalTokens:  m.totalTokens,
		baseSystem:   m.baseSystem,
		baseSystemSections: append([]llm.SystemSection(nil),
			m.baseSystemSections...),
		historyCursor:       &m.historyCursor,
		SessionSwitch:       m.ext.SessionSwitch,
		SessionBoundary:     m.ext.SessionBoundary,
		FreshPermissionMode: m.ext.FreshPermissionMode,
		AdoptMCPServer:      m.ext.AdoptMCPServer,
		sandbox:             m.ext.Sandbox,
		UseMarkdown:         normalizeOutputStyle(m.outputStyle) != outputStyleMinimal,
		outputStyle:         normalizeOutputStyle(m.outputStyle),
	}
	// Live-state closures so slash handlers can read TUI-only data
	// (such as bg-turn state) and write to TUI-only surfaces
	// (the input textarea) without REPL having to import textarea or
	// hold a Model pointer. The headless readline REPL leaves these
	// nil and the handlers gracefully fall back.
	r.InsertInput = func(text string) {
		m.input.InsertString(text)
	}
	r.BgTurnSnapshot = func() BgTurnState {
		if !m.turnActive {
			return BgTurnState{}
		}
		return BgTurnState{
			IsActive:    true,
			StartTime:   m.spinnerStartedAt,
			Model:       m.model,
			QueuedCount: len(m.queuedPrompts),
		}
	}
	r.BypassCache = func() {
		if m.loop != nil {
			m.loop.BypassNextCache = true
		}
	}
	r.ResetTokenUsageAfterCompaction = func() {
		resetTokenUsageAfterCompaction(&m.totalTokens, m.session, m.sessionID)
	}
	r.ApplyOutputStyle = func(style string) {
		m.outputStyle = normalizeOutputStyle(style)
	}
	r.RecapSnapshot = func() string {
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == "recap" {
				return m.messages[i].Content
			}
		}
		return ""
	}
	return r
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
