package runtime

// plan_overlay.go — the prompt fragment injected when the user starts
// metis in plan mode (`--mode plan` or `/plan` from chat). The
// permission gate already enforces read-only at the tool layer
// (internal/permission/gate.go::ModePlan), but the model needs to
// KNOW it's in plan mode so it stops trying to call Edit/Write/Bash-
// mutating tools, which would just produce stream of denials.
//
// Mirrors opencode's plan.txt / build-switch.txt pattern: a small
// overlay section that flips behavior without restating the whole
// base prompt.

// PlanOverlay returns the SystemPromptSection to insert when plan
// mode is active. Returns a zero-value section (Name=="") when called
// in a non-plan context — callers check Name != "" before appending,
// matching the CoordinatorOverlay() convention.
//
// active is the flag from the boot path: in main.go we pass
// `mode == permission.ModePlan`. Decoupled from the permission
// package so this file stays import-light.
func PlanOverlay(active bool) SystemPromptSection {
	if !active {
		return SystemPromptSection{}
	}
	return SystemPromptSection{
		Name:  "plan_mode",
		Cache: true,
		Body:  planOverlayBody,
	}
}

const planOverlayBody = `# Plan mode — read-only exploration only

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
