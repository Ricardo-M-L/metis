package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/timebudget"
)

const maxSubAgentOutputContinuations = 3

type subAgentTextContinuationKey struct{}
type subAgentOutputTurnKey struct{}

// One child task retains execution and acceptance ownership across the bounded
// text generations. These values must not become fresh Run-local obligations.
type subAgentOutputTurnState struct {
	finiteJobs           *finiteJobTracker
	awaitedJobs          map[string]struct{}
	deadline             time.Time
	verificationAttempts int
	acceptedText         string
	previousText         string
}

func subAgentOutputTurn(ctx context.Context) *subAgentOutputTurnState {
	state, _ := ctx.Value(subAgentOutputTurnKey{}).(*subAgentOutputTurnState)
	return state
}

func (l *Loop) drainSteerAtCompletion(ctx context.Context, closeInput bool) string {
	// Parent input remains on the durable continuation queue until the outer
	// owner can run it with its original tools. A text-only segment must not
	// checkpoint it as consumed and then reject it at its one-request guard.
	if subAgentTextContinuation(ctx) {
		return ""
	}
	if closeInput {
		return l.drainSteerOrClose()
	}
	return l.drainSteer()
}

func subAgentTextContinuation(ctx context.Context) bool {
	value, _ := ctx.Value(subAgentTextContinuationKey{}).(bool)
	return value
}

func outputLimitStop(reason string) bool {
	switch reason {
	case "max_tokens", "length", "max_output_tokens":
		return true
	}
	return false
}

type subAgentOutputSegment struct {
	stop       string
	text       string
	unsafeCall bool
	terminal   Event
}

// A model may restart its whole prior partial answer despite the continuation
// instruction. Hold only that matching prefix, and drop it only after the
// complete previous segment has matched. Distinct new text streams immediately.
type outputContinuationPrefix struct {
	prior   []string
	pending string
	decided bool
}

func (p *outputContinuationPrefix) write(text string) string {
	if p.decided || len(p.prior) == 0 {
		return text
	}
	p.pending += text
	for _, prior := range p.prior {
		if len(p.pending) < len(prior) && strings.HasPrefix(prior, p.pending) {
			return ""
		}
	}
	p.decided = true
	value := p.pending
	matched := 0
	for _, prior := range p.prior {
		if len(prior) > matched && strings.HasPrefix(value, prior) {
			matched = len(prior)
		}
	}
	value = value[matched:]
	p.pending = ""
	return value
}

func (p *outputContinuationPrefix) finish() string {
	if p.decided {
		return ""
	}
	// A short matching start is not a complete replay and must be preserved.
	value := p.pending
	p.pending = ""
	p.decided = true
	for _, prior := range p.prior {
		if value == prior {
			return ""
		}
	}
	return value
}

func outputContinuationPrefixes(ctx context.Context) []string {
	state := subAgentOutputTurn(ctx)
	if !subAgentTextContinuation(ctx) || state == nil {
		return nil
	}
	var prior []string
	if state.acceptedText != "" {
		prior = append(prior, state.acceptedText)
	}
	if state.previousText != "" && state.previousText != state.acceptedText {
		prior = append(prior, state.previousText)
	}
	return prior
}

// Normalize canonical assistant text before history checkpointing with exactly
// the same complete-prefix rule used by the live event relay. Physical usage
// still reflects the actual provider response; provider-managed state for the
// unnormalized response may not be reused after a canonical projection change.
func normalizeSubAgentOutputAssistant(ctx context.Context, content []llm.ContentBlock) ([]llm.ContentBlock, bool) {
	prior := outputContinuationPrefixes(ctx)
	if len(prior) == 0 {
		return content, false
	}
	var raw strings.Builder
	for _, block := range content {
		if block.Type == "text" {
			raw.WriteString(block.Text)
		}
	}
	prefix := outputContinuationPrefix{prior: prior}
	text := prefix.write(raw.String()) + prefix.finish()
	if text == raw.String() {
		return content, false
	}
	result := make([]llm.ContentBlock, 0, len(content))
	written := false
	for _, block := range content {
		if block.Type == "provider_state" {
			continue
		}
		if block.Type == "text" {
			if written {
				continue
			}
			written = true
			block.Text = text
			if text == "" {
				continue
			}
		}
		result = append(result, block)
	}
	return result, true
}

func forwardSubAgentOutputEvent(ctx context.Context, out chan<- Event, event Event) {
	if out == nil {
		return
	}
	// Loop.emit already published the event to the trace hook. A relay must
	// never call emit again, or physical usage and trace rows would be doubled.
	if event.Kind == EventTokens && tryEmitEvent(out, event) {
		return
	}
	select {
	case out <- event:
	case <-ctx.Done():
		if event.Kind == EventTokens {
			tryEmitEvent(out, event)
		}
	}
}

func runSubAgentOutputSegment(ctx context.Context, out chan<- Event, loop *Loop) (segment subAgentOutputSegment, runErr error) {
	events := make(chan Event, 64)
	done := make(chan error, 1)
	go func() {
		defer close(events)
		defer func() {
			if value := recover(); value != nil {
				done <- fmt.Errorf("sub-agent run panic: %v", value)
			}
		}()
		done <- loop.Run(ctx, events)
	}()
	var text strings.Builder
	prefix := outputContinuationPrefix{prior: outputContinuationPrefixes(ctx)}
	for event := range events {
		if event.Kind == EventLoopDone {
			segment.stop, segment.terminal = event.StopReason, event
			continue
		}
		if event.Kind == EventInfo && event.Source == "incomplete_tool_call" {
			segment.unsafeCall = true
		}
		if event.Kind == EventTextDelta {
			event.TextDelta = prefix.write(event.TextDelta)
			if event.TextDelta == "" {
				continue
			}
			text.WriteString(event.TextDelta)
		}
		forwardSubAgentOutputEvent(ctx, out, event)
	}
	if tail := prefix.finish(); tail != "" {
		text.WriteString(tail)
		forwardSubAgentOutputEvent(ctx, out, Event{Kind: EventTextDelta, TextDelta: tail})
	}
	segment.text = text.String()
	if segment.terminal.Kind != EventLoopDone {
		segment.stop = "provider_incomplete"
		segment.terminal = Event{Kind: EventLoopDone, StopReason: segment.stop}
	}
	return segment, <-done
}

// runSubAgentOutputRecovery owns every automatic request after a length stop.
// It is separate from parent-directed continuation: no tools, new producer,
// permission upgrade, cap escalation, or external-effect replay is permitted.
func runSubAgentOutputRecovery(ctx context.Context, out chan<- Event, loop *Loop, teammate *Teammate, transcript *SubAgentTranscript) (runErr error) {
	started := time.Now()
	turnBudget, budgetErr := timebudget.FromEnv("METIS_TURN_MAX_SECONDS")
	if budgetErr != nil {
		return budgetErr
	}
	state := &subAgentOutputTurnState{finiteJobs: &finiteJobTracker{pool: loop.Jobs}, awaitedJobs: make(map[string]struct{})}
	ctx = context.WithValue(ctx, subAgentOutputTurnKey{}, state)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := state.finiteJobs.cleanup(cleanupCtx); err != nil {
			err = fmt.Errorf("finite background job cleanup: %w", err)
			runErr = errors.Join(runErr, err)
			emit(ctx, out, Event{Kind: EventError, Err: err})
		}
	}()
	loop.mu.RLock()
	initialIteration := loop.iterIdx
	loop.mu.RUnlock()
	if teammate != nil {
		defer teammate.setOutputRecovery(false, -1)
	}
	initialAttempts := 0
	if teammate != nil {
		initialAttempts = teammate.Snapshot().ContinuationAttempts
	}
	for attempt := 0; ; attempt++ {
		requestCtx := ctx
		if attempt > 0 {
			requestCtx = context.WithValue(ctx, subAgentTextContinuationKey{}, true)
		}
		segment, runErr := runSubAgentOutputSegment(requestCtx, out, loop)
		if runErr != nil {
			return runErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A repeated-only natural stop cannot turn the original unfinished
		// answer into a success. Preserve the output-limit failure truth.
		if attempt > 0 && strings.TrimSpace(segment.text) == "" && !IsIncompleteStopReason(segment.stop) {
			segment.stop = "max_tokens"
			segment.terminal.StopReason = segment.stop
		}
		loop.mu.RLock()
		iterations := loop.iterIdx - initialIteration
		loop.mu.RUnlock()
		canContinue := outputLimitStop(segment.stop) && !segment.unsafeCall &&
			strings.TrimSpace(segment.text) != "" && initialAttempts+attempt < maxSubAgentOutputContinuations &&
			(loop.MaxIters <= 0 || iterations < loop.MaxIters) &&
			(turnBudget <= 0 || time.Since(started) < turnBudget)
		// Text cannot establish an outstanding asynchronous check. Keep its
		// real failed/incomplete length result rather than dropping job evidence
		// on re-entry or pretending a killed required job passed.
		if reason, _ := state.finiteJobs.completionStatus(); reason != "" {
			canContinue = false
		}
		// Explicit parent input wins over automatic text-only continuation. It
		// remains on the existing durable queue and is drained by the sole runner.
		if transcript != nil && len(transcript.PendingFollowups()) != 0 {
			canContinue = false
		}
		if !canContinue {
			forwardSubAgentOutputEvent(ctx, out, segment.terminal)
			return nil
		}
		if teammate != nil {
			teammate.setOutputRecovery(true, initialAttempts+attempt+1)
		}
		state.acceptedText += segment.text
		state.previousText = segment.text
		// The initial Run can contain commentary before a tool and a later
		// truncated answer. Match its last canonical assistant body as well as
		// the cumulative visible output, rather than merging those generations.
		history := loop.History()
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role == llm.RoleAssistant {
				state.previousText = assistantText(history[i].Content)
				break
			}
		}
		continuationNumber := initialAttempts + attempt + 1
		body := fmt.Sprintf("<system-reminder>\n<subagent_output_continuation reason=%q attempt=\"%d\" max_attempts=\"%d\"/>\nThe previous response reached its output limit before finishing. Continue the remaining answer from the exact stopping point; do not repeat earlier text, restart the task, or claim unperformed work. This is a text-only continuation: no tools may be called or described as executed. Preserve the original task and finish concisely. If more tool work is required, say that the task remains incomplete.\n</system-reminder>", segment.stop, continuationNumber, maxSubAgentOutputContinuations)
		loop.mu.Lock()
		loop.Messages = append(loop.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: body, ProviderHint: map[string]string{"metis.output_continuation": segment.stop, "metis.output_continuation_attempt": fmt.Sprint(continuationNumber)}}}})
		loop.mu.Unlock()
		emit(ctx, out, Event{Kind: EventContextInjected, Source: "recovery", ContextText: body})
		if transcript != nil {
			if err := transcript.Checkpoint(loop.History()); err != nil {
				return fmt.Errorf("checkpoint output continuation: %w", err)
			}
		}
		emit(ctx, out, Event{Kind: EventInfo, Source: "output_continuation", Info: fmt.Sprintf("(sub-agent output limit reached; continuing text-only, attempt %d/%d)", continuationNumber, maxSubAgentOutputContinuations)})
	}
}
