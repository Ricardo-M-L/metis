package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func (r *REPL) bindREPLWakeupContinuation() {
	if r.CronService == nil || r.Session == nil || r.Loop.Registry == nil {
		return
	}
	if _, ok := r.Loop.Registry.Get("ScheduleWakeup"); !ok {
		return
	}
	h, _, err := r.Session.LoadHeader(r.SessionID)
	if err != nil {
		return
	}
	cwd, _ := os.Getwd()
	identity := agent.ContinuationIdentity{SessionID: r.SessionID, WorkDir: cwd, Provider: r.providerName, Model: r.model}
	if h.WorkDir != "" && !agent.ContinuationWorkspaceMatches(h.WorkDir, cwd) {
		identity = agent.ContinuationIdentity{}
	}
	r.Loop.Registry.Replace(builtin.NewScheduleWakeup(r.Gate, r.CronService).WithContinuation(identity))
}

type replInputResult struct {
	text string
	err  error
}

// Only the REPL owner executes turns. The pending single-line read never
// mutates session state and is reused by permission/AskUser prompts, avoiding
// competing stdin readers when a due continuation needs an approval.
func (r *REPL) readInputWithWakeups(ctx context.Context) (string, error) {
	if r.CronService == nil {
		return r.readInput()
	}
	return r.readInputUsing(func() (string, error) {
		if r.idleInput == nil {
			result := make(chan replInputResult, 1)
			r.idleInput = result
			go func() { text, err := r.readRawSingleLine(); result <- replInputResult{text, err} }()
		}
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case result := <-r.idleInput:
				r.idleInput = nil
				return result.text, result.err
			case <-ticker.C:
				if err := r.consumeREPLWakeup(ctx); err != nil {
					fmt.Fprintf(r.out, "[wakeup] error: %v\n", err)
				}
			}
			// A continuation's permission prompt may have consumed the idle read.
			if r.idleInput == nil {
				result := make(chan replInputResult, 1)
				r.idleInput = result
				go func() { text, err := r.readRawSingleLine(); result <- replInputResult{text, err} }()
			}
		}
	})
}

func (r *REPL) consumeREPLWakeup(ctx context.Context) error {
	if r.Session == nil || r.SessionID == "" {
		return nil
	}
	header, _, err := r.Session.LoadHeader(r.SessionID)
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	if header.WorkDir != "" && !agent.ContinuationWorkspaceMatches(header.WorkDir, cwd) {
		return fmt.Errorf("scheduled wakeup pending: resume original workspace %q", header.WorkDir)
	}
	identity := agent.ContinuationIdentity{SessionID: r.SessionID, WorkDir: cwd, Provider: r.providerName, Model: r.model}
	if err = ctx.Err(); err != nil {
		return err
	}
	run, err := r.CronService.AcquireDueContinuation(identity, time.Now())
	if err != nil || run == nil {
		return err
	}
	r.Loop.AppendContextForTaskPlan(run.Job.Prompt, run.Job.TaskPlanID)
	runtime.RecordContextInput(r.SessionID, "scheduled-wakeup", run.Job.Prompt)
	err = r.persistTail()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = run.Start()
	}
	start := len(r.Loop.History())
	if err == nil {
		fmt.Fprintf(r.out, "[wakeup] id=%s session=%s running\n", run.Job.ID, r.SessionID)
		err = r.runTurn(ctx)
		if err == nil && r.lastStopReason != "end_turn" {
			err = fmt.Errorf("scheduled wakeup stopped before completion: %s", r.lastStopReason)
		}
	}
	status := "succeeded"
	if err != nil {
		status = "failed"
	}
	if ctx.Err() != nil {
		status = "cancelled"
		err = errors.Join(err, ctx.Err())
	}
	output := ""
	history := r.Loop.History()
	if start < len(history) {
		for _, message := range history[start:] {
			if message.Role == "assistant" {
				for _, block := range message.Content {
					if block.Type == "text" {
						output += block.Text
					}
				}
			}
		}
	}
	err = errors.Join(err, run.Finish(status, "scheduled wakeup "+status, output, err))
	fmt.Fprintf(r.out, "[wakeup] id=%s session=%s status=%s (receipt: metis cron history %s)\n", run.Job.ID, r.SessionID, status, run.Job.ID)
	return err
}
