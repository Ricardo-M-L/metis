package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

type bindingEventRepository struct {
	memory.Repository
	eventID  string
	passages []memory.Passage
	onEvent  func()
	err      error
}

func (r *bindingEventRepository) MarkRetrievedEvent(eventID string, passages []memory.Passage) error {
	r.eventID, r.passages = eventID, append([]memory.Passage(nil), passages...)
	if r.onEvent != nil {
		r.onEvent()
	}
	return r.err
}

type bindingLegacyUsageRepository struct {
	memory.Repository
	passages []memory.Passage
	err      error
}

func (r *bindingLegacyUsageRepository) MarkRetrieved(passages []memory.Passage) error {
	r.passages = append([]memory.Passage(nil), passages...)
	return r.err
}

func TestWorkspaceBindingMarkRetrievedEventForwardsFrozenWorkspace(t *testing.T) {
	sentinel := errors.New("usage persistence failed")
	source := &bindingEventRepository{err: sentinel}
	target := &bindingEventRepository{}
	binding := &WorkspaceMemoryBinding{current: source}
	source.onEvent = func() {
		if !binding.mu.TryLock() {
			t.Fatal("binding lock held while forwarding usage event")
		}
		binding.mu.Unlock()
		binding.CommitWorkspace(&WorkspaceMemoryRebind{repository: target, scope: "target"})
	}
	passages := []memory.Passage{{ID: "source-passage", Scope: "source"}}
	if err := binding.MarkRetrievedEvent("request-one", passages); !errors.Is(err, sentinel) {
		t.Fatalf("usage error not forwarded: %v", err)
	}
	if source.eventID != "request-one" || len(source.passages) != 1 || source.passages[0].ID != "source-passage" {
		t.Fatalf("source receipt changed: %+v", source)
	}
	if target.eventID != "" || len(target.passages) != 0 {
		t.Fatal("workspace switch redirected an in-flight receipt")
	}
	if err := binding.MarkRetrievedEvent("request-two", nil); err != nil || target.eventID != "request-two" {
		t.Fatalf("new receipt did not use target workspace: event=%q err=%v", target.eventID, err)
	}
}

func TestWorkspaceBindingMarkRetrievedEventPreservesIdempotency(t *testing.T) {
	manager, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Archival().Insert(memory.Passage{ID: "used", Content: "Cedar deployment archive"}); err != nil {
		t.Fatal(err)
	}
	binding := &WorkspaceMemoryBinding{current: manager}
	for i := 0; i < 2; i++ {
		if err := binding.MarkRetrievedEvent("provider-request", []memory.Passage{{ID: "used"}}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := manager.Search(memory.SearchOptions{Query: "Cedar deployment"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("search recorded passage: hits=%+v err=%v", hits, err)
	}
	if hits[0].UseCount != 1 {
		t.Fatalf("repeated request recorded %d uses, want 1", hits[0].UseCount)
	}
}

func TestWorkspaceBindingWorkspaceSwitchDoesNotWaitForUsageIO(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	source := &bindingEventRepository{onEvent: func() {
		close(started)
		<-release
	}}
	target := &bindingEventRepository{}
	binding := &WorkspaceMemoryBinding{current: source}
	done := make(chan error, 1)
	go func() { done <- binding.MarkRetrievedEvent("source-request", nil) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("source usage operation did not start")
	}
	switched := make(chan struct{})
	go func() {
		binding.CommitWorkspace(&WorkspaceMemoryRebind{repository: target, scope: "target"})
		close(switched)
	}()
	select {
	case <-switched:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("workspace switch waited on repository IO under the binding lock")
	}
	close(release)
	if err := <-done; err != nil || source.eventID != "source-request" || target.eventID != "" {
		t.Fatalf("in-flight event changed workspace: source=%q target=%q err=%v", source.eventID, target.eventID, err)
	}
}

func TestWorkspaceBindingMarkRetrievedEventFallsBackForLegacyRepository(t *testing.T) {
	sentinel := errors.New("legacy persistence failed")
	legacy := &bindingLegacyUsageRepository{err: sentinel}
	binding := &WorkspaceMemoryBinding{current: legacy}
	if err := binding.MarkRetrievedEvent("request", []memory.Passage{{ID: "one"}}); !errors.Is(err, sentinel) {
		t.Fatalf("legacy fallback error not forwarded: %v", err)
	}
	if len(legacy.passages) != 1 || legacy.passages[0].ID != "one" {
		t.Fatalf("legacy fallback selection changed: %+v", legacy.passages)
	}
	var unavailable *memory.MemoryManager
	for _, binding := range []*WorkspaceMemoryBinding{nil, {}, {current: unavailable}} {
		if err := binding.MarkRetrievedEvent("request", []memory.Passage{{ID: "one"}}); !errors.Is(err, errMemoryRepositoryUnavailable) {
			t.Fatalf("unavailable repository error = %v", err)
		}
	}
}

func TestWorkspaceBindingDurableMemoryManagerFreezesConcreteWorkspace(t *testing.T) {
	root := t.TempDir()
	workspaceA, workspaceB := t.TempDir(), t.TempDir()
	managerA, err := memory.NewMemoryManagerForWorkspace(root, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	binding := &WorkspaceMemoryBinding{current: managerA, root: root}
	frozen := binding.DurableMemoryManager()
	if frozen != managerA {
		t.Fatal("concrete source manager was not preserved")
	}
	prepared, err := binding.PrepareWorkspace(workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	binding.CommitWorkspace(prepared)
	target := binding.DurableMemoryManager()
	if target == nil || target == frozen || target.WorkspaceScope() == frozen.WorkspaceScope() {
		t.Fatal("workspace activation did not publish a distinct concrete view")
	}
	scopeA, _ := memory.WorkspaceScope(workspaceA)
	if frozen.WorkspaceScope() != scopeA {
		t.Fatal("workspace switch changed the source hand-off scope")
	}
	var unavailable *memory.MemoryManager
	for _, binding := range []*WorkspaceMemoryBinding{
		nil, {}, {current: unavailable},
		{current: bindingLegacyRepository{managerA}},
		{current: &WorkspaceMemoryBinding{current: managerA}},
	} {
		if got := binding.DurableMemoryManager(); got != nil {
			t.Fatalf("custom or unavailable repository gained a durable lifecycle: %v", got)
		}
	}
}

func TestWorkspaceBindingCapabilitiesDoNotRestoreFilteredMemoryTool(t *testing.T) {
	manager, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(nil, manager))
	tools.ApplyToolVisibility(registry, []string{"Read"}, nil)
	loop := agent.NewLoop(nil, registry, nil, nil, "", 1)
	loop.Memory = manager
	binding, err := BindLoopWorkspaceMemory(loop, "")
	if err != nil || binding == nil || binding.DurableMemoryManager() != manager {
		t.Fatalf("bind concrete manager: binding=%v err=%v", binding, err)
	}
	if _, present := registry.Get("Memory"); present {
		t.Fatal("binding reintroduced Memory despite the tools allowlist")
	}
}
