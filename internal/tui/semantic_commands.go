package tui

// This file contains the concrete handlers for command names whose Metis
// semantics intentionally differ from similarly named commands in other
// clients. Registrations live in commands.go; keeping behavior here avoids
// coupling the migration to palette/catalog work.

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

// cmdQuick is an honest local request-shaping mode: it lowers reasoning effort
// and halves max output tokens. It does not claim a provider-native fast tier.
func cmdQuick(r *REPL, args string) string {
	if r == nil || r.Loop == nil {
		return "quick: agent loop unavailable"
	}
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "":
		state := "off"
		if r.Loop.FastEnabled() {
			state = "on"
		}
		return "quick output: " + state + " — use: quick on | quick off | quick toggle"
	case "on", "true", "1", "yes":
		r.Loop.SetFast(true)
		return "quick output: on (effort=low, max_tokens halved for the next turn)"
	case "off", "false", "0", "no":
		r.Loop.SetFast(false)
		return "quick output: off"
	case "toggle", "t":
		if r.Loop.ToggleFast() {
			return "quick output: on"
		}
		return "quick output: off"
	default:
		return "quick: use quick on | quick off | quick toggle"
	}
}

// cmdTodos keeps TodoRead/TodoWrite's per-session checklist under an
// unambiguous name. The old implementation is reused while its user-facing
// prefix is translated.
func cmdTodos(r *REPL, args string) string {
	out := cmdTasks(r, args)
	out = strings.Replace(out, "tasks:", "todos:", 1)
	return out
}

// cmdBackgroundTasks manages only processes registered in Loop.Jobs. It never
// scans or kills arbitrary OS processes.
func cmdBackgroundTasks(r *REPL, args string) string {
	if r == nil || r.Loop == nil || r.Loop.Jobs == nil {
		return "tasks: background job registry unavailable"
	}
	parts := strings.Fields(strings.TrimSpace(args))
	sub := "list"
	if len(parts) > 0 {
		sub = strings.ToLower(parts[0])
	}
	switch sub {
	case "list", "ls":
		return renderBackgroundTasks(r.Loop.Jobs)
	case "output", "show":
		if len(parts) < 2 {
			return "usage: /tasks output <job-id>"
		}
		job, ok := r.Loop.Jobs.Get(parts[1])
		if !ok {
			return fmt.Sprintf("tasks: unknown job %q", parts[1])
		}
		body, err := jobs.ReadJobOutput(job.OutputPath, 50_000)
		if err != nil {
			return fmt.Sprintf("tasks: output %s: %v", job.ID, err)
		}
		return renderBackgroundTaskOutput(job, body)
	case "stop", "kill":
		if len(parts) < 2 {
			return "usage: /tasks stop <job-id>"
		}
		if err := r.Loop.Jobs.Stop(parts[1], 2*time.Second); err != nil {
			return "tasks: " + err.Error()
		}
		return "tasks: stop requested for " + parts[1]
	default:
		return "tasks: unknown subcommand " + sub + " (try: list | output <job-id> | stop <job-id>)"
	}
}

func renderBackgroundTasks(registry *jobs.Registry) string {
	all := registry.List()
	if len(all) == 0 {
		return "tasks: no background jobs this session"
	}
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "tasks: %d background job(s)\n", len(all))
	for _, job := range all {
		b.WriteString(renderBackgroundTaskRow(job, now))
		b.WriteByte('\n')
	}
	b.WriteString("use `/tasks output <id>` or `/tasks stop <id>`")
	return b.String()
}

func renderBackgroundTaskOutput(job jobs.Job, body string) string {
	return fmt.Sprintf("task %s · %s\n%s",
		safeTaskTerminalText(job.ID, 128), job.Status,
		safeTaskTerminalText(body, 50_000))
}

func renderBackgroundTaskRow(job jobs.Job, now time.Time) string {
	end := job.EndTime
	if end.IsZero() {
		end = now
	}
	row := fmt.Sprintf("  %s  %-9s  %s  %s",
		safeTaskTerminalText(job.ID, 128), job.Status,
		end.Sub(job.StartTime).Truncate(time.Second),
		safeTaskTerminalText(job.Description, 160))
	if job.Status != jobs.StatusRunning {
		row += fmt.Sprintf(" (exit %d)", job.ExitCode)
	}
	return row
}

// safeTaskTerminalText is the terminal boundary for background-process output
// and descriptions. Background commands control both byte streams, so styling
// the raw text would allow CSI/OSC/DCS/BEL/C1 sequences to reach the user's
// terminal. Keep ordinary Unicode and newlines readable while spelling every
// other control rune as inert text. The input-rune cap prevents expansion of a
// control-heavy output from defeating jobs.ReadJobOutput's byte limit.
func safeTaskTerminalText(raw string, maxRunes int) string {
	var b strings.Builder
	seen := 0
	for len(raw) > 0 {
		if maxRunes > 0 && seen >= maxRunes {
			b.WriteString("\n… (truncated)")
			break
		}
		seen++
		r, size := utf8.DecodeRuneInString(raw)
		if r == utf8.RuneError && size == 1 {
			// Job output is a byte stream. Preserve invalid UTF-8 bytes as
			// visible text too; in particular a raw 0x9b is the 8-bit CSI
			// introducer and must not collapse to an ambiguous replacement rune.
			fmt.Fprintf(&b, `\x%02x`, raw[0])
			raw = raw[1:]
			continue
		}
		raw = raw[size:]
		switch {
		case r == '\n':
			b.WriteRune('\n')
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r <= 0x1f || (r >= 0x7f && r <= 0x9f):
			if r <= 0xff {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u{%x}`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cmdSessionShare reports or controls Metis's real localhost sharing bridge.
// It never invents a cloud session URL.
func cmdSessionShare(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "", "status":
		if addr := bridgeCurrentAddr(); addr != "" {
			return "session: shared read-only on http://" + addr + " (/transcript /events /health)"
		}
		return "session: local only — use `/session start` to expose a read-only localhost bridge"
	case "start", "on":
		return cmdShare(r, "start")
	case "stop", "off":
		return cmdShare(r, "stop")
	default:
		return "session: use status | start | stop"
	}
}

func cmdSessionInfo(r *REPL, args string) string { return cmdSession(r, args) }
