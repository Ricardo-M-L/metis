package session

import (
	"fmt"
	"os"
	"path/filepath"
)

// removeAttachmentDirectory only removes the exact authorized sidecar. Root
// symlinks are unlinked, never traversed; unrelated sibling paths are untouched.
func (s *Store) removeAttachmentDirectory(path string) error {
	if s.removeAttachments != nil {
		return s.removeAttachments(path)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete session attachments: %w", err)
	}
	defer root.Close()
	if err := root.RemoveAll(filepath.Base(path)); err != nil {
		return fmt.Errorf("delete session attachments: %w", err)
	}
	return nil
}
