package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

type rewindSummaryResultMsg struct {
	requestID uint64
	loop      *agent.Loop
	sessionID string
	turn      int
	plan      *agent.RewindSummaryPlan
	summary   string
	err       error
}

// openRewindScreen builds the dense user-message timeline independently from
// the sparse file checkpoints. This keeps conversation-only and summary
// actions available even when a prompt did not edit code.
func (m *Model) openRewindScreen() {
	if m == nil || m.loop == nil {
		if m != nil {
			m.messages = append(m.messages, Message{Role: "warning", Content: "rewind: no active agent loop", Timestamp: time.Now()})
		}
		return
	}
	points := m.loop.RewindPoints()
	if len(points) == 0 {
		m.messages = append(m.messages, Message{Role: "info", Content: "(nothing to rewind — no conversation checkpoints yet)", Timestamp: time.Now()})
		return
	}
	entries := make([]screen.RewindEntry, 0, len(points))
	for _, point := range points {
		entries = append(entries, screen.RewindEntry{
			Turn:              point.Turn,
			Prompt:            point.Prompt,
			HasCodeCheckpoint: point.HasCodeCheckpoint,
			LatestEdit:        point.LatestEdit,
		})
	}
	picker := screen.NewRewindScreen(entries)
	picker.Resize(m.width, m.height)
	m.activeScreen = picker
}

// applyLegacyRewind retains the original one-step "last edit, code +
// conversation" behavior for `/rewind last` and the rewind screen's `l`
// shortcut.
func (m *Model) applyLegacyRewind() {
	if m == nil || m.loop == nil {
		if m != nil {
			m.messages = append(m.messages, Message{Role: "warning", Content: "rewind: no active agent loop", Timestamp: time.Now()})
		}
		return
	}
	persist := func(history []llm.Message) error {
		return m.session.ReplaceHistoryAndMark(m.sessionID, history, &m.historyCursor)
	}
	res, err := m.loop.RewindWithPersist(persist)
	if err != nil {
		m.messages = append(m.messages, Message{Role: "error", Content: "rewind failed: " + err.Error(), Timestamp: time.Now()})
		return
	}
	m.trimVisibleRewindTurns(res.TurnsUndone)
	m.toolEvents = nil
	m.messages = append(m.messages, Message{
		Role:      "success",
		Content:   fmt.Sprintf("(rewound: restored files + undid %d turn(s) — %s)", res.TurnsUndone, res.Label),
		Timestamp: time.Now(),
	})
}

func (m *Model) applyRewindScreen(picker *screen.RewindScreen) tea.Cmd {
	if picker == nil || picker.Action() == screen.RewindActionCancel || picker.SelectedTurn() < 1 {
		m.messages = append(m.messages, Message{Role: "info", Content: "(rewind dialog dismissed)", Timestamp: time.Now()})
		return nil
	}
	if m.loop == nil {
		m.messages = append(m.messages, Message{Role: "warning", Content: "rewind: no active agent loop", Timestamp: time.Now()})
		return nil
	}

	turn := picker.SelectedTurn()
	if picker.Action() == screen.RewindActionSummary {
		if m.rewindSummaryPending {
			m.messages = append(m.messages, Message{Role: "info", Content: "(rewind summary already in progress)", Timestamp: time.Now()})
			return nil
		}
		plan, err := m.loop.PrepareSummarizeFromTurn(turn)
		if err != nil {
			m.messages = append(m.messages, Message{Role: "error", Content: "rewind failed: " + err.Error(), Timestamp: time.Now()})
			return nil
		}
		m.rewindSummarySeq++
		requestID := m.rewindSummarySeq
		loop := m.loop
		sessionID := m.sessionID
		base := m.ctx
		if base == nil {
			base = context.Background()
		}
		m.rewindSummaryPending = true
		m.messages = append(m.messages, Message{Role: "info", Content: fmt.Sprintf("(summarizing conversation from turn %d...)", turn), Timestamp: time.Now()})
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(base, 60*time.Second)
			defer cancel()
			summary, summaryErr := loop.GenerateRewindSummary(ctx, plan)
			return rewindSummaryResultMsg{
				requestID: requestID,
				loop:      loop,
				sessionID: sessionID,
				turn:      turn,
				plan:      plan,
				summary:   summary,
				err:       summaryErr,
			}
		}
	}

	var (
		result agent.RewindResult
		err    error
	)
	persist := func(history []llm.Message) error {
		return m.session.ReplaceHistoryAndMark(m.sessionID, history, &m.historyCursor)
	}
	switch picker.Action() {
	case screen.RewindActionBoth:
		result, err = m.loop.RewindToTurnWithPersist(turn, agent.RewindCodeAndConversation, persist)
	case screen.RewindActionConversation:
		result, err = m.loop.RewindToTurnWithPersist(turn, agent.RewindConversation, persist)
	case screen.RewindActionCode:
		result, err = m.loop.RewindToTurn(turn, agent.RewindCode)
	default:
		return nil
	}
	if err != nil {
		m.messages = append(m.messages, Message{Role: "error", Content: "rewind failed: " + err.Error(), Timestamp: time.Now()})
		return nil
	}
	m.applyRewindResult(picker.Action(), turn, result)
	return nil
}

func (m *Model) handleRewindSummaryResult(msg rewindSummaryResultMsg) {
	if !m.rewindSummaryPending || msg.requestID != m.rewindSummarySeq {
		return
	}
	m.rewindSummaryPending = false
	if msg.loop != m.loop || msg.sessionID != m.sessionID {
		m.messages = append(m.messages, Message{Role: "warning", Content: "rewind summary ignored because the active session changed", Timestamp: time.Now()})
		return
	}
	if msg.err != nil {
		m.messages = append(m.messages, Message{Role: "error", Content: "rewind failed: " + msg.err.Error(), Timestamp: time.Now()})
		return
	}
	persist := func(history []llm.Message) error {
		return m.session.ReplaceHistoryAndMark(m.sessionID, history, &m.historyCursor)
	}
	result, err := m.loop.CommitRewindSummaryWithPersist(msg.plan, msg.summary, persist)
	if err != nil {
		m.messages = append(m.messages, Message{Role: "error", Content: "rewind failed: " + err.Error(), Timestamp: time.Now()})
		return
	}
	m.applyRewindResult(screen.RewindActionSummary, msg.turn, result)
}

func (m *Model) applyRewindResult(action screen.RewindAction, turn int, result agent.RewindResult) {

	conversationChanged := result.ConversationRestored || result.Summary != ""
	if conversationChanged {
		m.trimVisibleRewindTurns(result.TurnsUndone)
		m.toolEvents = nil
	}
	if result.Prompt != "" && conversationChanged {
		m.input.SetValue(result.Prompt)
	}
	if result.Summary != "" {
		resetTokenUsageAfterCompaction(&m.totalTokens, m.session, m.sessionID)
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   "[Conversation from here summarized: " + result.Summary + "]",
			Timestamp: time.Now(),
		})
	}
	var confirmation string
	switch action {
	case screen.RewindActionBoth:
		confirmation = fmt.Sprintf("rewound to turn %d: restored code + conversation; prompt returned to input", turn)
	case screen.RewindActionConversation:
		confirmation = fmt.Sprintf("rewound to turn %d: restored conversation; current code kept; prompt returned to input", turn)
	case screen.RewindActionCode:
		confirmation = fmt.Sprintf("rewound to turn %d: restored code; conversation kept", turn)
	case screen.RewindActionSummary:
		confirmation = fmt.Sprintf("summarized conversation from turn %d; code kept; prompt returned to input", turn)
	}
	confirmation = strings.TrimSpace(confirmation)
	if confirmation != "" {
		m.messages = append(m.messages, Message{Role: "success", Content: "(" + confirmation + ")", Timestamp: time.Now()})
	}
}

func (m *Model) trimVisibleRewindTurns(turns int) {
	for index := 0; index < turns; index++ {
		m.messages = trimVisibleMessagesToLastUser(m.messages)
	}
}
