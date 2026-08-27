package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// cmdCoordinator: launches a coordinator run. Two roles:
//
//	metis coordinator dispatch <runID> <prompt>   (coordinator: write tasks, wait)
//	metis coordinator worker <runID>              (worker: poll, run, post)
//
// The coordinator/worker split lets users start N workers in separate
// terminals (or via metis daemon) and orchestrate from one place. The
// 4-phase pattern (research → synth → impl → verify) is left to the
// coordinator-side prompt; the framework provides the mailbox transport.
func cmdCoordinator(ctx context.Context, args []string) error {
	if len(args) < 2 {
		fmt.Print(coordinatorHelp)
		return nil
	}
	role := args[0]
	runID := args[1]
	cfg := rtpkg.DefaultMailboxConfig(runID)

	switch role {
	case "dispatch":
		if len(args) < 3 {
			return fmt.Errorf("usage: metis coordinator dispatch <runID> <prompt>")
		}
		prompt := strings.Join(args[2:], " ")
		taskID := fmt.Sprintf("research-%d", time.Now().UnixNano())
		if err := rtpkg.DispatchTask(cfg, rtpkg.WorkerTask{
			ID:     taskID,
			Phase:  "research",
			Prompt: prompt,
		}); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "coordinator: dispatched %s, waiting up to %s for reply...\n",
			taskID, cfg.WorkerTimeout)
		results, err := rtpkg.AwaitResults(ctx, cfg, []string{taskID})
		if err != nil {
			fmt.Fprintln(os.Stderr, "warn:", err)
		}
		for id, r := range results {
			fmt.Println("=== result", id, "===")
			fmt.Println(r.Output)
		}
		return nil

	case "worker":
		fmt.Fprintf(os.Stderr, "coordinator-worker: polling %s\n", cfg.ToWorkerDir)
		rt, err := setupRuntime(ctx, &cliFlags{})
		if err != nil {
			return err
		}
		defer rt.Cleanup()
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			task, err := rtpkg.PollForTask(cfg)
			if err != nil {
				fmt.Fprintln(os.Stderr, "worker poll err:", err)
				time.Sleep(cfg.PollInterval)
				continue
			}
			if task == nil {
				time.Sleep(cfg.PollInterval)
				continue
			}
			fmt.Fprintf(os.Stderr, "worker: claimed %s (phase=%s)\n", task.ID, task.Phase)
			start := time.Now()
			out, runErr := runOneShotForCoordinator(ctx, rt, task.Prompt)
			res := rtpkg.WorkerResult{
				TaskID:   task.ID,
				Phase:    task.Phase,
				OK:       runErr == nil,
				Output:   out,
				Duration: time.Since(start).String(),
			}
			if runErr != nil {
				res.Error = runErr.Error()
			}
			if perr := rtpkg.PostResult(cfg, res); perr != nil {
				fmt.Fprintln(os.Stderr, "worker post err:", perr)
			}
		}

	default:
		fmt.Print(coordinatorHelp)
		return nil
	}
}

// runOneShotForCoordinator drives one non-interactive agent turn and
// returns the assembled assistant text. Mirrors the daemon-side helper
// pattern; kept inline because cross-file Go helpers in cmd/ would
// require a third file just to share two ten-line functions.
func runOneShotForCoordinator(ctx context.Context, rt *runtime, prompt string) (string, error) {
	rt.loop.AppendUser(prompt)
	var sb strings.Builder
	eventCh := make(chan agent.Event, 64)
	errCh := make(chan error, 1)
	go func() {
		errCh <- rtpkg.RunWithTraceTurn(ctx, rt.sessionID, func(turnCtx context.Context) error {
			return rt.loop.Run(turnCtx, eventCh)
		})
	}()
	for ev := range eventCh {
		switch ev.Kind {
		case agent.EventTextDelta:
			sb.WriteString(ev.TextDelta)
		case agent.EventLoopDone:
			return sb.String(), <-errCh
		case agent.EventError:
			return sb.String(), ev.Err
		}
	}
	return sb.String(), <-errCh
}

const coordinatorHelp = `metis coordinator — minimum-viable multi-agent orchestration

Usage:
  metis coordinator dispatch <runID> <prompt>   # send a task, wait for reply
  metis coordinator worker <runID>              # poll, run tasks, post replies

Both sides share a mailbox under ~/.metis/coordinator/<runID>/. Start
multiple worker processes (different terminals or via metis daemon) for
parallelism. The dispatch side currently sends one task and waits;
extend this driver to implement the full 4-phase research→synth→impl→
verify pattern from claude-code's COORDINATOR_MODE.
`
