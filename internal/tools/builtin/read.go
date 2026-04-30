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

type Read struct{ gate *permission.Gate }

func (Read) Name() string { return "Read" }
func (Read) Description() string {
	return "Read a file from the local filesystem. Returns lines with 1-indexed line numbers."
}
func (Read) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path"},
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "absolute path to read"},
			"offset": map[string]any{"type": "integer", "description": "1-indexed line to start at"},
			"limit":  map[string]any{"type": "integer", "description": "max lines to return"},
		},
	}
}
func (Read) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (r Read) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := r.gate.Check(context.Background(), "Read", strFromAny(in["path"]))
	return mapDecision(d), src
}

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
	if emitted == 0 {
		return &tools.Result{Output: "(file is empty or offset past end)"}, nil
	}
	return &tools.Result{Output: b.String()}, nil
}
