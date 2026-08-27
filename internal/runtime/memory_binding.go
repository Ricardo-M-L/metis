package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

var errMemoryRepositoryUnavailable = errors.New("memory repository is unavailable")

// WorkspaceMemoryBinding is the one stable Repository identity shared by a
// Desktop loop, its Memory tool, distillation, and Auto Memory. Session
// activation swaps only the binding's current workspace view; consumers never
// retain independently-rebound pointers.
//
// A repository operation snapshots current under a read lock, then releases
// the lock before doing filesystem work. The Desktop lifecycle barrier joins
// source-session distillation and Auto Memory before CommitWorkspace, so an
// already-started operation may finish on its source snapshot while every new
// operation observes the target view.
type WorkspaceMemoryBinding struct {
	mu             sync.RWMutex
	current        memory.Repository
	root           string
	workspaceScope string
}

// WorkspaceMemoryRebind is a fully constructed target view. Preparing it is
// fallible and happens before the source session boundary; committing it is a
// small, non-failing pointer swap after all source writers have joined.
type WorkspaceMemoryRebind struct {
	repository memory.Repository
	scope      string
}

// BindLoopWorkspaceMemory replaces a loop's concrete repository with one
// stable binding. If the Memory tool is visible, it is replaced in-place with
// a tool backed by that same binding. A filtered-out Memory tool is never
// reintroduced.
//
// Callers must use this during server construction, before accepting turns.
func BindLoopWorkspaceMemory(loop *agent.Loop, initialWorkspace string) (*WorkspaceMemoryBinding, error) {
	if loop == nil || !usableMemoryRepository(loop.Memory) {
		return nil, nil
	}
	if existing, ok := loop.Memory.(*WorkspaceMemoryBinding); ok {
		return existing, nil
	}
	binding := &WorkspaceMemoryBinding{
		current: loop.Memory,
		root:    loop.Memory.Root(),
	}
	// Do not merely label the existing repository with the restored header's
	// workspace. setupRuntime constructed its concrete manager from process cwd,
	// which can differ from the initial resumed session. Reconstruct the actual
	// workspace view before the Desktop accepts its first turn. Custom Repository
	// decorators stay intact and deliberately remain unlabelled; their first
	// explicit activation will go through PrepareWorkspace instead of silently
	// claiming a scope we cannot verify.
	var bindErr error
	if _, concrete := loop.Memory.(*memory.MemoryManager); concrete && strings.TrimSpace(initialWorkspace) != "" {
		if initial, err := memory.NewMemoryManagerForWorkspace(binding.root, initialWorkspace); err == nil {
			binding.current = initial
			binding.workspaceScope, _ = memory.WorkspaceScope(initialWorkspace)
		} else {
			// Fail closed: retaining the launch-cwd repository while claiming the
			// restored header's workspace would silently leak and misattribute data.
			binding.current = nil
			bindErr = fmt.Errorf("bind initial memory workspace: %w", err)
		}
	}
	loop.Memory = binding
	if loop.Registry != nil {
		if _, visible := loop.Registry.Get("Memory"); visible {
			loop.Registry.Replace(builtin.NewMemory(loop.Gate, binding).WithSourceSessionIDFn(CurrentSessionID))
		}
	}
	return binding, bindErr
}

// PrepareWorkspace constructs and validates a target workspace view without
// mutating the live binding. Empty paths preserve compatibility with old
// session headers that predate WorkDir.
func (b *WorkspaceMemoryBinding) PrepareWorkspace(workspacePath string) (*WorkspaceMemoryRebind, error) {
	if b == nil || strings.TrimSpace(workspacePath) == "" {
		return nil, nil
	}
	scope, err := memory.WorkspaceScope(workspacePath)
	if err != nil {
		return nil, err
	}
	b.mu.RLock()
	currentScope := b.workspaceScope
	root := b.root
	b.mu.RUnlock()
	if scope == currentScope {
		return nil, nil
	}
	if strings.TrimSpace(root) == "" {
		return nil, errMemoryRepositoryUnavailable
	}
	repository, err := memory.NewMemoryManagerForWorkspace(root, workspacePath)
	if err != nil {
		return nil, err
	}
	return &WorkspaceMemoryRebind{repository: repository, scope: scope}, nil
}

// CommitWorkspace atomically publishes a prepared workspace view. nil is a
// deliberate no-op for same-workspace and legacy empty-WorkDir activations.
func (b *WorkspaceMemoryBinding) CommitWorkspace(prepared *WorkspaceMemoryRebind) {
	if b == nil || prepared == nil || !usableMemoryRepository(prepared.repository) {
		return
	}
	b.mu.Lock()
	b.current = prepared.repository
	b.workspaceScope = prepared.scope
	b.mu.Unlock()
}

func (b *WorkspaceMemoryBinding) repository() memory.Repository {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	repository := b.current
	b.mu.RUnlock()
	return repository
}

func (b *WorkspaceMemoryBinding) Root() string {
	if b == nil {
		return ""
	}
	b.mu.RLock()
	root := b.root
	b.mu.RUnlock()
	return root
}

func (b *WorkspaceMemoryBinding) Core() *memory.CoreMemory {
	if repository := b.repository(); repository != nil {
		return repository.Core()
	}
	return nil
}

func (b *WorkspaceMemoryBinding) ReadCoreBlock(label string) (*memory.Block, error) {
	if repository := b.repository(); repository != nil {
		return repository.ReadCoreBlock(label)
	}
	return nil, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) AddCoreBlock(label, content string) (*memory.Block, error) {
	if repository := b.repository(); repository != nil {
		return repository.AddCoreBlock(label, content)
	}
	return nil, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) ReplaceCoreBlock(label, match, content string) (*memory.Block, error) {
	if repository := b.repository(); repository != nil {
		return repository.ReplaceCoreBlock(label, match, content)
	}
	return nil, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) RemoveCoreBlock(label, match string) (*memory.Block, error) {
	if repository := b.repository(); repository != nil {
		return repository.RemoveCoreBlock(label, match)
	}
	return nil, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) CoreBlockStats() (map[string]memory.BlockStats, error) {
	if repository := b.repository(); repository != nil {
		return repository.CoreBlockStats()
	}
	return nil, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) Archival() *memory.ArchivalMemory {
	if repository := b.repository(); repository != nil {
		return repository.Archival()
	}
	return nil
}

func (b *WorkspaceMemoryBinding) BuildContext() string {
	if repository := b.repository(); repository != nil {
		return repository.BuildContext()
	}
	return ""
}

func (b *WorkspaceMemoryBinding) AutoRetrieve(query string, k int) string {
	if repository := b.repository(); repository != nil {
		return repository.AutoRetrieve(query, k)
	}
	return ""
}

func (b *WorkspaceMemoryBinding) PreviewAutoRetrieve(query string, k int) string {
	if repository := b.repository(); repository != nil {
		return repository.PreviewAutoRetrieve(query, k)
	}
	return ""
}

func (b *WorkspaceMemoryBinding) AutoRetrieveCandidates(query string, k int) []memory.Passage {
	if repository := b.repository(); repository != nil {
		return repository.AutoRetrieveCandidates(query, k)
	}
	return nil
}

func (b *WorkspaceMemoryBinding) Search(opts memory.SearchOptions) ([]memory.Passage, error) {
	if repository := b.repository(); repository != nil {
		return repository.Search(opts)
	}
	return nil, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) SearchCandidates(query string, k int) []memory.Passage {
	if repository := b.repository(); repository != nil {
		return repository.SearchCandidates(query, k)
	}
	return nil
}

func (b *WorkspaceMemoryBinding) MarkRetrieved(passages []memory.Passage) error {
	if repository := b.repository(); repository != nil {
		return repository.MarkRetrieved(passages)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) CommitTopic(ctx context.Context, mutation memory.TopicMutation) error {
	if repository := b.repository(); repository != nil {
		return repository.CommitTopic(ctx, mutation)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) RemoveTopic(ctx context.Context, path, sourceSessionID string) error {
	if repository := b.repository(); repository != nil {
		return repository.RemoveTopic(ctx, path, sourceSessionID)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) MaintainTopics(ctx context.Context, request memory.TopicMaintenanceRequest) (memory.TopicMaintenanceResult, error) {
	if repository := b.repository(); repository != nil {
		return repository.MaintainTopics(ctx, request)
	}
	return memory.TopicMaintenanceResult{}, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) RefreshTopics(ctx context.Context) error {
	if repository := b.repository(); repository != nil {
		return repository.RefreshTopics(ctx)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) Invalidate() {
	if repository := b.repository(); repository != nil {
		repository.Invalidate()
	}
}

func (b *WorkspaceMemoryBinding) RecordTurn(ctx context.Context, sessionID, sourceMessageID, userMsg, asstMsg string) error {
	if repository := b.repository(); repository != nil {
		return repository.RecordTurn(ctx, sessionID, sourceMessageID, userMsg, asstMsg)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) OnTurnEnd(ctx context.Context, userMsg, asstMsg string) {
	if repository := b.repository(); repository != nil {
		repository.OnTurnEnd(ctx, userMsg, asstMsg)
	}
}

func (b *WorkspaceMemoryBinding) DistillTurn(ctx context.Context, provider llm.Provider, userMsg, asstMsg string) error {
	if repository := b.repository(); repository != nil {
		return repository.DistillTurn(ctx, provider, userMsg, asstMsg)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) DistillTurnWithMetadata(ctx context.Context, provider llm.Provider, sessionID, sourceMessageID, userMsg, asstMsg string) error {
	if repository := b.repository(); repository != nil {
		return repository.DistillTurnWithMetadata(ctx, provider, sessionID, sourceMessageID, userMsg, asstMsg)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) SaveDailyNote(sessionID, source, summary string) error {
	if repository := b.repository(); repository != nil {
		return repository.SaveDailyNote(sessionID, source, summary)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) ListDailyNotes(limit int) ([]memory.DailyNote, error) {
	if repository := b.repository(); repository != nil {
		return repository.ListDailyNotes(limit)
	}
	return nil, errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) DeleteSession(sessionID string) error {
	if repository := b.repository(); repository != nil {
		return repository.DeleteSession(sessionID)
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) Save() error {
	if repository := b.repository(); repository != nil {
		return repository.Save()
	}
	return errMemoryRepositoryUnavailable
}

func (b *WorkspaceMemoryBinding) Freshness() memory.Freshness {
	if repository := b.repository(); repository != nil {
		return repository.Freshness()
	}
	return memory.Freshness{}
}

var _ memory.Repository = (*WorkspaceMemoryBinding)(nil)
