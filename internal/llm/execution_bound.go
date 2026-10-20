package llm

import (
	"context"
	"errors"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

// BindExecutionProvider binds every provider call, including detached helpers,
// to the execution which owns its lease and usage accounting.
func BindExecutionProvider(base Provider, execution context.Context) Provider {
	if base == nil || execution == nil {
		return base
	}
	p := &executionBoundProvider{Provider: base, execution: execution}
	metadata, hasMetadata := base.(ContextCapabilitiesProvider)
	history, hasHistory := base.(ContextHistoryPolicy)
	_, reportsVision := base.(pubprov.VisionCapabilityReporter)
	_, supportsVision := base.(pubprov.VisionSupporter)
	hasVision := reportsVision || supportsVision
	contextProvider := &executionBoundContextProvider{executionBoundProvider: p, metadata: metadata}
	vision := &executionBoundVision{provider: base}
	// Preserve capability presence. In particular, inventing a history policy
	// changes active-context accounting, while erasing metadata loses discovery.
	switch {
	case hasMetadata && hasHistory && hasVision:
		return struct {
			*executionBoundProvider
			ContextCapabilitiesProvider
			ContextHistoryPolicy
			*executionBoundVision
		}{p, contextProvider, history, vision}
	case hasMetadata && hasHistory:
		return struct {
			*executionBoundProvider
			ContextCapabilitiesProvider
			ContextHistoryPolicy
		}{p, contextProvider, history}
	case hasMetadata && hasVision:
		return struct {
			*executionBoundProvider
			ContextCapabilitiesProvider
			*executionBoundVision
		}{p, contextProvider, vision}
	case hasMetadata:
		return struct {
			*executionBoundProvider
			ContextCapabilitiesProvider
		}{p, contextProvider}
	case hasHistory && hasVision:
		return struct {
			*executionBoundProvider
			ContextHistoryPolicy
			*executionBoundVision
		}{p, history, vision}
	case hasHistory:
		return struct {
			*executionBoundProvider
			ContextHistoryPolicy
		}{p, history}
	case hasVision:
		return struct {
			*executionBoundProvider
			*executionBoundVision
		}{p, vision}
	default:
		return p
	}
}

type executionBoundProvider struct {
	Provider
	execution context.Context
}

// Recovery consumers test this marker's value rather than its presence.
func (p *executionBoundProvider) ManagesRecoverySession() bool {
	managed, ok := p.Provider.(interface{ ManagesRecoverySession() bool })
	return ok && managed.ManagesRecoverySession()
}

func (p *executionBoundProvider) Complete(caller context.Context, req Request) (*Response, error) {
	ctx, release, err := p.callContext(caller)
	if err != nil {
		return nil, err
	}
	defer release()
	return p.Provider.Complete(ctx, req)
}

func (p *executionBoundProvider) Stream(caller context.Context, req Request) (StreamReader, error) {
	ctx, release, err := p.callContext(caller)
	if err != nil {
		return nil, err
	}
	reader, err := p.Provider.Stream(ctx, req)
	if err != nil {
		release()
		return reader, err
	}
	if reader == nil {
		release()
		return nil, errors.New("provider returned no stream reader")
	}
	return &executionBoundStream{StreamReader: reader, release: release}, nil
}

// caller values retain request-local trace, session and recovery identity.
// Detached callers can still recover execution-owned values such as a lease.
// Usage accounting is separately pinned below, so a caller cannot redirect it.
type executionBoundValues struct {
	context.Context
	execution context.Context
}

func (c executionBoundValues) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.execution.Value(key)
}

func (p *executionBoundProvider) callContext(caller context.Context) (context.Context, func(), error) {
	if caller == nil {
		caller = context.Background()
	}
	if err := p.execution.Err(); err != nil {
		return nil, nil, err
	}
	if err := caller.Err(); err != nil {
		return nil, nil, err
	}
	// Caller values and cancellation retain their native parent semantics.
	// The explicit execution callback below adds the owning hard lifecycle,
	// including when the caller is Background or WithoutCancel.
	values := context.Context(executionBoundValues{Context: caller, execution: p.execution})
	deadlineRelease := func() {}
	callerDeadline, hasCallerDeadline := caller.Deadline()
	executionDeadline, hasExecutionDeadline := p.execution.Deadline()
	if hasExecutionDeadline && (!hasCallerDeadline || executionDeadline.Before(callerDeadline)) {
		values, deadlineRelease = context.WithDeadline(values, executionDeadline)
	}
	ctx, cancel := context.WithCancelCause(values)
	stopExecution := context.AfterFunc(p.execution, func() {
		// Every real execution deadline has an equivalent or earlier timer above.
		// Let that timer preserve DeadlineExceeded rather than racing a manual
		// cancel that would turn the deadline into Canceled.
		if p.execution.Err() == context.DeadlineExceeded && hasExecutionDeadline {
			return
		}
		cancel(context.Cause(p.execution))
	})
	var once sync.Once
	release := func() {
		once.Do(func() {
			stopExecution()
			cancel(context.Canceled)
			deadlineRelease()
		})
	}
	if observer := transport.UsageObserverFromContext(p.execution); observer != nil {
		ctx = transport.WithUsageObserver(ctx, observer)
	}
	// Recheck after callbacks are installed so already-ended contexts cannot
	// reach the underlying provider during setup.
	if err := p.execution.Err(); err != nil {
		release()
		return nil, nil, err
	}
	if err := caller.Err(); err != nil {
		release()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, nil, err
	}
	return ctx, release, nil
}

type executionBoundStream struct {
	StreamReader
	release   func()
	closeOnce sync.Once
	closeErr  error
}

func (s *executionBoundStream) Recv() (StreamEvent, error) {
	event, err := s.StreamReader.Recv()
	if err != nil {
		s.release()
	}
	return event, err
}

func (s *executionBoundStream) Close() error {
	s.closeOnce.Do(func() {
		// Cancel first so Close can join a reader blocked on a live request.
		s.release()
		s.closeErr = s.StreamReader.Close()
	})
	return s.closeErr
}

type executionBoundContextProvider struct {
	*executionBoundProvider
	metadata ContextCapabilitiesProvider
}

func (p *executionBoundContextProvider) ContextCapabilities() ContextCapabilities {
	return p.metadata.ContextCapabilities()
}

func (p *executionBoundContextProvider) RefreshContextCapabilities(caller context.Context) error {
	ctx, release, err := p.callContext(caller)
	if err != nil {
		return err
	}
	defer release()
	return p.metadata.RefreshContextCapabilities(ctx)
}

type executionBoundVision struct{ provider Provider }

func (p *executionBoundVision) VisionCapability() VisionCapability {
	return pubprov.ProviderVisionCapability(p.provider)
}

func (p *executionBoundVision) SupportsVision() bool {
	return p.VisionCapability() == VisionSupported
}
