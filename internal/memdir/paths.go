// Package memdir is the on-disk auto-memory store: a per-project
// directory of markdown files with YAML frontmatter, indexed by a
// MEMORY.md table-of-contents. Mirrors openclaude / Claude Code's
// "memory directory" pattern (~/.claude/projects/<dir>/memory/).
//
// Directory layout:
//
//	~/.metis/memory/                   ← global memdir root
//	  MEMORY.md                        ← table-of-contents (one bullet per file)
//	  user_role.md                     ← memory file (frontmatter + body)
//	  feedback_testing.md
//	  project_release_freeze.md
//	  reference_oa_databases.md
//
// Each file is a self-contained memory: frontmatter declares
// `name / description / type`, the body is free-form context. The
// MEMORY.md index is regenerated whenever a memory is added/removed
// (the entrypoint constant ENTRYPOINT_NAME == "MEMORY.md").
//
// Why a directory and not a single file: each memory has its own
// staleness cadence (a "feedback" memory may live forever; a "project"
// memory expires when the deadline passes). Per-file mtime + per-file
// edit makes pruning, dedup, and the LLM's "edit just this one"
// workflow trivial.
package memdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// ENTRYPOINT_NAME is the index file name. It is NOT a memory itself —
// it's the per-directory table-of-contents that points to all sibling
// .md files. Treated specially: never returned by Scan, never counted
// as a "saved memory" in extractMemories.
const ENTRYPOINT_NAME = "MEMORY.md"

// DefaultRoot is the canonical memdir root: $METIS_HOME/memory (falling back
// to ~/.metis/memory). It deliberately delegates to config.Home so Desktop,
// CLI, tests, and auto-memory cannot split their state across two roots.
//
// Returning an error rather than silently using a temp dir makes
// startup-path config bugs loud (HOME unset in containers, e.g.).
func DefaultRoot() (string, error) {
	home := config.Home()
	if strings.TrimSpace(home) == "" {
		return "", errors.New("memdir: METIS home is empty")
	}
	return filepath.Join(home, "memory"), nil
}

// EnsureRoot creates the memdir if missing. Idempotent. Permissions
// 0o700 because memories may contain user-specific facts (paths, tokens
// the user pasted into chat, internal hostnames) that other accounts
// on the box shouldn't read.
func EnsureRoot(root string) error {
	if root == "" {
		return errors.New("memdir: empty root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return SecurePermissions(root)
}

// SecurePermissions normalizes the memory tree to private POSIX modes:
// directories 0700 and regular files 0600. It resolves a symlinked root but
// never follows symlink entries inside the tree. Calling it repeatedly is
// safe and also upgrades installations created by older 0755/0644 code.
func SecurePermissions(root string) error {
	if root == "" {
		return errors.New("memdir: empty root")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(resolved, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return os.Chmod(path, 0o600)
		}
		return nil
	})
}

// IndexPath returns the MEMORY.md path under root.
func IndexPath(root string) string {
	return filepath.Join(root, ENTRYPOINT_NAME)
}

// IsAutoMemPath reports whether the given path is inside the memdir
// root, after symlink resolution. Used by the auto-mem tool whitelist
// to gate Edit/Write — the forked extractor agent may only write
// inside this directory.
//
// Returns false on absolute path canonicalization errors (a missing
// parent directory makes EvalSymlinks fail; treat as "not in memdir"
// rather than letting a writable Edit slip through).
func IsAutoMemPath(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	// Resolve symlinks on the root once; the candidate may not exist
	// yet (extractor about to Write a new file), so don't EvalSymlinks
	// it — Abs is enough for the equality check below.
	if eval, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = eval
	}
	candAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	// If the candidate's directory exists, resolve symlinks on it so
	// that `~/.metis/memory -> /tmp/x/memory` aliases match.
	if eval, err := filepath.EvalSymlinks(filepath.Dir(candAbs)); err == nil {
		candAbs = filepath.Join(eval, filepath.Base(candAbs))
	}
	rel, err := filepath.Rel(rootAbs, candAbs)
	if err != nil {
		return false
	}
	// Reject "..": that escapes the root.
	if strings.HasPrefix(rel, "..") || rel == ".." {
		return false
	}
	// Direct match (the directory itself, no file) is not an
	// "auto-mem path" — extractor must target a file, not the dir.
	if rel == "." {
		return false
	}
	return true
}

// IsEntrypoint reports whether basename(path) is MEMORY.md. Useful
// for filtering the index out of "memories saved this run" counts.
func IsEntrypoint(path string) bool {
	return filepath.Base(path) == ENTRYPOINT_NAME
}
