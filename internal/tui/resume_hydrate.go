package tui

// resume_hydrate.go — translate loop.Messages → m.messages + m.toolEvents
// at TUI startup so a `--resume <id>` session shows the historical
// transcript instead of a blank chat.
//
// Why this file exists:
//
// metis carries TWO message stores by design (loop.Messages, the LLM-
// shaped slice used to build the next API request; m.messages, the
// TUI-shaped slice used for chat-surface rendering). They normally
// grow together via the live event stream — EventTextDelta /
// EventToolStart / EventToolResult each append to the right side
// from cmd_events.go.
//
// runtime.ApplyResume only restores loop.Messages from disk. There's
// no symmetric hydration into m.messages, so until 2026-05-15 a
// resumed session opened to a blank chat surface even though the
// LLM had full context. claude-code avoids this by using a single
// shared `messages` array (REPL.tsx:1182, initialized via
// `loadConversationForResume`); metis's split keeps the two stores
// independent so a hydration step is necessary.
//
// Translation rules (assistant + user blocks → Message + ToolEvent):
//
//   - text block on a user message     → Message{Role: "user"}
//   - text block on an assistant message → Message{Role: "assistant"}
//   - tool_use block (assistant)         → ToolEvent{Kind: "start"}
//   - tool_result block (user)           → upgrade matching ToolEvent
//                                          to Kind "end" with output
//
// Empty-text blocks are skipped (a tool-only assistant message
// produces only ToolEvents). A trailing info marker tells the user
// the transcript came from disk so they don't think the chat just
// somehow remembered without a session restore.

import (
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// hydrateFromLoopHistory mirrors the live append flow but starts from
// loop.Messages rather than the event stream. Idempotent only by
// accident — call it ONCE at NewModel time before the loop starts
// emitting events. Subsequent calls would duplicate the timeline.
func (m *Model) hydrateFromLoopHistory() {
	if m.loop == nil {
		return
	}
	history := m.loop.History()
	if len(history) == 0 {
		return
	}
	// Anchor hydrated items to a base time STRICTLY before time.Now()
	// so any future live event keeps its natural "later" ordering. Use
	// 1-microsecond per-item offsets so the timeline merge sorts
	// across messages + toolEvents in original conversation order
	// (sortTimelineByTime is stable, but only stable on identical ts
	// — distinct ts is what gives us the cross-slice interleaving).
	base := time.Now().Add(-time.Hour)
	itemIdx := 0
	tsNext := func() time.Time {
		t := base.Add(time.Duration(itemIdx) * time.Microsecond)
		itemIdx++
		return t
	}

	// Track tool_use IDs so a later tool_result block can upgrade the
	// matching ToolEvent's Kind / Output / IsError. We hold pointers
	// into m.toolEvents — safe because we only ever append to it
	// during this hydration pass (no reslice = no element move).
	toolByID := map[string]*ToolEvent{}

	for _, lm := range history {
		role := "user"
		if lm.Role == llm.RoleAssistant {
			role = "assistant"
		}
		for _, blk := range lm.Content {
			switch blk.Type {
			case "text":
				if strings.TrimSpace(blk.Text) == "" {
					continue
				}
				m.messages = append(m.messages, Message{
					Role:      role,
					Content:   blk.Text,
					Timestamp: tsNext(),
					ID:        m.nextID(),
				})
			case "tool_use":
				m.toolEvents = append(m.toolEvents, ToolEvent{
					ID:        m.nextID(),
					Kind:      "start",
					ToolName:  blk.ToolName,
					Input:     blk.ToolInput,
					StartTime: tsNext(),
				})
				if blk.ToolUseID != "" {
					toolByID[blk.ToolUseID] = &m.toolEvents[len(m.toolEvents)-1]
				}
			case "tool_result":
				if te, ok := toolByID[blk.ToolUseID]; ok {
					te.Kind = "end"
					te.Output = blk.ToolResult
					te.IsError = blk.IsError
					// Duration is unknowable post-resume; leave at 0
					// so the per-tool summary line renders without
					// the elapsed-time prefix instead of lying.
				}
				// tool_result without a preceding tool_use can happen
				// on a corrupted transcript or an old format — we
				// silently drop rather than fabricate a ToolEvent
				// with no name (renderToolEvent expects ToolName).
			}
		}
	}

	// Tail marker so the user knows the transcript came from disk.
	// Uses a fresh tsNext so it sorts AFTER the last hydrated item
	// but still before any future live event.
	m.messages = append(m.messages, Message{
		Role:      "info",
		Content:   fmt.Sprintf("(resumed: %d messages restored from session %s)", len(history), shortSessionID(m.sessionID)),
		Timestamp: tsNext(),
		ID:        m.nextID(),
	})
}

// shortSessionID truncates a long session UUID for the resume marker
// — full UUIDs are 36 chars and crowd the chat surface. We keep the
// first 8 + "…" because that's already enough to disambiguate among
// a typical user's recent sessions.
func shortSessionID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:8] + "…"
}
