package agent

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/artifact"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// Bind once before model execution, while the originating owner is pinned.
// Tool results may use the user wire role, but can never supply this request.
func (l *Loop) bindArtifactEditContext(ctx context.Context) (context.Context, error) {
	l.mu.Lock()
	var latest []llm.Message
	for i := len(l.Messages) - 1; i >= 0; i-- {
		message := l.Messages[i]
		if message.Role != llm.RoleUser {
			continue
		}
		toolOutput := false
		userImage := false
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				toolOutput = true
				break
			}
			if block.Type == "image" && !block.Synthetic {
				userImage = true
			}
		}
		if !toolOutput && (visibleTextOf(message) != "" || userImage) {
			// An image-only request is a fresh turn too. Its empty text clears
			// the old annotation instead of searching older user history.
			latest = []llm.Message{message}
			break
		}
	}
	prompt := lastUserTextLocked(latest)
	l.mu.Unlock()
	return artifact.BindEditReference(ctx, tasks.SessionIDFromContext(ctx), prompt)
}
