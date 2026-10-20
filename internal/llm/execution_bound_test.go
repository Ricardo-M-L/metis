package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

type executionTestKey string

type executionTestProvider struct {
	complete func(context.Context, Request) (*Response, error)
	stream   func(context.Context, Request) (StreamReader, error)
}

func (*executionTestProvider) Name() string          { return "execution-fixture" }
func (*executionTestProvider) ModelID() string       { return "execution-model" }
func (*executionTestProvider) MaxContextTokens() int { return 1048576 }
func (p *executionTestProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	return p.complete(ctx, req)
}
func (p *executionTestProvider) Stream(ctx context.Context, req Request) (StreamReader, error) {
	return p.stream(ctx, req)
}

type executionTestObserver struct{ begins, ends atomic.Int32 }

func (o *executionTestObserver) Begin(context.Context, transport.RequestMetadata) (transport.RequestUsageObserver, error) {
	o.begins.Add(1)
	return o, nil
}
func (*executionTestObserver) Header(string) error                    { return nil }
func (*executionTestObserver) Snapshot(transport.UsageSnapshot) error { return nil }
func (o *executionTestObserver) End(transport.UsageStatus) error {
	o.ends.Add(1)
	return nil
}

type executionTestRoundTripper func(*http.Request) (*http.Response, error)

func (r executionTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return r(req)
}

func TestExecutionBoundDetachedHelperOwnsPhysicalUsage(t *testing.T) {
	owner, unrelated := new(executionTestObserver), new(executionTestObserver)
	execution := transport.WithUsageObserver(context.WithValue(context.Background(), executionTestKey("lease"), "owned-lease"), owner)
	execution = context.WithValue(execution, executionTestKey("session"), "stale-execution-session")
	calls := 0
	client := &http.Client{Transport: transport.WrapRoundTripper(executionTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if owner.begins.Load() != int32(calls) {
			t.Fatal("request dispatched before owning execution reserved usage")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"response","type":"message","usage":{"input_tokens":19,"output_tokens":3}}`))}, nil
	}), "anthropic", nil)}
	base := &executionTestProvider{complete: func(ctx context.Context, req Request) (*Response, error) {
		if ctx.Value(executionTestKey("lease")) != "owned-lease" || ctx.Value(executionTestKey("session")) != "current-helper-session" || ctx.Value(executionTestKey("trace")) != "helper-trace" {
			t.Fatalf("execution authority or caller identity lost: lease=%v session=%v trace=%v", ctx.Value(executionTestKey("lease")), ctx.Value(executionTestKey("session")), ctx.Value(executionTestKey("trace")))
		}
		httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://fixture.invalid/v1/messages", strings.NewReader(`{"model":"execution-model","stream":false}`))
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		return &Response{InputTokens: 19, OutputTokens: 3}, err
	}}
	p := BindExecutionProvider(base, execution)
	caller := context.WithValue(context.Background(), executionTestKey("session"), "current-helper-session")
	caller = context.WithValue(caller, executionTestKey("trace"), "helper-trace")
	caller = transport.WithUsageObserver(caller, unrelated)
	for _, ctx := range []context.Context{caller, context.WithoutCancel(caller)} {
		if _, err := p.Complete(ctx, Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || owner.begins.Load() != 2 || owner.ends.Load() != 2 || unrelated.begins.Load() != 0 {
		t.Fatalf("physical helper usage escaped authority: calls=%d owned=%d/%d unrelated=%d", calls, owner.begins.Load(), owner.ends.Load(), unrelated.begins.Load())
	}
}

func TestExecutionBoundRejectsEndedExecutionBeforeDispatch(t *testing.T) {
	execution, stop := context.WithCancel(context.Background())
	stop()
	calls := 0
	base := &executionTestProvider{complete: func(context.Context, Request) (*Response, error) { calls++; return &Response{}, nil }, stream: func(context.Context, Request) (StreamReader, error) { calls++; return nil, nil }}
	p := BindExecutionProvider(base, execution)
	if _, err := p.Complete(context.Background(), Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete after execution end: %v", err)
	}
	if _, err := p.Stream(context.WithoutCancel(execution), Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream after execution end: %v", err)
	}
	if calls != 0 {
		t.Fatalf("ended execution dispatched %d requests", calls)
	}
}

func TestExecutionBoundCancelsActiveDetachedCall(t *testing.T) {
	execution, stop := context.WithCancel(context.Background())
	defer stop()
	entered, finished := make(chan struct{}), make(chan error, 1)
	base := &executionTestProvider{complete: func(ctx context.Context, _ Request) (*Response, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	p := BindExecutionProvider(base, execution)
	go func() { _, err := p.Complete(context.Background(), Request{}); finished <- err }()
	<-entered
	stop()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached helper survived execution cancellation")
	}
}

func TestExecutionBoundCallerCancellationAndEarliestDeadline(t *testing.T) {
	for _, executionEarlier := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "execution"}[executionEarlier], func(t *testing.T) {
			soon, later := time.Now().Add(40*time.Millisecond), time.Now().Add(time.Second)
			execDeadline, callerDeadline := later, soon
			if executionEarlier {
				execDeadline, callerDeadline = soon, later
			}
			execution, stopExecution := context.WithDeadline(context.Background(), execDeadline)
			defer stopExecution()
			caller, stopCaller := context.WithDeadline(context.Background(), callerDeadline)
			defer stopCaller()
			base := &executionTestProvider{complete: func(ctx context.Context, _ Request) (*Response, error) {
				if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(soon) {
					t.Fatalf("deadline=%v exists=%v want=%v", deadline, ok, soon)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			if _, err := BindExecutionProvider(base, execution).Complete(caller, Request{}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("earliest deadline: %v", err)
			}
		})
	}
	execution, stopExecution := context.WithCancel(context.Background())
	defer stopExecution()
	caller, stopCaller := context.WithCancel(context.Background())
	stopCaller()
	calls := 0
	base := &executionTestProvider{complete: func(context.Context, Request) (*Response, error) { calls++; return nil, nil }}
	if _, err := BindExecutionProvider(base, execution).Complete(caller, Request{}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancelled caller dispatched: err=%v calls=%d", err, calls)
	}
}

type executionTestReader struct {
	ctx    context.Context
	closed atomic.Int32
	recv   func() (StreamEvent, error)
}

func (r *executionTestReader) Close() error               { r.closed.Add(1); return nil }
func (r *executionTestReader) Recv() (StreamEvent, error) { return r.recv() }

func TestExecutionBoundStreamKeepsLifecycleUntilClose(t *testing.T) {
	execution, stop := context.WithCancel(context.Background())
	defer stop()
	var callCtx context.Context
	raw := &executionTestReader{recv: func() (StreamEvent, error) { return StreamEvent{Type: "text_delta", TextDelta: "still streaming"}, nil }}
	base := &executionTestProvider{stream: func(ctx context.Context, _ Request) (StreamReader, error) { callCtx = ctx; return raw, nil }}
	reader, err := BindExecutionProvider(base, execution).Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if callCtx.Err() != nil {
		t.Fatal("Stream return canceled the active request")
	}
	if _, err := reader.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if callCtx.Err() != context.Canceled || raw.closed.Load() != 1 {
		t.Fatalf("Close failed lifecycle cleanup: err=%v closes=%d", callCtx.Err(), raw.closed.Load())
	}
	if err := reader.Close(); err != nil || raw.closed.Load() != 1 {
		t.Fatal("Close not idempotent")
	}
}

func TestExecutionBoundStreamCancellationAndTerminalRelease(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "execution_cancel", true: "eof"}[terminal], func(t *testing.T) {
			execution, stop := context.WithCancel(context.Background())
			defer stop()
			var callCtx context.Context
			raw := &executionTestReader{recv: func() (StreamEvent, error) {
				if terminal {
					return StreamEvent{}, io.EOF
				}
				<-callCtx.Done()
				return StreamEvent{}, callCtx.Err()
			}}
			base := &executionTestProvider{stream: func(ctx context.Context, _ Request) (StreamReader, error) {
				callCtx = ctx
				return raw, nil
			}}
			reader, err := BindExecutionProvider(base, execution).Stream(context.WithoutCancel(execution), Request{})
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { _, err := reader.Recv(); finished <- err }()
			if !terminal {
				stop()
			}
			select {
			case err := <-finished:
				want := error(context.Canceled)
				if terminal {
					want = io.EOF
				}
				if !errors.Is(err, want) || callCtx.Err() != context.Canceled {
					t.Fatalf("terminal recv err=%v ctx=%v", err, callCtx.Err())
				}
			case <-time.After(time.Second):
				t.Fatal("stream ignored owning execution cancellation")
			}
			if err := reader.Close(); err != nil || raw.closed.Load() != 1 {
				t.Fatal("terminal stream did not close once")
			}
		})
	}
}

func TestExecutionBoundKeepsCancellationCause(t *testing.T) {
	for _, authority := range []string{"caller", "execution"} {
		t.Run(authority, func(t *testing.T) {
			execution, stopExecution := context.WithCancelCause(context.Background())
			defer stopExecution(context.Canceled)
			caller, stopCaller := context.WithCancelCause(context.Background())
			defer stopCaller(context.Canceled)
			entered, finished := make(chan struct{}), make(chan error, 1)
			base := &executionTestProvider{complete: func(ctx context.Context, _ Request) (*Response, error) {
				close(entered)
				<-ctx.Done()
				return nil, context.Cause(ctx)
			}}
			cause := errors.New("fixture authority revoked")
			go func() { _, err := BindExecutionProvider(base, execution).Complete(caller, Request{}); finished <- err }()
			<-entered
			if authority == "caller" {
				stopCaller(cause)
			} else {
				stopExecution(cause)
			}
			select {
			case err := <-finished:
				if !errors.Is(err, cause) {
					t.Fatalf("cancellation cause lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation callback did not finish")
			}
		})
	}
}

type executionTestCapabilities struct {
	*executionTestProvider
	refresh func(context.Context) error
}

func (*executionTestCapabilities) ContextCapabilities() ContextCapabilities {
	return ContextCapabilities{Window: 1048576, MaxInputTokens: 900000, MaxOutputTokens: 64000, Source: "fixture"}
}
func (p *executionTestCapabilities) RefreshContextCapabilities(ctx context.Context) error {
	return p.refresh(ctx)
}
func (*executionTestCapabilities) ContextIncludesAssistantBlock(block ContentBlock) bool {
	return block.Type != "thinking"
}
func (*executionTestCapabilities) VisionCapability() VisionCapability { return VisionUnknown }
func (*executionTestCapabilities) ManagesRecoverySession() bool       { return true }

func TestExecutionBoundPreservesProviderCapabilitiesAndRefreshAuthority(t *testing.T) {
	execution, stop := context.WithCancel(context.WithValue(context.Background(), executionTestKey("lease"), "refresh-lease"))
	defer stop()
	refreshed := 0
	base := &executionTestCapabilities{executionTestProvider: &executionTestProvider{}, refresh: func(ctx context.Context) error {
		refreshed++
		if ctx.Value(executionTestKey("lease")) != "refresh-lease" {
			t.Fatal("metadata refresh lost execution context")
		}
		return nil
	}}
	p := BindExecutionProvider(base, execution)
	if p.Name() != base.Name() || p.ModelID() != base.ModelID() || p.MaxContextTokens() != base.MaxContextTokens() {
		t.Fatal("provider identity or model window changed")
	}
	caps, ok := p.(ContextCapabilitiesProvider)
	if !ok || caps.ContextCapabilities() != base.ContextCapabilities() {
		t.Fatal("context resolver was erased")
	}
	if err := caps.RefreshContextCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if policy, ok := p.(ContextHistoryPolicy); !ok || policy.ContextIncludesAssistantBlock(ContentBlock{Type: "thinking"}) {
		t.Fatal("history policy was erased")
	}
	if pubprov.ProviderVisionCapability(p) != VisionUnknown {
		t.Fatal("unknown vision declaration was fabricated")
	}
	if recovery, ok := p.(interface{ ManagesRecoverySession() bool }); !ok || !recovery.ManagesRecoverySession() {
		t.Fatal("provider recovery marker lost")
	}
	stop()
	if err := caps.RefreshContextCapabilities(context.Background()); !errors.Is(err, context.Canceled) || refreshed != 1 {
		t.Fatalf("ended refresh dispatched: err=%v calls=%d", err, refreshed)
	}
	plain := BindExecutionProvider(&executionTestProvider{}, context.Background())
	if _, ok := plain.(ContextCapabilitiesProvider); ok {
		t.Fatal("plain provider gained a context capability")
	}
	if _, ok := plain.(ContextHistoryPolicy); ok {
		t.Fatal("plain provider gained a history policy")
	}
	if pubprov.ProviderVisionCapability(plain) != VisionUnknown {
		t.Fatal("plain provider vision changed")
	}
}

func TestExecutionBoundOptionalCapabilityCombinations(t *testing.T) {
	base := &executionTestProvider{}
	full := &executionTestCapabilities{executionTestProvider: base, refresh: func(context.Context) error { return nil }}
	for bits := 0; bits < 8; bits++ {
		var underlying Provider
		switch bits {
		case 0:
			underlying = base
		case 1:
			underlying = struct {
				Provider
				ContextCapabilitiesProvider
			}{base, full}
		case 2:
			underlying = struct {
				Provider
				ContextHistoryPolicy
			}{base, full}
		case 3:
			underlying = struct {
				Provider
				ContextCapabilitiesProvider
				ContextHistoryPolicy
			}{base, full, full}
		case 4:
			underlying = struct {
				Provider
				pubprov.VisionCapabilityReporter
			}{base, full}
		case 5:
			underlying = struct {
				Provider
				ContextCapabilitiesProvider
				pubprov.VisionCapabilityReporter
			}{base, full, full}
		case 6:
			underlying = struct {
				Provider
				ContextHistoryPolicy
				pubprov.VisionCapabilityReporter
			}{base, full, full}
		case 7:
			underlying = full
		}
		bound := BindExecutionProvider(underlying, context.Background())
		_, metadata := bound.(ContextCapabilitiesProvider)
		_, history := bound.(ContextHistoryPolicy)
		_, vision := bound.(pubprov.VisionCapabilityReporter)
		if metadata != (bits&1 != 0) || history != (bits&2 != 0) || vision != (bits&4 != 0) {
			t.Fatalf("optional capabilities changed: bits=%d metadata=%t history=%t vision=%t", bits, metadata, history, vision)
		}
	}
	if BindExecutionProvider(nil, context.Background()) != nil || BindExecutionProvider(base, nil) != base {
		t.Fatal("unbound provider identity changed")
	}
}

type executionLegacyVisionProvider struct {
	*executionTestProvider
	supported bool
}

func (p *executionLegacyVisionProvider) SupportsVision() bool { return p.supported }

func TestExecutionBoundLegacyVisionPreservesDeclaration(t *testing.T) {
	for _, supported := range []bool{false, true} {
		base := &executionLegacyVisionProvider{executionTestProvider: &executionTestProvider{}, supported: supported}
		bound := BindExecutionProvider(base, context.Background())
		if pubprov.ProviderVisionCapability(bound) != pubprov.ProviderVisionCapability(base) {
			t.Fatalf("legacy vision changed: supported=%t", supported)
		}
	}
}
