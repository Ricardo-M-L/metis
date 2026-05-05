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
func (Write) Description() string {
	return "Write content to a file (creates or overwrites). Always use absolute paths. Caller MUST have read the file first if it already exists, otherwise Write refuses on existing files."
}
func (Write) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path", "content"},
		"properties": map[string]any{
			"path":    map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
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
