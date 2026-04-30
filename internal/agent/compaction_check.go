package agent

import (
	"context"
	"fmt"
	"strings"
)

// maybeCompact runs auto-compaction on the loop's history when the
// configured threshold is exceeded. No-op when l.Compactor is nil
// (compaction disabled in this build / for this session).
//
// Lock window matches the original Run() inline block exactly:
// l.mu held across ShouldCompact + Compact + Messages assignment, so
// concurrent History() / Reset() calls can't see a torn intermediate
// state. Compactor.Compact is allowed to be expensive (it makes an
// LLM call internally); holding mu through that is intentional —
// any caller hitting RLock during compaction simply waits.
func (l *Loop) maybeCompact(ctx context.Context, out chan<- Event) {
	if l.Compactor == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.Compactor.ShouldCompact(l.Messages) {
		return
	}
	before := len(l.Messages)
	compacted, err := l.Compactor.Compact(ctx, l.Messages)
	if err != nil || len(compacted) >= before {
		return
	}
	l.Messages = compacted
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("context compacted: %d → %d messages", before, len(compacted)),
	})
}

// tryRecoverOverflow inspects an error returned by Provider.Stream and, if
// it looks like a context-window overflow, force-compacts the history and
// reports true so the caller can retry the request once. When compaction
// is disabled or the error doesn't look like an overflow, returns false.
//
// Detection is provider-agnostic: we string-match on the common phrasing
// ("context window", "exceeds limit", "too many tokens", "context_length")
// because Anthropic, OpenAI, and the various Anthropic-compat gateways
// (MiniMax with code 2013, OpenRouter, ...) each surface a slightly
// different error body. False positives just mean a wasted compaction
// cycle, which is cheap.
func (l *Loop) tryRecoverOverflow(ctx context.Context, err error, out chan<- Event) bool {
	if l.Compactor == nil || err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "context") &&
		!strings.Contains(msg, "too many tokens") &&
		!strings.Contains(msg, "exceeds limit") {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	before := len(l.Messages)
	if before <= l.Compactor.ProtectFirst+l.Compactor.ProtectLast+2 {
		return false
	}
	compacted, cerr := l.Compactor.Compact(ctx, l.Messages)
	if cerr != nil || len(compacted) >= before {
		return false
	}
	l.Messages = compacted
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("context overflow: force-compacted %d → %d messages, retrying", before, len(compacted)),
	})
	return true
}
