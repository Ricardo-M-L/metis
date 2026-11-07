package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func (r *runtime) continuationIdentity() agent.ContinuationIdentity {
	cwd, _ := os.Getwd()
	return agent.ContinuationIdentity{SessionID: r.sessionID, WorkDir: cwd, Provider: r.providerName, Model: r.model}
}

// Rebind only an already-visible tool; never bypass profile/disallow filters.
func (r *runtime) bindWakeupContinuation() {
	if r.registry == nil || r.cronSvc == nil {
		return
	}
	if _, ok := r.registry.Get("ScheduleWakeup"); ok {
		identity := r.continuationIdentity()
		if r.store != nil {
			if h, _, err := r.store.LoadHeader(r.sessionID); err != nil || h == nil || (h.WorkDir != "" && h.WorkDir != identity.WorkDir) {
				fmt.Fprintln(os.Stderr, "[wakeup] scheduling disabled: resume the original session workspace before creating continuations")
				identity = agent.ContinuationIdentity{}
			}
		}
		r.registry.Replace(builtin.NewScheduleWakeup(r.gate, r.cronSvc).WithContinuation(identity))
	}
}

// One owner serializes every Loop.Run, including the original foreground turn.
// Waiting is scoped to the invocation context: cancel never detaches a daemon.
func runHeadlessWakeupTurns(ctx context.Context, rt *runtime, output chan<- agent.Event, checkpoint func() error, desktop bool) error {
	var run *agent.ContinuationRun
	for {
		historyStart := len(rt.loop.History())
		events := make(chan agent.Event, 64)
		done := make(chan error, 1)
		go func() {
			if desktop {
				done <- runDesktopWorkerTurns(ctx, rt.loop, rt.subAgentRoster, rt.sessionID, events, checkpoint)
			} else {
				done <- rtpkg.RunWithTraceTurn(ctx, rt.sessionID, func(turnCtx context.Context) error { return rt.loop.Run(turnCtx, events) })
			}
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
		err := errors.Join(<-done, eventErr)
		if checkpoint != nil {
			err = errors.Join(err, checkpoint())
		}
		if err == nil && terminal == nil {
			err = errors.New("headless loop returned without terminal event")
		}
		if err == nil && run != nil && terminal.StopReason != "end_turn" {
			err = fmt.Errorf("wakeup stopped before completion: %s", terminal.StopReason)
		}
		if run != nil {
			status := "succeeded"
			if err != nil {
				status = "failed"
			}
			if ctx.Err() != nil {
				status = "cancelled"
				err = errors.Join(err, ctx.Err())
			}
			history := rt.loop.History()
			if historyStart < len(history) {
				history = history[historyStart:]
			} else {
				history = nil
			}
			text := cronFinalOutput(history)
			err = errors.Join(err, run.Finish(status, text, text, err))
			fmt.Fprintf(os.Stderr, "[wakeup] id=%s session=%s status=%s (receipt: metis cron history %s)\n", run.Job.ID, rt.sessionID, status, run.Job.ID)
			run = nil
		}
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if terminal == nil {
			return errors.New("headless loop returned without terminal event")
		}
		// Desktop's parent owns scheduled wakeups and workspace admission.
		// A worker must release its turn lock rather than wait for a due job.
		if terminal.StopReason != "end_turn" || rt.cronSvc == nil || desktop {
			forwardDesktopWorkerEvent(ctx, output, *terminal)
			return nil
		}
		next, err := awaitHeadlessWakeup(ctx, rt)
		if err != nil {
			return err
		}
		if next == nil {
			forwardDesktopWorkerEvent(ctx, output, *terminal)
			return nil
		}
		run = next
		// Context, not forged user input. Persist before Start/provider admission.
		rt.loop.AppendContext(fmt.Sprintf("<scheduled_wakeup id=%q>\n%s\n</scheduled_wakeup>", run.Job.ID, run.Job.Prompt))
		if checkpoint != nil {
			err = checkpoint()
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = run.Start()
		}
		if err != nil {
			status := "failed"
			if ctx.Err() != nil {
				status = "cancelled"
			}
			return errors.Join(err, run.Finish(status, "", "", err))
		}
		fmt.Fprintf(os.Stderr, "[wakeup] id=%s session=%s running\n", run.Job.ID, rt.sessionID)
		forwardDesktopWorkerEvent(ctx, output, agent.Event{Kind: agent.EventTurnEnd})
	}
}

func awaitHeadlessWakeup(ctx context.Context, rt *runtime) (*agent.ContinuationRun, error) {
	identity := rt.continuationIdentity()
	if rt.store != nil {
		if h, _, err := rt.store.LoadHeader(rt.sessionID); err == nil && h.WorkDir != "" && h.WorkDir != identity.WorkDir {
			return nil, fmt.Errorf("scheduled wakeup pending: resume the original workspace %q (current workspace %q)", h.WorkDir, identity.WorkDir)
		}
	}
	announced := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := rt.cronSvc.AcquireDueContinuation(identity, time.Now())
		if err != nil || run != nil {
			return run, err
		}
		pending, err := rt.cronSvc.PendingContinuations(identity, time.Now())
		if err != nil {
			return nil, err
		}
		if len(pending) == 0 {
			return nil, nil
		}
		if !announced {
			fmt.Fprintf(os.Stderr, "[wakeup] waiting for %d pending continuation(s), session=%s; Ctrl-C cancels this invocation (no detached execution)\n", len(pending), rt.sessionID)
			announced = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
