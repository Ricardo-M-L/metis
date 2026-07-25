package runtime

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
)

// plan_overlay.go — the prompt fragment attached while the live permission
// mode is plan. The permission gate enforces the boundary at the tool layer;
// this section tells the model how to explore, ask questions, and request plan
// approval without wasting iterations on denied tools.
//
// Mirrors opencode's plan.txt / build-switch.txt pattern: a small
// overlay section that flips behavior without restating the whole
// base prompt.

// PlanOverlay returns a stateless, per-request section when plan mode is
// active. Callers should evaluate active from the LIVE mode immediately before
// building an LLM request, append the returned section only for that request,
// and omit it as soon as ExitPlanMode is approved. A zero-value section
// (Name=="") is returned outside plan mode, matching CoordinatorOverlay's
// convention.
//
// The section is deliberately volatile and not cacheable: whether it is
// present is session state, not part of the stable system-prompt prefix.
func PlanOverlay(active bool) SystemPromptSection {
	if !active {
		return SystemPromptSection{}
	}
	return SystemPromptSection{
		Name:     "plan_mode",
		Cache:    false,
		Volatile: true,
		Body:     planOverlayBody,
	}
}

const planOverlayBody = `# Plan mode instructions

Apply this section only while the live permission mode is plan. The user does
not want implementation yet. Do not make edits, run state-changing commands,
send messages, or otherwise mutate the system while plan mode remains active.

Use read-only exploration tools such as Read, LS, Glob, Grep, WebFetch, and
read-only Bash commands. You may use Agent to delegate read-only exploration;
its child inherits the plan permission boundary. Fork and TodoWrite are not
available in plan mode, so do not call them.

Workflow:

1. Explore the relevant code and reuse existing patterns where possible.
2. Use AskUser only when requirements or trade-offs cannot be resolved from
   the codebase.
3. Prepare a compact implementation plan with concrete steps, affected files,
   risks, and verification.
4. When the plan is ready for approval, call ExitPlanMode with the complete
   markdown plan in its plan argument. Do not request plan approval in prose or
   through AskUser; ExitPlanMode is the only approval path.

A planning turn should end only by calling AskUser for a necessary
clarification or ExitPlanMode for plan approval. After ExitPlanMode reports
that the user approved and the live mode is no longer plan, these restrictions
no longer apply and implementation may proceed under the newly selected
permission mode. If approval is rejected or cancelled, remain in plan mode and
revise the proposal.`

// legacyPlanOverlayBodyV1 is the exact static body persisted in session
// Header.System by Metis releases that assembled plan mode only at startup.
// Keep this byte-for-byte: RemoveLegacyPlanOverlay intentionally refuses fuzzy
// matches so a user's own planning instructions are never stripped merely for
// sharing a heading or a few sentences with the old built-in overlay.
const legacyPlanOverlayBodyV1 = `# Plan mode — read-only exploration only

You are in plan mode. The permission gate will deny every tool that
mutates state: Edit, Write, NotebookEdit, Bash (any non-read-only
command), MessageTeammate to send PRs, etc. Don't try to call them;
the user will see the denial and you'll have to retry. Allowed tools:
Read, LS, Glob, Grep, WebFetch, and any sub-agent (Agent / Fork) you
spawn in read-only mode.

Workflow for plan mode:

  1. Explore. Use Read, Grep, Glob freely to understand the codebase.
     Spawn Agent sub-agents for deep dives that would otherwise burn
     your main context.
  2. Synthesize. Decide what would need to change, in what order,
     with what trade-offs.
  3. Produce a plan. Write it out as the final assistant message:
     a numbered list of concrete steps, the files each step touches,
     and the risks / open questions. Use TodoWrite to track the plan
     items so the user sees the checklist in the UI.
  4. STOP and wait. Do NOT start implementing. The user reviews the
     plan, then exits plan mode (` + "`/auto`" + ` or ` + "`/bypass`" + `) when ready —
     that's the signal to execute. If you implement now, you'll just
     hit denials.

Keep the plan compact. Bullet points, not essays. Skip "next steps"
and "future improvements" sections unless the user asked. The plan
should be a thing the user can scan in 30 seconds and the next-turn
you can execute step-by-step without rereading.`

// RemoveLegacyPlanOverlay removes the obsolete startup-only Plan overlay from
// a resumed session's rendered Header.System. It removes at most one exact
// section body plus cache boundaries RenderSections attached immediately
// around that section, then normalizes only the newly joined section gap.
//
// Near matches, excerpts, new Plan instructions, and arbitrary user-authored
// text are returned byte-for-byte unchanged. Old sessions could persist either
// cache marker depending on which section followed the overlay, so both exact
// marker spellings are recognized.
func RemoveLegacyPlanOverlay(system string) string {
	start := strings.Index(system, legacyPlanOverlayBodyV1)
	if start < 0 {
		return system
	}
	end := start + len(legacyPlanOverlayBodyV1)

	// RenderSections always places a standalone section at a string boundary
	// separated by a blank line. Requiring those boundaries prevents removing
	// the legacy prose when it merely appears inside a larger user paragraph.
	if start > 0 && !strings.HasSuffix(system[:start], "\n\n") {
		return system
	}
	if end < len(system) && !strings.HasPrefix(system[end:], "\n\n") {
		return system
	}

	left := system[:start]
	right := system[end:]
	// The old Plan section was Cache=true. When it followed another cached
	// section RenderSections inserted Boundary2 before Plan; leaving that marker
	// behind after deleting Plan sends the raw sentinel to providers (Boundary2
	// is only meaningful after Boundary1). It belongs to the removed adjacency.
	prePlanBoundary := "\n\n" + anthropic.SystemPromptCacheBoundary2 + "\n\n"
	if strings.HasSuffix(left, prePlanBoundary) {
		left = strings.TrimSuffix(left, prePlanBoundary)
	}
	// Likewise, the cached Plan section normally emitted Boundary1 before the
	// following volatile project/env section (or Boundary2 before another cached
	// section). Neither marker remains valid after Plan itself is removed.
	for _, marker := range []string{
		anthropic.SystemPromptCacheBoundary,
		anthropic.SystemPromptCacheBoundary2,
	} {
		prefix := "\n\n" + marker
		if strings.HasPrefix(right, prefix) {
			right = strings.TrimPrefix(right, prefix)
			break
		}
	}

	left = strings.TrimRight(left, "\n")
	right = strings.TrimLeft(right, "\n")
	switch {
	case left == "":
		return right
	case right == "":
		return left
	default:
		return left + "\n\n" + right
	}
}
