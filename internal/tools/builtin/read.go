package builtin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type Read struct {
	tools.BaseTool
	gate       *permission.Gate
	state      *ReadFileState
	authorizer *invocationAuthorizer[approvedExistingPath]
	// afterOpen is a test seam for deterministic path-swap regression tests.
	// Production constructors leave it nil.
	afterOpen func()
}

func newReadPathAuthorizer() *invocationAuthorizer[approvedExistingPath] {
	return newInvocationAuthorizer[approvedExistingPath]()
}

func (Read) Name() string { return "Read" }

// ShortDescription — see Bash.ShortDescription for the rationale.
// Pins the absolute-path + Read-not-cat habit so sub-agents that
// inherited a Read-only tool palette still know to use this over Bash.
func (Read) ShortDescription() string {
	return "Read a file from the local filesystem. Returns 1-indexed line-numbered content (default 2000 lines from top). Always use Read NOT cat/head/tail. Path must be absolute. After Read, Edit/Write can mutate the file in the same session."
}

func (Read) Description() string {
	return `Read a file from the local filesystem. The output is the file content prefixed with 1-indexed line numbers in ` + "`cat -n`" + ` format: 6-digit line number + tab + content. By default, returns up to 2000 lines starting from the top.

Always use Read for file contents — NOT Bash with cat/head/tail/less/more. Read gives the user a structured audit row, deduplicates against the session state tracker, and lets Edit/Write verify the file hasn't drifted on disk before mutating it.

When to slice with offset/limit instead of the default:
  - You already know the rough line range (e.g. from a Grep hit at line 482).
  - The file is over a few thousand lines and you only need a specific section.

When NOT to use Read:
  - Finding files by name pattern → use Glob (e.g. ` + "`**/*.go`" + `).
  - Searching for text across files → use Grep.
  - Re-reading a file you just edited in this turn — the state tracker already knows the current content; another Read is wasted tokens.
  - Reading binary files (images, compiled binaries). Read will return raw bytes which the model can't interpret.

Hard requirements:
  - ` + "`path`" + ` MUST be absolute. Relative paths are rejected.
  - File size cap is 256 MiB; oversized files return an error directing you to ` + "`Bash head/tail`" + ` for a peek.

After Read, the path is "read" for the session: subsequent Edit/Write on that path will be refused if the file changed on disk. A targeted Edit may replace one exact unique string after a partial view because it preserves every other byte and verifies the full-file hash. A full-file Write still requires a complete Read first.

## Examples

<example>
user: What does the parser do?
assistant: Read(path="/repo/src/parser/parser.go")
<reasoning>
Whole-file read, no offset/limit. Default is the right choice when you don't yet know what you're looking for.
</reasoning>
</example>

<example>
context: Grep found "func LoadConfig" at /repo/internal/config.go:142.
assistant: Read(path="/repo/internal/config.go", offset=130, limit=80)
<reasoning>
Targeted slice around a known line. Avoids loading a 1500-line file when only ~80 lines are relevant.
</reasoning>
</example>

<example>
context: I just Edit'd /repo/main.go in the previous turn.
assistant: Continues directly — does NOT re-Read /repo/main.go.
<reasoning>
The state tracker already knows the post-edit content. Re-reading is wasted tokens; Edit already validated the diff.
</reasoning>
</example>`
}
func (Read) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path"},
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute path to the file. Relative paths are rejected.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "1-indexed starting line. Use when you know the rough region of interest (e.g. from a Grep hit). Default 1 (start of file).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum lines to return. Default 2000. Increase for big files only when you genuinely need the extra range; remember context is precious.",
			},
		},
	}
}

// NormalizeInput keeps restored transcripts that used Claude-style file_path
// compatible with Metis' canonical path schema. It performs no type coercion.
func (Read) NormalizeInput(input map[string]any) (map[string]any, error) {
	return pubtool.NormalizeAliases(input, map[string]string{"file_path": "path"})
}

func (Read) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

// IsReadOnly: Read never mutates filesystem state. Snip is allowed to
// summarise its tool_result aggressively.
func (Read) IsReadOnly(map[string]any) bool { return true }

// MaxResultSizeChars opts Read out of ingestion-time spilling: persisting
// Read output to a file the model recovers WITH Read is circular, and
// Read already self-bounds via its line/offset limits. Mirrors
// claude-code's maxResultSizeChars: Infinity on FileRead
// (toolResultStorage.ts::getPersistenceThreshold).
func (Read) MaxResultSizeChars() int { return tools.ResultSizeUnlimited }

func (r Read) PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error {
	path := strFromAny(in["path"])
	binding, err := prepareExistingPath(path, false)
	if err != nil {
		return err
	}
	if !binding.matchesCurrent(binding.targetInfo) {
		return errors.New("Read target changed during permission preparation")
	}
	r.authorizer.record(ctx, binding)
	return nil
}

func (r Read) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	path := strFromAny(in["path"])
	d, src := permission.DecisionAllow, ""
	if r.gate != nil {
		d, src = r.gate.CheckPath(ctx, "Read", path, path)
	}
	if d != permission.DecisionDeny {
		if err := r.PrepareAuthorizedInvocation(ctx, in); err != nil {
			return tools.PermissionDeny, security.RedactSubprocessText(err.Error())
		}
	}
	return mapDecision(d), src
}

// fileUnchangedStub is returned by Read when sessionReadState already
// holds a full snapshot of the path AND the on-disk file hasn't
// drifted. Verbatim from claude-code's FILE_UNCHANGED_STUB
// (tools/FileReadTool/prompt.ts) — keeping the wording identical so
// model behaviour matches the upstream prompt's well-trained
// expectations — plus one metis-specific escape hatch sentence:
// after context compaction the "earlier Read tool_result" the stub
// points at may no longer exist, and export 2026-08-15-150806 shows
// the model looping 16x on an identical block ("你说得对，这个数据
// **有问题**" → Write → Read → unchanged → repeat) with no way out.
// Read only short-circuits on the default (offset=1, limit=2000), so
// a non-default slice read is the documented way to force content.
const fileUnchangedStub = "File unchanged since last read. The " +
	"content from the earlier Read tool_result in this conversation " +
	"is still current — refer to that instead of re-reading. " +
	"If that earlier result is no longer visible in context (e.g. " +
	"after compaction), force the content by re-reading a slice with " +
	"non-default offset/limit (e.g. limit: 500)."

// MaxReadFileSize caps the total bytes Read will load into memory
// before returning. claude-code uses 1 GiB; we pick 256 MiB because
// Go runtime memory pressure on a 16 GB Mac with a long context
// window is more painful than on Bun. Set high enough that source
// trees (linux kernel ~ 1.4 GB checked out) won't accidentally OOM
// us if the model passes a giant file path; small enough that we
// fail fast instead of swapping. Override via METIS_READ_MAX_BYTES.
const MaxReadFileSize = 256 * 1024 * 1024

func (r Read) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, _ := in["path"].(string)
	if path == "" {
		// Richer error than the bare "path is required" — same
		// lesson as Glob (image #34) and LS, applied here on
		// 2026-05-21 after a sub-agent looped on this exact terse
		// message. The hint reads the input bag for misuse fields
		// (`file_path`, `command`, `pattern`, ...) and points at
		// the right tool / arg name.
		hint := readMisuseHint(in)
		return &tools.Result{
			Output: "Read: `path` field is required (absolute path to a file, e.g. \"/Users/x/proj/main.go\"). " +
				"Read takes file contents — for directories use LS, for shell commands use Bash, for name-pattern search use Glob." +
				hint,
			IsError: true,
		}, nil
	}
	if !filepath.IsAbs(path) {
		return &tools.Result{
			Output:  "Read: `path` must be absolute (you passed \"" + truncForReadHint(path, 80) + "\"). Prepend the project root or `$HOME` to make it absolute (e.g. cwd-based: /Users/.../yourproject/" + path + ").",
			IsError: true,
		}, nil
	}
	if r.gate != nil && r.gate.Mode() == permission.ModeBypassPermissions {
		credentialPath := path
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			credentialPath = resolved
		}
		if permission.IsSecretReadPath(path) || permission.IsSecretReadPath(credentialPath) {
			return &tools.Result{
				Output:  "Read denied: credential files are unavailable in bypassPermissions",
				IsError: true,
			}, nil
		}
	}
	offset := intArg(in, "offset", 1)
	limit := intArg(in, "limit", 2000)
	binding, hasInvocationID, foundBinding := r.authorizer.consume(ctx)
	if hasInvocationID && !foundBinding {
		if _, prepErr := prepareExistingPath(path, false); prepErr != nil {
			if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
				return &tools.Result{Output: fmt.Sprintf("%s is a directory, not a file. Use LS to list its entries, then Read individual files.", path), IsError: true}, nil
			}
			return nil, prepErr
		}
		return &tools.Result{Output: "Read denied: permission binding missing for this invocation", IsError: true}, nil
	}
	if !hasInvocationID {
		var prepErr error
		binding, prepErr = prepareExistingPath(path, false)
		if prepErr != nil {
			if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
				return &tools.Result{Output: fmt.Sprintf("%s is a directory, not a file. Use LS to list its entries, then Read individual files.", path), IsError: true}, nil
			}
			return nil, prepErr
		}
		if r.gate != nil {
			decision, source := r.gate.CheckPath(ctx, "Read", path, path)
			if decision != permission.DecisionAllow {
				return &tools.Result{Output: "Read denied: " + security.RedactSubprocessText(source), IsError: true}, nil
			}
		}
	}
	requestedAbs, absErr := filepath.Abs(filepath.Clean(path))
	if absErr != nil || requestedAbs != binding.rawPath {
		return &tools.Result{Output: "Read denied: invocation input changed after permission check", IsError: true}, nil
	}

	// Open first, then verify the lexical and resolved path still identify the
	// opened inode. Every byte below comes from this pinned descriptor; no
	// post-authorization os.ReadFile(path) can be redirected by a symlink swap.
	f, st, resolvedPath, err := openPinnedReadFile(path, MaxReadFileSize, r.afterOpen)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if st != nil && st.IsDir() {
			return &tools.Result{
				Output:  fmt.Sprintf("%s is a directory, not a file. Use LS to list its entries, then Read individual files.", path),
				IsError: true,
			}, nil
		}
		return &tools.Result{Output: "Read denied: " + security.RedactSubprocessText(err.Error()), IsError: true}, nil
	}
	defer f.Close()
	statePath := filepath.Clean(resolvedPath)
	if resolvedPath != binding.resolvedPath || !os.SameFile(binding.targetInfo, st) {
		return &tools.Result{Output: "Read denied: source path changed after permission check", IsError: true}, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxReadFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxReadFileSize {
		return &tools.Result{Output: fmt.Sprintf("file too large: exceeds %d byte cap", MaxReadFileSize), IsError: true}, nil
	}
	stAfter, err := f.Stat()
	if err != nil || !os.SameFile(st, stAfter) || st.Size() != stAfter.Size() || !st.ModTime().Equal(stAfter.ModTime()) {
		return &tools.Result{Output: "Read denied: source changed while reading", IsError: true}, nil
	}
	// Locate only credential values whose key/marker context spans source
	// lines. offset/limit can remove that context, so these source byte ranges
	// are applied to the rendered page before the ordinary final redactor.
	fileRedactor := security.NewFileCredentialRedactor(data)

	var b strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	lineno := 0
	emitted := 0
	truncated := false
	sourceLineStart := 0
	for sc.Scan() {
		lineno++
		line := sc.Text()
		currentLineStart := sourceLineStart
		sourceLineStart += len(sc.Bytes())
		if sourceLineStart < len(data) && data[sourceLineStart] == '\r' {
			sourceLineStart++
		}
		if sourceLineStart < len(data) && data[sourceLineStart] == '\n' {
			sourceLineStart++
		}
		// Honor cancellation so a huge-file scan (up to the 256 MiB cap) or
		// a slow/blocking read can't pin this goroutine after the parent
		// turn is cancelled or times out. Cheap: a non-blocking select every
		// ~512 lines.
		if lineno%512 == 0 {
			select {
			case <-ctx.Done():
				return &tools.Result{Output: "Read cancelled: " + ctx.Err().Error(), IsError: true}, nil
			default:
			}
		}
		if lineno < offset {
			continue
		}
		if emitted >= limit {
			// Scanner reached one more source line, so this is genuine
			// truncation. Merely emitting exactly limit lines at EOF is a full
			// view and must not permanently block a later whole-file Write.
			truncated = true
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", lineno, fileRedactor.RedactLineAt(currentLineStart, line))
		emitted++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	output := b.String()
	finalOutput := security.RedactSubprocessText(output)
	viewRedacted := fileRedactor.HasRedactions() || finalOutput != output

	// FILE_UNCHANGED_STUB is safe only when a prior full view and the current
	// rendered view both contain every source byte. A redacted view must remain
	// partial so it cannot grant whole-file Write authority.
	if r.state != nil && offset == 1 && limit == 2000 && !viewRedacted {
		if prior, ok := r.state.Get(statePath); ok && !prior.IsPartialView {
			if prior.MTime.Equal(st.ModTime()) {
				return &tools.Result{Output: fileUnchangedStub}, nil
			}
			if hashBytes(data) == prior.Hash {
				r.state.Record(statePath, st.ModTime(), data)
				return &tools.Result{Output: fileUnchangedStub}, nil
			}
		}
	}

	// Record in ReadFileState so a subsequent Edit/Write on this path
	// can detect out-of-band drift between Read and Edit. data and st came
	// from one pinned descriptor, and the post-read descriptor check above
	// rejected size/mtime changes during the snapshot.
	//
	// The view classifier flags partial reads (offset != 1, or hit the
	// limit before EOF). A full-file Write refuses partial-view entries;
	// targeted Edit remains safe because it replaces an exact unique string
	// and validates this full-file hash before touching the file. The hash we
	// record is over the FULL file — what the LLM saw is partial, but the
	// staleness check needs whole-file ground truth.
	if r.state != nil {
		partial := offset != 1 || truncated || viewRedacted
		if partial {
			r.state.RecordPartial(statePath, st.ModTime(), data, offset, limit)
		} else {
			r.state.Record(statePath, st.ModTime(), data)
		}
	}

	if emitted == 0 {
		return &tools.Result{Output: "(file is empty or offset past end)"}, nil
	}
	return &tools.Result{Output: finalOutput}, nil
}

func openPinnedReadFile(path string, maxBytes int64, afterOpen func()) (*os.File, os.FileInfo, string, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, nil, "", err
	}
	lexicalBefore, err := os.Lstat(absPath)
	if err != nil {
		return nil, nil, "", err
	}
	resolvedBefore, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, nil, "", err
	}
	targetBefore, err := os.Stat(absPath)
	if err != nil {
		return nil, nil, "", err
	}
	if !targetBefore.Mode().IsRegular() {
		return nil, targetBefore, filepath.Clean(resolvedBefore), fmt.Errorf("%s is not a regular file", path)
	}
	if maxBytes > 0 && targetBefore.Size() > maxBytes {
		return nil, targetBefore, filepath.Clean(resolvedBefore), fmt.Errorf("file too large: %d bytes exceeds %d byte cap", targetBefore.Size(), maxBytes)
	}
	file, err := os.Open(absPath)
	if err != nil {
		return nil, nil, "", err
	}
	if afterOpen != nil {
		afterOpen()
	}
	openedInfo, openErr := file.Stat()
	lexicalAfter, lexicalErr := os.Lstat(absPath)
	resolvedAfter, resolvedErr := filepath.EvalSymlinks(absPath)
	targetAfter, targetErr := os.Stat(absPath)
	if openErr != nil || lexicalErr != nil || resolvedErr != nil || targetErr != nil ||
		!openedInfo.Mode().IsRegular() || !os.SameFile(targetBefore, openedInfo) ||
		!os.SameFile(lexicalBefore, lexicalAfter) || !os.SameFile(openedInfo, targetAfter) ||
		filepath.Clean(resolvedBefore) != filepath.Clean(resolvedAfter) {
		file.Close()
		return nil, nil, "", errors.Join(openErr, lexicalErr, resolvedErr, targetErr,
			fmt.Errorf("source path changed while opening: %s", path))
	}
	return file, openedInfo, filepath.Clean(resolvedAfter), nil
}

// readMisuseHint inspects the input bag when `path` is empty and
// names the right tool / arg for shapes we've seen the model use
// by mistake. Mirrors globMisuseHint — appended to the
// "path is required" error so the next turn lands on the correct
// tool instead of looping.
//
// Common confusions (image #34/#43 sub-agent telemetry):
//   - `file_path` (snake-case variant) → just suggest renaming to `path`
//   - `command` / `cmd` → wanted Bash
//   - `pattern` → wanted Glob
//   - `query` → wanted Grep
func readMisuseHint(in map[string]any) string {
	if fp, _ := in["file_path"].(string); fp != "" {
		return "\n\nYou passed `file_path` (\"" + truncForReadHint(fp, 80) + "\"). The argument name is `path`, not `file_path`. Try Read({path: \"" + truncForReadHint(fp, 80) + "\"})."
	}
	if cmd, _ := in["command"].(string); cmd != "" {
		return "\n\nYou passed a `command` field (\"" + truncForReadHint(cmd, 80) + "\"). That's the Bash tool's input — call Bash if you want to run a shell command."
	}
	if cmd, _ := in["cmd"].(string); cmd != "" {
		return "\n\nYou passed a `cmd` field. Read takes `path`. Call Bash if you want to run a shell command."
	}
	if pat, _ := in["pattern"].(string); pat != "" {
		return "\n\nYou passed a `pattern` field (\"" + truncForReadHint(pat, 80) + "\"). That's the Glob tool's input — call Glob for name-pattern search."
	}
	if q, _ := in["query"].(string); q != "" {
		return "\n\nYou passed a `query` field (\"" + truncForReadHint(q, 80) + "\"). For text-in-files search use Grep."
	}
	return ""
}

// truncForReadHint shortens a string for embedding in an error
// message so a 500-char path doesn't blow the result-row layout.
// Rune-aware to avoid mid-codepoint cuts on CJK paths.
func truncForReadHint(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes-1]) + "…"
}
