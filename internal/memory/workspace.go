package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const workspaceScopePrefix = "workspace:"

// WorkspaceScope returns a stable, non-identifying namespace for a workspace.
// Existing paths are resolved through symlinks so opening the same checkout
// through two aliases does not manufacture two memory namespaces.
func WorkspaceScope(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("memory: empty workspace path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = filepath.Clean(resolved)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return workspaceScopePrefix + hex.EncodeToString(sum[:16]), nil
}

func workspaceID(scope string) string {
	value := strings.TrimPrefix(strings.TrimSpace(scope), workspaceScopePrefix)
	if value == "" || value == scope {
		sum := sha256.Sum256([]byte(scope))
		value = hex.EncodeToString(sum[:16])
	}
	if len(value) > 16 {
		value = value[:16]
	}
	return value
}

func isGlobalMemoryType(memoryType string) bool {
	switch strings.ToLower(strings.TrimSpace(memoryType)) {
	case TypeUser, TypeFeedback:
		return true
	}
	return false
}

func scopedMemoryType(memoryType, requestedScope, workspaceScope string) string {
	requestedScope = strings.TrimSpace(requestedScope)
	if requestedScope != "" {
		if workspaceScope != "" && requestedScope == "project" {
			return workspaceScope
		}
		return requestedScope
	}
	if isGlobalMemoryType(memoryType) {
		return "user"
	}
	if workspaceScope != "" {
		return workspaceScope
	}
	return "global"
}

func (mm *MemoryManager) passageVisible(passage Passage) bool {
	if mm == nil || mm.workspaceScope == "" {
		return true
	}
	if isGlobalMemoryType(passage.Type) {
		return true
	}
	scope := strings.TrimSpace(passage.Scope)
	switch scope {
	case "", "global", "user":
		return true
	default:
		return scope == mm.workspaceScope
	}
}
