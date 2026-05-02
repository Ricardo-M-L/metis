package tui

// tui_events.go — translates agent.Event values into Model state
// mutations. The bubbletea Update loop drains m.eventCh on every tick;
// each event picks up here to splice text/tool/permission state into
// the chat surface.

import (
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/runtime"
)

func (m *Model) handleAgentEvent(ev agent.Event) {
	switch ev.Kind {
	case agent.EventThinkingDelta:
		// Extended-thinking trace — append live; rendered above the
		// streaming text with dim italic style. Don't update
		// firstStreamAt because thinking precedes the actual reply
		// and the "thought for Xs" timer keys off real text.
		m.thinkingText += ev.TextDelta
	case agent.EventTextDelta:
		if m.firstStreamAt.IsZero() {
			m.firstStreamAt = time.Now()
		}
		// First text after thinking — flush the thinking trace into a
		// persisted Message so it stays in the transcript when the
		// turn ends.
		if m.thinkingText != "" {
			m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
			m.thinkingText = ""
		}
		m.streamingText += ev.TextDelta
	case agent.EventToolStart:
		if m.thinkingText != "" {
			m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
			m.thinkingText = ""
		}
		if m.streamingText != "" {
			m.messages = append(m.messages, Message{Role: "assistant", Content: m.streamingText, Timestamp: time.Now()})
			m.streamingText = ""
		}
		m.toolEvents = append(m.toolEvents, ToolEvent{Kind: "start", ToolName: ev.ToolName, Input: ev.ToolInput, StartTime: time.Now()})
		m.spinnerVerb = toolVerb(ev.ToolName)
		m.spinnerSub = toolArgsPreview(ev.ToolName, ev.ToolInput)
		// Sub-agent visualization: when the Agent tool fires, push
		// a SubAgentInfo into the model so the status bar pill row
		// reflects the in-flight child loop.
		if ev.ToolName == "Agent" {
			name := "agent"
			if p, ok := ev.ToolInput["prompt"].(string); ok && p != "" {
				name = truncate(strings.TrimSpace(p), 24)
			}
			m.subAgents = append(m.subAgents, SubAgentInfo{
				ID:     ev.ToolUseID,
				Name:   name,
				Status: "running",
			})
		}
	case agent.EventToolResult:
		if ev.ToolResult == nil {
			return
		}
		for i := len(m.toolEvents) - 1; i >= 0; i-- {
			if m.toolEvents[i].ToolName == ev.ToolName && m.toolEvents[i].Kind == "start" {
				m.toolEvents[i].Kind = "result"
				m.toolEvents[i].Output = ev.ToolResult.Output
				m.toolEvents[i].IsError = ev.ToolResult.IsError
				m.toolEvents[i].Duration = time.Since(m.toolEvents[i].StartTime)
				break
			}
		}
		m.spinnerVerb = pickSpinnerVerb()
		m.spinnerSub = ""
		// Sub-agent done — remove the matching SubAgentInfo. Match
		// by tool-use ID so multiple concurrent sub-agents work.
		if ev.ToolName == "Agent" {
			out := m.subAgents[:0]
			for _, sa := range m.subAgents {
				if sa.ID != ev.ToolUseID {
					out = append(out, sa)
				}
			}
			m.subAgents = out
		}
	case agent.EventPermissionRequest:
		m.permActive = true
		m.permStartedAt = time.Now()
		m.permTool = ev.ToolName
		m.permArgs = toolArgsPreview(ev.ToolName, ev.PermissionInput)
		// claude-code surface: "Bash wants to run `rm -rf /tmp/foo`"
		// — name the tool AND show the args so the user can decide
		// based on what's actually about to happen, not just the tool.
		if m.permArgs != "" {
			m.permQuestion = fmt.Sprintf("allow %s · %s?", ev.ToolName, truncate(m.permArgs, 50))
		} else {
			m.permQuestion = fmt.Sprintf("allow %s?", ev.ToolName)
		}
		m.permCursor = 0
		m.permReply = ev.PermissionReply
	case agent.EventTokens:
		m.totalTokens.add(ev.InputTokens, ev.OutputTokens, ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
	case agent.EventTurnEnd:
		// keep toolEvents for display
	case agent.EventError:
		// Flush any in-flight thinking/streaming text before painting
		// the error so the partial reply isn't silently dropped. This
		// is the "current turn errored and now I can't see what was
		// happening" case the user flagged — without these flushes,
		// streamingText was orphaned in Model state and the transcript
		// jumped straight from spinner to error row.
		if m.thinkingText != "" {
			m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
			m.thinkingText = ""
		}
		if m.streamingText != "" {
			m.messages = append(m.messages, Message{Role: "assistant", Content: m.streamingText, Timestamp: time.Now()})
			m.streamingText = ""
		}
		// Strip provider JSON envelope, attach a recovery hint when we
		// recognize the failure class. Same error firing N times in a
		// row collapses into one row with "(×N)" — image #62 had 4
		// stacked context-window errors that filled the screen.
		errMsg, errHint := formatProviderError(ev.Err.Error())
		formatted := "API Error: " + errMsg
		// Find the last error row WITH the same content (skip a possible
		// trailing error-hint), so the dedupe still triggers even after
		// we appended a hint last time.
		dedupeIdx := -1
		for i := len(m.messages) - 1; i >= 0; i-- {
			r := m.messages[i].Role
			if r == "error-hint" {
				continue
			}
			if r == "error" && strings.HasPrefix(m.messages[i].Content, formatted) {
				dedupeIdx = i
			}
			break
		}
		if dedupeIdx >= 0 {
			n := extractDedupeCount(m.messages[dedupeIdx].Content) + 1
			m.messages[dedupeIdx].Content = fmt.Sprintf("%s (×%d)", formatted, n)
		} else {
			m.messages = append(m.messages, Message{Role: "error", Content: formatted, Timestamp: time.Now()})
			if errHint != "" {
				m.messages = append(m.messages, Message{Role: "error-hint", Content: errHint, Timestamp: time.Now()})
			}
		}
	case agent.EventInfo:
		// Sniff for compaction events — emit them with a distinctive
		// ✻ banner so the user sees "your context just got compressed"
		// instead of a dim grey info line buried in the transcript.
		// Without this, post-compaction "amnesia" surprises users.
		role := "info"
		if strings.HasPrefix(ev.Info, "context compacted:") {
			role = "compaction"
		}
		m.messages = append(m.messages, Message{Role: role, Content: ev.Info, Timestamp: time.Now()})
	case agent.EventPlan:
		// Plan-mode result: archive to ~/.metis/plans/ + show inline.
		_ = runtime.ArchivePlan(runtime.ArchivedPlan{
			SessionID: m.sessionID,
			Timestamp: time.Now(),
			ToolCalls: ev.ToolCalls,
		})
		m.messages = append(m.messages, Message{Role: "info", Content: fmt.Sprintf("(plan archived to ~/.metis/plans · %d tool calls)", len(ev.ToolCalls)), Timestamp: time.Now()})
	}
}
