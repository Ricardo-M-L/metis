package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func defaultShadowRoot() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".metis", "checkpoints")
	}
	return "/tmp/metis-checkpoints"
}

// DeleteSession removes the shadow repository owned by sessionID. An empty
// shadowRoot uses the same ~/.metis/checkpoints default as NewManager.
// Deletion is idempotent; callers must ensure no live Manager is using it.
func DeleteSession(sessionID, shadowRoot string) error {
	if !validDeleteSessionID(sessionID) {
		return errors.New("checkpoint: invalid session id")
	}
	if shadowRoot == "" {
		shadowRoot = defaultShadowRoot()
	}
	root := filepath.Clean(shadowRoot)
	resolvedRoot := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		resolvedRoot = filepath.Clean(resolved)
	}
	if root == "." || root == string(filepath.Separator) || resolvedRoot == string(filepath.Separator) {
		return errors.New("checkpoint: unsafe shadow root")
	}
	target := filepath.Join(root, sessionID)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel != sessionID {
		return errors.New("checkpoint: session path escapes shadow root")
	}

	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checkpoint: inspect session shadow: %w", err)
	}
	// Remove a final-component symlink as a link, never as the directory it
	// points to. os.RemoveAll currently has this property too; keeping the
	// branch explicit makes the safety boundary obvious and testable.
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checkpoint: delete session shadow: %w", err)
		}
		return nil
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("checkpoint: delete session shadow: %w", err)
	}
	return nil
}

func validDeleteSessionID(sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" || sessionID == "." || sessionID == ".." ||
		filepath.Base(sessionID) != sessionID || strings.ContainsAny(sessionID, `/\`) {
		return false
	}
	for _, r := range sessionID {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
