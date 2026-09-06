package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// validateCredentialTopology rejects inode aliases that path-based sandbox
// rules cannot hide. A hard link placed in the workspace before sandbox start
// would otherwise expose the same bytes under an unrelated pathname.
func validateCredentialTopology(home, metisHome string) error {
	for _, root := range metisControlRoots(home, metisHome) {
		privateDir := filepath.Join(root, metisCredentialDirectoryName)
		if info, err := os.Lstat(privateDir); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("sandbox: private credential path %q is not a real directory", privateDir)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sandbox: inspect private credential directory %q: %w", privateDir, err)
		}
		if err := filepath.WalkDir(privateDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			return rejectCredentialHardLink(path)
		}); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sandbox: validate private credential directory %q: %w", privateDir, err)
		}

		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sandbox: inspect legacy credential directory %q: %w", root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !isLegacyCredentialFilename(entry.Name()) {
				continue
			}
			if err := rejectCredentialHardLink(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func isLegacyCredentialFilename(name string) bool {
	switch name {
	case "auth.json", "llm-oauth.json", "mcp-oauth.json", ".llm-oauth.lock", ".mcp-oauth.lock":
		return true
	}
	return strings.HasPrefix(name, ".auth.json.") ||
		(strings.HasPrefix(name, ".llm-oauth-") && (strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".lock"))) ||
		(strings.HasPrefix(name, ".mcp-oauth-") && (strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".lock")))
}
