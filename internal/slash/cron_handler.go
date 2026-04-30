package slash

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/runtime"
)

// handleCronCommand parses `/cron <subcmd> [args]` and dispatches to the
// runtime's chat-friendly cron facade. Returns (output, handled). When the
// subcommand is empty or unknown the output carries a usage hint and
// handled=true (we still consumed the slash — caller shouldn't retry).
//
// Living here rather than inline in commands.go keeps the registry's
// lambda-heavy registration block readable and gives /cron its own
// testable seam if we want to add tests for the parsing.
func handleCronCommand(cfg *config.Config, args string) (string, bool) {
	args = strings.TrimSpace(args)
	if args == "" {
		return cronUsage(), true
	}
	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	svc, err := runtime.NewCronChatService(cfg)
	if err != nil {
		return "cron: " + err.Error(), true
	}

	switch sub {
	case "list", "ls":
		return svc.RenderList(), true
	case "add":
		// `/cron add <every> <prompt...>` — split at first space so the
		// prompt can contain spaces freely.
		fields := strings.SplitN(rest, " ", 2)
		if len(fields) < 2 {
			return "usage: /cron add <every> <prompt>  (e.g. /cron add 30m 'check the build')", true
		}
		id, err := svc.Add(fields[0], fields[1])
		if err != nil {
			return "cron: " + err.Error(), true
		}
		return "(created cron job " + id + ")", true
	case "rm", "remove":
		if rest == "" {
			return "usage: /cron rm <id>", true
		}
		if err := svc.Remove(rest); err != nil {
			return "cron: " + err.Error(), true
		}
		return "(removed cron job " + rest + ")", true
	case "pause":
		if rest == "" {
			return "usage: /cron pause <id>", true
		}
		if err := svc.Pause(rest); err != nil {
			return "cron: " + err.Error(), true
		}
		return "(paused " + rest + ")", true
	case "resume":
		if rest == "" {
			return "usage: /cron resume <id>", true
		}
		if err := svc.Resume(rest); err != nil {
			return "cron: " + err.Error(), true
		}
		return "(resumed " + rest + ")", true
	}
	return cronUsage(), true
}

func cronUsage() string {
	return "usage: /cron list | add <every> <prompt> | rm <id> | pause <id> | resume <id>\n" +
		"       interval examples: 30s, 5m, 1h"
}
