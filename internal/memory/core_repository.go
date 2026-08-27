package memory

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/memory/security"
)

var (
	// ErrCoreBlockNotFound means the caller named a block outside the
	// repository's fixed core-memory schema.
	ErrCoreBlockNotFound = errors.New("core memory block not found")
	// ErrCoreMatchNotFound means a replace/remove precondition was not present
	// in the latest authoritative block content.
	ErrCoreMatchNotFound = errors.New("core memory match not found")
)

const maxCoreBlockFileBytes = 256 << 10

// ReadCoreBlock returns a detached snapshot loaded from the authoritative
// core.d files while holding the repository's cross-process lock.
func (mm *MemoryManager) ReadCoreBlock(label string) (*Block, error) {
	if mm == nil || mm.core == nil {
		return nil, ErrCoreBlockNotFound
	}
	var result *Block
	err := withRepositoryLock(mm.root, func() error {
		mm.core.mu.Lock()
		defer mm.core.mu.Unlock()
		if err := mm.core.reloadAuthoritativeLocked(true); err != nil {
			return err
		}
		block := mm.core.blockLocked(label)
		if block == nil {
			return fmt.Errorf("%w: %s", ErrCoreBlockNotFound, label)
		}
		result = cloneBlock(block)
		return nil
	})
	if err != nil {
		return nil, err
	}
	mm.Invalidate()
	return result, nil
}

// AddCoreBlock appends one fact to the latest authoritative block. Reload,
// append, validation, truncation, and atomic rename happen under one
// repository lock so Desktop and CLI cannot overwrite each other's writes.
func (mm *MemoryManager) AddCoreBlock(label, content string) (*Block, error) {
	if content == "" {
		return nil, errors.New("core memory add: content required")
	}
	return mm.mutateCoreBlock(label, func(current string) (string, error) {
		if current == "" {
			return content, nil
		}
		return current + "\n" + content, nil
	})
}

// ReplaceCoreBlock replaces the first matching substring in the latest
// authoritative block.
func (mm *MemoryManager) ReplaceCoreBlock(label, match, content string) (*Block, error) {
	if match == "" {
		return nil, errors.New("core memory replace: match required")
	}
	return mm.mutateCoreBlock(label, func(current string) (string, error) {
		if !strings.Contains(current, match) {
			return "", fmt.Errorf("%w: %s", ErrCoreMatchNotFound, label)
		}
		return strings.Replace(current, match, content, 1), nil
	})
}

// RemoveCoreBlock removes the first matching substring from the latest
// authoritative block.
func (mm *MemoryManager) RemoveCoreBlock(label, match string) (*Block, error) {
	if match == "" {
		return nil, errors.New("core memory remove: match required")
	}
	return mm.mutateCoreBlock(label, func(current string) (string, error) {
		if !strings.Contains(current, match) {
			return "", fmt.Errorf("%w: %s", ErrCoreMatchNotFound, label)
		}
		return strings.Replace(current, match, "", 1), nil
	})
}

// CoreBlockStats reloads core.d before reporting usage, preventing a
// long-running Desktop from presenting stale CLI-written values.
func (mm *MemoryManager) CoreBlockStats() (map[string]BlockStats, error) {
	if mm == nil || mm.core == nil {
		return map[string]BlockStats{}, nil
	}
	var stats map[string]BlockStats
	err := withRepositoryLock(mm.root, func() error {
		mm.core.mu.Lock()
		defer mm.core.mu.Unlock()
		if err := mm.core.reloadAuthoritativeLocked(true); err != nil {
			return err
		}
		stats = mm.core.statsLocked()
		return nil
	})
	if err != nil {
		return nil, err
	}
	mm.Invalidate()
	return stats, nil
}

func (mm *MemoryManager) mutateCoreBlock(label string, mutate func(string) (string, error)) (*Block, error) {
	if mm == nil || mm.core == nil {
		return nil, ErrCoreBlockNotFound
	}
	var result *Block
	err := withRepositoryLock(mm.root, func() error {
		mm.core.mu.Lock()
		defer mm.core.mu.Unlock()
		if err := mm.core.reloadAuthoritativeLocked(true); err != nil {
			return err
		}
		block := mm.core.blockLocked(label)
		if block == nil {
			return fmt.Errorf("%w: %s", ErrCoreBlockNotFound, label)
		}
		next, err := mutate(block.Content)
		if err != nil {
			return err
		}
		result, err = mm.core.persistBlockContentLocked(block, next)
		return err
	})
	if err != nil {
		return nil, err
	}
	mm.Invalidate()
	return result, nil
}

func (cm *CoreMemory) blockLocked(label string) *Block {
	for _, block := range cm.blocks {
		if block.Label == label {
			return block
		}
	}
	return nil
}

func cloneBlock(block *Block) *Block {
	if block == nil {
		return nil
	}
	copy := *block
	return &copy
}

func validateCoreContent(content string) error {
	if redacted := memdir.Redact(content); redacted.Reject || len(redacted.Hits) > 0 {
		return ErrSensitiveMemory
	}
	if threats := security.ScanAll(content); len(threats) > 0 {
		return fmt.Errorf("%w: %s", ErrUnsafeMemory, threatKinds(threats))
	}
	return nil
}

func (cm *CoreMemory) persistBlockContentLocked(block *Block, content string) (*Block, error) {
	if err := validateCoreContent(content); err != nil {
		return nil, err
	}
	if len(content) > block.MaxChars {
		content = truncate(content, block.MaxChars)
	}
	oldContent, oldUpdatedAt := block.Content, block.UpdatedAt
	block.Content = content
	block.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := cm.saveBlockLocked(block); err != nil {
		block.Content, block.UpdatedAt = oldContent, oldUpdatedAt
		cm.snapshot = cm.renderLocked()
		return nil, err
	}
	cm.snapshot = cm.renderLocked()
	return cloneBlock(block), nil
}

// reloadAuthoritativeLocked replaces every in-memory block with the complete
// on-disk state. Missing files mean empty blocks. In strict mode, unreadable or
// unsafe files fail closed before any in-memory state is changed.
func (cm *CoreMemory) reloadAuthoritativeLocked(strict bool) error {
	if cm.memoryRoot == "" {
		cm.snapshot = cm.renderLocked()
		return nil
	}
	type loadedBlock struct {
		content   string
		updatedAt string
	}
	loaded := make(map[string]loadedBlock, len(cm.blocks))
	for _, block := range cm.blocks {
		path := cm.pathForBlock(block.Label)
		raw, info, err := readAuthoritativeRegularFile(cm.rootForBlock(block.Label), path, maxCoreBlockFileBytes)
		if errors.Is(err, os.ErrNotExist) {
			loaded[block.Label] = loadedBlock{}
			continue
		}
		if err != nil {
			if strict {
				return fmt.Errorf("read core memory block %s: %w", block.Label, err)
			}
			continue
		}
		content := parseMemoryFile(string(raw), block.Label)
		if strict {
			if err := validateCoreContent(content); err != nil {
				return fmt.Errorf("validate core memory block %s: %w", block.Label, err)
			}
		}
		updatedAt := info.ModTime().UTC().Format(time.RFC3339Nano)
		loaded[block.Label] = loadedBlock{content: content, updatedAt: updatedAt}
	}
	for _, block := range cm.blocks {
		value, ok := loaded[block.Label]
		if !ok {
			continue
		}
		block.Content = value.content
		if value.updatedAt != "" {
			block.UpdatedAt = value.updatedAt
		}
	}
	cm.snapshot = cm.renderLocked()
	return nil
}

func (cm *CoreMemory) statsLocked() map[string]BlockStats {
	stats := make(map[string]BlockStats, len(cm.blocks))
	for _, block := range cm.blocks {
		stats[block.Label] = BlockStats{
			Used:  len(block.Content),
			Limit: block.MaxChars,
			Pct:   float64(len(block.Content)) / float64(block.MaxChars) * 100,
		}
	}
	return stats
}
