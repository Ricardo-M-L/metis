package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

// A session spans one logical model response, not the entire agent turn. A
// successfully executed tool's result is already in req, and is never rerun
// when this request's stream is retried. All failed stream blocks remain local
// to consumeStream until terminal failure or a complete replacement response.
func newRequestRecovery(ctx context.Context, out chan<- Event) (context.Context, *transport.RecoverySession, error) {
	policy, err := transport.RecoveryPolicyFromEnv()
	if err != nil {
		return ctx, nil, err
	}
	if !policy.Enabled() {
		return ctx, nil, nil
	}
	session := transport.NewRecoverySession(policy)
	ctx = transport.WithRecoverySession(ctx, session)
	ctx = transport.WithRecoveryObserver(ctx, func(p transport.RecoveryProgress) {
		emitRecoveryInfo(out, fmt.Sprintf("network recovery: %s; attempt %d/%d; elapsed %s; remaining %s; wait %s", p.State, p.Attempt, p.MaxAttempts, p.Elapsed.Round(time.Millisecond), p.Remaining.Round(time.Millisecond), p.Delay.Round(time.Millisecond)))
	})
	return ctx, session, nil
}

// Recovery notices are best-effort telemetry, unlike required tool/permission
// events. A slow or absent UI must not hold a recovery window open. Do not
// spawn a goroutine to hide backpressure: that would leak one per attempt.
func emitRecoveryInfo(out chan<- Event, info string) {
	select {
	case out <- Event{Kind: EventInfo, Info: info}:
	default:
	}
}

func openStreamWithRecovery(ctx context.Context, p llm.Provider, req llm.Request, session *transport.RecoverySession) (llm.StreamReader, error) {
	if session == nil {
		return p.Stream(ctx, req)
	}
	if managed, ok := p.(interface{ ManagesRecoverySession() bool }); ok && managed.ManagesRecoverySession() {
		return p.Stream(ctx, req)
	}
	// Custom providers without internal recovery use the same finite policy.
	// An existing RetryExhaustedError is never retried here.
	policy, _ := transport.RecoveryPolicyForContext(ctx)
	var stream llm.StreamReader
	release, err := transport.RetryWithRecovery(ctx, policy, func(attemptCtx context.Context) error {
		var callErr error
		stream, callErr = p.Stream(attemptCtx, req)
		if callErr != nil && stream != nil {
			_ = stream.Close()
			stream = nil
		}
		return callErr
	})
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		release()
		return nil, err
	}
	return &recoveryStream{StreamReader: stream, release: release}, nil
}

type recoveryStream struct {
	llm.StreamReader
	release func()
}

func (s *recoveryStream) Close() error { defer s.release(); return s.StreamReader.Close() }

func (l *Loop) consumeStreamWithRecovery(ctx context.Context, p llm.Provider, req llm.Request, stream llm.StreamReader, out chan<- Event, session *transport.RecoverySession) (assistant []llm.ContentBlock, stop string, usage *usageTotals, err error) {
	defer func() {
		if err != nil && session != nil {
			// Keep only human-readable prose for terminal-failure auditing.
			// Incomplete tool calls, signed/encrypted reasoning and remote
			// response state must not poison a later resumed request.
			var prose []llm.ContentBlock
			for _, block := range assistant {
				if block.Type == "text" && block.Text != "" {
					prose = append(prose, llm.ContentBlock{Type: "text", Text: block.Text})
				}
			}
			assistant = prose
		}
	}()
	for {
		assistant, stop, usage, err := l.consumeStream(ctx, stream, out)
		_ = stream.Close()
		if err == nil || session == nil {
			return assistant, stop, usage, err
		}
		if ctx.Err() != nil {
			return assistant, stop, usage, ctx.Err()
		}
		if !session.RecordFailure(err) {
			return assistant, stop, usage, err
		}
		// UI deltas may already have been displayed. Announce that the failed
		// draft is discarded; it is not appended to history or executed.
		emitRecoveryInfo(out, "Model stream interrupted; discarding the incomplete draft and retrying the same request. No tool from that draft was executed.")
		if retryErr := session.WaitRetry(ctx); retryErr != nil {
			return assistant, stop, usage, retryErr
		}
		stream, err = openStreamWithRecovery(ctx, p, req, session)
		if err != nil {
			return assistant, stop, usage, err
		}
	}
}
