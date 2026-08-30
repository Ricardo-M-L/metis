package agent

// auto_mem_gate.go — the CanUseToolFn for the auto-memory forked
// extractor. Mirrors openclaude's createAutoMemCanUseTool. Inside the
// fork, reads use structured path-aware tools and writes are pinned to the
// memdir root. Shell commands stay disabled: a lexical command classifier
// cannot reliably resolve every symlink/quoting alias to a credential path.
//
// The gate's verdicts:
//
//   Read / Grep / Glob          → allow (read-only by construction).
//   Bash / shell tools          → deny (use the structured read tools).
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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// CreateAutoMemCanUseTool returns a CanUseToolFn that gates the
// forked extractor. memoryDir is the root the fork is allowed to
// write to. reg is retained for API compatibility with existing callers;
// shell execution is deliberately not delegated to it.
//
// The returned closure is safe to share across forks of the same
// memdir root.
func CreateAutoMemCanUseTool(memoryDir string, _ *tools.Registry) CanUseToolFn {
	// Reuse the main permission package's credential-read boundary instead of
	// maintaining a second list in the memory extractor. Bypass mode is chosen
	// deliberately: ordinary reads pass without interaction while credential
	// reads become a silent DENY.
	credentialGate := permission.New(permission.ModeBypassPermissions)
	return func(ctx context.Context, toolName string, input map[string]any) (bool, string) {
		switch toolName {
		case "Read", "Grep":
			if ok, reason := autoMemCredentialReadAllowed(ctx, credentialGate, toolName, input); !ok {
				return false, reason
			}
			return true, ""
		case "Glob", "ReadDir", "Find":
			return true, ""
		case "ReadMcpResource":
			// MCP resources are provider-controlled payloads, not ordinary local
			// files. The fork deliberately skips each tool's own CanUse method,
			// so a blanket allow here would let background memory extraction read
			// private resources and persist/send them without the parent boundary.
			return false, "auto-memory fork: MCP resources are not memory-safe by default"
		case "Bash", "BashOutput", "ShellCommand":
			return false, "auto-memory fork: shell tools are disabled; use Read, Grep, Glob, ReadDir, or Find"
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
		return false, fmt.Sprintf("auto-memory fork: tool %q not in whitelist (allowed: Read/Grep/Glob/ReadDir/Find, Edit/Write within %s)", toolName, memoryDir)
	}
}

func autoMemCredentialReadAllowed(ctx context.Context, gate *permission.Gate, toolName string, input map[string]any) (bool, string) {
	stringInput := autoMemPermissionInput(toolName, input)
	var decision permission.Decision
	var source string
	switch toolName {
	case "Read":
		path := stringField(input, "file_path")
		if path == "" {
			path = stringField(input, "path")
		}
		decision, source = gate.CheckPath(ctx, "Read", stringInput, path)
	case "Grep":
		root := stringField(input, "root")
		if strings.TrimSpace(root) == "" {
			// Match the real Grep tool: an omitted root means the current
			// directory, not "no filesystem target". This is security-relevant
			// when METIS was launched from its credential directory.
			root = "."
		}
		decision, source = gate.CheckPath(ctx, "Grep", stringInput, root)
	default:
		decision, source = gate.Check(ctx, "Bash", stringInput)
	}
	if decision != permission.DecisionAllow {
		return false, fmt.Sprintf("auto-memory fork: credential boundary denied %s (%s)", toolName, source)
	}
	return true, ""
}

func autoMemPermissionInput(toolName string, input map[string]any) string {
	if toolName == "Bash" || toolName == "BashOutput" || toolName == "ShellCommand" {
		if command := stringField(input, "command"); command != "" {
			return command
		}
		b, _ := json.Marshal(input)
		return string(b)
	}
	if toolName == "Read" {
		if path := stringField(input, "file_path"); path != "" {
			return path
		}
		return stringField(input, "path")
	}
	if toolName == "Grep" {
		root := stringField(input, "root")
		if strings.TrimSpace(root) == "" {
			root = "."
		}
		pattern := stringField(input, "pattern")
		if root != "" {
			return strings.TrimRight(root, "/\\") + "/\n" + pattern
		}
		return pattern
	}
	b, _ := json.Marshal(input)
	return string(b)
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
