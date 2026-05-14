package agent

// empty_stop_rescue.go — detector for "model declared end_turn but
// emitted no user-facing text". Mirrors the Bug B class from the
// 2026-05-14 session export at
// ~/.metis/exports/session-182928d4-8882-45f9-9151-824c610ebf9e-1778733268.jsonl
// — the model ran 11 tool calls in a row (Bash → git diff → Read*4 →
// Grep*4) and then said end_turn with an empty final assistant
// message. metis's Loop returned with stop_reason=end_turn, leaving
// the user staring at a blank screen for 2 minutes before they had
// to ask "完成了吗" themselves.
//
// claude-code addresses this via stopHook (query.ts:1282) — a hook
// can return blockingErrors to force another iteration. metis has no
// such hook; this detector + one-shot nudge is the equivalent.
//
// Strictly bounded: at most ONE nudge per turn. If the model stops
// silently a second time we accept it (something is wrong upstream
// and looping forever wastes tokens).

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// hasUserFacingText returns true when the assistant content carries
// at least one non-empty `text` block. `thinking` and `tool_use`
// blocks DON'T count — the user never sees thinking, and tool calls
// alone don't tell the user what was found.
//
// A whitespace-only text block also counts as empty: a single space
// or newline isn't an answer, just a stop-trigger glitch.
func hasUserFacingText(content []llm.ContentBlock) bool {
	for _, b := range content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}

// emptyStopRescueMessage is the system-reminder appended as a user
// message when the model stops without writing a final answer. The
// wording mirrors claude-code's "Keep working — do not summarize"
// nudge style (inverted: here we want a summary, there they wanted
// continued work). The 1-3 line cap matches the base-prompt style
// budget so we're not asking for something the model was told to
// avoid.
const emptyStopRescueMessage = `<system-reminder>
You finished your tool calls but did not write a final answer for
the user. They are looking at a blank screen and don't know whether
you are still working or done.

Summarize what you found / did in 1-3 lines and stop. Don't restart
the investigation — answer with what you already know. A one-line
"done — here's the result" is fine; an empty stop is not.
</system-reminder>`
