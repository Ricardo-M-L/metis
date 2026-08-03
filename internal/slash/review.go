package slash

// review.go — /review slash command, claude-code style.
//
// 2026-07-28 rewrite: previously this file parsed args into a target
// enum (staged/unstaged/all/branch/commit/PR), collected the diff via
// git subprocess, and inlined the diff into the prompt. That mirrored
// codex's picker-driven /review, but it had three problems in practice:
//
//  1. Defaults fought the user. "" resolved to reviewTargetStaged,
//     so users with uncommitted-but-unstaged work got back "no staged
//     changes" even though there was plenty to review.
//  2. Diff-inlining capped at 32KB meant large refactors got silently
//     truncated, with the model reviewing only the head of the change.
//  3. The hardcoded target list couldn't express real-world queries
//     like "compare HEAD~3..HEAD" or "review what's on origin/main
//     but not here".
//
// The rewrite mirrors claude-code's LOCAL_REVIEW_PROMPT
// (restored-src/src/commands/review.ts): a single free-form prompt
// that instructs the model how to inspect the repo with its own Bash /
// Grep / Read tools and decide what's worth reviewing. No subprocess
// calls from the handler, no diff cap, no enum of acceptable targets.
//
// Why this lives next to debug.go instead of inline in commands.go:
// the prompt body is long enough that embedding it in RegisterAll
// would drown the surrounding one-liner registrations.

import (
	"strings"
)

// RegisterReviewCommand wires /review into the slash registry. Kept
// out of RegisterAll's body for the same reason as /debug — the
// prompt is long enough to deserve its own file.
func RegisterReviewCommand(r *Registry) {
	r.Register(Cmd{
		Name:        "review",
		Description: "code review of local changes / branch / commit / PR (model picks what to inspect)",
		Handler:     reviewHandler,
	})
}

// reviewHandler emits the model-directed review prompt. It does NOT
// collect any diff itself — the model is expected to use Bash (git
// diff/show/merge-base/gh pr) and Read to gather what it needs. The
// args string is passed through verbatim so the model can interpret
// free-form requests ("main", "HEAD~3", "PR #42", "what I changed
// this morning").
func reviewHandler(args string) (string, Signal) {
	return buildReviewPrompt(strings.TrimSpace(args)), SignalCustomPrompt
}

// buildReviewPrompt assembles the final agent prompt. Modeled on
// claude-code's LOCAL_REVIEW_PROMPT (review.ts:9-31) with three
// metis-specific additions:
//
//   - Explicit decision tree for "no args" so the model doesn't
//     hallucinate a PR review when the user has uncommitted work.
//   - A "Report findings" section that matches the format
//     TestSlashE2E_TableDriven expects (VERDICT line + bulleted
//     findings with severity + file:line refs).
//   - A note about `gh pr` being optional — metis users may not
//     have the GitHub CLI installed.
func buildReviewPrompt(userInput string) string {
	var sb strings.Builder
	sb.WriteString("# Code Review\n\n")
	sb.WriteString("You are an expert code reviewer. The user wants a code review.\n\n")

	sb.WriteString("## Decide what to review\n\n")
	sb.WriteString("Inspect the repo state first, then pick the most useful target:\n\n")
	sb.WriteString("- **No user input (or \"review my changes\")** — run `git status` first.\n")
	sb.WriteString("  - If there are staged changes → `git diff --cached`\n")
	sb.WriteString("  - Else if there are unstaged changes → `git diff`\n")
	sb.WriteString("  - Else if there are untracked files → list them and review the relevant ones\n")
	sb.WriteString("  - Else → tell the user \"nothing to review\" and stop\n\n")
	sb.WriteString("- **PR number or \"pr\"** (e.g. `123`, `pr 123`, `#123`) — use `gh pr view <n>` for context and `gh pr diff <n>` for the diff. If `gh` is not installed or the PR isn't found, say so and suggest the user provide a branch or commit instead.\n\n")
	sb.WriteString("- **Branch name** (e.g. `main`, `origin/develop`) — `git diff $(git merge-base HEAD <branch>)..HEAD`\n\n")
	sb.WriteString("- **Commit SHA or range** (e.g. `abc123`, `HEAD~3`, `main..feature-x`) — `git show <sha>` or `git diff <range>`\n\n")
	sb.WriteString("- **Anything else** — treat as a free-form instruction. The user may have asked for a specific file, directory, or concern. Use Bash + Grep + Read to find what's relevant.\n\n")

	if userInput != "" {
		sb.WriteString("## User input\n\n")
		sb.WriteString(userInput)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## What to look for\n\n")
	sb.WriteString("- Bugs / incorrect behavior\n")
	sb.WriteString("- Style + idiom mismatches with surrounding code (read the file first — don't flag patterns that match local convention)\n")
	sb.WriteString("- Missed edge cases or error paths\n")
	sb.WriteString("- Performance footguns (allocations in hot paths, N+1, …)\n")
	sb.WriteString("- Security concerns where applicable (input validation, injection, auth)\n\n")

	sb.WriteString("## Output format\n\n")
	sb.WriteString("Start with exactly one verdict line:\n\n")
	sb.WriteString("- `VERDICT: PASS` — patch is correct, no blocking findings\n")
	sb.WriteString("- `VERDICT: NEEDS WORK` — has P0/P1 findings the author should fix\n")
	sb.WriteString("- `VERDICT: FAIL` — fundamentally broken, do not merge\n\n")
	sb.WriteString("Then a bulleted list of findings, one per distinct issue:\n\n")
	sb.WriteString("- Tag severity at the start: [P0] drop-everything, [P1] urgent, [P2] normal, [P3] nice-to-have\n")
	sb.WriteString("- Cite location as `path:line` (e.g. internal/agent/loop.go:142)\n")
	sb.WriteString("- One finding per bullet, 1–2 sentences max\n")
	sb.WriteString("- No filler (\"great job\", \"thanks for …\")\n")
	sb.WriteString("- If no findings qualify, output the verdict and nothing else\n")

	return sb.String()
}
