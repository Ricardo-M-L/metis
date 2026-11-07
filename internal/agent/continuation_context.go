package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"strings"
)

// AppendContext adds a durable internal event without impersonating user input.
// Like AppendUser, callers must serialize this with Run (idle-only).
func (l *Loop) AppendContext(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clearTurnMemoryLocked()
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: text, Synthetic: true}}})
}
