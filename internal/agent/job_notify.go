package agent

// job_notify.go — drains the bash job pool's notification channel at
// every Run iteration boundary and synthesizes <job_notification>
// system-reminder user messages so the model sees finished background
// commands without polling.
//
// Why a synthetic user message instead of a hook event: the model
// only "sees" what's in the prompt. Hook events are a side channel
// for tooling. A user-message is the unambiguous "here is something
// the agent should react to right now" — same envelope claude-code
// uses (<task_notification>...</task_notification> wrapping a JSON
// blob). The wrapping tag tells the model "this isn't a real user,
// don't reply conversationally — act on it."

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/security"
)

// injectJobNotifications drains every pending notification on
// l.JobNotify and appends one synthetic user message summarising the
// batch. Multiple jobs that finish within the same window collapse
// into one message to avoid spamming the prompt.
//
// No-op when JobNotify is nil (sub-agents, headless tests) or when
// the channel is empty (the common case — most iterations don't
// have any finished jobs to report).
func (l *Loop) injectJobNotifications(ctx context.Context, out chan<- Event) {
	if l.JobNotify == nil {
		return
	}
	notifs := drainChan(l.JobNotify)
	if len(notifs) == 0 {
		return
	}
	l.appendInjectedMessage(formatJobNotifications(notifs))
	// Surface to the TUI too so the user sees the same notification
	// banner the model is reacting to (helps explain why the model
	// suddenly says "I see job bg_xxx finished, ...").
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("[job pool] %d notification(s) injected", len(notifs)),
	})
}

// formatJobNotifications builds the synthetic user message body. The
// outer <job_notification> tags are recognized by the model as a
// system-reminder, mirroring claude-code's <task_notification>
// envelope. The body is human-readable lines plus a hint to use
// BashOutput / BashList for follow-up.
func formatJobNotifications(notifs []jobs.Notification) string {
	var b strings.Builder
	b.WriteString("<job_notification>\n")
	if len(notifs) == 1 {
		b.WriteString("A background bash job finished:\n")
	} else {
		fmt.Fprintf(&b, "%d background bash jobs finished:\n", len(notifs))
	}
	for _, n := range notifs {
		command := security.RedactSubprocessText(n.Command)
		fmt.Fprintf(&b, "  • %s — %s — exit=%d — elapsed=%s — `%s`\n",
			n.JobID,
			n.Status,
			n.ExitCode,
			n.Elapsed.Truncate(time.Second),
			truncateOneLine(command, 60),
		)
	}
	b.WriteString("Use BashOutput to read the captured output, BashList to see all jobs.\n")
	b.WriteString("</job_notification>")
	return b.String()
}

// truncateOneLine is a tiny helper that collapses newlines and caps
// length so a multi-line heredoc job command renders predictably in
// the notification.
func truncateOneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
