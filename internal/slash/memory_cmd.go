package slash

// memory_cmd.go — `/memory` slash subcommands. Reads/writes the
// auto-memory directory (~/.metis/memory/) backing the extractMemories
// v2 system.
//
// Subcommands:
//
//   /memory               — alias for /memory list
//   /memory list          — list all stored memories grouped by type
//   /memory show <file>   — print one file's contents (frontmatter + body)
//   /memory rm <file>     — delete a memory (path-validated to memdir)
//   /memory path          — print the memdir root path
//
// Lookups by `<file>` accept either the bare basename (`user_role.md`)
// or with extension. We resolve under root and reject anything that
// escapes via memdir.IsAutoMemPath.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

// handleMemoryCommand parses args and dispatches. Always returns a
// non-empty string the runtime prints back to the user; never returns
// a Signal — these commands have no side-effects on the session.
func handleMemoryCommand(args string) string {
	root, err := memdir.DefaultRoot()
	if err != nil {
		return fmt.Sprintf("(memory: cannot resolve root: %v)", err)
	}
	args = strings.TrimSpace(args)
	parts := strings.SplitN(args, " ", 2)
	cmd := strings.ToLower(parts[0])
	var rest string
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case "", "list", "ls":
		return memoryList(root)
	case "show", "cat":
		return memoryShow(root, rest)
	case "rm", "delete", "remove":
		return memoryRm(root, rest)
	case "path", "where":
		return memoryPath(root)
	default:
		return "(memory: unknown subcommand " + cmd + " — try: list | show <file> | rm <file> | path)"
	}
}

func memoryList(root string) string {
	files, err := memdir.ScanMemoryFiles(context.Background(), root)
	if err != nil {
		return fmt.Sprintf("(memory: scan: %v)", err)
	}
	if len(files) == 0 {
		return fmt.Sprintf("(memory: empty — root: %s)\nTurn on auto-memory with --auto-memory or METIS_AUTO_MEMORY=1.", root)
	}
	return memdir.FormatManifest(root, files)
}

func memoryShow(root, name string) string {
	if name == "" {
		return "(memory show: missing filename — usage: /memory show <name>)"
	}
	path := resolveMemoryPath(root, name)
	if !memdir.IsAutoMemPath(root, path) {
		return fmt.Sprintf("(memory show: %q is outside the memdir root)", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(memory show: %v)", err)
	}
	return string(b)
}

func memoryRm(root, name string) string {
	if name == "" {
		return "(memory rm: missing filename — usage: /memory rm <name>)"
	}
	path := resolveMemoryPath(root, name)
	if !memdir.IsAutoMemPath(root, path) {
		return fmt.Sprintf("(memory rm: %q is outside the memdir root — refusing)", name)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Sprintf("(memory rm: %v)", err)
	}
	// Regenerate index so MEMORY.md doesn't reference a missing file.
	if files, err := memdir.ScanMemoryFiles(context.Background(), root); err == nil {
		_ = memdir.WriteIndex(root, files)
	}
	return fmt.Sprintf("(memory rm: deleted %s)", filepath.Base(path))
}

func memoryPath(root string) string {
	return root
}

// resolveMemoryPath turns a user-provided name (with or without `.md`
// extension, with or without leading directory) into an absolute path
// under root. Does NOT verify it stays inside root — caller's job via
// memdir.IsAutoMemPath.
func resolveMemoryPath(root, name string) string {
	name = strings.TrimSpace(name)
	// Strip any leading slash so `/user_role.md` works the same as
	// `user_role.md`. We're not honoring absolute-path inputs.
	name = strings.TrimLeft(name, "/")
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	return filepath.Join(root, filepath.Base(name))
}
