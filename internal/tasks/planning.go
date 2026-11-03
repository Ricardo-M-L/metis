package tasks

import "strings"

// PlanningItems is the canonical read projection for task progress. METIS has
// two persisted input formats for backward compatibility (TodoWrite's compact
// list and Task*'s structured owner/dependency records); every UI and reminder
// consumes this merged projection so the formats cannot produce two different
// views of the same session.
func PlanningItems(sessionID string) ([]Item, error) {
	todos, err := Load(sessionID)
	if err != nil {
		return nil, err
	}
	var structured []*Task
	if store := TaskStoreForSession(sessionID); store != nil {
		structured = store.List(false)
	}
	return mergePlanningItems(todos.Items, structured), nil
}

// PlanningItemsSnapshot reads the display projection from disk, including
// writes made by Desktop workers in other processes. It never replaces or
// mutates an active writer's cached TaskStore. Both read paths use the same
// merge rules so refreshing the UI cannot change reminder/runtime semantics.
func PlanningItemsSnapshot(sessionID string) ([]Item, error) {
	todos, err := Load(sessionID)
	if err != nil {
		return nil, err
	}
	var structured []*Task
	if strings.TrimSpace(sessionID) != "" {
		// Construction only loads the existing file; it does not create or
		// persist a store until a mutating task operation is requested.
		structured = NewTaskStore(sessionID).List(false)
	}
	return mergePlanningItems(todos.Items, structured), nil
}

func mergePlanningItems(todos []Item, structured []*Task) []Item {
	out := append([]Item(nil), todos...)
	byContent := make(map[string]int, len(out))
	for i := range out {
		byContent[normalizeTaskContent(out[i].Content)] = i
	}

	for _, task := range structured {
		key := normalizeTaskContent(task.Subject)
		if index, ok := byContent[key]; ok {
			// Whichever representation was updated most recently owns the
			// lifecycle state; structured ownership is useful regardless.
			if task.UpdatedAt.After(out[index].UpdatedAt) {
				out[index].Status = string(task.Status)
				out[index].UpdatedAt = task.UpdatedAt
			}
			if task.Owner != "" {
				out[index].Owner = task.Owner
			}
			continue
		}
		item := Item{
			ID:        "task-" + task.ID,
			Content:   task.Subject,
			Status:    string(task.Status),
			Priority:  "medium",
			Owner:     task.Owner,
			CreatedAt: task.CreatedAt,
			UpdatedAt: task.UpdatedAt,
		}
		byContent[key] = len(out)
		out = append(out, item)
	}
	return out
}
