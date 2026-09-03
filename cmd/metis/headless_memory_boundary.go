package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// distillationBoundary is the narrow lifecycle contract shared by every
// headless entry point. Keeping the boundary independent from agent.Loop makes
// the ordering, timeout and per-session isolation directly testable.
type distillationBoundary interface {
	FlushPendingDistillation(sessionID string) int
	WaitForDistillation(ctx context.Context, sessionID string) error
}

// persistHeadlessMemoryBoundary turns residual successful exchanges into
// registered distillation jobs and joins only the owning session before the
// caller may tear down provider/repository dependencies. The wait deliberately
// receives a fresh bounded context: callers invoke this only after their work
// context completed successfully, and a late parent cancellation must not race
// a clean durability hand-off.
func persistHeadlessMemoryBoundary(
	loop distillationBoundary,
	sessionID, source string,
	grace time.Duration,
) error {
	if loop == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if grace <= 0 {
		grace = runtimeDistillationShutdownGrace
	}

	loop.FlushPendingDistillation(sessionID)
	waitCtx, cancel := context.WithTimeout(context.Background(), grace)
	err := loop.WaitForDistillation(waitCtx, sessionID)
	cancel()
	if err == nil {
		return nil
	}
	if strings.TrimSpace(source) == "" {
		source = "headless session"
	}
	return fmt.Errorf("%s: join memory distillation for %s: %w", source, sessionID, err)
}

// persistHeadlessMemoryBoundary is intentionally success-only. Its callers
// invoke it only after the owned work returned nil; therefore it must not
// re-check a parent context that can be cancelled after the successful turn.
// Error and in-turn cancellation paths skip this method and flow directly into
// runtime.Cleanup, whose destructive barrier discards partial residual content.
func (r *runtime) persistHeadlessMemoryBoundary(source string, grace time.Duration) error {
	if r == nil || r.loop == nil || strings.TrimSpace(r.sessionID) == "" {
		return nil
	}
	return persistHeadlessMemoryBoundary(r.loop, r.sessionID, source, grace)
}

// collectHeadlessEvents owns the producer channel lifecycle. Loop.Run does not
// close caller-owned channels; the wrapper must close after Run returns on
// success, error or cancellation so range consumers cannot wait forever.
func collectHeadlessEvents(run func(events chan<- agent.Event) error) (string, error) {
	if run == nil {
		return "", nil
	}
	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		defer close(events)
		done <- run(events)
	}()

	var text strings.Builder
	var eventErr error
	var incompleteReason string
	for event := range events {
		switch event.Kind {
		case agent.EventTextDelta:
			text.WriteString(event.TextDelta)
		case agent.EventError:
			if eventErr == nil {
				eventErr = event.Err
			}
		case agent.EventLoopDone:
			if agent.IsIncompleteStopReason(event.StopReason) {
				incompleteReason = event.StopReason
			}
		}
	}
	if err := <-done; err != nil {
		return text.String(), err
	}
	if eventErr == nil && incompleteReason != "" {
		eventErr = fmt.Errorf("task incomplete: %s", incompleteReason)
	}
	return text.String(), eventErr
}

// runHeadlessOneShot is shared by the daemon and coordinator worker. A single
// implementation keeps their cancellation/channel-close behavior identical
// and establishes exactly one success durability boundary per task.
func runHeadlessOneShot(ctx context.Context, r *runtime, prompt, source string) (string, error) {
	if r == nil || r.loop == nil {
		return "", fmt.Errorf("%s: runtime loop is unavailable", source)
	}
	r.loop.AppendUser(prompt)
	text, runErr := collectHeadlessEvents(func(events chan<- agent.Event) error {
		return rtpkg.RunWithTraceTurn(ctx, r.sessionID, func(turnCtx context.Context) error {
			return r.loop.Run(turnCtx, events)
		})
	})
	if runErr != nil {
		return text, runErr
	}
	if err := r.persistHeadlessMemoryBoundary(source, runtimeDistillationShutdownGrace); err != nil {
		return text, err
	}
	return text, nil
}
