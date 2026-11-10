package tasks

// PlanPresentation separates the declared steps from additional execution
// records without changing the canonical plan consumed by runtime checks.
type PlanPresentation struct {
	Steps          []Item
	ExecutionItems []Item
}

func PlanningPresentation(sessionID string) (PlanPresentation, error) {
	return planningPresentation(sessionID, false)
}

func PlanningPresentationSnapshot(sessionID string) (PlanPresentation, error) {
	return planningPresentation(sessionID, true)
}

func planningPresentation(sessionID string, snapshot bool) (PlanPresentation, error) {
	view := PlanPresentation{Steps: []Item{}, ExecutionItems: []Item{}}
	todos, err := Load(sessionID)
	if err != nil {
		return view, err
	}
	items := planningItemsForList(todos, todos.PlanID, snapshot)
	primary := len(todos.Items)
	if primary == 0 {
		// A Task*-only plan still needs an ordinary visible progress list.
		view.Steps = append(view.Steps, items...)
		return view, nil
	}
	// mergePlanningItems preserves the compact list's identities and order,
	// then appends only unmatched structured records. Partition that result,
	// rather than guessing whether differently worded tasks mean the same thing.
	view.Steps = append(view.Steps, items[:primary]...)
	view.ExecutionItems = append(view.ExecutionItems, items[primary:]...)
	return view, nil
}

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
	return planningItemsForList(todos, todos.PlanID, false), nil
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
	return planningItemsForList(todos, todos.PlanID, true), nil
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
