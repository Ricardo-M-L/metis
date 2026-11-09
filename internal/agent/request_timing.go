package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/google/uuid"
)

// Timing never enters the UI channel and never performs trace IO on a delta.
// Each Run owns its collector, including child loops that inherit a parent ctx.
func notifyTimingHook(ev Event) {
	// Timing is advisory; a broken telemetry sink cannot change a request result.
	defer func() { _ = recover() }()
	notifyTraceHook(ev)
}

func beginRunTiming(ctx context.Context) (context.Context, func()) {
	traceHookMu.RLock()
	enabled := traceHook != nil
	traceHookMu.RUnlock()
	if !enabled {
		return ctx, func() {}
	}
	// Separate timing ownership must not change ordinary event identities or
	// the root/child lifecycle of legacy embedders.
	owner := NewTraceInvocationID()
	bind := Event{Kind: EventRequestTimingBind, TraceInvocationID: owner,
		TraceParentInvocationID: TraceInvocationIDFromContext(ctx), SubAgentParentID: ParentToolUseIDFromContext(ctx)}
	notifyTimingHook(bind)
	ctx, collector := transport.WithTimingCollector(ctx)
	return ctx, func() {
		defer notifyTimingHook(Event{Kind: EventRequestTimingUnbind, TraceInvocationID: owner})
		for _, record := range collector.Records() {
			r := record
			notifyTimingHook(Event{Kind: EventRequestTiming, RequestTiming: &r, TraceInvocationID: owner})
		}
	}
}

func refreshCapabilitiesTimed(ctx context.Context, refresh func(context.Context) error) error {
	transport.RecordTiming(ctx, "capability_refresh_start")
	err := refresh(ctx)
	transport.RecordTimingResult(ctx, "capability_refresh_end", transport.TimingOutcome(err), 0)
	return err
}

func timedProviderStream(ctx context.Context, p llm.Provider, req llm.Request) (llm.StreamReader, error) {
	// The request has passed tool selection and prompt assembly. Observe its
	// actual logical context before the provider translates it; only the trace
	// hook receives this boundary, never a UI delta channel.
	model := req.Model
	if model == "" {
		model = p.ModelID()
	}
	notifyTimingHook(Event{Kind: EventRequestContext,
		TraceInvocationID: TraceInvocationIDFromContext(ctx),
		SubAgentParentID:  ParentToolUseIDFromContext(ctx),
		TraceStreamID:     uuid.NewString(),
		RequestContext: &TraceRequestContext{System: req.System, SystemSections: req.SystemSections,
			Tools: req.Tools, Model: model, Provider: p.Name()},
	})
	if !transport.TimingEnabled(ctx) {
		return p.Stream(ctx, req)
	}
	ctx = transport.BeginTimingCall(ctx)
	transport.RecordTiming(ctx, "provider_stream_start")
	s, err := p.Stream(ctx, req)
	transport.RecordTimingResult(ctx, "provider_stream_return", transport.TimingOutcome(err), 0)
	if s == nil {
		return s, err
	}
	return &timingStream{StreamReader: s, ctx: transport.SnapshotTiming(ctx)}, err
}

type timingStream struct {
	llm.StreamReader
	ctx            context.Context
	thinking, text bool
	endOnce        sync.Once
}

func (s *timingStream) Recv() (llm.StreamEvent, error) {
	ev, err := s.StreamReader.Recv()
	// Observe the decoded provider event, before filtering, UI delivery or burst
	// coalescing. Whitespace-only text is not a nonempty answer. Opaque/redacted
	// thinking is deliberately not treated as a reasoning delta.
	if err == nil || errors.Is(err, io.EOF) {
		if ev.Type == "thinking_delta" && ev.TextDelta != "" && !s.thinking {
			s.thinking = true
			transport.RecordTiming(s.ctx, "first_reasoning_delta")
		}
		if ev.Type == "text_delta" && strings.TrimSpace(ev.TextDelta) != "" && !s.text {
			s.text = true
			transport.RecordTiming(s.ctx, "first_nonempty_text_delta")
		}
	}
	if err != nil {
		s.end(err)
	} else if ev.Type == "error" {
		streamErr := ev.Err
		if streamErr == nil {
			streamErr = errors.New("provider stream failed")
		}
		s.end(streamErr)
	} else if ev.Type == "message_stop" {
		// Consumers return on message_stop without reading a subsequent EOF.
		s.end(nil)
	}
	return ev, err
}

func (s *timingStream) end(err error) {
	s.endOnce.Do(func() {
		if errors.Is(err, io.EOF) {
			err = nil
		}
		transport.RecordTimingResult(s.ctx, "stream_end", transport.TimingOutcome(err), 0)
	})
}
func (s *timingStream) Close() error {
	err := s.StreamReader.Close()
	s.endOnce.Do(func() {
		outcome := "closed"
		if s.ctx.Err() != nil {
			outcome = transport.TimingOutcome(s.ctx.Err())
		} else if err != nil {
			outcome = transport.TimingOutcome(err)
		}
		transport.RecordTimingResult(s.ctx, "stream_end", outcome, 0)
	})
	return err
}
