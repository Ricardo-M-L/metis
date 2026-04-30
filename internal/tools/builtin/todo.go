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
type Todo struct{ gate *permission.Gate }

func (Todo) Name() string { return "TodoWrite" }
func (Todo) Description() string {
	return "Create or update a task in the session task list. Persists to ~/.metis/tasks/."
}
func (Todo) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"content"},
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Optional id to update; if absent we match by content or append.",
			},
			"status":   map[string]any{"type": "string", "description": "in_progress | completed | pending"},
			"content":  map[string]any{"type": "string"},
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
	return &tools.Result{Output: fmt.Sprintf("%s %s [%s] %s", icon, prioIcon, saved.ID, saved.Content)}, nil
}

// TodoRead lists every task in the current session's list. Pairs with
// TodoWrite — gives the LLM a way to see what's already in flight
// without re-emitting all rows.
type TodoRead struct{ gate *permission.Gate }

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
	var b strings.Builder
	for _, it := range tl.Items {
		icon := "○"
		switch it.Status {
		case "completed":
			icon = "●"
		case "in_progress":
			icon = "◐"
		}
		fmt.Fprintf(&b, "%s [%s] %-10s  %s\n", icon, it.ID, it.Status, it.Content)
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}
