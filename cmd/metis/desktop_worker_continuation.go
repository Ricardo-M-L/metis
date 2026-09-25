package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// runDesktopWorkerTurns retains the private runtime while background work can
// still notify it. Only the loop consumes those notifications, so resumed
// requests retain the original tool results, permission gate and trace owner.
// Intermediate LoopDone events are held back: the parent owns one complete,
// durable foreground invocation and must see exactly one terminal event.
func runDesktopWorkerTurns(ctx context.Context, loop *agent.Loop, roster *agent.Roster, sessionID string, output chan<- agent.Event, checkpoint func() error) error {
	for {
		events := make(chan agent.Event, 64)
		done := make(chan error, 1)
		go func() {
			done <- rtpkg.RunWithTraceTurn(ctx, sessionID, func(turnCtx context.Context) error { return loop.Run(turnCtx, events) })
			close(events)
		}()
		var terminal *agent.Event
		var eventErr error
		for event := range events {
			if event.Kind == agent.EventLoopDone {
				copy := event
				terminal = &copy
				continue
			}
			if event.Kind == agent.EventError && event.Err != nil {
				eventErr = event.Err
			}
			forwardDesktopWorkerEvent(ctx, output, event)
		}
		runErr := errors.Join(<-done, eventErr)
		if checkpoint != nil {
			runErr = errors.Join(runErr, checkpoint())
		}
		if runErr != nil {
			return runErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if terminal == nil {
			return errors.New("desktop worker loop returned without a terminal event")
		}
		if terminal.StopReason != "end_turn" {
			forwardDesktopWorkerEvent(ctx, output, *terminal)
			return nil
		}
		resume, err := awaitDesktopWorkerBackground(ctx, loop, roster)
		if err != nil {
			return err
		}
		if !resume {
			forwardDesktopWorkerEvent(ctx, output, *terminal)
			return nil
		}
		// This is a model continuation, not a synthetic user input. Keep it in
		// the same session ownership while opening the next trace/API turn.
		forwardDesktopWorkerEvent(ctx, output, agent.Event{Kind: agent.EventTurnEnd})
		forwardDesktopWorkerEvent(ctx, output, agent.Event{Kind: agent.EventInfo, Info: "Continuing after background work completed"})
	}
}

func forwardDesktopWorkerEvent(ctx context.Context, output chan<- agent.Event, event agent.Event) {
	select {
	case output <- event:
	case <-ctx.Done():
	}
}

func awaitDesktopWorkerBackground(ctx context.Context, loop *agent.Loop, roster *agent.Roster) (bool, error) {
	if loop == nil {
		return false, fmt.Errorf("desktop worker loop is unavailable")
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if loop.HasPendingSubAgentNotifications() || (loop.Jobs != nil && loop.JobNotify != nil && loop.Jobs.HasPendingNotifications()) {
			return true, nil
		}
		jobsPending := loop.Jobs != nil && loop.JobNotify != nil && loop.Jobs.HasPendingWork()
		agentsPending := roster != nil && roster.Count() > 0
		if !jobsPending && !agentsPending {
			// A publisher may enqueue between the first readiness check and the
			// observed quiescent lifecycle edge. Recheck without stealing its data.
			if loop.HasPendingSubAgentNotifications() || (loop.Jobs != nil && loop.JobNotify != nil && loop.Jobs.HasPendingNotifications()) {
				return true, nil
			}
			return false, nil
		}
		var wakeup <-chan struct{}
		if loop.Jobs != nil {
			wakeup = loop.Jobs.Wakeup()
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-wakeup:
		case <-ticker.C:
		}
	}
}
