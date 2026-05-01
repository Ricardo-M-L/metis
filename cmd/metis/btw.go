package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	llmpkg "github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// askSideQuestion implements the /btw side-question backend. Mirrors
// claude-code's runForkedAgent design:
//
//   - Reuses the parent's exact System prompt + recent message history
//     so the prompt-cache hit lets the call complete in ~one second.
//   - Maximum one assistant turn (Tools=nil; the LLM can't tool-call).
//   - Caps output to a small budget — these are quick clarifications,
//     not deep research.
//   - Result is ephemeral: not appended to the loop history, not
//     written to the session JSONL. Only displayed in the modal.
//
// The function is safe to call concurrently with an in-flight main
// turn — the agent.Loop is untouched here.
func (r *runtime) askSideQuestion(ctx context.Context, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("empty question")
	}
	if r.provider == nil {
		return "", fmt.Errorf("provider not initialized")
	}

	// Cap the work — side questions should never block the user for long.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Wrap the question with framing so the model knows it's a brief
	// aside, not the main task. Keeps replies tight.
	wrapped := "[side question — answer briefly, do not call tools]\n\n" + question

	hist := append([]llmpkg.Message(nil), r.loop.History()...)
	hist = append(hist, llmpkg.Message{
		Role:    llmpkg.RoleUser,
		Content: []llmpkg.ContentBlock{{Type: "text", Text: wrapped}},
	})

	req := provider.Request{
		Model:     r.model,
		System:    r.loop.System,
		Messages:  hist,
		MaxTokens: 1024,
		Stream:    false,
	}
	resp, err := r.provider.Complete(cctx, req)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("model returned no text")
	}
	return out, nil
}
