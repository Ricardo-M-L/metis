package builtin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	taskstore "github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// 6 task-management tools wrap runtime.TaskStore. The store is per-session
// (set by setupRuntime) — these tools fetch the *current* store via
// runtime.CurrentTaskStore() at call time, so they don't need the store
// passed in to construction.
//
// Mirrors claude-code's Task tools layout (one tool per CRUD operation).
// All output is JSON so the LLM can parse + reason cheaply.

func currentStoreOrErr() (*taskstore.TaskStore, error) {
	s := taskstore.CurrentTaskStore()
	if s == nil {
		return nil, errors.New("no task store for the current session")
	}
	return s, nil
}

// --- TaskCreate ---

type TaskCreate struct{ tools.BaseTool; gate *permission.Gate }

func (TaskCreate) Name() string { return "TaskCreate" }
func (TaskCreate) Description() string {
	return "Create a structured task entry. Use this to plan multi-step work; mark in_progress when starting and completed when done."
}
func (TaskCreate) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"subject", "description"},
		"properties": map[string]any{
			"subject":     map[string]any{"type": "string", "description": "brief title (imperative)"},
			"description": map[string]any{"type": "string", "description": "what needs to be done"},
			"activeForm":  map[string]any{"type": "string", "description": "present-continuous form for the spinner (e.g. \"Running tests\")"},
			"metadata":    map[string]any{"type": "object", "description": "arbitrary metadata"},
		},
	}
}
func (TaskCreate) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (t TaskCreate) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := t.gate.Check(context.Background(), "TaskCreate", "")
	return mapDecision(d), src
}
func (t TaskCreate) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store, err := currentStoreOrErr()
	if err != nil {
		return nil, err
	}
	subj, _ := in["subject"].(string)
	desc, _ := in["description"].(string)
	active, _ := in["activeForm"].(string)
	var meta map[string]any
	if m, ok := in["metadata"].(map[string]any); ok {
		meta = m
	}
	tk, err := store.Create(subj, desc, active, meta)
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: fmt.Sprintf("Task #%s created successfully: %s", tk.ID, tk.Subject)}, nil
}

// --- TaskGet ---

type TaskGet struct{ tools.BaseTool; gate *permission.Gate }

func (TaskGet) Name() string { return "TaskGet" }
func (TaskGet) Description() string {
	return "Get full details for a task by id, including description, status, output buffer, and dependencies."
}
func (TaskGet) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"taskId"},
		"properties": map[string]any{
			"taskId": map[string]any{"type": "string", "description": "task id to fetch"},
		},
	}
}
func (TaskGet) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (t TaskGet) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := t.gate.Check(context.Background(), "TaskGet", "")
	return mapDecision(d), src
}
func (t TaskGet) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store, err := currentStoreOrErr()
	if err != nil {
		return nil, err
	}
	id, _ := in["taskId"].(string)
	tk, ok := store.Get(id)
	if !ok {
		return nil, fmt.Errorf("task %q not found", id)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "id:          %s\n", tk.ID)
	fmt.Fprintf(&b, "subject:     %s\n", tk.Subject)
	fmt.Fprintf(&b, "description: %s\n", tk.Description)
	if tk.ActiveForm != "" {
		fmt.Fprintf(&b, "activeForm:  %s\n", tk.ActiveForm)
	}
	fmt.Fprintf(&b, "status:      %s\n", tk.Status)
	if tk.Owner != "" {
		fmt.Fprintf(&b, "owner:       %s\n", tk.Owner)
	}
	if len(tk.Blocks) > 0 {
		fmt.Fprintf(&b, "blocks:      %s\n", strings.Join(tk.Blocks, ", "))
	}
	if len(tk.BlockedBy) > 0 {
		fmt.Fprintf(&b, "blockedBy:   %s\n", strings.Join(tk.BlockedBy, ", "))
	}
	fmt.Fprintf(&b, "createdAt:   %s\n", tk.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(&b, "updatedAt:   %s\n", tk.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
	if tk.Output != "" {
		b.WriteString("\noutput:\n")
		b.WriteString(tk.Output)
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// --- TaskList ---

type TaskList struct{ tools.BaseTool; gate *permission.Gate }

func (TaskList) Name() string { return "TaskList" }
func (TaskList) Description() string {
	return "List all tasks in id order. Returns id + subject + status + owner + blockedBy summary per row."
}
func (TaskList) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"includeDeleted": map[string]any{"type": "boolean", "description": "include tombstoned tasks (default false)"},
		},
	}
}
func (TaskList) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (t TaskList) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := t.gate.Check(context.Background(), "TaskList", "")
	return mapDecision(d), src
}
func (t TaskList) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store, err := currentStoreOrErr()
	if err != nil {
		return nil, err
	}
	includeDel, _ := in["includeDeleted"].(bool)
	all := store.List(includeDel)
	if len(all) == 0 {
		return &tools.Result{Output: "(no tasks)"}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s):\n", len(all))
	for _, tk := range all {
		blockedBy := ""
		if len(tk.BlockedBy) > 0 {
			blockedBy = " (blockedBy: " + strings.Join(tk.BlockedBy, ",") + ")"
		}
		owner := ""
		if tk.Owner != "" {
			owner = " <" + tk.Owner + ">"
		}
		fmt.Fprintf(&b, "  #%-3s  %-12s%s  %s%s\n", tk.ID, tk.Status, owner, tk.Subject, blockedBy)
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// --- TaskUpdate ---

type TaskUpdate struct{ tools.BaseTool; gate *permission.Gate }

func (TaskUpdate) Name() string { return "TaskUpdate" }
func (TaskUpdate) Description() string {
	return "Update a task. Common patches: status (pending|in_progress|completed|deleted), description, owner. Pair with TaskCreate when planning."
}
func (TaskUpdate) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"taskId"},
		"properties": map[string]any{
			"taskId":       map[string]any{"type": "string"},
			"status":       map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "deleted"}},
			"subject":      map[string]any{"type": "string"},
			"description":  map[string]any{"type": "string"},
			"activeForm":   map[string]any{"type": "string"},
			"owner":        map[string]any{"type": "string"},
			"addBlocks":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"addBlockedBy": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
}
func (TaskUpdate) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (t TaskUpdate) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := t.gate.Check(context.Background(), "TaskUpdate", "")
	return mapDecision(d), src
}
func (t TaskUpdate) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store, err := currentStoreOrErr()
	if err != nil {
		return nil, err
	}
	id, _ := in["taskId"].(string)
	patch := taskstore.TaskPatch{}
	if s, ok := in["status"].(string); ok && s != "" {
		st := taskstore.TaskStatus(s)
		patch.Status = &st
	}
	if s, ok := in["subject"].(string); ok && s != "" {
		patch.Subject = &s
	}
	if s, ok := in["description"].(string); ok {
		patch.Description = &s
	}
	if s, ok := in["activeForm"].(string); ok {
		patch.ActiveForm = &s
	}
	if s, ok := in["owner"].(string); ok {
		patch.Owner = &s
	}
	patch.AddBlocks = stringSlice(in["addBlocks"])
	patch.AddBlockedBy = stringSlice(in["addBlockedBy"])
	tk, err := store.Update(id, patch)
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: fmt.Sprintf("Updated task #%s status", tk.ID)}, nil
}

// --- TaskOutput ---

type TaskOutput struct{ tools.BaseTool; gate *permission.Gate }

func (TaskOutput) Name() string { return "TaskOutput" }
func (TaskOutput) Description() string {
	return "Append output to a task's buffer (e.g. command results, log lines). Useful for streaming progress reports."
}
func (TaskOutput) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"taskId", "output"},
		"properties": map[string]any{
			"taskId": map[string]any{"type": "string"},
			"output": map[string]any{"type": "string"},
		},
	}
}
func (TaskOutput) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (t TaskOutput) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := t.gate.Check(context.Background(), "TaskOutput", "")
	return mapDecision(d), src
}
func (t TaskOutput) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store, err := currentStoreOrErr()
	if err != nil {
		return nil, err
	}
	id, _ := in["taskId"].(string)
	out, _ := in["output"].(string)
	if out == "" {
		return nil, fmt.Errorf("output is required")
	}
	if _, err := store.Update(id, taskstore.TaskPatch{AppendOutput: out}); err != nil {
		return nil, err
	}
	return &tools.Result{Output: fmt.Sprintf("Appended %d bytes of output to task #%s", len(out), id)}, nil
}

// --- TaskStop ---

type TaskStop struct{ tools.BaseTool; gate *permission.Gate }

func (TaskStop) Name() string { return "TaskStop" }
func (TaskStop) Description() string {
	return "Mark a task as deleted. The task is kept on disk but excluded from default List output."
}
func (TaskStop) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"taskId"},
		"properties": map[string]any{
			"taskId": map[string]any{"type": "string"},
		},
	}
}
func (TaskStop) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (t TaskStop) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := t.gate.Check(context.Background(), "TaskStop", "")
	return mapDecision(d), src
}
func (t TaskStop) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store, err := currentStoreOrErr()
	if err != nil {
		return nil, err
	}
	id, _ := in["taskId"].(string)
	st := taskstore.TaskDeleted
	if _, err := store.Update(id, taskstore.TaskPatch{Status: &st}); err != nil {
		return nil, err
	}
	return &tools.Result{Output: fmt.Sprintf("Task #%s deleted", id)}, nil
}

// stringSlice extracts a []string from an interface that may be []any
// (json input always uses []any). Other shapes return nil.
func stringSlice(v any) []string {
	if v == nil {
		return nil
	}
	if s, ok := v.([]string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// taskNameSorter ensures deterministic test fixtures across go versions.
var _ = sort.Strings
