// Package transcript holds pure operations over a conversation history.
//
// Why a separate package: the agent.Loop has grown into a god-struct that
// mixes streaming, tool dispatch, hooks, compaction, detection, and (until
// this round) raw transcript editing. Splitting transcript ops out keeps
// each surface independently testable — `transcript.Undo` is a one-line
// pure function that doesn't need a Provider, Gate, Registry, Hooks, etc.
//
// All functions here MUST be pure: input slice → output slice. Locking and
// observability are the caller's responsibility.
package transcript

import "github.com/Ricardo-M-L/metis/internal/llm"

// Undo trims the last completed turn off `msgs` and returns the result.
//
// A "turn" is the span starting at the most recent plain-text user message
// (i.e. the last user input that's NOT a bundle of tool_result blocks) and
// running to the end of the slice. That captures: user text → assistant
// response → any subsequent tool_result/assistant pingpongs.
//
// Returns ok=false (and the slice unchanged) when there is no plain-text
// user message to fall back to. That keeps the caller from emitting a
// confusing "undone" message after a no-op.
func Undo(msgs []llm.Message) ([]llm.Message, bool) {
	idx := LastPlainUserIndex(msgs)
	if idx < 0 {
		return msgs, false
	}
	out := append([]llm.Message(nil), msgs[:idx]...)
	return out, true
}

// UndoWithPrefill is like Undo but ALSO returns the text the user
// typed in that last turn. The caller (TUI) uses it as prefill_text
// for the input box so the user can edit-then-resend instead of
// retyping from scratch — mirrors kimi-cli's `/undo` UX.
//
// prefill is empty when ok is false. Plain-text user messages can
// have multiple text blocks (rare but valid: e.g. a Skill prepended
// a context block before the user typed their question); we return
// only the LAST text block, which is what the user typed.
func UndoWithPrefill(msgs []llm.Message) (out []llm.Message, prefill string, ok bool) {
	idx := LastPlainUserIndex(msgs)
	if idx < 0 {
		return msgs, "", false
	}
	for _, c := range msgs[idx].Content {
		if c.Type == "text" && c.Text != "" {
			prefill = c.Text
		}
	}
	out = append([]llm.Message(nil), msgs[:idx]...)
	return out, prefill, true
}

// LastPlainUserIndex returns the slice index of the most recent user message
// that contains plain text (vs. a synthetic tool_result-only user message).
// Returns -1 if no such message exists.
//
// Rationale: tool_result blocks are wrapped in user-role messages on the
// wire, so a naive "find last user message" would land you mid-tool-loop
// rather than at the user's real prompt.
func LastPlainUserIndex(msgs []llm.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != llm.RoleUser {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				return i
			}
		}
	}
	return -1
}

// Snapshot returns a defensive copy so callers can mutate freely without
// affecting the source slice. Cheap because Message values are flat.
func Snapshot(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	return out
}

// CountTurns counts how many user→assistant exchanges are in `msgs`.
// Used by /history (Round 4) and /status to summarize session length.
//
// One "turn" = one plain-text user message regardless of how many
// tool-loop iterations followed; that matches what a user would count
// when scrolling back through the chat.
func CountTurns(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				n++
				break
			}
		}
	}
	return n
}
