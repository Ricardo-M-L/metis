package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrSubAgentContinuationClosed = errors.New("sub-agent continuation is closing; wait for completion before resuming")
	ErrSubAgentContinuationOwner  = errors.New("only the direct parent may continue this sub-agent")
)

// SubAgentContinuationReceipt acknowledges durable parent input, not task
// completion. Queued input survives cancellation in the child transcript.
type SubAgentContinuationReceipt struct {
	AgentID string `json:"agent_id"`
	InputID string `json:"input_id"`
	Status  string `json:"status"`
	Durable bool   `json:"durable"`
}

type subAgentContinuation struct {
	mu         sync.Mutex
	parentID   string
	loop       *Loop
	transcript *SubAgentTranscript
	running    bool
	closed     bool
}

// BindContinuation attaches an immutable direct parent and its owned producer.
// Call after persistence and loop construction, before publishing the runner.
func (t *Teammate) BindContinuation(parentID string, loop *Loop, transcript *SubAgentTranscript) error {
	if t == nil || strings.TrimSpace(parentID) == "" || loop == nil || transcript == nil {
		return errors.New("parent, child loop, and durable transcript are required for continuation")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.continuation != nil || t.doneClosed || t.cancelRequested || !t.Status.IsActive() {
		return ErrSubAgentContinuationClosed
	}
	t.continuation = &subAgentContinuation{parentID: parentID, loop: loop, transcript: transcript}
	return nil
}

// SubmitContinuation records input before acknowledging it, and steers the
// same producer when its current Run still accepts input. The runner drains a
// final-race queued input itself, without creating a second producer.
func (t *Teammate) SubmitContinuation(parentID, text string) (SubAgentContinuationReceipt, error) {
	// Match Loop.SteerInject's normalization before journaling; otherwise a
	// trailing newline could make the checkpoint look different from delivery.
	text = strings.TrimSpace(text)
	if t == nil || text == "" {
		return SubAgentContinuationReceipt{}, errors.New("non-empty parent follow-up is required")
	}
	t.mu.RLock()
	continuation := t.continuation
	stopping := t.doneClosed || t.cancelRequested || !t.Status.IsActive()
	agentID := t.AgentID
	t.mu.RUnlock()
	if continuation == nil || stopping {
		return SubAgentContinuationReceipt{}, ErrSubAgentContinuationClosed
	}
	continuation.mu.Lock()
	defer continuation.mu.Unlock()
	if parentID == "" || continuation.parentID != parentID {
		return SubAgentContinuationReceipt{}, ErrSubAgentContinuationOwner
	}
	if continuation.closed {
		return SubAgentContinuationReceipt{}, ErrSubAgentContinuationClosed
	}
	input, err := continuation.transcript.EnqueueFollowup(text)
	if err != nil {
		return SubAgentContinuationReceipt{}, fmt.Errorf("persist parent follow-up: %w", err)
	}
	status := "queued"
	if continuation.loop.SteerInject(input.UserText()) && continuation.running {
		status = "steered"
	}
	return SubAgentContinuationReceipt{AgentID: agentID, InputID: input.ID, Status: status, Durable: true}, nil
}

// RunSubAgentWithContinuations is the sole Run owner for one child. Admission
// and the no-more-input boundary share a lock so an acknowledged final-token
// race is either consumed here or remains durable for later cold recovery.
func RunSubAgentWithContinuations(ctx context.Context, out chan<- Event, loop *Loop, teammate *Teammate, transcript *SubAgentTranscript) error {
	if teammate == nil {
		return loop.Run(ctx, out)
	}
	teammate.mu.RLock()
	continuation := teammate.continuation
	teammate.mu.RUnlock()
	if continuation == nil {
		return loop.Run(ctx, out)
	}
	continuation.mu.Lock()
	if continuation.closed || continuation.running || continuation.loop != loop || continuation.transcript != transcript {
		continuation.mu.Unlock()
		return ErrSubAgentContinuationClosed
	}
	continuation.running = true
	continuation.mu.Unlock()
	defer func() {
		continuation.mu.Lock()
		continuation.closed = true
		continuation.running = false
		continuation.mu.Unlock()
	}()

	for {
		runErr := loop.Run(ctx, out)
		continuation.mu.Lock()
		if runErr != nil || ctx.Err() != nil {
			continuation.closed = true
			continuation.mu.Unlock()
			if runErr != nil {
				return runErr
			}
			return ctx.Err()
		}
		pending := transcript.PendingFollowups()
		if len(pending) == 0 {
			continuation.closed = true
			continuation.mu.Unlock()
			return nil
		}
		for _, input := range pending {
			loop.AppendUser(input.UserText())
		}
		if err := transcript.Checkpoint(loop.History()); err != nil {
			continuation.closed = true
			continuation.mu.Unlock()
			return fmt.Errorf("checkpoint queued parent follow-up: %w", err)
		}
		continuation.mu.Unlock()
	}
}
