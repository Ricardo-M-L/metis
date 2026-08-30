package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/memory/security"
)

// TopicSource freezes the provenance attached to one Auto Memory mutation.
// A topic has one owning session; later sessions must create another topic
// instead of rewriting history owned by the first conversation.
type TopicSource struct {
	SessionID  string
	MessageID  string
	Scope      string
	Confidence float64
}

// TopicMutation is a complete replacement committed under the repository's
// process + cross-process lock. ExpectedSHA256 is optional; Edit callers set it
// to the hash of the bytes they read so a concurrent update cannot be lost.
type TopicMutation struct {
	Path           string
	Content        []byte
	Source         TopicSource
	ExpectedSHA256 string
}

// TopicMaintenanceRequest is the single post-extraction hygiene operation.
// Touched accepts basenames, root-relative paths, or absolute paths beneath
// the canonical root.
type TopicMaintenanceRequest struct {
	Touched []string
	Source  TopicSource
	Now     time.Time
}

type TopicMaintenanceResult struct {
	Pruned []string
}

const maxTopicFileBytes = 64 * 1024 * 1024

// TopicContentSHA256 returns the revision token used by TopicMutation.
func TopicContentSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// CommitTopic validates, sanitizes, stamps, and atomically commits a topic
// while holding the same durable lock as DeleteSession. This is the only
// production Auto Memory write boundary.
func (mm *MemoryManager) CommitTopic(ctx context.Context, mutation TopicMutation) error {
	if mm == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mm.workspaceScope != "" && (strings.TrimSpace(mutation.Source.Scope) == "" || mutation.Source.Scope == "project") {
		mutation.Source.Scope = mm.workspaceScope
	}
	if err := validateTopicSource(mutation.Source); err != nil {
		return err
	}
	return withRepositoryLock(mm.root, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := resolveTopicTargetLocked(mm.root, mutation.Path)
		if err != nil {
			return err
		}
		if err := rejectDeletedSessionLocked(mm.root, mutation.Source.SessionID); err != nil {
			return err
		}
		pinnedRoot, relative, err := pinnedTopicPath(mm.root, target)
		if err != nil {
			return err
		}
		if err := checkTopicRevisionAndOwner(pinnedRoot, relative, mutation); err != nil {
			return err
		}
		prepared, err := prepareTopicContent(mutation.Content, mutation.Source, time.Now())
		if err != nil {
			return err
		}
		if err := memdir.AtomicWritePrivateFile(pinnedRoot, relative, prepared, 0o600); err != nil {
			return err
		}
		if err := refreshTopicIndexesLocked(ctx, mm.root); err != nil {
			return err
		}
		mm.Invalidate()
		return nil
	})
}

// RemoveTopic removes a topic and refreshes its derived index in the same
// repository transaction. A non-empty sourceSessionID is the Auto Memory path
// and must match the topic owner. Empty is an explicit user-admin deletion
// (for /memory rm) and may remove a topic regardless of provenance.
func (mm *MemoryManager) RemoveTopic(ctx context.Context, path, sourceSessionID string) error {
	if mm == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validatePersistedMetadata(sourceSessionID); err != nil {
		return err
	}
	return withRepositoryLock(mm.root, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := resolveTopicTargetLocked(mm.root, path)
		if err != nil {
			return err
		}
		pinnedRoot, relative, err := pinnedTopicPath(mm.root, target)
		if err != nil {
			return err
		}
		if sourceSessionID != "" {
			if err := rejectDeletedSessionLocked(mm.root, sourceSessionID); err != nil {
				return err
			}
			if err := requireTopicOwner(pinnedRoot, relative, sourceSessionID); err != nil {
				return err
			}
		}
		if err := memdir.RemovePrivateRegularFile(pinnedRoot, relative); err != nil {
			return err
		}
		if err := refreshTopicIndexesLocked(ctx, mm.root); err != nil {
			return err
		}
		mm.Invalidate()
		return nil
	})
}

// MaintainTopics applies privacy/provenance repair to bypass/fallback writes,
// prunes decayed topics, and regenerates indexes under one repository lock.
// Normal production Write/Edit commits are already canonical; this pass is
// intentionally idempotent for them.
func (mm *MemoryManager) MaintainTopics(ctx context.Context, request TopicMaintenanceRequest) (TopicMaintenanceResult, error) {
	var result TopicMaintenanceResult
	if mm == nil {
		return result, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if mm.workspaceScope != "" && (strings.TrimSpace(request.Source.Scope) == "" || request.Source.Scope == "project") {
		request.Source.Scope = mm.workspaceScope
	}
	if err := validateTopicSource(request.Source); err != nil {
		return result, err
	}
	if request.Now.IsZero() {
		request.Now = time.Now()
	}
	err := withRepositoryLock(mm.root, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rejectDeletedSessionLocked(mm.root, request.Source.SessionID); err != nil {
			// A non-repository writer may have raced deletion. Remove only files
			// whose stamped owner is the deleted session, plus unattributed files
			// in this run's exact touched set. Production commits can never reach
			// this branch because CommitTopic checked under this lock.
			cleanupErr := cleanupDeletedRunTopicsLocked(mm.root, request.Touched, request.Source.SessionID)
			cleanupErr = errors.Join(cleanupErr, deleteTopicFilesBySessionLocked(mm.root, request.Source.SessionID))
			indexErr := refreshTopicIndexesLocked(ctx, mm.root)
			mm.Invalidate()
			return errors.Join(err, cleanupErr, indexErr)
		}
		for _, path := range request.Touched {
			if err := ctx.Err(); err != nil {
				return err
			}
			target, err := resolveTopicTargetLocked(mm.root, path)
			if err != nil {
				return err
			}
			if err := maintainTopicFileLocked(target, request.Source, request.Now); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		for _, dir := range topicDirectories(mm.root) {
			sweep, err := memdir.DecayAndPrune(ctx, dir, request.Now)
			if err != nil {
				return err
			}
			result.Pruned = append(result.Pruned, sweep.Pruned...)
			if len(sweep.Errors) > 0 {
				return errors.Join(sweep.Errors...)
			}
		}
		if err := refreshTopicIndexesLocked(ctx, mm.root); err != nil {
			return err
		}
		mm.Invalidate()
		return nil
	})
	return result, err
}

func cleanupDeletedRunTopicsLocked(root string, touched []string, sessionID string) error {
	var errs []error
	for _, path := range touched {
		target, err := resolveTopicTargetLocked(root, path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		raw, err := os.ReadFile(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fm, _, parseErr := memdir.ParseFile(raw)
		if parseErr != nil || fm == nil || fm.OriginSessionID == "" || fm.OriginSessionID == sessionID {
			if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				errs = append(errs, removeErr)
			}
		}
	}
	return errors.Join(errs...)
}

func (mm *MemoryManager) RefreshTopics(ctx context.Context) error {
	if mm == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return withRepositoryLock(mm.root, func() error {
		if err := refreshTopicIndexesLocked(ctx, mm.root); err != nil {
			return err
		}
		mm.Invalidate()
		return nil
	})
}

func validateTopicSource(source TopicSource) error {
	if err := validatePersistedMetadata(source.SessionID, source.MessageID, source.Scope); err != nil {
		return err
	}
	if source.Confidence < 0 || source.Confidence > 1 {
		return fmt.Errorf("memory topic confidence %v outside [0,1]", source.Confidence)
	}
	return nil
}

func checkTopicRevisionAndOwner(root, relative string, mutation TopicMutation) error {
	raw, err := memdir.ReadPrivateRegularFile(root, relative, maxTopicFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		if mutation.ExpectedSHA256 != "" {
			return ErrTopicConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	if mutation.ExpectedSHA256 != "" && !strings.EqualFold(mutation.ExpectedSHA256, TopicContentSHA256(raw)) {
		return ErrTopicConflict
	}
	return requireTopicOwnerBytes(raw, mutation.Source.SessionID)
}

func requireTopicOwner(root, relative, sourceSessionID string) error {
	raw, err := memdir.ReadPrivateRegularFile(root, relative, maxTopicFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return requireTopicOwnerBytes(raw, sourceSessionID)
}

func requireTopicOwnerBytes(raw []byte, sourceSessionID string) error {
	fm, _, err := memdir.ParseFile(raw)
	if err != nil {
		return err
	}
	owner := strings.TrimSpace(fm.OriginSessionID)
	sourceSessionID = strings.TrimSpace(sourceSessionID)
	if sourceSessionID == "" {
		if owner != "" {
			return fmt.Errorf("%w: topic is owned by session %q", ErrTopicOwnership, owner)
		}
		return nil
	}
	if owner == "" {
		return fmt.Errorf("%w: legacy topic has no source; write a new file", ErrTopicOwnership)
	}
	if owner != sourceSessionID {
		return fmt.Errorf("%w: topic is owned by session %q, not %q; write a new topic file", ErrTopicOwnership, owner, sourceSessionID)
	}
	return nil
}

func prepareTopicContent(content []byte, source TopicSource, now time.Time) ([]byte, error) {
	redacted := memdir.Redact(string(content))
	if redacted.Reject {
		return nil, ErrSensitiveMemory
	}
	if threats := security.ScanAll(redacted.Redacted); hasBlockingThreat(threats) {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeMemory, threatKinds(threats))
	}
	fm, body, err := memdir.ParseFile([]byte(redacted.Redacted))
	if err != nil {
		return nil, err
	}
	if err := fm.Validate(); err != nil {
		return nil, err
	}
	if !fm.Type.IsValid() {
		return nil, fmt.Errorf("memdir: invalid memory type %q", fm.Type)
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, errors.New("memdir: empty memory body")
	}
	if fm.OriginSessionID == "" {
		fm.OriginSessionID = strings.TrimSpace(source.SessionID)
	} else if source.SessionID != "" && fm.OriginSessionID != source.SessionID {
		return nil, fmt.Errorf("%w: replacement declares session %q; write a new topic file", ErrTopicOwnership, fm.OriginSessionID)
	}
	if source.MessageID != "" {
		fm.SourceMessageID = source.MessageID
	}
	if source.Scope != "" {
		fm.Scope = source.Scope
	}
	if source.Confidence > 0 {
		fm.Confidence = source.Confidence
	}
	fm.MarkUpdated(now)
	return memdir.RenderFile(fm, string(body))
}

func maintainTopicFileLocked(path string, source TopicSource, now time.Time) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	redacted := memdir.Redact(string(raw))
	if redacted.Reject {
		return os.Remove(path)
	}
	if threats := security.ScanAll(redacted.Redacted); hasBlockingThreat(threats) {
		return os.Remove(path)
	}
	fm, body, parseErr := memdir.ParseFile([]byte(redacted.Redacted))
	if fm == nil {
		fm = &memdir.Frontmatter{}
	}
	if source.SessionID != "" {
		owner := strings.TrimSpace(fm.OriginSessionID)
		if owner == "" {
			return fmt.Errorf("%w: touched legacy topic has no source; write a new topic file", ErrTopicOwnership)
		}
		if owner != source.SessionID {
			return fmt.Errorf("%w: touched topic is owned by session %q, not %q", ErrTopicOwnership, owner, source.SessionID)
		}
	}
	if parseErr != nil {
		fm = &memdir.Frontmatter{}
		if strings.TrimSpace(string(body)) == "" {
			body = []byte(redacted.Redacted)
		}
	}
	normalizeTopicFrontmatter(fm, filepath.Base(path), string(body))
	if fm.OriginSessionID == "" && source.SessionID != "" {
		fm.OriginSessionID = source.SessionID
	}
	if source.MessageID != "" {
		fm.SourceMessageID = source.MessageID
	}
	if source.Scope != "" {
		fm.Scope = source.Scope
	}
	if source.Confidence > 0 {
		fm.Confidence = source.Confidence
	}
	// CommitTopic already stamps this exact source. Do not count the same
	// extraction twice during the deferred hygiene pass.
	alreadyCanonical := fm.UpdatedAt != "" && source.MessageID != "" && fm.SourceMessageID == source.MessageID
	if !alreadyCanonical {
		fm.MarkUpdated(now)
	}
	out, err := memdir.RenderFile(fm, string(body))
	if err != nil {
		return err
	}
	return atomicWriteFile(path, string(out), 0o600)
}

func normalizeTopicFrontmatter(fm *memdir.Frontmatter, filename, body string) {
	stem := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	if fm.Name == "" {
		fm.Name = strings.TrimSpace(strings.ReplaceAll(stem, "_", " "))
	}
	if fm.Description == "" {
		description := strings.Join(strings.Fields(strings.TrimSpace(body)), " ")
		if len(description) > 120 {
			description = description[:120]
		}
		if description == "" {
			description = fm.Name
		}
		fm.Description = description
	}
	if !fm.Type.IsValid() {
		fm.Type = memdir.TypeProject
	}
}

// pinnedTopicPath maps a validated absolute topic path back to the real root
// that os.Root must pin. It rejects a symlink at the configured root itself,
// while still tolerating platform aliases in ancestors (macOS /var ->
// /private/var).
func pinnedTopicPath(root, target string) (string, string, error) {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", "", err
	}
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil {
		return "", "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", "", fmt.Errorf("memory topic root is not a real directory: %s", rootAbs)
	}
	targetAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", "", err
	}
	if relative, relErr := memdir.RootRelativePath(rootAbs, targetAbs); relErr == nil {
		return rootAbs, relative, nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", err
	}
	relative, err := memdir.RootRelativePath(resolvedRoot, targetAbs)
	if err != nil {
		return "", "", err
	}
	return resolvedRoot, relative, nil
}

func resolveTopicTargetLocked(root, candidate string) (string, error) {
	if strings.TrimSpace(candidate) == "" {
		return "", errors.New("memory topic path is empty")
	}
	if err := memdir.EnsureRoot(root); err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	target := candidate
	if !filepath.IsAbs(target) {
		target = filepath.Join(resolvedRoot, filepath.FromSlash(target))
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	// macOS exposes /var as a symlink to /private/var. Resolve the existing
	// parent (the destination itself may not exist yet) so an absolute path
	// produced from the configured alias is compared in the same namespace as
	// the canonical repository root.
	if resolvedParent, resolveErr := filepath.EvalSymlinks(filepath.Dir(target)); resolveErr == nil {
		target = filepath.Join(resolvedParent, filepath.Base(target))
	}
	rel, err := filepath.Rel(resolvedRoot, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("memory topic %q is outside repository root", candidate)
	}
	if !strings.EqualFold(filepath.Ext(rel), ".md") {
		return "", errors.New("memory topic must be a .md file")
	}
	cleanRel := filepath.Clean(rel)
	dirRel := filepath.Dir(cleanRel)
	if dirRel != "." && dirRel != "topics" {
		return "", errors.New("memory topic must live at repository root or topics/")
	}
	base := filepath.Base(cleanRel)
	for _, reserved := range []string{FileMemMemory, FileMemUser, FileMemSystem, FileMemWorking, FileMemSummary} {
		if strings.EqualFold(base, reserved) {
			return "", fmt.Errorf("memory topic path %q is reserved", base)
		}
	}
	parent := filepath.Join(resolvedRoot, dirRel)
	if dirRel != "." {
		info, statErr := os.Lstat(parent)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		if statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return "", fmt.Errorf("memory topic parent %q is not a private directory", parent)
		}
	}
	target = filepath.Join(resolvedRoot, cleanRel)
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("memory topic %q is not a regular file", target)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	return target, nil
}

func topicDirectories(root string) []string {
	dirs := []string{root}
	if info, err := os.Stat(filepath.Join(root, "topics")); err == nil && info.IsDir() {
		dirs = append(dirs, filepath.Join(root, "topics"))
	}
	return dirs
}

func refreshTopicIndexesLocked(ctx context.Context, root string) error {
	for _, dir := range topicDirectories(root) {
		if err := ctx.Err(); err != nil {
			return err
		}
		files, err := memdir.ScanMemoryFiles(ctx, dir)
		if err != nil {
			return err
		}
		if err := memdir.WriteIndex(dir, files); err != nil {
			return err
		}
	}
	return nil
}
