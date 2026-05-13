package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Write struct {
	gate  *permission.Gate
	state *ReadFileState
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

func (w Write) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := w.gate.Check(context.Background(), "Write", strFromAny(in["path"]))
	return mapDecision(d), src
}

// MaxWriteContentBytes caps the size Write will accept in one shot.
// Beyond this the model is almost certainly trying to dump output it
// should have streamed.
const MaxWriteContentBytes = 64 * 1024 * 1024

func (w Write) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	path, _ := in["path"].(string)
	content, _ := in["content"].(string)
	if path == "" {
		return nil, errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	if len(content) > MaxWriteContentBytes {
		return &tools.Result{
			Output:  fmt.Sprintf("content too large: %d bytes exceeds %d byte cap", len(content), MaxWriteContentBytes),
			IsError: true,
		}, nil
	}

	// Staleness check on overwrite: if file exists and Read recorded
	// a different content/mtime than what's there now, refuse so the
	// model re-Reads first.
	if st, statErr := os.Stat(path); statErr == nil {
		if w.state != nil {
			if entry, ok := w.state.Get(path); ok {
				if entry.IsPartialView {
					return &tools.Result{
						Output:  FilePartialViewNotEditable + ": " + path,
						IsError: true,
					}, nil
				}
				if data, rerr := os.ReadFile(path); rerr == nil {
					currentHash := hashBytes(data)
					if currentHash != entry.Hash && !st.ModTime().Equal(entry.MTime) {
						return &tools.Result{
							Output:  FileUnexpectedlyModified + ": " + path,
							IsError: true,
						}, nil
					}
				}
			} else {
				// File exists, no Read in this session → require it.
				// Skipping this rule lets the model clobber files
				// blindly, which is the bug Write's docstring warns
				// against. Skipped only if state is nil (test bypass).
				return &tools.Result{
					Output:  "Write refused: " + path + " exists but has not been Read this session — call Read first to confirm intent",
					IsError: true,
				}, nil
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	if w.state != nil {
		if st, serr := os.Stat(path); serr == nil {
			w.state.Record(path, st.ModTime(), []byte(content))
		}
	}
	return &tools.Result{Output: "wrote " + path}, nil
}
