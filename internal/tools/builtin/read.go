package builtin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Read struct {
	tools.BaseTool
	gate  *permission.Gate
	state *ReadFileState
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

After Read, the path is "read" for the session: subsequent Edit/Write on that path will work, AND will be refused if the file changed on disk between Read and Edit/Write. If you want to edit a partial-view file (offset != 1 or hit the limit), Read it again fully first — otherwise Edit refuses to mutate regions you never saw.

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

func (r Read) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := r.gate.Check(context.Background(), "Read", strFromAny(in["path"]))
	return mapDecision(d), src
}

// fileUnchangedStub is returned by Read when sessionReadState already
// holds a full snapshot of the path AND the on-disk file hasn't
// drifted. Verbatim from claude-code's FILE_UNCHANGED_STUB
// (tools/FileReadTool/prompt.ts) — keeping the wording identical so
// model behaviour matches the upstream prompt's well-trained
// expectations.
const fileUnchangedStub = "File unchanged since last read. The " +
	"content from the earlier Read tool_result in this conversation " +
	"is still current — refer to that instead of re-reading."

// MaxReadFileSize caps the total bytes Read will load into memory
// before returning. claude-code uses 1 GiB; we pick 256 MiB because
// Go runtime memory pressure on a 16 GB Mac with a long context
// window is more painful than on Bun. Set high enough that source
// trees (linux kernel ~ 1.4 GB checked out) won't accidentally OOM
// us if the model passes a giant file path; small enough that we
// fail fast instead of swapping. Override via METIS_READ_MAX_BYTES.
const MaxReadFileSize = 256 * 1024 * 1024

func (r Read) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
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
	offset := intArg(in, "offset", 1)
	limit := intArg(in, "limit", 2000)

	// Stat first: cheap, lets us reject GB-sized files before
	// allocating, and gives us the mtime for ReadFileState.
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		// Symmetric with LS's regular-file rescue: a model that
		// passes a directory path to Read gets a confusing
		// "is a directory" syscall error and tends to re-call
		// Read with the same path. Steer to LS instead.
		return &tools.Result{
			Output:  fmt.Sprintf("%s is a directory, not a file. Use LS to list its entries, then Read individual files.", path),
			IsError: true,
		}, nil
	}
	if st.Size() > MaxReadFileSize {
		return &tools.Result{
			Output:  fmt.Sprintf("file too large: %d bytes exceeds %d byte cap (use Bash with head/tail to inspect)", st.Size(), MaxReadFileSize),
			IsError: true,
		}, nil
	}

	// 2026-05-23: FILE_UNCHANGED_STUB short-circuit. If sessionReadState
	// already has a FULL snapshot of this path AND the on-disk file
	// hasn't drifted (mtime equal — fast path; hash equal — slow path
	// covering touch / cloud-sync that bumps mtime without changing
	// content), return a short stub instead of re-emitting the full
	// content. Mirrors claude-code's FILE_UNCHANGED_STUB
	// (tools/FileReadTool/FileReadTool.ts). Cuts ~30% Read tokens on
	// runs where parent + sub-agents repeatedly Read the same files
	// (the kimi-cli-go porting test 2026-05-23 hit this hard: 186
	// Reads, ~half on files already in state).
	//
	// Skip the stub for partial reads (offset / limit set) — the
	// caller is explicitly asking for a slice, not a re-read of the
	// whole file. Also skip on partial-view prior entries (Edit would
	// reject those anyway, but a full Read should pull the actual
	// content so the model can edit it).
	if r.state != nil && offset == 1 && limit == 2000 {
		if prior, ok := r.state.Get(path); ok && !prior.IsPartialView {
			if prior.MTime.Equal(st.ModTime()) {
				return &tools.Result{Output: fileUnchangedStub}, nil
			}
			// mtime drifted — verify hash before claiming unchanged.
			if data, rerr := os.ReadFile(path); rerr == nil {
				if hashBytes(data) == prior.Hash {
					// Content unchanged; just refresh mtime so
					// next call hits the fast path.
					r.state.Record(path, st.ModTime(), data)
					return &tools.Result{Output: fileUnchangedStub}, nil
				}
			}
			// Drift detected — fall through, do a real Read.
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	lineno := 0
	emitted := 0
	for sc.Scan() {
		lineno++
		if lineno < offset {
			continue
		}
		if emitted >= limit {
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", lineno, sc.Text())
		emitted++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Record in ReadFileState so a subsequent Edit/Write on this path
	// can detect out-of-band mtime drift (file changed between Read
	// and Edit). We re-stat + re-read after the user-facing scan so
	// writers that updated the file during the scan are caught — the
	// mtime/hash we want is the last-known on-disk state, not what we
	// saw mid-stream.
	//
	// The view classifier flags partial reads (offset != 1, or hit the
	// limit before EOF). Edit/Write refuse on partial-view entries:
	// the model would otherwise rewrite regions of the file it never
	// saw and silently lose those bytes. The hash we record is still
	// over the FULL file — what the LLM saw is partial, but the
	// staleness check needs whole-file ground truth.
	if r.state != nil {
		if data, rerr := os.ReadFile(path); rerr == nil {
			if st2, serr := os.Stat(path); serr == nil {
				partial := offset != 1 || emitted >= limit
				if partial {
					r.state.RecordPartial(path, st2.ModTime(), data, offset, limit)
				} else {
					r.state.Record(path, st2.ModTime(), data)
				}
			}
		}
	}

	if emitted == 0 {
		return &tools.Result{Output: "(file is empty or offset past end)"}, nil
	}
	return &tools.Result{Output: b.String()}, nil
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
