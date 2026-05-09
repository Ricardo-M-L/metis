package builtin

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// heavySkipDirs are directories Glob refuses to descend into. Beyond
// the original .git / node_modules / vendor / .venv, the 2026-05-05
// 41-second incident showed Glob walking the user's entire home tree
// (Library, Caches, Documents, ...) when the model launched it from
// $HOME — adding the macOS-system + heavy-cache + per-user-tooling
// dirs cuts the cold-start path enumeration from minutes to seconds.
//
// Bash's `find -prune` rule of thumb: skip dirs whose contents are
// uninteresting to a code-search but expensive to enumerate.
var heavySkipDirs = map[string]struct{}{
	// VCS / package metadata
	".git": {}, ".svn": {}, ".hg": {}, ".jj": {}, ".sl": {},
	"node_modules": {}, "vendor": {}, ".venv": {}, "venv": {},
	"target":      {}, // Rust / Java
	".gradle":     {},
	".cargo":      {},
	".rustup":     {},
	".cache":      {},
	".m2":         {}, // Maven
	".npm":        {},
	".yarn":       {},
	".pnpm-store": {},
	".bun":        {},

	// macOS / Apple ecosystem
	"Library":             {},
	"Applications":        {},
	".Trash":              {},
	".CFUserTextEncoding": {},
	".docker":             {},
	".vscode-server":      {},
	".cursor":             {},

	// Build / runtime artifacts
	"dist":          {},
	"build":         {},
	"out":           {},
	"target_debug":  {},
	"__pycache__":   {},
	".pytest_cache": {},
	".mypy_cache":   {},
	".ruff_cache":   {},
	".tox":          {},
	"coverage":      {},

	// Heavy generated / vendored content
	"DerivedData": {}, // Xcode
}

// defaultGlobMaxDepth caps walk depth when the model didn't pass
// max_depth and root is one of the user's home directories. Without
// this cap, the model launching glob "**/*.toml" at $HOME walks
// every cached file from every tool ever run — minutes of latency
// for results the model never reads.
//
// 8 deep is enough for any reasonable code repo; 32 is the looser
// cap for arbitrary roots (still finite enough to abort cleanly on
// pathological symlink loops).
const (
	defaultGlobMaxDepthHome  = 8
	defaultGlobMaxDepthOther = 32
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
			"max_depth": map[string]any{
				"type":        "integer",
				"description": "Max directory depth to descend (relative to root). Defaults to 8 when root is the user's home dir, 32 elsewhere. Pass 0 for unlimited (slow on huge trees).",
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

	// Resolve absolute root for depth calculation. WalkDir uses the
	// caller's relative path internally, but depth is measured against
	// the effective starting directory, so we compute prefixLen on the
	// cleaned absolute path.
	rootAbs, _ := filepath.Abs(root)
	rootClean := filepath.Clean(rootAbs)

	// Default max_depth: 8 when starting at the user's home dir
	// (covers any reasonable repo without enumerating Library/),
	// 32 when starting elsewhere. Pass max_depth=0 to disable the cap.
	home, _ := os.UserHomeDir()
	defaultDepth := defaultGlobMaxDepthOther
	if home != "" && rootClean == filepath.Clean(home) {
		defaultDepth = defaultGlobMaxDepthHome
	}
	maxDepth := -1 // -1 = use default; 0 = unlimited; >0 = explicit cap
	if v, ok := in["max_depth"]; ok {
		if n, nok := numberInt(v); nok {
			maxDepth = n
		}
	}
	if maxDepth < 0 {
		maxDepth = defaultDepth
	}

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
			// Heavy skip-list: VCS internals, language package caches,
			// macOS Library tree, build outputs. Materially shortens
			// the cold-start glob from $HOME — see heavySkipDirs.
			if _, skip := heavySkipDirs[name]; skip {
				return filepath.SkipDir
			}
			// Hidden dirs other than the explicit ones above are
			// kept walkable (.config, .vscode-config, ...) so user
			// search isn't surprised by missing dotfiles.
			//
			// Depth cap (when set): count separators between the
			// root and the current path. The root itself is depth 0.
			if maxDepth > 0 {
				abs, aerr := filepath.Abs(path)
				if aerr == nil {
					rel, rerr := filepath.Rel(rootClean, filepath.Clean(abs))
					if rerr == nil {
						depth := 0
						if rel != "." {
							depth = strings.Count(rel, string(filepath.Separator)) + 1
						}
						if depth >= maxDepth {
							return filepath.SkipDir
						}
					}
				}
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
