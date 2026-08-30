package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Write struct {
	tools.BaseTool
	gate           *permission.Gate
	state          *ReadFileState
	authorizer     *invocationAuthorizer[approvedWriteTarget]
	afterOpen      func()
	beforeCommit   func()
	afterDirectory func(string)
	beforeLeaf     func()
}

func (Write) Name() string { return "Write" }

// ShortDescription — see Bash.ShortDescription for the rationale.
// Repeats the most-common mistake (using Write where Edit would do)
// inline so the model gets the nudge without the full base prompt.
func (Write) ShortDescription() string {
	return "Create new file or overwrite an existing one with the exact `content`. Prefer Edit for existing files; Write loses original content. Path must be absolute. If file exists, you must have Read it this session. Don't write README/docs unless asked."
}

func (Write) Description() string {
	return `Create a new file, or completely overwrite an existing one, with the exact ` + "`content`" + ` provided. Write replaces the file's entire contents — nothing is merged or appended.

Prefer Edit over Write whenever the file already exists. Use Write only when:
  - The file is genuinely new (and the directory exists, or you don't mind ` + "`mkdir -p`" + ` semantics).
  - You're doing a wholesale rewrite where >50% of the file changes.
  - Writing a generated artifact (e.g. JSON config, formatted output) where preserving any pre-existing content is wrong.

Hard requirements (Write will refuse otherwise):
  - ` + "`path`" + ` MUST be absolute.
  - If the file already exists, you MUST have Read it in this session, and it must not have changed on disk since that Read. This prevents silently clobbering edits made by the user, another agent, or another tool in the same turn.

What NOT to Write:
  - Documentation files (README, CHANGELOG, *.md) unless the user explicitly asked. Don't volunteer documentation as a "nice extra."
  - Files with emojis unless the user explicitly requested them.
  - Files that exist solely to summarize the conversation ("notes.md", "plan.md"). Use the conversation itself; the user can scroll up.
  - Comments explaining what just-written code does in obvious ways. Trust well-named identifiers.

Write is irreversible. The user's permission mode decides whether confirmation is needed.`
}
func (Write) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path", "content"},
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute path. If the file exists, caller must have Read it this session (state tracker enforces this).",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The full new file contents. Include a trailing newline. No partial writes — what you pass replaces the whole file.",
			},
		},
	}
}
func (Write) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }

// IsDestructive: Write overwrites whatever was at path. Hard to undo
// without backup.
func (Write) IsDestructive(map[string]any) bool { return true }

func (w Write) PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error {
	path := strFromAny(in["path"])
	binding, err := prepareWriteTarget(path)
	if err != nil {
		return err
	}
	if !binding.stillPrepared() {
		return errors.New("Write target changed during permission preparation")
	}
	w.authorizer.record(ctx, binding)
	return nil
}

func (w Write) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	path := strFromAny(in["path"])
	d, src := permission.DecisionAllow, ""
	if w.gate != nil {
		d, src = w.gate.CheckPath(ctx, "Write", path, path)
	}
	if d != permission.DecisionDeny {
		if err := w.PrepareAuthorizedInvocation(ctx, in); err != nil {
			return tools.PermissionDeny, security.RedactSubprocessText(err.Error())
		}
	}
	return mapDecision(d), src
}

// MaxWriteContentBytes caps the size Write will accept in one shot.
// Beyond this the model is almost certainly trying to dump output it
// should have streamed.
const MaxWriteContentBytes = 64 * 1024 * 1024

func (w Write) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, _ := in["path"].(string)
	content, _ := in["content"].(string)
	if path == "" {
		// 2026-05-22: unify with Read/Glob/LS error style — soft
		// Result{IsError:true} + redirect hint instead of bare
		// errors.New short-circuit. Helps the model recover on
		// common misuse (passing `file_path` instead of `path`,
		// passing a `command` field thinking Write is Bash, etc).
		return &tools.Result{
			Output:  "Write: `path` field is required (absolute path to a file)." + writeMisuseHint(in),
			IsError: true,
		}, nil
	}
	if !filepath.IsAbs(path) {
		return &tools.Result{
			Output:  "Write: `path` must be absolute (you passed \"" + truncForReadHint(path, 80) + "\"). Use the full path like /Users/.../project/" + path + ".",
			IsError: true,
		}, nil
	}
	if len(content) > MaxWriteContentBytes {
		return &tools.Result{
			Output:  fmt.Sprintf("content too large: %d bytes exceeds %d byte cap", len(content), MaxWriteContentBytes),
			IsError: true,
		}, nil
	}
	binding, hasInvocationID, foundBinding := w.authorizer.consume(ctx)
	if hasInvocationID && !foundBinding {
		if _, prepErr := prepareWriteTarget(path); prepErr != nil {
			if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
				return &tools.Result{Output: fmt.Sprintf("%s is a directory, not a file. Write needs an absolute file path; pick a filename inside the directory.", path), IsError: true}, nil
			}
			return nil, prepErr
		}
		return &tools.Result{Output: "Write denied: permission binding missing for this invocation", IsError: true}, nil
	}
	if !hasInvocationID {
		var prepErr error
		binding, prepErr = prepareWriteTarget(path)
		if prepErr != nil {
			if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
				return &tools.Result{Output: fmt.Sprintf("%s is a directory, not a file. Write needs an absolute file path; pick a filename inside the directory.", path), IsError: true}, nil
			}
			return nil, prepErr
		}
		if w.gate != nil {
			decision, source := w.gate.CheckPath(ctx, "Write", path, path)
			if decision != permission.DecisionAllow {
				return &tools.Result{Output: "Write denied: " + source, IsError: true}, nil
			}
		}
	}
	requestedAbs, absErr := filepath.Abs(filepath.Clean(path))
	if absErr != nil || (binding.existing != nil && requestedAbs != binding.existing.rawPath) ||
		(binding.newPath != nil && requestedAbs != binding.newPath.rawPath) {
		return &tools.Result{Output: "Write denied: invocation input changed after permission check", IsError: true}, nil
	}

	if binding.existing != nil {
		approved := *binding.existing
		f, _, openErr := openApprovedExisting(approved, os.O_RDWR, w.afterOpen)
		if openErr != nil {
			// An approved existing file may never degrade into create semantics.
			return &tools.Result{Output: "Write denied: approved existing target changed or disappeared", IsError: true}, nil
		}
		defer f.Close()
		data, _, readErr := readPinnedFile(f, MaxReadFileSize)
		if readErr != nil {
			return nil, readErr
		}
		statePath := approved.resolvedPath
		if w.state != nil {
			entry, ok := w.state.getFixed(statePath)
			if !ok {
				return &tools.Result{Output: "Write refused: " + path + " exists but has not been Read this session — call Read first to confirm intent", IsError: true}, nil
			}
			if entry.IsPartialView {
				return &tools.Result{Output: FilePartialViewNotEditable + ": " + path, IsError: true}, nil
			}
			if hashBytes(data) != entry.Hash {
				return &tools.Result{Output: FileUnexpectedlyModified + ": " + path, IsError: true}, nil
			}
		}
		if w.beforeCommit != nil {
			w.beforeCommit()
		}
		if _, verifyErr := verifyPinnedContent(f, hashBytes(data), MaxReadFileSize); verifyErr != nil {
			return &tools.Result{Output: FileUnexpectedlyModified + ": " + path, IsError: true}, nil
		}
		postInfo, writeErr := replacePinnedFile(f, []byte(content))
		if writeErr != nil {
			return nil, writeErr
		}
		if w.state != nil {
			if approved.matchesCurrent(postInfo) {
				w.state.recordFixed(statePath, postInfo.ModTime(), []byte(content))
			} else {
				w.state.deleteFixed(statePath)
			}
		}
	} else if binding.newPath != nil {
		approved := *binding.newPath
		f, createErr := createApprovedNewFile(approved, w.afterDirectory, w.beforeLeaf, w.afterOpen)
		if createErr != nil {
			// An approved-new file may never upgrade into overwrite semantics.
			return &tools.Result{Output: "Write denied: approved-new target or parent changed before creation", IsError: true}, nil
		}
		defer f.Close()
		postInfo, writeErr := replacePinnedFile(f, []byte(content))
		if writeErr != nil {
			return nil, writeErr
		}
		if w.state != nil {
			if newPathMatchesCurrent(approved, postInfo) {
				w.state.recordFixed(approved.stateKey, postInfo.ModTime(), []byte(content))
			} else {
				w.state.deleteFixed(approved.stateKey)
			}
		}
	} else {
		return &tools.Result{Output: "Write denied: invalid empty target binding", IsError: true}, nil
	}
	return &tools.Result{Output: "wrote " + path}, nil
}

// writeMisuseHint inspects the input bag when `path` is empty and
// names the right tool / arg for shapes we've seen the model misuse.
// Mirrors readMisuseHint / globMisuseHint. Common confusions:
//   - `file_path` (snake-case variant) → rename to `path`
//   - `command` / `cmd` → wanted Bash
//   - `pattern` → wanted Glob (file search, not write)
func writeMisuseHint(in map[string]any) string {
	if fp, _ := in["file_path"].(string); fp != "" {
		return "\n\nYou passed `file_path` (\"" + truncForReadHint(fp, 80) + "\"). The argument name is `path`, not `file_path`. Try Write({path: \"" + truncForReadHint(fp, 80) + "\", content: \"...\"})."
	}
	if cmd, _ := in["command"].(string); cmd != "" {
		return "\n\nYou passed a `command` field. That's the Bash tool's input — call Bash if you want to run a shell command. Write is for creating files."
	}
	if _, hasContent := in["content"]; !hasContent {
		return "\n\nDid you also forget `content`? Write needs BOTH `path` (where to write) and `content` (what to write)."
	}
	return ""
}
