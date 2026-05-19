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
		m.spinnerPhase = "thinking" // arrow → ↓
	case agent.EventRedactedThinking:
		// Safety classifier replaced a chunk of reasoning with opaque
		// cipher text. Flush any in-flight plaintext thinking first
		// so the transcript shows chronological [thinking, redacted,
		// thinking?, …] ordering. We persist the cipher text in
		// Content so resume → toAnthropic round-trip can echo it back
		// (the data plane is wired in commit 1; this is the UI side).
		if m.thinkingText != "" {
			m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
			m.thinkingText = ""
		}
		m.messages = append(m.messages, Message{
			Role:      "redacted_thinking",
			Content:   ev.TextDelta, // base64 cipher text, never user-visible
			Timestamp: time.Now(),
		})
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
		m.spinnerPhase = "responding" // arrow → ↓
	case agent.EventToolStart:
		if m.thinkingText != "" {
			m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
			m.thinkingText = ""
		}
		if m.streamingText != "" {
			m.messages = append(m.messages, Message{Role: "assistant", Content: m.streamingText, Timestamp: time.Now()})
			m.streamingText = ""
		}
		// Reset firstStreamAt + flip spinner phase to "tool" (renders ↓
		// for tool-input/use/responding/thinking per claude-code's
		// SpinnerAnimationRow.tsx mode switch). When the tool finishes
		// and the next API call starts, EventToolResult will flip back
		// to "requesting" (↑).
		m.firstStreamAt = time.Time{}
		m.spinnerPhase = "tool"
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
				ID:        ev.ToolUseID,
				Name:      name,
				Status:    "running",
				StartedAt: time.Now(),
			})
		} else if ev.SubAgentParentID != "" {
			// Forwarded child tool start — attribute to the right
			// pill so the user sees per-sub-agent progress when N
			// sub-agents run in parallel. The "sub: " prefix is
			// stamped by forwardSubAgentEvent; strip it for the
			// per-pill LastTool label so the chip stays compact.
			for i := range m.subAgents {
				if m.subAgents[i].ID != ev.SubAgentParentID {
					continue
				}
				m.subAgents[i].ToolsCount++
				m.subAgents[i].LastTool = strings.TrimPrefix(ev.ToolName, "sub: ")
				break
			}
		}
	case agent.EventToolArgsDelta:
		// kimi-cli-style streaming tool args. The LLM is typing
		// the JSON for an in-flight tool; surface it on the spinner
		// subline so the user sees "Read · /tmp/foo..." appear
		// character-by-character instead of a long silent pause.
		// The full args still arrive via tool_use_stop → EventToolStart
		// where ToolInput is the parsed map, so this only affects
		// the LIVE spinner — the persisted ToolEvent uses the parsed
		// version. Truncate aggressively to keep the spinner row
		// readable on narrow terminals.
		m.toolArgsStream = append(m.toolArgsStream, []byte(ev.TextDelta)...)
		// Cap the live preview at 4KB so a runaway args string
		// doesn't balloon memory between tool_use_stop calls. Real
		// tool args are almost never this large.
		if len(m.toolArgsStream) > 4096 {
			m.toolArgsStream = m.toolArgsStream[:4096]
		}
		m.spinnerSub = previewStreamingArgs(ev.ToolName, m.toolArgsStream)
	case agent.EventToolResult:
		// Reset streaming-args buffer so the next tool starts clean.
		m.toolArgsStream = m.toolArgsStream[:0]
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
		m.spinnerVerb = chooseSpinnerVerb(m.sessionID)
		m.spinnerSub = ""
		// Tool finished → the agent is about to send another API
		// request with the tool result. Flip back to "requesting"
		// (↑) until the next stream byte arrives.
		m.spinnerPhase = "requesting"
		// Sub-agent done — Mi3 (2026-05-18): instead of yanking the
		// pill immediately, flip Status to completed/failed and stamp
		// FinishedAt so the spinner-tick pruner can keep showing the
		// terminal state for ~2s. Gives the user a glimpse of the
		// final outcome (✓ or ✗) before the chip vanishes — pre-fix
		// the pill blinked away the instant the result landed, so
		// success and failure were visually identical.
		if ev.ToolName == "Agent" {
			for i := range m.subAgents {
				if m.subAgents[i].ID != ev.ToolUseID {
					continue
				}
				if ev.ToolResult != nil && ev.ToolResult.IsError {
					m.subAgents[i].Status = "failed"
				} else {
					m.subAgents[i].Status = "completed"
				}
				m.subAgents[i].FinishedAt = time.Now()
				break
			}
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
	case agent.EventAskUser:
		m.askUserActive = true
		m.askUserStartedAt = time.Now()
		m.askUserQuestion = ev.AskUserQuestion
		m.askUserOptions = append([]string(nil), ev.AskUserOptions...)
		m.askUserAllowFreeform = ev.AskUserAllowFreeform
		m.askUserCursor = 0
		// Default focus on the option list when options exist; jump to
		// freeform when the model gave us no options (the tool forces
		// allowFreeform true in that case to keep the user from being
		// stranded with nothing to click).
		m.askUserFreeformOn = len(m.askUserOptions) == 0 && m.askUserAllowFreeform
		m.askUserReply = ev.AskUserReply
		m.askUserInput = newAskUserInput()
	case agent.EventTokens:
		m.totalTokens.add(ev.InputTokens, ev.OutputTokens, ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
	case agent.EventTurnEnd:
		// Flush any in-flight thinking/streaming text before the turn
		// closes so the final reply appears in the transcript. Without
		// this, a no-tool response leaves streamingText orphaned in
		// Model state and the user sees nothing.
		if m.thinkingText != "" {
			m.messages = append(m.messages, Message{Role: "thinking", Content: strings.TrimSpace(m.thinkingText), Timestamp: time.Now()})
			m.thinkingText = ""
		}
		if m.streamingText != "" {
			m.messages = append(m.messages, Message{Role: "assistant", Content: m.streamingText, Timestamp: time.Now()})
			m.streamingText = ""
		}
		// keep toolEvents for display
	case agent.EventError:
		// Compaction may have been the thing that errored — clear the
		// terminal-tab progress indicator before painting the error so
		// the user doesn't see a stuck indeterminate spinner.
		if m.spinnerOverride != "" {
			SetTerminalProgress(ProgressClear)
			m.spinnerOverride = ""
			m.spinnerCompactionBytes = 0
		}
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
	case agent.EventCompactionStart:
		// LLM-driven compaction tier (Collapse or Compact) is about to
		// call summarize. Pin the spinner label so the user sees what's
		// happening during the otherwise silent 5-30s window. Mirrors
		// claude-code REPL.tsx:2504 setSpinnerMessage('Compacting…').
		//
		// Label intentionally drops the tier suffix to match claude-code's
		// image-#19 wording "Compacting conversation..." verbatim — the
		// tier (collapse vs compact) is internal bookkeeping the user
		// doesn't need exposed in the spinner row.
		m.spinnerOverride = "Compacting conversation..."
		m.spinnerCompactionBytes = 0
		// OSC 9;4 indeterminate — terminals that support it (iTerm2
		// 3.6.6+, Ghostty 1.2.0+, ConEmu) render a progress indicator
		// on the tab/taskbar so the user has a visual cue even when
		// the in-pane spinner is scrolled off-screen.
		SetTerminalProgress(ProgressIndeterminate)
	case agent.EventCompactionProgress:
		// summarize stream is feeding bytes — bump the counter (kept
		// in state for potential future use, e.g. debug overlay) but
		// no longer rendered in the spinner row (user feedback: the
		// terminal-tab indicator is enough; the inline token count
		// was noise).
		m.spinnerCompactionBytes = ev.InputTokens
	case agent.EventContextCompacted:
		// Either tier finished (success or failure) — release the
		// override so the normal thinking verb returns on the next
		// turn or tool call.
		m.spinnerOverride = ""
		m.spinnerCompactionBytes = 0
		SetTerminalProgress(ProgressClear)
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
		// Plan-mode result: archive to ~/.metis/plans/ + render the
		// full proposal inline so the user can actually review what
		// the model intends to do (the pre-2026-05-18 version only
		// printed "N tool calls" with no detail, which made plan
		// mode unusable — users couldn't see what they were approving).
		_ = runtime.ArchivePlan(runtime.ArchivedPlan{
			SessionID: m.sessionID,
			Timestamp: time.Now(),
			ToolCalls: ev.ToolCalls,
		})
		callsWord := "tool call"
		if len(ev.ToolCalls) != 1 {
			callsWord = "tool calls"
		}
		m.messages = append(m.messages, Message{Role: "info", Content: fmt.Sprintf("(plan archived to ~/.metis/plans · %d %s)", len(ev.ToolCalls), callsWord), Timestamp: time.Now()})
		// Per-tool detail row — name + truncated args preview.
		for i, tc := range ev.ToolCalls {
			args := toolArgsPreview(tc.Name, tc.Input)
			line := fmt.Sprintf("  %d. %s", i+1, tc.Name)
			if args != "" {
				line += " · " + truncate(args, 100)
			}
			m.messages = append(m.messages, Message{Role: "info", Content: line, Timestamp: time.Now()})
		}
		// Closing hint — without a yes/no overlay, the user needs to
		// know how to actually run these tools.
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "(plan-mode hint: Shift+Tab to leave plan mode, then re-prompt to execute · or edit your request to refine the plan)",
			Timestamp: time.Now(),
		})
	case agent.EventDreamingStart:
		// Phase C — dreaming subagent is about to run. Pin the spinner
		// verb to "Dreaming..." so the user sees what's happening
		// during the otherwise-silent 10-30 s background run. Mirrors
		// the compaction-display pattern (image #19) verbatim — same
		// chrome, different verb, different summary.
		m.spinnerOverride = "Dreaming..."
		m.spinnerCompactionBytes = 0
		// OSC 9;4 indeterminate progress on the terminal tab — works
		// even when the spinner row is scrolled off-screen.
		SetTerminalProgress(ProgressIndeterminate)
	case agent.EventDreamingProgress:
		// Reserved for finer-grained progress (phase tags, file counts).
		// Today the extractor emits Start/End only; Progress is wired
		// up-front so a future iteration can populate it without
		// changing the TUI surface.
		m.spinnerCompactionBytes = ev.InputTokens
	case agent.EventDreamingEnd:
		// Phase C — fork completed. Clear override, drop the tab
		// indicator, and surface the one-line summary inline. The
		// extractor's Info field carries "+N memories, +M skills" or
		// "failed: <reason>".
		m.spinnerOverride = ""
		m.spinnerCompactionBytes = 0
		SetTerminalProgress(ProgressClear)
		role := "info"
		content := "context dreamed: " + ev.Info
		if strings.HasPrefix(ev.Info, "failed:") {
			role = "error"
		} else if ev.Info == "no changes" {
			// Drop the message entirely — the user doesn't need a
			// notification that "nothing was worth saving". Mirrors
			// the autocompact silent path when collapse decides
			// there's nothing to fold.
			return
		} else {
			// Reuse the compaction role so the renderer reaches for the
			// distinctive ✻ banner instead of dim-grey info styling.
			role = "compaction"
			content = "context dreamed: " + ev.Info
		}
		m.messages = append(m.messages, Message{Role: role, Content: content, Timestamp: time.Now()})
	}
}
