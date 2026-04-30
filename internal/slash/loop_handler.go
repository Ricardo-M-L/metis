package slash

import (
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/runtime"
)

// loopNamePrefix tags every job /loop creates so /loop list / /loop stop
// can isolate them from normal /cron jobs. Users are free to ignore the
// prefix and manage them via /cron directly — both surfaces work.
const loopNamePrefix = "loop:"

// handleLoopCommand parses `/loop <subcmd>` and shells out to CronChatService
// with a "loop:" name prefix. /loop is a sugar wrapper — every loop is
// just a cron job under the hood, so you get persistence + pause/resume
// for free.
//
// Subcommands:
//
//	/loop <every> <prompt>          — start a new loop
//	/loop list                       — show running loops
//	/loop stop <id>                  — stop one loop
//	/loop stop all                   — stop every loop
func handleLoopCommand(cfg *config.Config, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return loopUsage()
	}

	svc, err := runtime.NewCronChatService(cfg)
	if err != nil {
		return "loop: " + err.Error()
	}

	parts := strings.SplitN(args, " ", 2)
	first := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch first {
	case "list", "ls":
		return renderLoopList(svc)
	case "stop":
		if rest == "" {
			return "usage: /loop stop <id|all>"
		}
		return stopLoop(svc, rest)
	}

	// Default: first token is the interval, rest is the prompt.
	if rest == "" {
		return loopUsage()
	}
	return startLoop(svc, first, rest)
}

func startLoop(svc *runtime.CronChatService, every, prompt string) string {
	id, err := svc.Add(every, loopNamePrefix+prompt)
	if err != nil {
		return "loop: " + err.Error()
	}
	return fmt.Sprintf("(started loop %s · every %s)\n  prompt: %q", id, every, prompt)
}

func renderLoopList(svc *runtime.CronChatService) string {
	jobs := loopJobsOnly(svc)
	if len(jobs) == 0 {
		return "(no active loops — `/loop <every> <prompt>` to start one)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d loop(s):\n", len(jobs))
	for _, j := range jobs {
		state := "running"
		if !j.Enabled {
			state = "disabled"
		} else if j.Paused {
			state = "paused"
		}
		// Display prompt without the "loop:" prefix that we tucked in
		// at creation time.
		prompt := strings.TrimPrefix(j.Prompt, loopNamePrefix)
		if len(prompt) > 50 {
			prompt = prompt[:47] + "…"
		}
		fmt.Fprintf(&b, "  %s  %s  %q\n", j.ID, state, prompt)
	}
	return strings.TrimRight(b.String(), "\n")
}

func stopLoop(svc *runtime.CronChatService, target string) string {
	if target == "all" {
		jobs := loopJobsOnly(svc)
		if len(jobs) == 0 {
			return "(no loops to stop)"
		}
		stopped := 0
		for _, j := range jobs {
			if err := svc.Remove(j.ID); err == nil {
				stopped++
			}
		}
		return fmt.Sprintf("(stopped %d loop(s))", stopped)
	}
	if err := svc.Remove(target); err != nil {
		return "loop: " + err.Error()
	}
	return "(stopped loop " + target + ")"
}

// loopJobsOnly filters CronChatService's job list down to ones we created
// via /loop (identified by the "loop:" prompt prefix). Goes through the
// underlying CronService directly because CronChatService doesn't expose
// a Get() — we only need it for filtering, not mutation.
func loopJobsOnly(svc *runtime.CronChatService) []*agent.CronJob {
	all := svc.AllJobs()
	out := make([]*agent.CronJob, 0, len(all))
	for _, j := range all {
		if strings.HasPrefix(j.Prompt, loopNamePrefix) {
			out = append(out, j)
		}
	}
	return out
}

func loopUsage() string {
	return "usage: /loop <every> <prompt>   start a new autopilot loop\n" +
		"       /loop list                  list running loops\n" +
		"       /loop stop <id|all>         stop one or all\n" +
		"       interval examples: 30s, 5m, 1h"
}
