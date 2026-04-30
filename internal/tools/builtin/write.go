package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Write struct{ gate *permission.Gate }

func (Write) Name() string { return "Write" }
func (Write) Description() string {
	return "Write content to a file (creates or overwrites). Always use absolute paths. Caller MUST have read the file first if it already exists."
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
func (w Write) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := w.gate.Check(context.Background(), "Write", strFromAny(in["path"]))
	return mapDecision(d), src
}

func (Write) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	path, _ := in["path"].(string)
	content, _ := in["content"].(string)
	if path == "" {
		return nil, errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return &tools.Result{Output: "wrote " + path}, nil
}
