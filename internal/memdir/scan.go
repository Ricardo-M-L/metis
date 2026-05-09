package memdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MemoryFile is a single parsed memory entry in the directory.
// Body is intentionally truncated for the manifest view — the full
// content is read on-demand by the forked agent's Read tool. This
// keeps the manifest small enough to inject as a single user message
// without burning prompt tokens on stale content.
type MemoryFile struct {
	Path        string      // absolute path
	Name        string      // basename without extension, e.g. "user_role"
	Frontmatter Frontmatter // parsed YAML header (may be partial on err)
	BodySize    int         // bytes of body content
	ModTime     time.Time
	ParseError  error // non-nil if YAML failed (file still listed)
}

// Title returns the human-readable title for the manifest. Prefers
// frontmatter.Name, falls back to filename. Never empty.
func (m *MemoryFile) Title() string {
	if t := strings.TrimSpace(m.Frontmatter.Name); t != "" {
		return t
	}
	return m.Name
}

// Type returns the memory type string. Empty when frontmatter omits it
// (treated as "unclassified" in the manifest).
func (m *MemoryFile) Type() MemoryType {
	return m.Frontmatter.Type
}

// ScanMemoryFiles walks root non-recursively, parsing every *.md file
// except MEMORY.md. Returns entries sorted by ModTime descending (most
// recently touched first) — the manifest is presented to the extractor
// agent in this order so recently relevant memories anchor its
// reasoning.
//
// Honors ctx cancellation between files (each file is bounded by a
// fresh os.ReadFile, no streaming). Errors on individual files are
// captured in MemoryFile.ParseError, not returned — a malformed memo
// shouldn't blind the extractor to all the others.
//
// Returns (nil, nil) when root doesn't exist — that's a freshly
// installed metis with no memories yet, not an error.
func ScanMemoryFiles(ctx context.Context, root string) ([]MemoryFile, error) {
	if root == "" {
		return nil, errors.New("memdir: empty root")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []MemoryFile
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ENTRYPOINT_NAME {
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), ".md") {
			continue
		}
		full := filepath.Join(root, name)
		mf := MemoryFile{
			Path: full,
			Name: strings.TrimSuffix(name, filepath.Ext(name)),
		}
		info, infoErr := e.Info()
		if infoErr == nil {
			mf.ModTime = info.ModTime()
		}
		raw, readErr := os.ReadFile(full)
		if readErr != nil {
			mf.ParseError = readErr
			out = append(out, mf)
			continue
		}
		fm, body, parseErr := ParseFile(raw)
		if fm != nil {
			mf.Frontmatter = *fm
		}
		mf.BodySize = len(body)
		mf.ParseError = parseErr
		out = append(out, mf)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ModTime.After(out[j].ModTime)
	})
	return out, nil
}

// FilterByType returns entries whose frontmatter.Type matches t.
// Convenience for callers that want to render per-type sections in
// the manifest (the default Manifest renderer already does this).
func FilterByType(files []MemoryFile, t MemoryType) []MemoryFile {
	var out []MemoryFile
	for _, f := range files {
		if f.Frontmatter.Type == t {
			out = append(out, f)
		}
	}
	return out
}
