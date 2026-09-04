package slash

import "strings"

// BatchPrompt renders the multi-stage prompt that converts `/batch <task>`
// into a Research → Plan → Execute workflow. Mirrors claude-code's
// bundled batch skill — same shape, same constraints (5–30 sub-agents,
// each in its own worktree, end-to-end test recipe). The orchestrator
// LLM uses metis's existing Agent tool with `isolation: "worktree"` and
// `run_in_background: true` for each sub-agent.
//
// Kept in slash so the keybind_submit dispatcher can compose it without
// bouncing through the runtime layer.
func BatchPrompt(task string) string {
	task = strings.TrimSpace(task)
	var b strings.Builder
	b.WriteString("/batch — multi-agent orchestration\n\n")
	b.WriteString("Task: ")
	b.WriteString(task)
	b.WriteString("\n\n")
	b.WriteString("Run this in three phases. Do NOT skip phases.\n\n")
	b.WriteString("PHASE 1 — RESEARCH (in main loop)\n")
	b.WriteString("- Read relevant code, configs, tests to understand scope.\n")
	b.WriteString("- List every file / module / call site this change touches.\n")
	b.WriteString("- Note codebase conventions a worker agent must follow.\n")
	b.WriteString("- Identify a concrete end-to-end verification recipe (`make test`, a curl, a manual check). Unit tests alone are not sufficient.\n\n")
	b.WriteString("PHASE 2 — PLAN (enter plan mode, ExitPlanMode for approval)\n")
	b.WriteString("- Decompose the work into 5–30 INDEPENDENT units. Each unit must be:\n")
	b.WriteString("    * mergeable on its own (no cross-unit ordering)\n")
	b.WriteString("    * roughly uniform in size\n")
	b.WriteString("    * scoped per directory / per module / per call-site cluster\n")
	b.WriteString("- Write the plan as a numbered list with: title, files touched, the e2e recipe used to verify, expected commit message.\n")
	b.WriteString("- Call ExitPlanMode and wait for user approval before phase 3.\n\n")
	b.WriteString("PHASE 3 — EXECUTE (parallel sub-agents)\n")
	b.WriteString("- Execute units in provider-safe waves of 2–4 Agents with isolation=\"worktree\" and run_in_background=true; after a 429/TPM error, wait for active work to finish and reduce the next wave.\n")
	b.WriteString("- Pass the agent: the unit description, codebase conventions discovered in Phase 1, the e2e recipe, and the worker contract below.\n")
	b.WriteString("- Worker contract (each sub-agent must follow):\n")
	b.WriteString("    1. Implement the unit and only the unit.\n")
	b.WriteString("    2. Run the e2e recipe; iterate until green.\n")
	b.WriteString("    3. Commit with the agreed message.\n")
	b.WriteString("    4. Push to a fresh branch and open a PR.\n")
	b.WriteString("    5. Report exactly one final line: `PR: <url>`  (or `PR: none — <reason>` if blocked).\n")
	b.WriteString("- Start the next wave only after the prior wave has returned; do not treat the hard agent cap as a recommended launch size.\n")
	b.WriteString("- Aggregate when all agents return: a status table (unit | status | PR url) + a one-line summary (`X/N units landed`).\n\n")
	b.WriteString("CONSTRAINTS\n")
	b.WriteString("- Do not commit on the main branch from the orchestrator turn.\n")
	b.WriteString("- Do not let sub-agents talk to each other; isolation is the point.\n")
	b.WriteString("- If a sub-agent reports `PR: none — <reason>`, surface the reason in the final report; do not retry silently.\n")
	return b.String()
}
