package webui

import (
	"time"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// planningStatus is a display-only view. Saved tasks remain available to the
// model, reminders and history, including completed work from previous turns.
func planningStatus(sessionID string) (items []tasks.Item, turn int, startedAt string) {
	view, turn, startedAt := planningStatusPresentation(sessionID)
	items = append(items, view.Steps...)
	items = append(items, view.ExecutionItems...)
	return items, turn, startedAt
}

// The primary plan and its additional execution records share one snapshot and
// one input boundary. Keeping both groups here avoids mixing two worker writes
// while building a status response.
func planningStatusPresentation(sessionID string) (view tasks.PlanPresentation, turn int, startedAt string) {
	view = tasks.PlanPresentation{Steps: []tasks.Item{}, ExecutionItems: []tasks.Item{}}
	if sessionID == "" {
		return view, 0, ""
	}
	view, _ = tasks.PlanningPresentationSnapshot(sessionID)
	trace := rtpkg.CurrentTraceStore()
	if trace == nil {
		return view, 0, ""
	}
	// Desktop workers write from a different process. Import only their new
	// JSONL tail before consulting the cached input boundary.
	if err := trace.Refresh(sessionID); err != nil {
		return view, 0, ""
	}
	turn, start := trace.LatestInputBoundary(sessionID)
	if turn <= 0 || start.IsZero() {
		return view, 0, ""
	}
	visible := func(items []tasks.Item) []tasks.Item {
		out := make([]tasks.Item, 0, len(items))
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
			out = append(out, item)
		}
		return out
	}
	view.Steps = visible(view.Steps)
	view.ExecutionItems = visible(view.ExecutionItems)
	return view, turn, start.Format(time.RFC3339Nano)
}
