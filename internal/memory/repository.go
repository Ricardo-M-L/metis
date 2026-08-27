package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory/security"
)

// Repository is the single persistence and recall contract shared by the
// runtime, Memory tool, auto-memory extractor, Desktop, and CLI lifecycle.
// MemoryManager is the filesystem implementation; the interface keeps those
// callers from inventing another storage format while preserving the existing
// concrete API for compatibility.
type Repository interface {
	Root() string
	Core() *CoreMemory
	ReadCoreBlock(label string) (*Block, error)
	AddCoreBlock(label, content string) (*Block, error)
	ReplaceCoreBlock(label, match, content string) (*Block, error)
	RemoveCoreBlock(label, match string) (*Block, error)
	CoreBlockStats() (map[string]BlockStats, error)
	Archival() *ArchivalMemory
	BuildContext() string
	AutoRetrieve(query string, k int) string
	PreviewAutoRetrieve(query string, k int) string
	AutoRetrieveCandidates(query string, k int) []Passage
	Search(opts SearchOptions) ([]Passage, error)
	SearchCandidates(query string, k int) []Passage
	MarkRetrieved(passages []Passage) error
	CommitTopic(ctx context.Context, mutation TopicMutation) error
	RemoveTopic(ctx context.Context, path, sourceSessionID string) error
	MaintainTopics(ctx context.Context, request TopicMaintenanceRequest) (TopicMaintenanceResult, error)
	RefreshTopics(ctx context.Context) error
	Invalidate()
	RecordTurn(ctx context.Context, sessionID, sourceMessageID, userMsg, asstMsg string) error
	OnTurnEnd(ctx context.Context, userMsg, asstMsg string)
	DistillTurn(ctx context.Context, provider llm.Provider, userMsg, asstMsg string) error
	DistillTurnWithMetadata(ctx context.Context, provider llm.Provider, sessionID, sourceMessageID, userMsg, asstMsg string) error
	SaveDailyNote(sessionID, source, summary string) error
	ListDailyNotes(limit int) ([]DailyNote, error)
	DeleteSession(sessionID string) error
	Save() error
	Freshness() Freshness
}

var _ Repository = (*MemoryManager)(nil)

var (
	// ErrUnsafeMemory means content attempted to turn recalled data into
	// instructions, hijack a role, add a backdoor, or hide control chars.
	ErrUnsafeMemory = errors.New("unsafe memory content")
	// ErrSensitiveMemory means a write contained too many credentials to be
	// useful after redaction and was therefore refused.
	ErrSensitiveMemory = errors.New("sensitive memory content")
	// ErrSessionDeleted means a late Recall/Daily/Distill writer attempted to
	// persist data attributed to a session whose deletion tombstone is durable.
	ErrSessionDeleted = errors.New("memory source session deleted")
	// ErrTopicConflict means an Edit was prepared from an older topic version.
	// Retrying from the current file avoids silently losing another process's
	// update.
	ErrTopicConflict = errors.New("memory topic changed concurrently")
	// ErrTopicOwnership means an Auto Memory run attempted to overwrite or
	// remove a topic owned by another session (or claim an unattributed legacy
	// topic). Each topic has one source session so session deletion remains
	// exact and auditable.
	ErrTopicOwnership = errors.New("memory topic belongs to another session")
)

type contextSnapshotCache struct {
	fingerprint string
	value       string
	builds      uint64
}

type retrievalSnapshotCache struct {
	fingerprint string
	passages    []Passage
	docs        []*BM25Doc
	byID        map[string]Passage
	builds      uint64
}

// repositoryCache owns only immutable prompt/corpus snapshots. Disk stores
// retain their own locks; this lock never nests underneath a store write.
type repositoryCache struct {
	mu sync.Mutex
}

// Invalidate drops immutable prompt/corpus snapshots after an external writer
// (notably Auto Memory/Dream) updates topic files. Filesystem fingerprints are
// still the correctness fallback; this hook makes the next request observe the
// change without waiting for polling.
func (mm *MemoryManager) Invalidate() {
	if mm == nil {
		return
	}
	mm.cache.mu.Lock()
	mm.contextCache.fingerprint = ""
	mm.retrievalCache.fingerprint = ""
	mm.cache.mu.Unlock()
}

func hasBlockingThreat(threats []security.Threat) bool {
	for _, threat := range threats {
		switch threat.Kind {
		case security.ThreatInjection, security.ThreatRoleHijack, security.ThreatCredential,
			security.ThreatBackdoor, security.ThreatInvisible:
			return true
		}
	}
	return false
}

func threatKinds(threats []security.Threat) string {
	seen := map[string]struct{}{}
	for _, threat := range threats {
		seen[string(threat.Kind)] = struct{}{}
	}
	parts := make([]string, 0, len(seen))
	for kind := range seen {
		parts = append(parts, kind)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
