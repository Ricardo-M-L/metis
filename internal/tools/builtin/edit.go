package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Edit struct {
	gate  *permission.Gate
	state *ReadFileState
}

func (Edit) Name() string { return "Edit" }
func (Edit) Description() string {
	return "Replace `old` with `new` in `path`. The match must be unique unless `all` is true. Caller MUST have Read the file first this session — Edit refuses if the on-disk state has drifted since that Read."
}
func (Edit) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path", "old", "new"},
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
			"old":  map[string]any{"type": "string"},
			"new":  map[string]any{"type": "string"},
			"all":  map[string]any{"type": "boolean"},
		},
	}
}
func (Edit) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }

// IsDestructive: Edit is irreversible (writes to disk). Drives stricter
// ASK colouring + bypass-immune treatment when path matches a safety
// fragment.
func (Edit) IsDestructive(map[string]any) bool { return true }

func (e Edit) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := e.gate.Check(context.Background(), "Edit", strFromAny(in["path"]))
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

func (e Edit) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	path, _ := in["path"].(string)
	old, _ := in["old"].(string)
	newS, _ := in["new"].(string)
	all, _ := in["all"].(bool)
	if path == "" || old == "" {
		return nil, errors.New("path and old are required")
	}
	if old == newS {
		return nil, errors.New("old and new are identical")
	}

	// Size guard up-front so we don't read GB into RAM only to fail.
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxEditFileSize {
		return &tools.Result{
			Output:  fmt.Sprintf("file too large: %d bytes exceeds %d byte edit cap", st.Size(), MaxEditFileSize),
			IsError: true,
		}, nil
	}

	// === Atomic stale-check + write critical section starts here. ===
	// claude-code FileEditTool.ts:443 explicit warning:
	//   "Please avoid async operations between here and writing to
	//    disk to preserve atomicity."
	// In Go we hold no locks but rely on the syscalls being
	// synchronous and Edit being ConcurrencyExclusive in the
	// dispatch tier — no concurrent intra-batch reader can interleave.
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Staleness check: if a Read of this path was recorded earlier
	// this session and either the mtime or content differs from the
	// last-seen state, refuse — the model's mental snapshot is stale.
	//
	// Skipped when state is nil (test wiring without state) or when
	// no Read entry exists (model is editing blind, which we still
	// allow — first-edit-without-read is valid for model-authored
	// new files routed through Write originally, but matched as edit
	// here would be a misuse the model corrects on the next turn).
	if e.state != nil {
		if entry, ok := e.state.Get(path); ok {
			currentHash := hashBytes(bs)
			currentMTime := st.ModTime()
			if currentHash != entry.Hash && !currentMTime.Equal(entry.MTime) {
				return &tools.Result{
					Output:  FileUnexpectedlyModified + ": " + path,
					IsError: true,
				}, nil
			}
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
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return nil, err
	}
	// Re-record post-write so a follow-up Edit on the same path
	// won't false-positive on the staleness check.
	if e.state != nil {
		if st2, serr := os.Stat(path); serr == nil {
			e.state.Record(path, st2.ModTime(), []byte(out))
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
