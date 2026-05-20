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

// cmdDaemon implements `metis daemon` — KAIROS-style long-running mode.
// Watches ~/.metis/inbox/*.txt, runs each as a one-shot non-interactive
// agent turn, writes the result to ~/.metis/outbox/, deletes the
// processed inbox file. Idle time triggers memory distillation.
//
// Usage:
//
//	metis daemon                  # foreground; honors Ctrl-C
//	metis daemon --idle 10m       # custom distillation cadence
//	metis daemon --poll 2s        # custom inbox-scan interval
func cmdDaemon(ctx context.Context, args []string) error {
	cfg := rtpkg.DefaultDaemonConfig()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--idle":
			if i+1 < len(args) {
				if d, err := time.ParseDuration(args[i+1]); err == nil {
					cfg.IdleTimeout = d
				}
				i++
			}
		case "--poll":
			if i+1 < len(args) {
				if d, err := time.ParseDuration(args[i+1]); err == nil {
					cfg.PollInterval = d
				}
				i++
			}
		case "--help", "-h":
			fmt.Print(daemonHelp)
			return nil
		}
	}

	rt, err := setupRuntime(ctx, &cliFlags{})
	if err != nil {
		return err
	}
	defer rt.Cleanup()

	fmt.Fprintf(os.Stderr, "metis daemon: inbox=%s outbox=%s poll=%s idle=%s\n",
		cfg.InboxDir, cfg.OutboxDir, cfg.PollInterval, cfg.IdleTimeout)
	fmt.Fprintf(os.Stderr, "drop a *.txt file in inbox/ and the daemon will run it as a one-shot prompt.\n")

	taskHandler := func(ctx context.Context, prompt string) (string, error) {
		// One non-interactive turn per task. Reuses the same Loop
		// (so memory + history persist across tasks); reset between
		// tasks would defeat the long-running point.
		rt.loop.AppendUser(prompt)
		var sb strings.Builder
		eventCh := make(chan agent.Event, 64)
		errCh := make(chan error, 1)
		go func() { errCh <- rt.loop.Run(ctx, eventCh) }()
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

	distillHandler := func(ctx context.Context) error {
		// Idle distillation: ask the model to summarize recent turns
		// into long-lived memory. Reuses the existing Loop's
		// distillation hook (auto-distillation runs via loop.maybeDistill
		// every N turns; we trigger one extra here on idle).
		rt.loop.AppendUser("Please summarize the most recent task results into a few key durable facts and update memory accordingly. Reply with 'done' when finished.")
		eventCh := make(chan agent.Event, 64)
		errCh := make(chan error, 1)
		go func() { errCh <- rt.loop.Run(ctx, eventCh) }()
		for ev := range eventCh {
			if ev.Kind == agent.EventLoopDone || ev.Kind == agent.EventError {
				return <-errCh
			}
		}
		return <-errCh
	}

	return rtpkg.RunDaemon(ctx, cfg, taskHandler, distillHandler)
}

const daemonHelp = `metis daemon — long-running task processor (KAIROS-style)

Usage:
  metis daemon [--poll <duration>] [--idle <duration>]

Drop a *.txt file in ~/.metis/inbox/, the daemon will:
  1. Run its contents as a one-shot agent prompt
  2. Write the result to ~/.metis/outbox/<same-name>.txt
  3. Delete the inbox file

When the inbox is empty for --idle (default 30m), the daemon runs an
auto-distillation pass to consolidate memory.

Stop with Ctrl-C. Status is mirrored to ~/.metis/daemon.status.json.
`
