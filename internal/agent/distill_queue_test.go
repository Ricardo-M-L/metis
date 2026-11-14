package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type durableQueueProvider struct{ calls atomic.Int32 }

type profiledDistillationQueueProvider struct {
	durableQueueProvider
	profile string
}

func (p *profiledDistillationQueueProvider) ContextCapabilities() llm.ContextCapabilities {
	return llm.ContextCapabilities{ProviderID: p.profile, Model: p.ModelID(), Window: 100000}
}
func (*profiledDistillationQueueProvider) RefreshContextCapabilities(context.Context) error {
	return nil
}

func (*durableQueueProvider) Name() string          { return "queue-fixture" }
func (*durableQueueProvider) ModelID() string       { return "queue-model" }
func (*durableQueueProvider) MaxContextTokens() int { return 100000 }
func (p *durableQueueProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	p.calls.Add(1)
	return &llm.Response{}, nil
}
func (*durableQueueProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("unused stream")
}

type durableQueueRepository struct {
	memory.Repository
	process func(context.Context, string, string) error
}

// Custom implementations keep the original bounded, in-process lifecycle.
type legacyDistillationRepository struct{ memory.Repository }

type classifiedDistillationFixtureError struct{ code string }

func (*classifiedDistillationFixtureError) Error() string {
	return "private credential and conversation error details"
}
func (e *classifiedDistillationFixtureError) DistillationFailureCode() string { return e.code }

type explicitDurableQueueBinding struct {
	memory.Repository
	current *memory.MemoryManager
}

type admittedDistillationProvider struct {
	durableQueueProvider
	entered chan struct{}
	release chan struct{}
}

func (p *admittedDistillationProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	close(p.entered)
	<-p.release // Deliberately retain an already admitted input across deletion.
	return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: `[{"type":"user","content":"An admitted source must never resurrect after deletion."}]`}}}, nil
}

func (b *explicitDurableQueueBinding) DurableMemoryManager() *memory.MemoryManager { return b.current }

func (r *durableQueueRepository) DistillTurnWithMetadata(ctx context.Context, _ llm.Provider, session, source, _, _ string) error {
	return r.process(ctx, session, source)
}

func queueFixture(t *testing.T) (*memory.MemoryManager, *durableQueueProvider, distillSnapshot) {
	t.Helper()
	workspace := t.TempDir()
	manager, err := memory.NewMemoryManagerForWorkspace(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	provider := &durableQueueProvider{}
	return manager, provider, distillSnapshot{repository: manager, provider: provider, sessionID: "durable-session", sourceMessageID: "durable-message", sourceRequestID: "durable-request", workspacePath: workspace, providerName: "configured-fixture", userMsg: "I prefer deployment only after focused acceptance tests pass.", assistantMsg: "I will remember your deployment preference for future work.", turn: 1}
}

func TestDurableDistillationQueuePersistsAndDeduplicatesBeforeProvider(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	for i := 0; i < 2; i++ {
		if handled, err := enqueueDurableDistillation(snapshot); !handled || err != nil {
			t.Fatalf("enqueue = %v, %v", handled, err)
		}
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v, err=%v", jobs, err)
	}
	job := jobs[0]
	if job.Status != "pending" || job.Attempts != 0 || job.ProviderName != "configured-fixture" || job.Scope != manager.WorkspaceScope() || job.SourceRequestID != snapshot.sourceRequestID {
		t.Fatalf("frozen job = %+v", job)
	}
	if provider.calls.Load() != 0 {
		t.Fatal("enqueue invoked LLM")
	}
	for path, mode := range map[string]os.FileMode{filepath.Join(manager.Root(), distillationQueueDirectory): 0o700, filepath.Join(manager.Root(), distillationQueueDirectory, job.ID+".json"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private queue %s: info=%v err=%v", path, info, err)
		}
	}
}

func TestDurableDistillationQueueRetainsShortPreferenceAndSkipsExactRestatement(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	snapshot.userMsg, snapshot.assistantMsg = "以后用中文。", "好的。"
	if handled, err := enqueueDurableDistillation(snapshot); !handled || err != nil {
		t.Fatalf("short preference enqueue=%v,%v", handled, err)
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 || jobs[0].UserMessage != "以后用中文。" {
		t.Fatalf("short preference lost: jobs=%v err=%v", jobs, err)
	}
	known := "The user's workstation codename is Nimbus Finch."
	if err := manager.Archival().Insert(memory.Passage{ID: "existing-durable-fact", Type: memory.TypeUser, Content: known}); err != nil {
		t.Fatal(err)
	}
	snapshot.userMsg, snapshot.assistantMsg, snapshot.sourceMessageID = known, known, "repeat-message"
	if handled, err := enqueueDurableDistillation(snapshot); !handled || err != nil {
		t.Fatalf("restatement enqueue=%v,%v", handled, err)
	}
	jobs, err = listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("exact restatement consumed another job: jobs=%v err=%v", jobs, err)
	}
}

func TestCompletedTurnDurablyQueuesWithoutJoiningProvider(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory, loop.DistillEvery = manager, 5
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: snapshot.sessionID, WorkingDirectory: snapshot.workspacePath, Provider: "configured-fixture"}
	}
	var launches atomic.Int32
	loop.DurableDistillationLauncher = func(root string) error {
		launches.Add(1)
		return errors.New("simulated launch failure; queue retained")
	}
	loop.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.userMsg}}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.assistantMsg}}}}
	completed, err := loop.recordCompletedTurn(context.Background())
	if err != nil || completed == nil {
		t.Fatalf("record = %v, %v", completed, err)
	}
	if provider.calls.Load() != 0 || launches.Load() != 0 {
		t.Fatalf("provider=%d launcher=%d", provider.calls.Load(), launches.Load())
	}
	if ready, err := DurableDistillationQueueHasPending(manager.Root()); err != nil || ready {
		t.Fatalf("pre-cadence task became eligible: %v, %v", ready, err)
	}
	if err := loop.WaitForDistillation(context.Background(), snapshot.sessionID); err != nil {
		t.Fatal(err)
	}
	if handled, err := loop.PersistPendingDistillation(snapshot.sessionID); !handled || err != nil {
		t.Fatalf("boundary=%v,%v", handled, err)
	}
	if launches.Load() != 1 {
		t.Fatalf("boundary launcher calls=%d, want 1", launches.Load())
	}
	if count := loop.FlushPendingDistillation(snapshot.sessionID); count != 0 {
		t.Fatalf("repeated boundary re-released %d jobs", count)
	}
	if pending, err := DurableDistillationQueueHasPending(manager.Root()); err != nil || !pending {
		t.Fatalf("pending=%v,%v", pending, err)
	}
	if sessions, err := os.ReadDir(filepath.Join(manager.Root(), "recall")); err != nil || len(sessions) == 0 {
		t.Fatalf("synchronous recall lost: %v %v", sessions, err)
	}
}

func TestDurableDistillationQueueUsesAuthoritativeProfileAndRequestModel(t *testing.T) {
	for _, callbackProvider := range []string{"", "queue-fixture"} {
		t.Run("callback="+callbackProvider, func(t *testing.T) {
			manager, _, snapshot := queueFixture(t)
			provider := &profiledDistillationQueueProvider{profile: "custom-profile-not-adapter-name"}
			loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 3)
			loop.Memory, loop.Model = manager, "actual-request-model"
			loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
				return RuntimeStateSnapshot{SessionID: snapshot.sessionID, WorkingDirectory: snapshot.workspacePath, Provider: callbackProvider}
			}
			loop.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.userMsg}}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.assistantMsg}}}}
			if _, err := loop.recordCompletedTurn(context.Background()); err != nil {
				t.Fatal(err)
			}
			jobs, err := listDistillationQueueJobs(manager.Root())
			if err != nil || len(jobs) != 1 {
				t.Fatalf("queued jobs=%v err=%v", jobs, err)
			}
			if jobs[0].ProviderName != provider.profile || jobs[0].ModelID != loop.Model {
				t.Fatalf("queue lost actual profile/model: provider=%q model=%q", jobs[0].ProviderName, jobs[0].ModelID)
			}
		})
	}
}

func TestDurableDistillationQueueProfileFollowsLiveRebindAndRollback(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	a := &profiledDistillationQueueProvider{profile: "custom-a"}
	b := &profiledDistillationQueueProvider{profile: "custom-b"}
	loop := NewLoop(a, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory = manager
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: snapshot.sessionID, WorkingDirectory: snapshot.workspacePath, Provider: "queue-fixture"}
	}
	loop.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.userMsg}}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.assistantMsg}}}}
	loop.RebindProviderRuntime(a, "request-a", 1024, "system-a", nil)
	saved := loop.ProviderRuntimeState()
	loop.RebindProviderRuntime(b, "request-b", 2048, "system-b", nil)
	completedB, err := loop.recordCompletedTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	loop.RebindProviderRuntime(saved.Provider, saved.Model, saved.MaxOutputTokens, saved.System, saved.SystemSections)
	completedA, err := loop.recordCompletedTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	loop.RebindProviderModel(a, "request-a-model-only-override")
	completedOverride, err := loop.recordCompletedTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{completedB.sourceMessageID: {"custom-b", "request-b"}, completedA.sourceMessageID: {"custom-a", "request-a"}, completedOverride.sourceMessageID: {"custom-a", "request-a-model-only-override"}}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != len(want) {
		t.Fatalf("queued jobs=%v err=%v", jobs, err)
	}
	for _, job := range jobs {
		if expected := want[job.SourceMessageID]; job.ProviderName != expected[0] || job.ModelID != expected[1] {
			t.Fatalf("rebind/rollback profile drift: provider=%q model=%q expected=%v", job.ProviderName, job.ModelID, expected)
		}
	}
}

func TestDurableDistillationQueueFreezesWorkspaceBindingBeforeSessionCallback(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	other, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binding := &explicitDurableQueueBinding{Repository: other, current: manager}
	loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory = binding
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		binding.current = other
		return RuntimeStateSnapshot{SessionID: snapshot.sessionID, WorkingDirectory: snapshot.workspacePath, Provider: snapshot.providerName}
	}
	loop.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.userMsg}}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: snapshot.assistantMsg}}}}
	if _, err := loop.recordCompletedTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 || jobs[0].Scope != manager.WorkspaceScope() {
		t.Fatalf("source scope lost: jobs=%v err=%v", jobs, err)
	}
	if jobs, err := listDistillationQueueJobs(other.Root()); err != nil || len(jobs) != 0 {
		t.Fatalf("rebound repository received old exchange: jobs=%v err=%v", jobs, err)
	}
	if sessions := readRecallSessions(t, manager.Root()); len(sessions) != 1 {
		t.Fatalf("synchronous record missed frozen source: %v", sessions)
	}
}

func TestDurableDistillationQueueRecoversAbandonedLeaseAndKeepsCompletion(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	jobs, _ := listDistillationQueueJobs(manager.Root())
	job := jobs[0]
	job.Status, job.LeaseUntil, job.Attempts = "running", time.Now().Add(time.Hour), 1
	if err := writeDistillationQueueJob(manager.Root(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	resolver := func(_ context.Context, got DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		if got.SourceMessageID != snapshot.sourceMessageID || got.Attempts != 2 {
			t.Fatalf("recovered source = %+v", got)
		}
		return &durableQueueRepository{Repository: manager, process: func(_ context.Context, session, source string) error { calls++; return nil }}, provider, nil
	}
	for i := 0; i < 2; i++ {
		if err := RunDurableDistillationQueue(context.Background(), manager.Root(), resolver); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if calls != 1 || jobs[0].Status != "completed" || jobs[0].UserMessage != "" || jobs[0].AssistantMessage != "" {
		t.Fatalf("completion calls=%d job=%+v", calls, jobs[0])
	}
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if len(jobs) != 1 || jobs[0].Status != "completed" {
		t.Fatalf("completed identity requeued: %+v", jobs)
	}
}

func TestDurableDistillationQueueFailureRemainsRetryableOnRestart(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := RunDurableDistillationQueue(ctx, manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return &durableQueueRepository{Repository: manager, process: func(context.Context, string, string) error {
			return &memory.DistillationProviderError{Err: errors.New("private provider detail")}
		}}, provider, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("worker deadline=%v", err)
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 || jobs[0].Status != "pending" || jobs[0].Attempts != 1 || jobs[0].LastFailure != "provider_error" || jobs[0].UserMessage == "" {
		t.Fatalf("failure evidence=%+v err=%v", jobs, err)
	}
	data, _ := os.ReadFile(filepath.Join(manager.Root(), distillationQueueDirectory, jobs[0].ID+".json"))
	if strings.Contains(string(data), "private provider detail") {
		t.Fatal("provider error persisted verbatim")
	}
	job := jobs[0]
	job.NextAttemptAt = time.Now().Add(-time.Second)
	if err := writeDistillationQueueJob(manager.Root(), job); err != nil {
		t.Fatal(err)
	}
	if err := RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return &durableQueueRepository{Repository: manager, process: func(context.Context, string, string) error { return nil }}, provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if jobs[0].Status != "completed" || jobs[0].Attempts != 2 {
		t.Fatalf("retry=%+v", jobs[0])
	}
}

func TestDurableDistillationQueueStoresOnlySafeFailureClassification(t *testing.T) {
	for _, code := range []string{"config_namespace", "workspace", "provider_configuration", "repository", "private-secret-must-not-be-stored"} {
		t.Run(code, func(t *testing.T) {
			manager, _, snapshot := queueFixture(t)
			if _, err := enqueueDurableDistillation(snapshot); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			err := RunDurableDistillationQueue(ctx, manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
				cancel()
				return nil, nil, &classifiedDistillationFixtureError{code: code}
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("bounded failed attempt=%v", err)
			}
			jobs, err := listDistillationQueueJobs(manager.Root())
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs=%v err=%v", jobs, err)
			}
			want := code
			if strings.HasPrefix(code, "private") {
				want = "processing_error"
			}
			if jobs[0].LastFailure != want || jobs[0].Status != "pending" {
				t.Fatalf("failure classification=%q status=%q", jobs[0].LastFailure, jobs[0].Status)
			}
			data, _ := os.ReadFile(filepath.Join(manager.Root(), distillationQueueDirectory, jobs[0].ID+".json"))
			if strings.Contains(string(data), "private credential") || strings.Contains(string(data), "private-secret") {
				t.Fatal("queue persisted untrusted error data")
			}
		})
	}
}

func TestDurableDistillationQueueRevokesDeletedSessionBeforeProvider(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteSession(snapshot.sessionID); err != nil {
		t.Fatal(err)
	}
	if err := RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return manager, provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := listDistillationQueueJobs(manager.Root())
	if provider.calls.Load() != 0 || jobs[0].Status != "revoked" || jobs[0].UserMessage != "" {
		t.Fatalf("deleted source processed: calls=%d job=%+v", provider.calls.Load(), jobs[0])
	}
}

func TestDurableDistillationQueueRevokesBeforeUnavailableResolver(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	jobs, _ := listDistillationQueueJobs(manager.Root())
	stale := jobs[0]
	if err := manager.DeleteSession(snapshot.sessionID); err != nil {
		t.Fatal(err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if jobs[0].Status != "revoked" || jobs[0].UserMessage != "" || jobs[0].AssistantMessage != "" {
		t.Fatalf("delete did not synchronously scrub without a worker: %+v", jobs[0])
	}
	// A legacy/stale external writer must still be blocked before resolving
	// a removed workspace or a now-unavailable provider profile.
	if err := writeDistillationQueueJob(manager.Root(), stale); err != nil {
		t.Fatal(err)
	}
	var resolves int
	if err := RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		resolves++
		return nil, nil, errors.New("workspace and provider are unavailable")
	}); err != nil {
		t.Fatal(err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if resolves != 0 || jobs[0].Status != "revoked" || jobs[0].UserMessage != "" {
		t.Fatalf("revocation depended on resolver: resolves=%d job=%+v", resolves, jobs[0])
	}
}

func TestDurableDistillationQueueFinishCannotRestoreDeletedPayload(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		if err := manager.DeleteSession(snapshot.sessionID); err != nil {
			t.Fatal(err)
		}
		return nil, nil, errors.New("resolver failed after concurrent delete")
	}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := listDistillationQueueJobs(manager.Root())
	if jobs[0].Status != "revoked" || jobs[0].UserMessage != "" || jobs[0].AssistantMessage != "" {
		t.Fatalf("worker restored deleted payload: %+v", jobs[0])
	}
}

func TestDurableDistillationQueueDeletionDoesNotWaitForAdmittedInputOrAllowLateWrite(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	provider := &admittedDistillationProvider{entered: make(chan struct{}), release: make(chan struct{})}
	snapshot.provider = provider
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
			return manager, provider, nil
		})
	}()
	release := func() {
		select {
		case <-provider.release:
		default:
			close(provider.release)
		}
	}
	t.Cleanup(release)
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider work was not admitted")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- manager.DeleteSession(snapshot.sessionID) }()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		release()
		t.Fatal("deletion held a lock across admitted network work")
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 || jobs[0].Status != "revoked" || jobs[0].UserMessage != "" {
		t.Fatalf("admitted source was not synchronously scrubbed: %+v, %v", jobs, err)
	}
	release()
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted provider did not exit after release")
	}
	fresh, err := memory.NewMemoryManagerForWorkspace(manager.Root(), snapshot.workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if hits, err := fresh.Archival().Search(memory.SearchOptions{Query: "resurrect"}); err != nil || len(hits) != 0 {
		t.Fatalf("admitted input resurrected deleted memory: %+v, %v", hits, err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if jobs[0].Status != "revoked" || jobs[0].UserMessage != "" || jobs[0].AssistantMessage != "" {
		t.Fatalf("late completion restored deleted payload: %+v", jobs[0])
	}
}

func TestDurableDistillationQueueConcurrentEnqueueAndDeleteCannotRetainPayload(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	for iteration := range 12 {
		current := snapshot
		current.sessionID = snapshot.sessionID + strings.Repeat("x", iteration+1)
		start := make(chan struct{})
		failures := make(chan error, 2)
		var writers sync.WaitGroup
		writers.Add(2)
		go func() { defer writers.Done(); <-start; _, err := enqueueDurableDistillation(current); failures <- err }()
		go func() { defer writers.Done(); <-start; failures <- manager.DeleteSession(current.sessionID) }()
		close(start)
		writers.Wait()
		close(failures)
		for err := range failures {
			if err != nil && !errors.Is(err, memory.ErrSessionDeleted) {
				t.Fatal(err)
			}
		}
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.Status != "revoked" || job.UserMessage != "" || job.AssistantMessage != "" {
			t.Fatalf("enqueue raced past deletion: %+v", job)
		}
	}
}

func TestDurableDistillationQueueOldFailureDoesNotStarveDueJob(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	jobs, _ := listDistillationQueueJobs(manager.Root())
	old := jobs[0]
	old.Attempts = 10
	old.CreatedAt = time.Now().Add(-time.Hour)
	if err := writeDistillationQueueJob(manager.Root(), old); err != nil {
		t.Fatal(err)
	}
	snapshot.sourceMessageID, snapshot.sourceRequestID = "new-valid-message", "new-valid-request"
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var completed int
	err := RunDurableDistillationQueue(ctx, manager.Root(), func(_ context.Context, job DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		if job.ID == old.ID {
			return nil, nil, errors.New("old profile remains unavailable")
		}
		return &durableQueueRepository{Repository: manager, process: func(context.Context, string, string) error { completed++; cancel(); return nil }}, provider, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("finite worker budget=%v", err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if completed != 1 {
		t.Fatalf("old failure starved valid due job: completed=%d jobs=%+v", completed, jobs)
	}
	for _, job := range jobs {
		if job.ID == old.ID && (job.Attempts != 11 || job.Status != "pending") {
			t.Fatalf("old failure not retained: %+v", job)
		}
		if job.ID != old.ID && job.Status != "completed" {
			t.Fatalf("valid job incomplete: %+v", job)
		}
	}
}

func TestDurableDistillationQueueRejectsWorkspaceAndSymlinkEscape(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	snapshot.workspacePath = t.TempDir()
	if _, err := enqueueDurableDistillation(snapshot); err == nil {
		t.Fatal("mismatched workspace accepted")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(manager.Root(), distillationQueueDirectory)); err != nil {
		t.Skip(err)
	}
	snapshot.workspacePath = ""
	// A global manager reaches the queue path independently of current cwd.
	global, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(global.Root(), distillationQueueDirectory)); err != nil {
		t.Fatal(err)
	}
	snapshot.repository = global
	if _, err := enqueueDurableDistillation(snapshot); err == nil {
		t.Fatal("symlink queue accepted")
	}
}

func TestDurableDistillationQueueRecoversAfterWorkerKill(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	child := exec.Command(os.Args[0], "-test.run=^TestDurableDistillationQueueKilledWorkerHelper$")
	child.Env = append(os.Environ(), "METIS_QUEUE_KILL_HELPER=1", "METIS_QUEUE_KILL_ROOT="+manager.Root(), "METIS_QUEUE_KILL_WORKSPACE="+snapshot.workspacePath, "METIS_QUEUE_KILL_MARKER="+marker)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not reach running lease")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if active, err := DurableDistillationQueueWorkerActive(manager.Root()); err != nil || !active {
		t.Fatalf("worker active=%v,%v", active, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	jobs, _ := listDistillationQueueJobs(manager.Root())
	if jobs[0].Status != "running" || jobs[0].Attempts != 1 {
		t.Fatalf("missing durable running evidence: %+v", jobs[0])
	}
	if err := RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return &durableQueueRepository{Repository: manager, process: func(context.Context, string, string) error { return nil }}, provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	if jobs[0].Status != "completed" || jobs[0].Attempts != 2 {
		t.Fatalf("strong-kill recovery=%+v", jobs[0])
	}
}

func TestDurableDistillationQueueKilledWorkerHelper(t *testing.T) {
	if os.Getenv("METIS_QUEUE_KILL_HELPER") != "1" {
		return
	}
	root := os.Getenv("METIS_QUEUE_KILL_ROOT")
	manager, err := memory.NewMemoryManagerForWorkspace(root, os.Getenv("METIS_QUEUE_KILL_WORKSPACE"))
	if err != nil {
		t.Fatal(err)
	}
	err = RunDurableDistillationQueue(context.Background(), root, func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return &durableQueueRepository{Repository: manager, process: func(ctx context.Context, _, _ string) error {
			if err := os.WriteFile(os.Getenv("METIS_QUEUE_KILL_MARKER"), []byte("running"), 0o600); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}}, &durableQueueProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
