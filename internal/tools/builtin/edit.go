package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type Edit struct {
	tools.BaseTool
	gate         *permission.Gate
	state        *ReadFileState
	authorizer   *invocationAuthorizer[approvedExistingPath]
	afterOpen    func()
	beforeCommit func()
}

func (Edit) Name() string { return "Edit" }

// ShortDescription — see Bash.ShortDescription for the rationale.
// Pins the must-know failure modes (Read-first, exact-match, indent
// preservation, uniqueness) without spelling out the full strategy
// section.
func (Edit) ShortDescription() string {
	return "Replace `old` with `new` in file at `path`. Literal find-and-replace; preserve exact indentation (tabs vs spaces) from prior Read. Must Read the file this session; `old` must be unique (or pass `all: true`); `old` must differ from `new`; path absolute."
}

func (Edit) Description() string {
	return `Replace exactly the string ` + "`old`" + ` with ` + "`new`" + ` in the file at ` + "`path`" + `. This is a literal find-and-replace, not a regex — every byte (including indentation, tabs vs spaces, trailing whitespace, and newlines) must match.

Hard requirements (Edit will refuse otherwise):
  - You MUST have Read the file in this session before editing. The state tracker remembers the last on-disk hash + mtime per path; Edit aborts if the file changed on disk since that Read.
  - ` + "`old`" + ` MUST be unique in the file. If the snippet appears more than once, either pass ` + "`all: true`" + ` to replace every match, or pick a longer ` + "`old`" + ` with surrounding context that uniquely identifies the target.
  - ` + "`old`" + ` MUST differ from ` + "`new`" + ` — a no-op edit is rejected.
  - ` + "`path`" + ` MUST be absolute.

When editing code from Read output, preserve the exact indentation AFTER the line-number prefix. Read formats each line as: 6-digit line number + tab + content. Copy only the content into ` + "`old`" + ` — never include the line-number prefix or the tab between them. If the file uses tabs, your ` + "`old`" + ` must use tabs; if spaces, spaces; getting this wrong is the single most common edit failure.

Strategy:
  - For one targeted change, prefer a longer ` + "`old`" + ` with 1-2 lines of unique context around the change site over a short ` + "`old`" + ` plus ` + "`all: true`" + `.
  - For 3+ separate changes in the same file, emit multiple Edit calls in sequence (cheap, audit-friendly) instead of one giant ` + "`old`" + `/` + "`new`" + ` block.
  - For wholly new file content or full rewrites, use Write instead.
  - For multi-file refactors (rename a symbol across the repo), don't pack many old→new into one call — call Edit once per file. The model gets clearer feedback when one fails.

Edit is irreversible (writes to disk immediately). The user's permission mode decides whether the call needs explicit approval; you don't need to guess.

## Examples

<example>
context: Rename one variable in foo.go that appears 5 times.
assistant: Edit(path="/repo/foo.go", old="userId", new="userID", all=true)
<reasoning>
all=true is right for a global unambiguous rename. Saves 4 round-trips vs editing each occurrence with unique context.
</reasoning>
</example>

<example>
context: Add a single field to a struct.
assistant: Edit(path="/repo/types.go",
  old="type User struct {\n\tName string\n}",
  new="type User struct {\n\tName  string\n\tEmail string\n}")
<reasoning>
Targeted change. old includes 2 lines of unique context so the match is unambiguous without all=true.
</reasoning>
</example>

<example>
context: Same symbol needs renaming across 4 files.
assistant: One Edit call per file (separate calls).
<reasoning>
DO NOT pack 4 files into one call. Per-file Edits give clearer error messages when one fails — e.g. unique-context violation in file 3 doesn't roll back files 1-2.
</reasoning>
</example>`
}
func (Edit) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path", "old", "new"},
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute path to the file. Caller must have Read this path in the current session, and the file must not have changed on disk since that Read.",
			},
			"old": map[string]any{
				"type":        "string",
				"description": "Literal substring to find — preserve exact indentation, tabs vs spaces, and newlines. Must be unique in the file unless `all` is true. Must differ from `new`.",
			},
			"new": map[string]any{
				"type":        "string",
				"description": "Replacement string. Indentation must match the surrounding code (same tabs vs spaces convention as the rest of the file).",
			},
			"all": map[string]any{
				"type":        "boolean",
				"description": "Replace every occurrence of `old` instead of requiring uniqueness. Use when intentionally renaming/normalizing across the whole file (e.g. variable rename). Default false.",
			},
		},
	}
}

// NormalizeInput maps the finite set of historical Edit spellings to the
// canonical schema before validation and authorization. An explicit empty new
// string remains a valid deletion request.
func (Edit) NormalizeInput(input map[string]any) (map[string]any, error) {
	return pubtool.NormalizeAliases(input, map[string]string{
		"file_path":   "path",
		"old_string":  "old",
		"new_string":  "new",
		"replace_all": "all",
	})
}

func (Edit) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }

// IsDestructive: Edit is irreversible (writes to disk). Drives stricter
// ASK colouring + bypass-immune treatment when path matches a safety
// fragment.
func (Edit) IsDestructive(map[string]any) bool { return true }

func (e Edit) PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error {
	path := strFromAny(in["path"])
	binding, err := prepareExistingPath(path, false)
	if err != nil {
		return err
	}
	if !binding.matchesCurrent(binding.targetInfo) {
		return errors.New("Edit target changed during permission preparation")
	}
	e.authorizer.record(ctx, binding)
	return nil
}

func (e Edit) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	path := strFromAny(in["path"])
	d, src := permission.DecisionAllow, ""
	if e.gate != nil {
		d, src = e.gate.CheckPath(ctx, "Edit", path, path)
	}
	if d != permission.DecisionDeny {
		if err := e.PrepareAuthorizedInvocation(ctx, in); err != nil {
			return tools.PermissionDeny, security.RedactSubprocessText(err.Error())
		}
	}
	return mapDecision(d), src
}

// MaxEditFileSize caps the file size Edit will modify. Same rationale
// as MaxReadFileSize but tighter — editing a 256 MiB file in-memory
// then writing back is a sure recipe for swap thrashing.
const MaxEditFileSize = 64 * 1024 * 1024

// FileUnexpectedlyModified is the error a stale-write check returns.
// Surfaced to the LLM so it knows to re-Read before retrying. Mirrors
// claude-code FILE_UNEXPECTEDLY_MODIFIED_ERROR.
const FileUnexpectedlyModified = "file unexpectedly modified after last read; re-Read the file before editing"

// FilePartialViewNotEditable is the error a full-file Write returns after a
// partial Read. Targeted Edit is allowed because it replaces one exact,
// unique old string and still verifies the full-file hash captured by Read;
// Write would replace unseen regions and therefore remains blocked.
const FilePartialViewNotEditable = "file was Read with offset/limit (partial view); re-Read the full file before overwriting"

func (e Edit) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, pathOK := in["path"].(string)
	old, oldOK := in["old"].(string)
	newS, newOK := in["new"].(string)
	if !pathOK {
		return &tools.Result{Output: "Edit: `path` must be a string", IsError: true}, nil
	}
	if !oldOK {
		return &tools.Result{Output: "Edit: required `old` field must be a string", IsError: true}, nil
	}
	// An explicitly empty replacement is a valid deletion. Missing/wrong-type
	// `new` must not be coerced to that destructive operation when Execute is
	// called outside the dispatcher.
	if !newOK {
		return &tools.Result{Output: "Edit: required `new` field must be a string", IsError: true}, nil
	}
	all, _ := in["all"].(bool)
	if path == "" || old == "" {
		return nil, errors.New("path and old are required")
	}
	if old == newS {
		return nil, errors.New("old and new are identical")
	}
	if !filepath.IsAbs(path) {
		return &tools.Result{Output: "Edit: `path` must be absolute", IsError: true}, nil
	}
	binding, hasInvocationID, foundBinding := e.authorizer.consume(ctx)
	if hasInvocationID && !foundBinding {
		if _, prepErr := prepareExistingPath(path, false); prepErr != nil {
			if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
				return &tools.Result{Output: fmt.Sprintf("%s is a directory, not a file. Edit operates on file contents — use LS to list entries, Read to view a specific file, then Edit it.", path), IsError: true}, nil
			}
			return nil, prepErr
		}
		return &tools.Result{Output: "Edit denied: permission binding missing for this invocation", IsError: true}, nil
	}
	if !hasInvocationID {
		var prepErr error
		binding, prepErr = prepareExistingPath(path, false)
		if prepErr != nil {
			if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
				return &tools.Result{Output: fmt.Sprintf("%s is a directory, not a file. Edit operates on file contents — use LS to list entries, Read to view a specific file, then Edit it.", path), IsError: true}, nil
			}
			return nil, prepErr
		}
		if e.gate != nil {
			decision, source := e.gate.CheckPath(ctx, "Edit", path, path)
			if decision != permission.DecisionAllow {
				return &tools.Result{Output: "Edit denied: " + source, IsError: true}, nil
			}
		}
	}
	requestedAbs, absErr := filepath.Abs(filepath.Clean(path))
	if absErr != nil || requestedAbs != binding.rawPath {
		return &tools.Result{Output: "Edit denied: invocation input changed after permission check", IsError: true}, nil
	}
	f, _, err := openApprovedExisting(binding, os.O_RDWR, e.afterOpen)
	if err != nil {
		return &tools.Result{Output: "Edit denied: approved target changed before execution", IsError: true}, nil
	}
	defer f.Close()
	bs, _, err := readPinnedFile(f, MaxEditFileSize)
	if err != nil {
		return nil, err
	}
	statePath := binding.resolvedPath
	preservePartialView := false
	partialOffset, partialLimit := 1, 0

	// Staleness check: if a Read of this path was recorded earlier
	// this session and either the mtime or content differs from the
	// last-seen state, refuse — the model's mental snapshot is stale.
	//
	// Skipped only when state is nil (test/embedding compatibility). With a
	// session state, an existing file must have a matching Read/Write snapshot;
	// otherwise a path alias or blind Edit could manufacture full Write authority.
	if e.state != nil {
		if entry, ok := e.state.getFixed(statePath); ok {
			preservePartialView = entry.IsPartialView
			partialOffset, partialLimit = entry.Offset, entry.Limit
			if entry.IsPartialView && all {
				return &tools.Result{
					Output:  "Edit refused: all=true is unavailable after a partial or redacted Read; use one exact unique replacement",
					IsError: true,
				}, nil
			}
			// The content hash is the precise signal: if it changed, the file
			// was modified out-of-band since we Read it → refuse (stale write).
			// The old `&&` also required mtime to differ, so a content change
			// that preserved mtime (mtime granularity, `touch -r`, restore)
			// slipped through and clobbered the on-disk change. mtime alone is
			// not a content signal (a bare `touch` changes mtime but not hash),
			// so the hash check is both necessary and sufficient.
			currentHash := hashBytes(bs)
			if currentHash != entry.Hash {
				return &tools.Result{
					Output:  FileUnexpectedlyModified + ": " + path,
					IsError: true,
				}, nil
			}
		} else {
			return &tools.Result{
				Output:  "Edit refused: " + path + " has not been Read this session — call Read first",
				IsError: true,
			}, nil
		}
	}

	body := string(bs)
	count := strings.Count(body, old)
	if count == 0 {
		return nil, errors.New("old string not found in file")
	}
	if !all && count > 1 {
		return nil, fmt.Errorf("old string appears %d times; pass all=true or supply more context for uniqueness", count)
	}
	var out string
	if all {
		out = strings.ReplaceAll(body, old, newS)
	} else {
		out = strings.Replace(body, old, newS, 1)
	}
	if e.beforeCommit != nil {
		e.beforeCommit()
	}
	if _, err := verifyPinnedContent(f, hashBytes(bs), MaxEditFileSize); err != nil {
		return &tools.Result{Output: FileUnexpectedlyModified + ": " + path, IsError: true}, nil
	}
	postInfo, err := replacePinnedFile(f, []byte(out))
	if err != nil {
		return nil, err
	}
	// Re-record post-write so a follow-up Edit on the same path does not
	// false-positive on staleness. A targeted Edit must not upgrade a partial
	// or redacted Read into whole-file Write authority: the model still has not
	// seen every byte that the edit preserved.
	if e.state != nil {
		if binding.matchesCurrent(postInfo) {
			if preservePartialView {
				e.state.recordPartialFixed(statePath, postInfo.ModTime(), []byte(out), partialOffset, partialLimit)
			} else {
				e.state.recordFixed(statePath, postInfo.ModTime(), []byte(out))
			}
		} else {
			e.state.deleteFixed(statePath)
		}
	}
	return &tools.Result{Output: fmt.Sprintf("edited %s (%d replacements)", path, ifelse(all, count, 1))}, nil
}

func ifelse[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
