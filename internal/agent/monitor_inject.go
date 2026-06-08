package agent

// monitor_inject.go — turns drained MonitorEvent values into a
// `<monitor_event>` user message the model sees on its next iteration.
// Mirrors job_notify.go in shape so the two reminder envelopes look
// alike from the model's perspective. Kept in a separate file so the
// jobs.Notification path stays readable; the diff between the two is
// purely the per-event line format.

import (
	"context"
	"fmt"
	"strings"
)

// injectMonitorEvents drains every pending MonitorEvent on
// l.Monitors.Events() and appends one synthetic user message
// summarising the batch. No-op when no registry is wired or the
// channel is empty.
//
// Multiple matches collapse into one message — same rationale as
// formatJobNotifications: the model gets one prompt addition per
// turn boundary, not one per match.
func (l *Loop) injectMonitorEvents(out chan<- Event) {
	if l.Monitors == nil {
		return
	}
	events := drainChan(l.Monitors.Events())
	if len(events) == 0 {
		return
	}
	l.appendInjectedMessage(formatMonitorEvents(events))
	emit(context.Background(), out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("[monitor] %d match(es) injected", len(events)),
	})
}

// formatMonitorEvents builds the synthetic user message body. The
// outer <monitor_event> tag mirrors <job_notification> so the same
// system-reminder muscle memory the model has for finished jobs
// applies here.
func formatMonitorEvents(events []MonitorEvent) string {
	var b strings.Builder
	b.WriteString("<monitor_event>\n")
	if len(events) == 1 {
		b.WriteString("A Monitor pattern matched:\n")
	} else {
		fmt.Fprintf(&b, "%d Monitor matches since last turn:\n", len(events))
	}
	for _, e := range events {
		desc := e.Description
		if desc == "" {
			desc = e.JobID
		}
		fmt.Fprintf(&b, "  • %s @ %s — %s\n",
			desc, e.When.Format("15:04:05"), truncateOneLine(e.Match, 120))
	}
	b.WriteString("Use BashOutput to read the surrounding context, BashList to see all jobs.\n")
	b.WriteString("</monitor_event>")
	return b.String()
}
