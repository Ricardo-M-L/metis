package builtin

import (
	"context"
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

type Glob struct {
	tools.BaseTool
	gate *permission.Gate
}

func (Glob) Name() string { return "Glob" }
func (Glob) Description() string {
	return `Find files matching a doublestar glob pattern. Examples: "**/*.go" (all .go files recursively), "src/**/*.ts" (TS files under src), "*.md" (top-level markdown). Returns paths sorted by modification time, newest first.

Use Glob for file-name / path patterns. Use Grep when you need to match file CONTENTS instead. Use Bash + find only when you need filters Glob can't express (mtime, size, perms).

## Examples

<example>
context: Find every Go test file in the repo.
assistant: Glob(pattern="**/*_test.go")
<reasoning>
Doublestar recurses through all subdirs. Sorted newest-first so the file you most recently touched appears at the top.
</reasoning>
</example>

<example>
context: Find TypeScript files in a specific subdir of a monorepo.
assistant: Glob(pattern="**/*.ts", root="/repo/packages/web/src")
<reasoning>
Scoping with root is much faster than matching against the whole monorepo.
</reasoning>
</example>

<example>
context: Locate anything that looks like a config file in one shot.
assistant: Glob(pattern="**/{*.toml,*.yaml,*.json,*.ini}")
<reasoning>
Brace expansion catches multiple extensions in one call instead of 4 separate Glob invocations.
</reasoning>
</example>`
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
	// Include the search root in the permission payload. Pattern-only checks
	// let a rule denying a sensitive directory be bypassed simply by moving
	// that directory from the glob pattern into `root`.
	d, src := g.gate.CheckPath(context.Background(), "Glob", searchPermissionInput(in), searchScopePath(in))
	return mapDecision(d), src
}

func (Glob) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	pattern, _ := in["pattern"].(string)
	if pattern == "" {
		// Richer error message — "pattern required" alone was too
		// terse. Image #34 repro (2026-05-21): a sub-agent called
		// Glob with NO pattern (passed `command` field with a shell
		// invocation instead), got "pattern required", and looped.
		// The new message names the expected shape AND points at
		// the right tools for the misuse cases we've seen in the
		// wild (shell commands → Bash, file contents → Read,
		// directory listing → LS).
		hint := globMisuseHint(in)
		return &tools.Result{
			Output: "Glob: `pattern` field is required (e.g. \"**/*.go\" or \"src/**/*.ts\"). " +
				"Glob ONLY finds files by name pattern; it doesn't run shell commands, read file contents, or list directories." +
				hint,
			IsError: true,
		}, nil
	}
	// Shell-command-shaped pattern: a string with shell metachars
	// like `||`, `&&`, `2>`, `/dev/null`, leading `ls `/`find `/`grep `
	// is almost certainly the model trying to use Glob as a Bash
	// substitute. Reject with a Bash redirect BEFORE the doublestar
	// engine produces a "no matches" result (which the model would
	// then misread as "wrong glob; try different syntax" and loop).
	if hint := globShellShapeHint(pattern); hint != "" {
		return &tools.Result{
			Output:  "Glob: pattern looks like a shell command, not a glob. " + hint,
			IsError: true,
		}, nil
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
	//
	// Additional clamp: when rootClean is NOT inside a git work tree,
	// drop to walkBudget()'s tighter cap (4 / 200) — the agent has
	// no signal of project boundary, so don't let the LLM accidentally
	// fan out across unrelated dirs. See scope.go.
	home, _ := os.UserHomeDir()
	defaultDepth := defaultGlobMaxDepthOther
	if home != "" && rootClean == filepath.Clean(home) {
		defaultDepth = defaultGlobMaxDepthHome
	}
	if d, items := walkBudget(rootClean); d > 0 {
		if d < defaultDepth {
			defaultDepth = d
		}
		if items > 0 && items < limit {
			limit = items
		}
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

// globMisuseHint inspects the input bag for fields that suggest the
// model meant a different tool. Triggered when `pattern` is empty:
// if we see a `command` or `cmd` field it's almost certainly a
// model that confused Glob with Bash; a `path` field with a file
// extension points at Read or LS. The hint is appended to the
// "pattern required" error so the model's next turn lands on the
// right tool instead of looping on Glob with a different empty arg.
func globMisuseHint(in map[string]any) string {
	if cmd, _ := in["command"].(string); cmd != "" {
		return "\n\nYou passed a `command` field (\"" + truncForHint(cmd, 80) + "\"). That's the Bash tool's input — call Bash if you want to run a shell command."
	}
	if cmd, _ := in["cmd"].(string); cmd != "" {
		return "\n\nYou passed a `cmd` field. Glob takes `pattern`. Call Bash if you want to run a shell command."
	}
	if p, _ := in["path"].(string); p != "" {
		return "\n\nYou passed a `path` field (\"" + truncForHint(p, 80) + "\"). If it's a directory, use LS. If it's a file, use Read. Glob is for name-pattern searches."
	}
	if q, _ := in["query"].(string); q != "" {
		return "\n\nYou passed a `query` field. For text-in-files search use Grep; for name-pattern search use Glob with `pattern: \"**/*.ext\"`."
	}
	return ""
}

// globShellShapeHint detects a pattern that smells like a shell
// command and returns a Bash-redirect hint. Heuristics chosen to
// catch the most common confusions without false-positiving on a
// valid path-y glob:
//
//   - shell metachars `||` / `&&` / `2>` / `|` / `>` / `<`
//   - device redirects `/dev/null`, `/dev/stdin`, etc.
//   - leading shell tokens `ls ` / `find ` / `grep ` / `cat ` /
//     `echo ` / `cd ` (a token + space is a strong signal — a real
//     glob would rarely start with one of these words followed by
//     space).
//
// Returns "" when the pattern looks like a normal glob.
func globShellShapeHint(p string) string {
	for _, sub := range []string{"||", "&&", "2>", "&>", ">|", "/dev/null", "/dev/stdin", "/dev/stderr"} {
		if strings.Contains(p, sub) {
			return "Found `" + sub + "`. Use Bash(command=\"" + truncForHint(p, 80) + "\") to run shell commands; use LS for directory listings."
		}
	}
	leads := []string{"ls ", "find ", "grep ", "cat ", "echo ", "cd ", "rg ", "head ", "tail ", "awk ", "sed "}
	for _, lead := range leads {
		if strings.HasPrefix(p, lead) {
			return "Pattern starts with `" + strings.TrimSpace(lead) + "`. That's a shell command — use Bash, or pick LS/Read/Grep for the dedicated equivalent."
		}
	}
	return ""
}

// truncForHint shortens a string for embedding in an error message
// so a 500-char shell command doesn't blow the result-row layout.
// Rune-aware to avoid mid-codepoint cuts on CJK paths.
func truncForHint(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes-1]) + "…"
}
