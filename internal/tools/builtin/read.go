package builtin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Read struct {
	gate  *permission.Gate
	state *ReadFileState
}

func (Read) Name() string { return "Read" }
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

After Read, the path is "read" for the session: subsequent Edit/Write on that path will work, AND will be refused if the file changed on disk between Read and Edit/Write. If you want to edit a partial-view file (offset != 1 or hit the limit), Read it again fully first — otherwise Edit refuses to mutate regions you never saw.`
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

func (r Read) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := r.gate.Check(context.Background(), "Read", strFromAny(in["path"]))
	return mapDecision(d), src
}

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
		return nil, errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	offset := intArg(in, "offset", 1)
	limit := intArg(in, "limit", 2000)

	// Stat first: cheap, lets us reject GB-sized files before
	// allocating, and gives us the mtime for ReadFileState.
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxReadFileSize {
		return &tools.Result{
			Output:  fmt.Sprintf("file too large: %d bytes exceeds %d byte cap (use Bash with head/tail to inspect)", st.Size(), MaxReadFileSize),
			IsError: true,
		}, nil
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
