package agent

// peer_notify.go — drains the named-teammate Mailbox at every Run
// iteration boundary and synthesizes <peer_message> system-reminder
// user messages so the model sees what its teammates told it since
// the previous turn (G.3, 2026-05-12).
//
// Same envelope strategy as job_notify.go: the model only "sees" what
// lands in the prompt, so peer messages have to arrive as user
// messages tagged with a recognizable wrapping (<peer_message>...).
// This mirrors claude-code's SendMessageTool delivery — sender posts
// to a peer's queue, recipient sees it on its next turn boundary.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// injectPeerMessages drains every pending PeerMessage on
// l.PeerInbox and appends one synthetic user message summarising
// the batch. Multiple messages arriving between turns collapse into
// one envelope so the prompt isn't fragmented.
//
// No-op when PeerInbox is nil (top-level user loop, headless tests)
// or when the channel is empty (no peer told us anything since the
// last drain).
func (l *Loop) injectPeerMessages(out chan<- Event) {
	if l.PeerInbox == nil {
		return
	}
	msgs := drainPeerMessages(l.PeerInbox)
	if len(msgs) == 0 {
		return
	}
	body := formatPeerMessages(msgs)
	l.Messages = append(l.Messages, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: body}},
	})
	emit(context.Background(), out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("[peer messaging] %d message(s) delivered", len(msgs)),
	})
}

// drainPeerMessages pulls every PeerMessage currently buffered on
// the channel without blocking. Returns them in arrival order.
func drainPeerMessages(ch <-chan PeerMessage) []PeerMessage {
	var out []PeerMessage
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, m)
		default:
			return out
		}
	}
}

// formatPeerMessages builds the synthetic user message body. Outer
// <peer_message> tags signal the model this is an inbound from
// another teammate, not the human operator.
func formatPeerMessages(msgs []PeerMessage) string {
	var b strings.Builder
	b.WriteString("<peer_message>\n")
	if len(msgs) == 1 {
		fmt.Fprintf(&b, "Teammate %q sent you a message at %s:\n",
			msgs[0].From, msgs[0].Sent.Format(time.RFC3339))
		b.WriteString(msgs[0].Body)
		if !strings.HasSuffix(msgs[0].Body, "\n") {
			b.WriteByte('\n')
		}
	} else {
		fmt.Fprintf(&b, "%d teammates sent you messages:\n", len(msgs))
		for _, m := range msgs {
			fmt.Fprintf(&b, "  --- from %s (%s) ---\n", m.From, m.Sent.Format(time.RFC3339))
			b.WriteString(m.Body)
			if !strings.HasSuffix(m.Body, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	b.WriteString("</peer_message>")
	return b.String()
}
