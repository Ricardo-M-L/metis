// Package agent — orphan_repair.go.
//
// Defense-in-depth: if Run exits with an assistant tool_use block that
// never got a paired tool_result (e.g. the parent ctx was cancelled
// between the assistant message commit and the tool batch finishing,
// or a tool panicked above the recovery layer), the conversation as
// persisted is API-invalid — the next Anthropic request rejects it
// with "tool_use without tool_result" and resume blows up before the
// model even sees the user's follow-up.
//
// Session 8cfc076b (2026-05-17) caught this in the wild: a foreground
// Agent tool_use was followed in the raw session file by a plain-text
// user message ("你这是停止了吗") with no tool_result in between. On
// resume the API would 400; the model couldn't recover.
//
// The repair walks the messages, pairs tool_use occurrences with
// downstream tool_results, and synthesizes a stub
// `(tool_use never completed — turn interrupted)` user message
// containing one tool_result per orphan. Always idempotent — a
// fully-paired history is returned unchanged.
package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// orphanRepairMessage is the text written into the synthetic
// tool_result body. Kept short so it doesn't waste tokens on resume,
// but informative enough that the model knows the tool didn't actually
// run and can decide whether to re-issue.
const orphanRepairMessage = "(tool_use never completed — turn was interrupted before this tool finished. Re-issue if you still need the result.)"

// RepairOrphanedToolUses scans messages for assistant tool_use blocks
// that lack a matching downstream tool_result and appends a synthetic
// user message that pairs each one. Returns the (possibly extended)
// slice. Safe to call on nil or empty input. Idempotent: calling
// twice produces the same result as calling once.
//
// "Matching" is one-to-one and chronological: a tool_result consumes
// one earlier, still-unmatched tool_use with the same id. Order matters
// because a hypothetical message chain like:
//
//	user(tool_result id=A)  ← stale, from prior turn
//	assistant(tool_use id=A) ← orphan; the result above is unrelated
//
// should still flag A as orphaned. A result consumed by an earlier use
// likewise cannot satisfy a later provider reuse of the same id.
func RepairOrphanedToolUses(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return messages
	}

	// Walk in order. Each id owns a queue of unmatched tool_use
	// occurrences; a downstream result consumes the oldest occurrence,
	// preserving message order even when a provider reuses an id.
	type pending struct {
		id      string
		matched bool
	}
	var open []pending
	pendingByID := make(map[string][]int)

	for _, m := range messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				if b.ToolUseID != "" {
					idx := len(open)
					open = append(open, pending{id: b.ToolUseID})
					pendingByID[b.ToolUseID] = append(pendingByID[b.ToolUseID], idx)
				}
			case "tool_result":
				queue := pendingByID[b.ToolUseID]
				if b.ToolUseID == "" || len(queue) == 0 {
					continue
				}
				open[queue[0]].matched = true
				if len(queue) == 1 {
					delete(pendingByID, b.ToolUseID)
				} else {
					pendingByID[b.ToolUseID] = queue[1:]
				}
			}
		}
	}

	// Preserve occurrence order, including repeated provider ids.
	var orphans []string
	for _, p := range open {
		if p.matched {
			continue
		}
		orphans = append(orphans, p.id)
	}
	if len(orphans) == 0 {
		return messages
	}

	stub := make([]llm.ContentBlock, 0, len(orphans))
	for _, id := range orphans {
		stub = append(stub, llm.ContentBlock{
			Type:       "tool_result",
			ToolUseID:  id,
			ToolResult: orphanRepairMessage,
			IsError:    true,
		})
	}
	return append(messages, llm.Message{
		Role:    llm.RoleUser,
		Content: stub,
	})
}

// repairOrphansInPlace mutates Loop.Messages under the loop lock.
// Called from Run's defer so every exit path heals the history before
// callers (persistTail, sessions resume) read it.
func (l *Loop) repairOrphansInPlace() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Messages = RepairOrphanedToolUses(l.Messages)
}
