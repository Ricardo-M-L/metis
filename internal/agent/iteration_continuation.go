package agent

import (
	"context"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const iterationContinuationMessage = `<system-reminder>
<iteration-continuation>
This iteration segment is complete; continue the original task in this same run. Preserve all completed work, tool results, and verification evidence, and continue from the next unfinished step. Follow the original user instructions, permissions, and safety boundaries. Do not repeat completed tool calls or side effects merely because a new segment started. The segment boundary does not require a summary, a final answer, or context compaction. Give a final response only when the requested work is complete or a specific blocker prevents further safe progress; explain any such blocker and the remaining work honestly.
</iteration-continuation>
</system-reminder>`

// injectIterationContinuation is called only after a complete history
// checkpoint and the Run's cancellation/time/USD checks. It adds context in
// place; it never starts another Run or resets execution, guard or audit state.
func (l *Loop) injectIterationContinuation(ctx context.Context, out chan<- Event, segmentIters, runIters, outputTokens int) {
	l.mu.Lock()
	l.Messages = append(l.Messages, llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type: "text", Text: iterationContinuationMessage, Synthetic: true,
		}},
	})
	l.mu.Unlock()
	emit(ctx, out, Event{Kind: EventContextInjected, Source: "iteration_continuation", ContextText: iterationContinuationMessage})
	emit(ctx, out, Event{
		Kind:   EventInfo,
		Source: "iteration_continuation",
		Info:   fmt.Sprintf("iteration segment complete (%d iterations; %d iterations and %d output tokens in this run) — continuing the current task", segmentIters, runIters, outputTokens),
	})
}
