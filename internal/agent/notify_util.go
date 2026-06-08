package agent

// notify_util.go — shared plumbing for the iter-boundary notification
// injectors (peer / job / dream / monitor / sub-agent). Every injector
// drains a buffered channel and appends a synthetic <...> user message
// to the transcript; this file holds the three pieces they share so the
// logic lives in exactly one place.

import (
	"fmt"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// drainChan pulls every value currently buffered on ch without blocking,
// returning them in arrival order. Generic replacement for the five
// per-type drain loops (drainPeerMessages, drainJobNotifications, ...)
// that were byte-for-byte identical except for the element type.
func drainChan[T any](ch <-chan T) []T {
	var out []T
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, v)
		default:
			return out
		}
	}
}

// appendInjectedMessage appends a synthetic user message to the
// transcript under l.mu. The iter-boundary injectors run on Run's
// goroutine while the TUI may concurrently call History() /
// TokenEstimate() (both take l.mu), so the append must be guarded —
// an unlocked append races the slice header against those readers.
func (l *Loop) appendInjectedMessage(text string) {
	l.mu.Lock()
	l.Messages = append(l.Messages, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: text}},
	})
	l.mu.Unlock()
}

// HumanizeDuration formats elapsed wall-clock time concisely:
// "<1s" for sub-second, then "Ns" / "Nm" / "Nh" / "Nd". time.Duration's
// own String() emits "35.123456789s" which is too noisy for prompts and
// status lines. Exported so the builtin tools (MessageTeammate's
// "finished N ago") and the agent package's notification injectors share
// one formatter — keeping the two surfaces consistent after any threshold
// change.
func HumanizeDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
