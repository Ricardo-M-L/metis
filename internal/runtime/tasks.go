package runtime

// Task persistence moved to internal/tasks so the builtin TodoWrite
// tool can call it without creating a runtime ↔ tools cycle. This file
// re-exports the surface for runtime callers (auth gate / setupRuntime
// helpers) so they don't need a second import.

import "github.com/Ricardo-M-L/metis/internal/tasks"

type (
	TaskItem = tasks.Item
	TaskList = tasks.List
)

// SetCurrentSessionID forwards to internal/tasks. Called by setupRuntime
// once the session id (resumed or fresh) is final.
func SetCurrentSessionID(id string) { tasks.SetCurrentSessionID(id) }

// CurrentSessionID returns whatever setupRuntime told the tasks pkg.
func CurrentSessionID() string { return tasks.CurrentSessionID() }

// LoadTasks reads the per-session task list.
func LoadTasks(sessionID string) (*TaskList, error) { return tasks.Load(sessionID) }

// SaveTasks writes the per-session task list.
func SaveTasks(tl *TaskList) error { return tasks.Save(tl) }

// UpsertTask is the canonical mutate-or-create path used by TodoWrite.
func UpsertTask(sessionID string, in TaskItem) (TaskItem, error) {
	return tasks.Upsert(sessionID, in)
}

// TasksDir returns the directory where session task files live.
func TasksDir() string { return tasks.Dir() }
