package agent

// sub_prompt.go — sub-agent system-prompt construction (G.13,
// 2026-05-12). Pulled out so Agent + Fork tools share the same
// prompt-shaping logic, and so future sub-agent variants (eval,
// distillation, dream-task) can opt into the same templates.
//
// Three modes:
//
//   - SubPromptFork    — warm-context spawn: child inherits the
//     parent's full conversation history (via the Fork tool's
//     Cache+Restore handshake). The prompt block reminds the child
//     it sees prior context and shouldn't re-read it.
//
//   - SubPromptAgent   — cold-context spawn: child gets a fresh
//     message history. The prompt explains the scope-restricted
//     "do this one focused task and return" posture.
//
//   - SubPromptTeammate — named teammate: same cold-context as
//     SubPromptAgent but with extra guidance that this agent can
//     RECEIVE peer messages (`<peer_message>` reminders) and SEND
//     them via MessageTeammate.
//
// Mirrors claude-code's src/utils/agent/prompt.ts which assembles a
// fork-vs-agent-vs-teammate variant with a similar scope-and-role
// preamble. metis's pre-G.13 path collapsed all three into "use
// minimalSystem when set, else parent's system" — losing the scope
// guidance that helps the child stay focused.

import (
	"strings"
)

// SubPromptMode discriminates the three system-prompt shapes.
type SubPromptMode int

const (
	// SubPromptFork — child inherits warm conversation history.
	SubPromptFork SubPromptMode = iota
	// SubPromptAgent — anonymous cold-start sub-agent.
	SubPromptAgent
	// SubPromptTeammate — named cold-start sub-agent with peer
	// messaging affordances.
	SubPromptTeammate
)

// SubPromptInputs is the bundle of values BuildSubPrompt assembles
// into the final system prompt. Most fields are optional; the
// builder picks reasonable defaults so callers can stay terse.
type SubPromptInputs struct {
	Mode SubPromptMode

	// Base is the parent's minimalSystem (preferred) or full system
	// prompt. If the parent computed a minimal variant, sub-agents
	// reuse it so we don't double-bill <project_context> tokens per
	// sub-agent. Empty Base produces a stub prompt — caller should
	// have at least the LLM provider's house style in there.
	Base string

	// TeammateName is the name the model can use to send/receive
	// peer messages. Only consulted in SubPromptTeammate mode.
	TeammateName string

	// ProfileSystemPrompt comes from an --agent profile's body
	// (G.7's bundled profiles or the user's ~/.metis/agents/*.md).
	// Appended after Base when non-empty — profile guidance is
	// more specific than the global system prompt and should win
	// when they overlap.
	ProfileSystemPrompt string
}

// BuildSubPrompt assembles the final system prompt for a sub-agent.
// Returns Base unchanged when Mode is unrecognized (defensive: a
// future caller adding a new mode without updating this switch
// shouldn't silently lose the parent's system prompt).
func BuildSubPrompt(in SubPromptInputs) string {
	header := subPromptHeader(in)
	var parts []string
	if header != "" {
		parts = append(parts, header)
	}
	if in.Base != "" {
		parts = append(parts, in.Base)
	}
	if in.ProfileSystemPrompt != "" {
		parts = append(parts, in.ProfileSystemPrompt)
	}
	return strings.Join(parts, "\n\n")
}

// subPromptHeader returns the role-and-scope preamble that goes
// BEFORE the parent's Base prompt. Keeping it in front means the
// child sees its job-as-a-sub-agent first; the parent's general
// guidance (provider style, project_context) acts as backdrop.
//
// Each template is intentionally short — the child's runtime
// budget should go to the task, not the role-statement.
func subPromptHeader(in SubPromptInputs) string {
	switch in.Mode {
	case SubPromptFork:
		return `You are a forked sub-agent. You can see the parent agent's full conversation history above. Treat that history as ALREADY READ — do not re-summarize or re-explore what was already covered. Pick up the parent's task and execute the focused sub-task you were just handed. Reply with the final result the parent will paste into its context; no "here's what I'll do" preamble.`

	case SubPromptAgent:
		return `You are a focused sub-agent. The parent agent delegated a single sub-task; complete it autonomously and return the result. You DO NOT see the parent's prior conversation — work from only what's in this fresh message history. If the task is ambiguous, pick the most plausible interpretation and proceed; surface the ambiguity in one trailing line, but don't ask back. Your final reply IS the report the parent will paste into its context, so write it that way (no "I'll start by..." or "let me know if you need more").`

	case SubPromptTeammate:
		who := "this teammate"
		if in.TeammateName != "" {
			who = in.TeammateName
		}
		return `You are ` + who + `, a named sub-agent in a multi-agent workflow. You DO NOT see the parent's prior conversation — work from only what's in this fresh message history. ` +
			`You can RECEIVE messages from other teammates as ` + "`<peer_message>`" + ` system-reminders between turns, and SEND messages to them via the MessageTeammate tool. Coordinate when it helps, but don't chat — every peer message costs the team a turn. Complete your focused task and return the result; the parent agent will synthesize across teammates.`

	default:
		return ""
	}
}
