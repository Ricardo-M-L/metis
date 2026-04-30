package tui

// tui_update.go — bubbletea Update() loop, the async turn driver, and
// the session persistence tail. Update is the single entry point for
// every key/window/tick event; it dispatches into handleKey,
// handleAgentEvent, or the active screen overlay.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/runtime"
)

func (m *Model) Init() tea.Cmd {
	// No initial command - let View() render first frame with banner
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Drain agent events
	select {
	case ev, ok := <-m.eventCh:
		if ok {
			m.handleAgentEvent(ev)
		}
	default:
	}

	// Active full-window screen (e.g. /history) takes over input + view.
	// We still let WindowSizeMsg reach the main chat so the underlying
	// layout is correct when the screen closes; everything else routes
	// only to the screen.
	if m.activeScreen != nil {
		if ws, ok := msg.(tea.WindowSizeMsg); ok {
			m.width = ws.Width
			m.height = ws.Height
			m.activeScreen.Resize(ws.Width, ws.Height)
			return m, nil
		}
		var cmd tea.Cmd
		m.activeScreen, cmd = m.activeScreen.Update(msg)
		if m.activeScreen.Done() {
			m.activeScreen = nil
		}
		return m, cmd
	}

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Reserve rows for the always-visible chrome:
		//   header (1) + separator (1) + spinner (2) + input (2)
		//   + status bar (2) + hints (1) + safety (1) ≈ 10
		const chrome = 10
		vpHeight := msg.Height - chrome
		if vpHeight < 5 {
			vpHeight = 5
		}
		// Reserve 2 cols on the right for the scrollbar overlay so
		// the bar always sits inside the visible width — previously
		// it appended past terminal-width and disappeared off-screen
		// (image #51 the user noticed claude-code has one and metis
		// doesn't).
		vpWidth := msg.Width - 2
		if vpWidth < 40 {
			vpWidth = msg.Width
		}
		m.viewport.Width = vpWidth
		m.viewport.Height = vpHeight
		// Compute textarea width consistent with renderInputLine's
		// box width math — subtract 8 from terminal width to land on
		// the inner content area. Capped at 100-4 so wide terminals
		// don't render an absurdly long input row.
		w := msg.Width - 8
		if w > 100-4 {
			w = 100 - 4
		}
		if w < 20 {
			w = 20
		}
		m.input.SetWidth(w)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		// Mouse wheel → viewport scroll. The viewport's own Update
		// reads bubbletea's MouseMsg and adjusts YOffset — we just
		// have to hand it the message. Without this dispatch the
		// MouseWheelEnabled flag is a no-op because nothing forwards
		// mouse events into the viewport.
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case spinnerTick:
		// Spinner glyph index is now time-gated (renderSpinnerStatus
		// computes it from elapsed via spinnerStepMs), so we no longer
		// touch m.spinnerFrame here. The tick still drives the
		// token-counter animation + event drain below.
		m.totalTokens.Animate()

		// Drain any agent events that landed since the last tick. The
		// previous code only popped one event per tick, which made bursty
		// streams (text_delta + tool_use back-to-back) lag visibly.
		for drained := false; !drained; {
			select {
			case ev, ok := <-m.eventCh:
				if ok {
					m.handleAgentEvent(ev)
				} else {
					drained = true
				}
			default:
				drained = true
			}
		}

		// Check turn completion BEFORE deciding to continue ticking — the
		// old code returned tickCmd while turnActive was still true, so the
		// doneCh branch never ran and the spinner kept spinning forever
		// after an error.
		select {
		case err, ok := <-m.doneCh:
			if ok {
				m.finalizeTurn(err)
			}
		default:
		}

		if m.turnActive || m.spinnerActive {
			return m, tickCmd
		}
		return m, nil

	case tick:
		return m, tickCmd
	}
	return m, nil
}

// finalizeTurn collects the post-stream cleanup that runs once doneCh
// fires: stop the spinner, flush any buffered streaming text, append
// the thought-summary + recap rows, log the turn for /lessons, and
// persist the tail to disk.
func (m *Model) finalizeTurn(err error) {
	m.turnActive = false
	m.spinnerActive = false
	// Snap displayed-token counter to truth so the final number doesn't
	// take an extra second to converge after the spinner stops ticking.
	m.totalTokens.Snap()
	// Flush thinking trace before flushing reply text so the order in
	// the transcript matches the order events arrived in.
	if m.thinkingText != "" {
		m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
		m.thinkingText = ""
	}
	if m.streamingText != "" {
		m.messages = append(m.messages, Message{Role: "assistant", Content: m.streamingText, Timestamp: time.Now()})
		m.streamingText = ""
	}
	// Append claude-code-style turn-end thought summary
	// ("✻ Cogitated for 1m 32s") so the user can see at a glance how
	// long each turn ran. Skipped on errors — the error row already
	// carries failure context, and stacking a summary below it reads
	// as celebratory.
	if err == nil && !m.spinnerStartedAt.IsZero() {
		if d := time.Since(m.spinnerStartedAt); d >= time.Second {
			m.messages = append(m.messages, Message{
				Role:      "thought-summary",
				Content:   fmt.Sprintf("%s for %s", pickTurnDoneVerb(), formatTurnDuration(d)),
				Timestamp: time.Now(),
			})
		}
	}
	// Structural recap below the thought-summary, when this turn did
	// enough work to be worth summarizing (≥2 tool calls).
	recap := ""
	if err == nil {
		recap = buildTurnRecap(m.toolEvents)
		if recap != "" {
			m.messages = append(m.messages, Message{
				Role:      "recap",
				Content:   recap,
				Timestamp: time.Now(),
			})
		}
	}
	// Continuous-learning log — append a structural record so /lessons
	// can surface it later. Cheap (1 JSON line append, no LLM); failures
	// are silent so a flaky disk never breaks the chat.
	if !m.spinnerStartedAt.IsZero() {
		var prompt string
		var tools []string
		var files []string
		seenTool := map[string]bool{}
		for i := len(m.messages) - 1; i >= 0 && prompt == ""; i-- {
			if m.messages[i].Role == "user" {
				prompt = m.messages[i].Content
			}
		}
		hadErrors := false
		for _, te := range m.toolEvents {
			if te.Kind != "result" {
				continue
			}
			if !seenTool[te.ToolName] {
				seenTool[te.ToolName] = true
				tools = append(tools, te.ToolName)
			}
			if te.IsError {
				hadErrors = true
			}
			if path := stringField(te.Input, "path", "file_path"); path != "" {
				files = append(files, basename(path))
			}
		}
		_ = runtime.AppendLearned(runtime.LearnedRecord{
			Timestamp:  time.Now(),
			SessionID:  m.sessionID,
			Prompt:     truncate(prompt, 200),
			ToolUsed:   tools,
			FilesTouch: files,
			Duration:   formatTurnDuration(time.Since(m.spinnerStartedAt)),
			Tokens:     m.totalTokens.Total(),
			HadErrors:  hadErrors,
			Recap:      recap,
		})
	}
	m.persistTail()
	if err != nil {
		// finalizeTurn fires when the loop's done channel returns. Most
		// of the time that error has ALREADY been surfaced via the
		// EventError path (tui_events.go), with formatProviderError
		// applied. Re-emitting here in raw "error: " format created
		// duplicate rows — one prettified ("API Error: ..."), one raw
		// JSON dump — which the user reported in image #9.
		//
		// Fix: route through formatProviderError too, AND dedupe
		// against the last error row so we don't double-print the
		// same failure under two formats.
		errMsg, _ := formatProviderError(err.Error())
		formatted := "API Error: " + errMsg
		alreadyShown := false
		for i := len(m.messages) - 1; i >= 0; i-- {
			r := m.messages[i].Role
			if r == "error-hint" {
				continue
			}
			if r == "error" {
				// Same content (or same content with `(×N)` suffix)
				// means EventError already surfaced this; skip the
				// redundant finalizeTurn echo.
				if strings.HasPrefix(m.messages[i].Content, formatted) {
					alreadyShown = true
				}
			}
			break
		}
		if !alreadyShown {
			m.messages = append(m.messages, Message{Role: "error", Content: formatted, Timestamp: time.Now()})
		}
	}
}

func (m *Model) persistTail() {
	if m.session == nil || m.sessionID == "" {
		return
	}
	hist := m.loop.History()
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == llm.RoleUser && len(hist[i].Content) > 0 && hist[i].Content[0].Type == "text" {
			for j := i + 1; j < len(hist); j++ {
				_ = m.session.AppendMessage(m.sessionID, hist[j])
			}
			return
		}
	}
}

func (m *Model) runTurnAsync() {
	// Wrap m.ctx in a cancellable child so Ctrl-C can interrupt this turn
	// without taking down the whole session.
	turnCtx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	defer func() {
		cancel()
		m.turnCancel = nil
	}()

	events := make(chan agent.Event, eventBufferSize())
	done := make(chan error, 1)
	go func() { done <- m.loop.Run(turnCtx, events); close(events) }()
	for ev := range events {
		m.eventCh <- ev
	}
	err := <-done
	m.doneCh <- err
}
