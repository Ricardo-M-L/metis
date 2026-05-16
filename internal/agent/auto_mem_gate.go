package agent

// auto_mem_gate.go — the CanUseToolFn for the auto-memory forked
// extractor. Mirrors openclaude's createAutoMemCanUseTool. Inside the
// fork, the model has freedom to read anywhere it likes (the parent
// context already trusts the user with file access), but writing is
// pinned to the memdir root — a wandering extractor can't accidentally
// edit the user's actual code.
//
// The gate's verdicts:
//
//   Read / Grep / Glob          → allow (read-only by construction).
//   Bash with isReadOnly=true   → allow (ls, cat, find, stat, head, tail, …).
//   Bash with isReadOnly=false  → deny.
//   Edit / Write to memdir/*    → allow.
//   Edit / Write elsewhere      → deny.
//   Anything else               → deny.
//
// Default-deny is intentional: any new tool the model could call (a
// future "DeleteFile" or "RunMigration") should be off until a human
// explicitly whitelists it. This is the inverse of the main agent's
// permission flow, where unknown tools fall through to PermissionAsk.

import (
	"context"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// CreateAutoMemCanUseTool returns a CanUseToolFn that gates the
// forked extractor. memoryDir is the root the fork is allowed to
// write to; reg is the same registry the fork uses to look up the
// tool (so we can ask Bash specifically for IsReadOnly without
// hard-coding a string-match list of safe binaries).
//
// The returned closure is safe to share across forks of the same
// memdir root.
func CreateAutoMemCanUseTool(memoryDir string, reg *tools.Registry) CanUseToolFn {
	return func(ctx context.Context, toolName string, input map[string]any) (bool, string) {
		switch toolName {
		case "Read", "Grep", "Glob", "ReadMcpResource", "ReadDir", "Find":
			return true, ""
		case "Bash", "BashOutput", "ShellCommand":
			return canUseBashReadOnly(ctx, reg, toolName, input)
		case "Edit", "Write", "MultiEdit":
			return canUseEditOrWrite(memoryDir, input)
		case "SkillSynth":
			// Phase B (2026-05-16) — the dreaming fork's exclusive write
			// path to ~/.metis/skills/<name>.md. The tool itself
			// validates name shape + frontmatter scope, so we only need
			// to authorise the call here, not re-validate inputs.
			return true, ""
		}
		// Default deny — keeps the fork safely scoped no matter what
		// future tools land in the registry.
		return false, fmt.Sprintf("auto-memory fork: tool %q not in whitelist (allowed: Read/Grep/Glob, read-only Bash, Edit/Write within %s)", toolName, memoryDir)
	}
}

// canUseBashReadOnly asks the registered Bash tool whether the
// proposed command is read-only. We don't reimplement the safe-list —
// the Bash tool already maintains it (`isReadOnlyCommand` in
// internal/tools/builtin/bash.go), and wiring through ReadOnlyAware
// keeps the two in lock-step automatically when Bash gets new safe
// binaries.
//
// If the tool isn't registered, or doesn't implement ReadOnlyAware,
// we fail closed (deny). That's safe even if it means the extractor
// can't shell out — it can still use Read/Grep/Glob.
func canUseBashReadOnly(_ context.Context, reg *tools.Registry, name string, input map[string]any) (bool, string) {
	t, ok := reg.Get(name)
	if !ok {
		return false, fmt.Sprintf("auto-memory fork: %s not registered", name)
	}
	ro, ok := t.(pubtool.ReadOnlyAware)
	if !ok {
		return false, fmt.Sprintf("auto-memory fork: %s has no ReadOnlyAware impl — fail-closed", name)
	}
	if !ro.IsReadOnly(input) {
		return false, "auto-memory fork: only read-only shell commands (ls, find, grep, cat, stat, wc, head, tail, …) are allowed"
	}
	return true, ""
}

// canUseEditOrWrite scopes file mutation to the memdir root. The
// model produces Edit / Write inputs with either `file_path` (claude-
// code parity) or `path` (some metis tools); accept either.
//
// If the path is missing or non-string, deny — a malformed input is a
// red flag we shouldn't paper over by allowing the call. The model
// will see the rejection in the next round and self-correct.
func canUseEditOrWrite(memoryDir string, input map[string]any) (bool, string) {
	path := stringField(input, "file_path")
	if path == "" {
		path = stringField(input, "path")
	}
	if path == "" {
		return false, "auto-memory fork: Edit/Write missing file_path"
	}
	if !memdir.IsAutoMemPath(memoryDir, path) {
		return false, fmt.Sprintf("auto-memory fork: Edit/Write target %q is outside memdir root %s", path, memoryDir)
	}
	return true, ""
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
