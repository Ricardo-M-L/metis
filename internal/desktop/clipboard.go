package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxClipboardFiles = 64

var ErrClipboardFilesUnsupported = errors.New("clipboard file paths are not supported on this platform")

// ClipboardFile is a Finder/Explorer clipboard item whose absolute local path
// was resolved by the native platform clipboard. Browsers intentionally hide
// this path from File objects, so the WebUI must not infer it from File.name.
type ClipboardFile struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

// ClipboardFiles returns existing local files and directories currently held
// by the native clipboard. It reads metadata only; it never opens file bodies
// or walks a copied directory.
func ClipboardFiles() ([]ClipboardFile, error) {
	paths, err := platformClipboardFilePaths()
	if err != nil {
		return nil, err
	}
	return clipboardFilesFromPaths(paths)
}

func clipboardFilesFromPaths(paths []string) ([]ClipboardFile, error) {
	if len(paths) > maxClipboardFiles {
		return nil, fmt.Errorf("clipboard contains too many files: %d", len(paths))
	}
	seen := make(map[string]struct{}, len(paths))
	items := make([]ClipboardFile, 0, len(paths))
	for _, raw := range paths {
		if raw == "" || strings.ContainsRune(raw, '\x00') || !filepath.IsAbs(raw) {
			return nil, fmt.Errorf("clipboard returned an invalid local path")
		}
		path := filepath.Clean(raw)
		if _, ok := seen[path]; ok {
			continue
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			// The clipboard can outlive a moved/deleted Finder item. Ignore the
			// stale entry rather than inserting an unreadable path.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect clipboard path: %w", err)
		}
		seen[path] = struct{}{}
		items = append(items, ClipboardFile{Path: path, Name: filepath.Base(path), IsDir: info.IsDir()})
	}
	return items, nil
}
