// Package preview demonstrates a small, deterministic report worker.
// It performs no file writes, network requests or command execution.
package preview

import (
	"context"
	"fmt"
	"strings"
)

// Task is one item in the delivery checklist.
type Task struct {
	Title string
	Owner string
	Done  bool
}

// Result contains the reader-facing summary of a task.
type Result struct {
	Title   string
	Summary string
}

// Process formats tasks in input order and respects cancellation.
func Process(ctx context.Context, tasks []Task) ([]Result, error) {
	results := make([]Result, 0, len(tasks))
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		title := strings.TrimSpace(task.Title)
		if title == "" {
			continue
		}
		owner := strings.TrimSpace(task.Owner)
		if owner == "" {
			owner = "交付小组"
		}
		status := "待复核"
		if task.Done {
			status = "已完成"
		}
		results = append(results, Result{
			Title:   title,
			Summary: fmt.Sprintf("%s · %s · %s", status, title, owner),
		})
	}
	return results, nil
}
