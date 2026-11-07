package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// prepareTaskPlan binds tools and reminders to one work plan. Human input can
// retire a completed plan; host events and child runs cannot silently do so.
func (l *Loop) prepareTaskPlan(ctx context.Context) (context.Context, error) {
	l.mu.Lock()
	humanInput, previous := l.taskPlanInputPending, l.taskPlanID
	if !humanInput && len(l.Messages) > 0 {
		for _, block := range l.Messages[len(l.Messages)-1].Content {
			if block.Synthetic && block.ProviderHint[TaskPlanHint] != "" {
				previous = block.ProviderHint[TaskPlanHint]
			}
		}
	}
	l.taskPlanInputPending = false
	l.mu.Unlock()
	if tasks.HasPlanContext(ctx) {
		return ctx, nil
	}
	sid := tasks.SessionIDFromContext(ctx)
	if sid == "" {
		return ctx, nil
	}
	var id string
	var err error
	if humanInput {
		id, err = tasks.BeginUserWork(sid)
	} else if previous != "" {
		id = previous
	} else {
		id, err = tasks.EnsureCurrentPlan(sid)
	}
	if err != nil {
		l.mu.Lock()
		l.taskPlanInputPending = humanInput
		l.mu.Unlock()
		return ctx, err
	}
	return tasks.WithPlanID(ctx, id), nil
}

// Preserve the append-only transcript while explicitly superseding old task
// reminders after a plan switch. Historical host reminders must not ask the
// model to copy an archived plan back into the current one.
func (l *Loop) syncTaskPlanNotice(ctx context.Context) {
	id := tasks.PlanIDFromContext(ctx)
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.taskPlanID == id {
		return
	}
	notice := l.taskPlanID != ""
	if !notice {
		for _, message := range l.Messages {
			for _, block := range message.Content {
				if strings.HasPrefix(block.Text, "<system-reminder>") && strings.Contains(block.Text, "Current unified task list:") {
					notice = true
				}
			}
		}
	}
	l.taskPlanID = id
	if notice {
		l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Type: "text", Synthetic: true,
			Text:         fmt.Sprintf("<current_task_plan id=%q>\nThis turn belongs to this work plan. Task lists and reminders from other plans are historical. Use TodoRead or TaskList for this plan; do not copy or mark archived tasks completed merely to satisfy an earlier reminder.\n</current_task_plan>", id),
			ProviderHint: map[string]string{TaskPlanHint: id},
		}}})
	}
}
