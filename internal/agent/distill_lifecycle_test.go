package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

type distillLifecycleProvider struct{ name string }

func (p distillLifecycleProvider) Name() string        { return p.name }
func (p distillLifecycleProvider) ModelID() string     { return p.name + "-model" }
func (distillLifecycleProvider) MaxContextTokens() int { return 32_000 }
func (distillLifecycleProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("distill lifecycle repository intercepts Complete")
}
func (distillLifecycleProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("stream not used")
}

type distillLifecycleCall struct {
	providerName    string
	sessionID       string
	sourceMessageID string
	userMsg         string
	assistantMsg    string
}

// distillLifecycleRepository embeds the production contract so the test only
// needs to intercept the distillation method under test. The deliberately
// stubborn implementation observes cancellation but does not return until
// release is closed, modeling a provider that ignores context cancellation.
type distillLifecycleRepository struct {
	memory.Repository
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	calls    chan distillLifecycleCall

	startOnce  sync.Once
	cancelOnce sync.Once
}

func newDistillLifecycleRepository() *distillLifecycleRepository {
	return &distillLifecycleRepository{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
		calls:    make(chan distillLifecycleCall, 1),
	}
}

func (r *distillLifecycleRepository) DistillTurnWithMetadata(
	ctx context.Context,
	provider llm.Provider,
	sessionID, sourceMessageID, userMsg, assistantMsg string,
) error {
	r.startOnce.Do(func() { close(r.started) })
	select {
	case <-ctx.Done():
		r.cancelOnce.Do(func() { close(r.canceled) })
		<-r.release
	case <-r.release:
	}
	r.calls <- distillLifecycleCall{
		providerName:    provider.Name(),
		sessionID:       sessionID,
		sourceMessageID: sourceMessageID,
		userMsg:         userMsg,
		assistantMsg:    assistantMsg,
	}
	return ctx.Err()
}

func TestDistillationLifecycleCapturesProvenanceAndWaitsForStubbornJob(t *testing.T) {
	repository := newDistillLifecycleRepository()
	provider := distillLifecycleProvider{name: "provider-a"}
	loop := NewLoop(provider, nil, nil, nil, "", 3)
	loop.Memory = repository
	loop.DistillEvery = 1
	loop.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Remember the durable launch codename Marble Finch for later sessions."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "Understood; I will retain the Marble Finch launch codename for future sessions."}}},
	}
	sessionID := "session-a"
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: sessionID}
	}

	loop.maybeDistill()
	select {
	case <-repository.started:
	case <-time.After(time.Second):
		t.Fatal("background distillation did not start")
	}

	// Simulate the Desktop moving to another session while the provider call is
	// still live. The in-flight job must keep the exact provider, provenance and
	// history captured at the completed-turn boundary.
	sessionID = "session-b"
	loop.mu.Lock()
	loop.Provider = distillLifecycleProvider{name: "provider-b"}
	loop.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "This belongs only to session B and must not be distilled as session A."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "This is the session B assistant reply and must stay separate from session A."}}},
	}
	loop.mu.Unlock()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- loop.CancelAndWaitForDistillation(context.Background(), "session-a")
	}()
	select {
	case <-repository.canceled:
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not reach the in-flight distillation")
	}
	select {
	case err := <-waitDone:
		t.Fatalf("wait returned before stubborn provider exited: %v", err)
	default:
	}

	close(repository.release)
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("cancel and wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not finish after distillation exited")
	}

	call := <-repository.calls
	if call.providerName != "provider-a" || call.sessionID != "session-a" ||
		!strings.HasPrefix(call.sourceMessageID, "session-a/message/") {
		t.Fatalf("distillation provenance changed across session switch: %+v", call)
	}
	if call.userMsg != "Remember the durable launch codename Marble Finch for later sessions." ||
		call.assistantMsg != "Understood; I will retain the Marble Finch launch codename for future sessions." {
		t.Fatalf("distillation history changed across session switch: %+v", call)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := loop.WaitForDistillation(ctx, "session-a"); err != nil {
		t.Fatalf("completed job remained registered: %v", err)
	}
	if got := loop.FlushPendingDistillation(""); got != 0 {
		t.Fatalf("destructive cancel retained deleted pending content: launched=%d", got)
	}
}

func TestWaitForDistillationHonorsContext(t *testing.T) {
	repository := newDistillLifecycleRepository()
	loop := NewLoop(distillLifecycleProvider{name: "provider"}, nil, nil, nil, "", 3)
	loop.Memory = repository
	loop.DistillEvery = 1
	loop.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Remember this sufficiently long durable user preference for another session."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "I will remember this sufficiently long durable user preference for another session."}}},
	}
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: "session-timeout"}
	}
	loop.maybeDistill()
	select {
	case <-repository.started:
	case <-time.After(time.Second):
		t.Fatal("background distillation did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := loop.WaitForDistillation(ctx, "session-timeout"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForDistillation error = %v, want deadline exceeded", err)
	}

	close(repository.release)
	if err := loop.WaitForDistillation(context.Background(), "session-timeout"); err != nil {
		t.Fatalf("final wait: %v", err)
	}
}

func TestMaybeDistillDropsSnapshotWhenSessionGenerationChanges(t *testing.T) {
	repository := newDistillLifecycleRepository()
	loop := NewLoop(distillLifecycleProvider{name: "provider"}, nil, nil, nil, "", 3)
	loop.Memory = repository
	loop.DistillEvery = 1
	loop.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Session A contains a durable fact that must never be attributed to session B."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "This is session A's assistant response and belongs only to session A."}}},
	}
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		// Model the precise dangerous interleaving: history was captured from A,
		// then a top-level session boundary won before provenance was resolved.
		loop.ResetSession([]llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Session B has unrelated content."}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "This response belongs only to session B."}}},
		})
		return RuntimeStateSnapshot{SessionID: "session-b"}
	}

	loop.maybeDistill()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := loop.WaitForDistillation(ctx, "session-b"); err != nil {
		close(repository.release)
		t.Fatalf("stale session A snapshot launched as session B: %v", err)
	}
	select {
	case <-repository.started:
		t.Fatal("stale exchange was distilled after ResetSession")
	default:
	}
}

func TestDistillEveryZeroDisablesCadenceAndBoundaryFlush(t *testing.T) {
	repository := newDistillLifecycleRepository()
	loop := NewLoop(distillLifecycleProvider{name: "provider"}, nil, nil, nil, "", 3)
	loop.Memory = repository
	loop.DistillEvery = 0
	loop.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Remember this durable preference even though distillation is explicitly disabled."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "This successful answer is long enough to otherwise qualify for distillation."}}},
	}
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: "distillation-disabled"}
	}

	loop.maybeDistill()
	if got := loop.FlushPendingDistillation("distillation-disabled"); got != 0 {
		t.Fatalf("disabled residual flush launched=%d, want 0", got)
	}
	select {
	case <-repository.started:
		t.Fatal("DistillEvery=0 launched a provider call")
	case <-time.After(25 * time.Millisecond):
	}
}

type boundedDistillRepository struct {
	memory.Repository
	active  atomic.Int32
	peak    atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (r *boundedDistillRepository) DistillTurnWithMetadata(
	ctx context.Context,
	_ llm.Provider,
	_, _, _, _ string,
) error {
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		peak := r.peak.Load()
		if active <= peak || r.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	r.started <- struct{}{}
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestResidualFlushBoundsProviderConcurrency(t *testing.T) {
	repository := &boundedDistillRepository{
		started: make(chan struct{}, 12),
		release: make(chan struct{}),
	}
	provider := distillLifecycleProvider{name: "bounded-provider"}
	loop := NewLoop(provider, nil, nil, nil, "", 3)
	loop.Memory = repository
	const sessionID = "bounded-residual-session"
	for i := 1; i <= 12; i++ {
		if !loop.queuePendingDistillation(distillSnapshot{
			repository:      repository,
			provider:        provider,
			sessionID:       sessionID,
			sourceMessageID: fmt.Sprintf("%s/message/%d", sessionID, i),
			userMsg:         "A sufficiently long durable user message for bounded concurrency.",
			assistantMsg:    "A sufficiently long durable assistant response for bounded concurrency.",
			turn:            i,
		}) {
			t.Fatalf("queue pending snapshot %d", i)
		}
	}
	if got := loop.FlushPendingDistillation(sessionID); got != 12 {
		t.Fatalf("launched jobs=%d, want 12 registered jobs", got)
	}
	for i := 0; i < maxConcurrentDistillations; i++ {
		select {
		case <-repository.started:
		case <-time.After(time.Second):
			close(repository.release)
			t.Fatalf("provider calls started=%d, want %d", i, maxConcurrentDistillations)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := repository.active.Load(); got != maxConcurrentDistillations {
		close(repository.release)
		t.Fatalf("active provider calls=%d, want bounded at %d", got, maxConcurrentDistillations)
	}
	if got := repository.peak.Load(); got > maxConcurrentDistillations {
		close(repository.release)
		t.Fatalf("peak provider calls=%d exceeded limit %d", got, maxConcurrentDistillations)
	}
	close(repository.release)
	if err := loop.WaitForDistillation(context.Background(), sessionID); err != nil {
		t.Fatalf("join bounded residual jobs: %v", err)
	}
}
