package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"testing"
)

func TestContinuationAppendContextIsDurableNotUser(t *testing.T) {
	loop := &Loop{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "authorized original task"}}}}}
	loop.AppendContext("[scheduled wakeup] check progress")
	messages := loop.Messages
	if len(messages) != 2 || messages[1].Role != llm.RoleUser || !messages[1].Content[0].Synthetic || messages[1].Content[0].Text != "[scheduled wakeup] check progress" {
		t.Fatalf("wakeup impersonates user or lost: %+v", messages)
	}
}
