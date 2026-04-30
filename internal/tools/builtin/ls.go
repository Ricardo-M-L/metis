package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type LS struct{ gate *permission.Gate }

func (LS) Name() string { return "LS" }
func (LS) Description() string {
	return "List directory entries. Use absolute path. Skips dot-dirs unless include_hidden=true."
}
func (LS) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path"},
		"properties": map[string]any{
			"path":           map[string]any{"type": "string"},
			"include_hidden": map[string]any{"type": "boolean"},
		},
	}
}
func (LS) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (l LS) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := l.gate.Check(context.Background(), "LS", strFromAny(in["path"]))
	return mapDecision(d), src
}

func (LS) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	path, _ := in["path"].(string)
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("absolute path required")
	}
	hidden, _ := in["include_hidden"].(bool)

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	type row struct{ name, kind string; size int64 }
	var rows []row
	for _, e := range entries {
		if !hidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		}
		rows = append(rows, row{name: e.Name(), kind: kind, size: info.Size()})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].kind != rows[j].kind {
			return rows[i].kind == "dir"
		}
		return rows[i].name < rows[j].name
	})
	var b strings.Builder
	for _, r := range rows {
		if r.kind == "dir" {
			fmt.Fprintf(&b, "%s/\n", r.name)
		} else {
			fmt.Fprintf(&b, "%s  (%s)\n", r.name, bytesString(int(r.size)))
		}
	}
	if b.Len() == 0 {
		return &tools.Result{Output: "(empty)"}, nil
	}
	return &tools.Result{Output: b.String()}, nil
}
