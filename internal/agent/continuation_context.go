package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"strings"
)

// AppendContext adds a durable internal event without impersonating user input.
// Like AppendUser, callers must serialize this with Run (idle-only).
func (l *Loop) AppendContext(text string) {
	l.AppendContextForTaskPlan(text, "")
}

// TaskPlanHint is host metadata persisted with synthetic continuations so a
// resumed worker keeps the work plan that scheduled it.
const TaskPlanHint = "metis.task_plan"

func (l *Loop) AppendContextForTaskPlan(text, planID string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clearTurnMemoryLocked()
	block := llm.ContentBlock{Type: "text", Text: text, Synthetic: true}
	if planID != "" {
		block.ProviderHint = map[string]string{TaskPlanHint: planID}
	}
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{block}})
}
