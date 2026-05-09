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
			"max":     map[string]any{"type": "integer", "description": "max matches to return (default 250). Pass 0 to unlimit."},
			"offset":  map[string]any{"type": "integer", "description": "skip the first N matches. Pair with `max` for pagination."},
		},
	}
}
func (Grep) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

// IsReadOnly: Grep never writes. Tags the tool_result for Snip.
func (Grep) IsReadOnly(map[string]any) bool { return true }

func (g Grep) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := g.gate.Check(context.Background(), "Grep", strFromAny(in["pattern"]))
	return mapDecision(d), src
}

// DefaultGrepLimit is the default cap when callers don't pass `max`.
// Mirrors claude-code GrepTool.ts:108 DEFAULT_HEAD_LIMIT = 250.
// Generous for exploratory searches, prevents context bloat from
// minified or generated code.
const DefaultGrepLimit = 250

func (Grep) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	patStr, _ := in["pattern"].(string)
	if patStr == "" {
		return nil, errors.New("pattern required")
	}
	root, _ := in["root"].(string)
	if root == "" {
		root = "."
	}
	// max=0 → unlimited (escape hatch). Unset → DefaultGrepLimit.
	maxRaw, hasMax := in["max"]
	max := DefaultGrepLimit
	if hasMax {
		if n, ok := numberInt(maxRaw); ok {
			max = n
		}
	}
	offset := intArg(in, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	globPat, _ := in["glob"].(string)

	re, err := regexp.Compile(patStr)
	if err != nil {
		return nil, fmt.Errorf("bad regex: %w", err)
	}

	var b strings.Builder
	skipped := 0   // matches skipped to honour `offset`
	hits := 0      // matches actually rendered
	totalSeen := 0 // every match (skipped + rendered) — used to detect truncation
	limitHit := false

	// Out-of-worktree depth clamp: same rationale as Glob (scope.go).
	// When the cwd is not inside a git work tree, cap walk depth so a
	// stray Grep("foo") from $HOME doesn't enumerate every cached file.
	rootAbs, _ := filepath.Abs(root)
	rootClean := filepath.Clean(rootAbs)
	walkDepthCap, _ := walkBudget(rootClean)

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if n == ".git" || n == "node_modules" || n == "vendor" || n == ".venv" {
				return filepath.SkipDir
			}
			if walkDepthCap > 0 {
				abs, aerr := filepath.Abs(path)
				if aerr == nil {
					rel, rerr := filepath.Rel(rootClean, filepath.Clean(abs))
					if rerr == nil && rel != "." {
						depth := strings.Count(rel, string(filepath.Separator)) + 1
						if depth >= walkDepthCap {
							return filepath.SkipDir
						}
					}
				}
			}
			return nil
		}
		if globPat != "" {
			ok, _ := doublestarMatch(globPat, path)
			if !ok {
				return nil
			}
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
			totalSeen++
			if skipped < offset {
				skipped++
				continue
			}
			if max > 0 && hits >= max {
				limitHit = true
				return filepath.SkipAll
			}
			fmt.Fprintf(&b, "%s:%d:%s\n", path, lineno, line)
			hits++
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, err
	}
	if hits == 0 && skipped == 0 {
		return &tools.Result{Output: "(no matches)"}, nil
	}
	// Pagination footer: only emitted when truncation actually happened.
	// Mirrors GrepTool.ts:121-127 — silence in the common "we have all
	// the results" case avoids polluting context with a useless line.
	if limitHit {
		fmt.Fprintf(&b, "\n[truncated at %d matches; pass offset=%d for the next page]\n", max, offset+max)
	} else if offset > 0 && hits == 0 {
		fmt.Fprintf(&b, "\n[offset %d past end of %d total matches]\n", offset, totalSeen)
	}
	return &tools.Result{Output: b.String()}, nil
}

// numberInt accepts both float64 (JSON default) and int representations.
func numberInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
