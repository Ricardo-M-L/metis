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

type Edit struct{ gate *permission.Gate }

func (Edit) Name() string { return "Edit" }
func (Edit) Description() string {
	return "Replace `old` with `new` in `path`. The match must be unique unless `all` is true. Useful for surgical edits."
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
func (e Edit) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := e.gate.Check(context.Background(), "Edit", strFromAny(in["path"]))
	return mapDecision(d), src
}

func (Edit) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
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
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
	return &tools.Result{Output: fmt.Sprintf("edited %s (%d replacements)", path, ifelse(all, count, 1))}, nil
}

func ifelse[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
