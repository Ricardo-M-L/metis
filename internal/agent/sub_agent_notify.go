package agent

// sub_agent_notify.go — drains the subAgentNotify channel at each Run
// iteration boundary and synthesizes <sub_agent_idle> system-reminder
// user messages so the model learns when background sub-agents complete
// without having to poll SubAgentList every turn.
//
// Mirrors claude-code's idle_notification flow: a background worker
// posts to the parent loop's notify channel on finish; the parent picks
// it up at the next iter boundary. Same envelope strategy as
// job_notify.go and peer_notify.go: injected as a user message so the
// model sees it inside the normal prompt context.

import (
	"context"
	"fmt"
	"strings"
)

// injectSubAgentNotifications drains every pending SubAgentNotification
// on l.subAgentNotify and appends one synthetic user message that lists
// all completed background sub-agents since the last turn.
//
// No-op when subAgentNotify is nil (shouldn't happen post-NewLoop, but
// handles headless tests that build Loop manually) or when the channel
// is empty.
func (l *Loop) injectSubAgentNotifications(out chan<- Event) {
	if l.subAgentNotify == nil {
		return
	}
	notifs := drainChan(l.subAgentNotify)
	if len(notifs) == 0 {
		return
	}
	l.appendInjectedMessage(formatSubAgentNotifications(notifs))
	emit(context.Background(), out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("[sub-agent idle] %d background sub-agent(s) finished", len(notifs)),
	})
}

// formatSubAgentNotifications builds the synthetic user message body.
// The <sub_agent_idle> envelope signals the model that these are
// completion events from background sub-agents, not messages from the
// human operator.
func formatSubAgentNotifications(ns []SubAgentNotification) string {
	var b strings.Builder
	b.WriteString("<sub_agent_idle>\n")
	if len(ns) == 1 {
		n := ns[0]
		fmt.Fprintf(&b, "Background sub-agent %q (%s) finished with status %s",
			n.Name, n.AgentID, n.Status)
		if n.Duration > 0 {
			fmt.Fprintf(&b, " in %s", HumanizeDuration(n.Duration))
		}
		b.WriteString(".\n")
		if n.Err != nil {
			fmt.Fprintf(&b, "Error: %v\n", n.Err)
		}
		if n.Summary != "" {
			fmt.Fprintf(&b, "Summary: %s\n", n.Summary)
		}
	} else {
		fmt.Fprintf(&b, "%d background sub-agents finished:\n", len(ns))
		for _, n := range ns {
			fmt.Fprintf(&b, "  - %q (%s): %s", n.Name, n.AgentID, n.Status)
			if n.Duration > 0 {
				fmt.Fprintf(&b, " (%s)", HumanizeDuration(n.Duration))
			}
			if n.Err != nil {
				fmt.Fprintf(&b, " — error: %v", n.Err)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("Use SubAgentOutput to read each sub-agent's full output.")
	b.WriteString("\n</sub_agent_idle>")
	return b.String()
}
