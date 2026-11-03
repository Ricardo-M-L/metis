package webui

import (
	"time"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// planningStatus is a display-only view. Saved tasks remain available to the
// model, reminders and history, including completed work from previous turns.
func planningStatus(sessionID string) (items []tasks.Item, turn int, startedAt string) {
	if sessionID == "" {
		return nil, 0, ""
	}
	items, _ = tasks.PlanningItemsSnapshot(sessionID)
	trace := rtpkg.CurrentTraceStore()
	if trace == nil {
		return items, 0, ""
	}
	// Desktop workers write from a different process. Import only their new
	// JSONL tail before consulting the cached input boundary.
	if err := trace.Refresh(sessionID); err != nil {
		return items, 0, ""
	}
	turn, start := trace.LatestInputBoundary(sessionID)
	if turn <= 0 || start.IsZero() {
		return items, 0, ""
	}
	visible := make([]tasks.Item, 0, len(items))
	for _, item := range items {
		updated := item.UpdatedAt
		if updated.IsZero() {
			updated = item.CreatedAt
		}
		// Unknown legacy timestamps cannot prove completion in this task.
		// Hide only the display row when a reliable task boundary is known.
		if item.Status == "completed" && (updated.IsZero() || updated.Before(start)) {
			continue
		}
		visible = append(visible, item)
	}
	return visible, turn, start.Format(time.RFC3339Nano)
}
