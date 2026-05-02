package builtin

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Glob struct{ gate *permission.Gate }

func (Glob) Name() string { return "Glob" }
func (Glob) Description() string {
	return "Find files matching a doublestar glob pattern. Examples: \"**/*.go\" (all .go files recursively), \"src/**/*.ts\" (TS files under src), \"*.md\" (top-level markdown). Returns paths sorted by modification time, newest first."
}
func (Glob) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"pattern"},
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Doublestar glob, e.g. \"**/*.go\" or \"src/**/*.ts\". Use \"**/*\" to match every file under root.",
			},
			"root": map[string]any{
				"type":        "string",
				"description": "Directory to search from. Defaults to cwd.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of paths to return. Defaults to 500.",
			},
		},
	}
}
func (Glob) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (g Glob) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := g.gate.Check(context.Background(), "Glob", strFromAny(in["pattern"]))
	return mapDecision(d), src
}

func (Glob) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	pattern, _ := in["pattern"].(string)
	if pattern == "" {
		return nil, errors.New("pattern required")
	}
	root, _ := in["root"].(string)
	if root == "" {
		root = "."
	}
	limit := intArg(in, "limit", 500)

	type hit struct {
		path string
		mod  int64
	}
	var hits []hit

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			// skip the usual heavyweight dirs
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".venv" {
				return filepath.SkipDir
			}
			return nil
		}
		matched, _ := doublestarMatch(pattern, path)
		if !matched {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		hits = append(hits, hit{path: path, mod: info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	var b strings.Builder
	for _, h := range hits {
		b.WriteString(h.path)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return &tools.Result{Output: "(no matches)"}, nil
	}
	return &tools.Result{Output: b.String()}, nil
}

// doublestarMatch is a minimal ** glob: ** matches across path separators,
// * matches a single path segment.
func doublestarMatch(pattern, path string) (bool, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Match(pattern, filepath.Base(path))
	}
	// Build a regex from the pattern.
	re := globToRegex(pattern)
	matched, err := matchRegex(re, path)
	return matched, err
}

func globToRegex(pat string) string {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('$')
	return b.String()
}
