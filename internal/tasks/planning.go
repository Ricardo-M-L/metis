package tasks

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
	out := append([]Item(nil), todos.Items...)
	byContent := make(map[string]int, len(out))
	for i := range out {
		byContent[normalizeTaskContent(out[i].Content)] = i
	}

	store := TaskStoreForSession(sessionID)
	if store == nil {
		return out, nil
	}
	for _, task := range store.List(false) {
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
	return out, nil
}
