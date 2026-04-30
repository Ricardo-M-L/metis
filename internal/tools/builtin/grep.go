package builtin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Grep struct{ gate *permission.Gate }

func (Grep) Name() string { return "Grep" }
func (Grep) Description() string {
	return "Search file contents with a Go regex. Returns matching lines with file:line prefix. Skips .git/node_modules/vendor."
}
func (Grep) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"pattern"},
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string"},
			"root":    map[string]any{"type": "string"},
			"glob":    map[string]any{"type": "string", "description": "filter file paths with this glob"},
			"max":     map[string]any{"type": "integer", "description": "max matches to return"},
		},
	}
}
func (Grep) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (g Grep) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := g.gate.Check(context.Background(), "Grep", strFromAny(in["pattern"]))
	return mapDecision(d), src
}

func (Grep) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	patStr, _ := in["pattern"].(string)
	if patStr == "" {
		return nil, errors.New("pattern required")
	}
	root, _ := in["root"].(string)
	if root == "" {
		root = "."
	}
	max := intArg(in, "max", 200)
	globPat, _ := in["glob"].(string)

	re, err := regexp.Compile(patStr)
	if err != nil {
		return nil, fmt.Errorf("bad regex: %w", err)
	}

	var b strings.Builder
	hits := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if n == ".git" || n == "node_modules" || n == "vendor" || n == ".venv" {
				return filepath.SkipDir
			}
			return nil
		}
		if globPat != "" {
			ok, _ := doublestarMatch(globPat, path)
			if !ok {
				return nil
			}
		}
		if hits >= max {
			return filepath.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		lineno := 0
		for sc.Scan() {
			lineno++
			line := sc.Text()
			if !re.MatchString(line) {
				continue
			}
			fmt.Fprintf(&b, "%s:%d:%s\n", path, lineno, line)
			hits++
			if hits >= max {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, err
	}
	if hits == 0 {
		return &tools.Result{Output: "(no matches)"}, nil
	}
	return &tools.Result{Output: b.String()}, nil
}
