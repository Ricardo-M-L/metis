package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Todo (TodoWrite) creates or updates a task in the current session's
// task list. Persists to ~/.metis/tasks/<sessionID>.json so the LLM
// can pick the list back up across turns and the user has an
// inspectable record per session.
//
// Update semantics: if the input `id` matches an existing task, that
// row is patched (status / priority / content). If `id` is missing, we
// also try matching by exact `content` — the LLM often re-emits the
// same content with a new status to mark progress. Neither match →
// new task appended.
type Todo struct {
	tools.BaseTool
	gate *permission.Gate
}

func (Todo) Name() string { return "TodoWrite" }
func (Todo) Description() string {
	return `Create or update a task in the session task list. Persists to ~/.metis/tasks/.

## When to use this tool

Use proactively in these scenarios:
1. Complex multi-step tasks — when a task requires 3 or more distinct steps.
2. Non-trivial work — careful planning needed; multiple operations.
3. User explicitly asks for a todo list.
4. User provides multiple tasks (numbered or comma-separated).
5. After receiving new instructions — capture user requirements immediately.
6. When you start working on a task — mark it as in_progress BEFORE beginning.
7. After completing a task — mark it as completed and add any follow-ups.

## When NOT to use this tool

Skip when:
1. There is only a single, straightforward task.
2. The task is trivial; tracking it adds no organisational benefit.
3. The task can be completed in less than 3 trivial steps.
4. The request is purely conversational / informational.

## UPDATE vs CREATE — match by content; id is auto-managed

  - Brand new task: pass content + (optional) status. metis generates an id internally.
  - Updating existing: pass the SAME content string + the new status. metis matches by content (exact then normalised — "Cluster 2: Wire Protocol" matches "2. Wire Protocol", etc.) and updates in place.
  - Keep content STABLE across updates. If you reword "implement plan module" to "Phase 1: plan", you risk creating a duplicate if the normaliser misses it.

## STATUS transitions

  - pending → in_progress when you start working on it
  - in_progress → completed when done
  - Only ONE task should be in_progress at a time; finish (or push back to pending) before starting the next.

## Examples

<example>
user: I want to add a dark mode toggle to the application settings. Make sure you run the tests and build when you're done.
assistant: *Creates todo list with these items:*
1. Create dark mode toggle component in Settings page
2. Add dark mode state management (context/store)
3. Implement CSS-in-JS styles for dark theme
4. Update existing components to support theme switching
5. Run tests + build, address any failures
*Begins working on the first task*
<reasoning>
Multi-step feature affecting UI + state + styling. The user also asked for tests/build, which the assistant captured as a final task so it isn't forgotten.
</reasoning>
</example>

<example>
user: Help me rename the function getCwd to getCurrentWorkingDirectory across my project
assistant: *Uses Grep to find all instances of getCwd*
I found 15 instances of 'getCwd' across 8 different files.
*Creates a todo per file that needs updating*
<reasoning>
After search reveals scope (multiple files), todo list ensures every instance is tracked systematically.
</reasoning>
</example>

<example>
user: How do I print 'Hello World' in Python?
assistant: print("Hello World")
<reasoning>
Single, trivial answer. Todo list adds nothing.
</reasoning>
</example>

<example>
user: Add a comment to the calculateTotal function explaining what it does.
assistant: *Uses Edit to add the comment in one shot.*
<reasoning>
One file, one location. No tracking needed.
</reasoning>
</example>

<example>
user: Run npm install and tell me what happens.
assistant: *Bash: npm install* + reports output.
<reasoning>
Single command, immediate result.
</reasoning>
</example>

To LIST current tasks before deciding to update or create, call TodoRead first (zero-cost; reads the same file).`
}
func (Todo) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"content"},
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Advanced/optional. Internal task ID for direct update. Almost always you should OMIT this — metis matches by content (exact + normalised) when id is absent. Only pass id if you've explicitly retrieved it from a prior TodoWrite tool_result.",
			},
			"status":   map[string]any{"type": "string", "description": "in_progress | completed | pending. Only ONE in_progress at a time."},
			"content":  map[string]any{"type": "string", "description": "Task description. Keep stable across updates — changing the wording without passing `id` creates a duplicate."},
			"priority": map[string]any{"type": "string", "description": "high | medium | low"},
		},
	}
}
func (Todo) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

// CanUse: TodoWrite is purely an internal task-tracker — writes JSON to
// ~/.metis/tasks/, does nothing destructive externally. Default-allow
// so non-interactive `metis run` can let the LLM track progress
// without prompting. Users who want stricter gating can add a
// [[permission.deny]] rule for TodoWrite in config.toml.
func (t Todo) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "task-tracker (no external side effects)"
}

func (Todo) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	content, _ := in["content"].(string)
	if strings.TrimSpace(content) == "" {
		return &tools.Result{Output: "TodoWrite: content required", IsError: true}, nil
	}
	id, _ := in["id"].(string)
	status, _ := in["status"].(string)
	priority, _ := in["priority"].(string)

	saved, err := tasks.Upsert(tasks.CurrentSessionID(), tasks.Item{
		ID: id, Content: content, Status: status, Priority: priority,
	})
	if err != nil {
		return &tools.Result{Output: "TodoWrite: " + err.Error(), IsError: true}, nil
	}

	icon := "○"
	switch saved.Status {
	case "completed":
		icon = "●"
	case "in_progress":
		icon = "◐"
	}
	prioIcon := map[string]string{"high": "🔴", "medium": "🟡", "low": "🟢"}[saved.Priority]
	out := fmt.Sprintf("%s %s [%s] %s", icon, prioIcon, saved.ID, saved.Content)
	if nudge := todoVerifierNudge(saved.Status); nudge != "" {
		out += "\n\n" + nudge
	}
	return &tools.Result{Output: out}, nil
}

// todoVerifierNudge mirrors the TaskUpdate nudge in task.go but reads
// the lightweight `tasks` (TodoWrite) store instead of the structured
// TaskStore. The first claude-code-go-port test run (2026-05-17)
// showed the model preferring TodoWrite over TaskCreate, which
// completely bypassed the original TaskUpdate nudge — model finished
// Phase 1 (19 Go files, 2503 LOC, build/vet pass) and STOPPED there,
// because it interpreted "verify" as "I ran go build" rather than
// "spawn the verify subagent". Putting the nudge here closes the
// loop: whichever task store the model uses, hitting 3+ completed
// with no verify step triggers the same advisory text in the tool
// result.
//
// Heuristic for "verify step": item content contains
// verify/test/review/vet/lint/audit (case-insensitive). Cheaper
// than the TaskStore version since `tasks.Item` has no separate
// owner field.
func todoVerifierNudge(justSetStatus string) string {
	if justSetStatus != "completed" {
		return ""
	}
	tl, err := tasks.Load(tasks.CurrentSessionID())
	if err != nil || tl == nil {
		return ""
	}
	completed := 0
	hasVerify := false
	for _, it := range tl.Items {
		if isVerifyTodoContent(it.Content) {
			hasVerify = true
		}
		if it.Status == "completed" {
			completed++
		}
	}
	if completed < 3 || hasVerify {
		return ""
	}
	return "NUDGE: you just closed a third TodoWrite item and none of the\n" +
		"tracked items is a verify/test/review step. Per the dispatch\n" +
		"contract: before claiming this work done, spawn a real verifier:\n" +
		"`Agent({subagent_type: \"verify\", prompt: \"<what to check + the\n" +
		"VERDICT line you expect back>\"})`. Running `go build` yourself\n" +
		"is NOT verifying — only the verify subagent issues a VERDICT.\n" +
		"Your own \"build succeeded\" / \"looks good\" notes do NOT count.\n" +
		"If you skip this and stop here, the user gets a project with\n" +
		"zero tests, no git history, and no built binary — which is\n" +
		"exactly the failure mode the contract exists to prevent."
}

// isVerifyTodoContent — substring scan for verify-related keywords in
// a TodoWrite item's free-text content. Symmetric with task.go's
// isVerifyTask but operates on Content rather than Subject+Owner.
func isVerifyTodoContent(content string) bool {
	lc := strings.ToLower(content)
	for _, kw := range []string{"verify", "test", "review", "vet", "lint", "audit"} {
		if strings.Contains(lc, kw) {
			return true
		}
	}
	return false
}

// TodoRead lists every task in the current session's list. Pairs with
// TodoWrite — gives the LLM a way to see what's already in flight
// without re-emitting all rows.
type TodoRead struct {
	tools.BaseTool
	gate *permission.Gate
}

func (TodoRead) Name() string                                 { return "TodoRead" }
func (TodoRead) Description() string                          { return "List all tasks in the current session's task list." }
func (TodoRead) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (TodoRead) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t TodoRead) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "read-only"
}
func (TodoRead) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	tl, err := tasks.Load(tasks.CurrentSessionID())
	if err != nil {
		return &tools.Result{Output: "TodoRead: " + err.Error(), IsError: true}, nil
	}
	if len(tl.Items) == 0 {
		return &tools.Result{Output: "(no tasks yet — call TodoWrite to create one)"}, nil
	}
	// Format: `<icon> <status> <content>`. Matches claude-code's
	// TodoItemSchema (utils/todo/types.ts) which exposes only
	// content + status + activeForm — no id. metis still uses id
	// internally as a dedup key (image #50 fix), but the model
	// doesn't need to see it: TodoWrite's `id` is optional and
	// the Upsert path falls back to content-normalised matching
	// when omitted (tasks.go::normalizeTaskContent). Showing id
	// historically tempted models to copy-paste rather than rely
	// on content stability — and added visual noise.
	var b strings.Builder
	for _, it := range tl.Items {
		icon := "○"
		switch it.Status {
		case "completed":
			icon = "●"
		case "in_progress":
			icon = "◐"
		}
		fmt.Fprintf(&b, "%s %-11s  %s\n", icon, it.Status, it.Content)
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}
