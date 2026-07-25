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
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Grep struct {
	tools.BaseTool
	gate *permission.Gate
}

func (Grep) Name() string { return "Grep" }
func (Grep) Description() string {
	return `Search file contents with a Go regex. Returns matching lines with file:line prefix. Skips .git / node_modules / vendor by default.

Use Grep for content matching. Use Glob when you only need filenames (no content). Use Bash + grep -r only when you need flags Grep can't express (-A/-B context, -P perl regex).

## Examples

<example>
context: Find every place that calls LoadConfig.
assistant: Grep(pattern="LoadConfig\\(")
<reasoning>
Function-call regex (note the escaped paren). Returns file:line:match across the whole repo.
</reasoning>
</example>

<example>
context: Find TODO comments only in Python files.
assistant: Grep(pattern="TODO|FIXME", glob="**/*.py")
<reasoning>
glob filter avoids scanning JS/Go/Rust files. Alternation handles both keywords in one call.
</reasoning>
</example>

<example>
context: Searching for a symbol that might have hundreds of hits — paginate.
assistant: Grep(pattern="UserID", max=50)
<reasoning>
Default cap is 250. When you want a quick first batch (e.g. to gauge scope), pass a smaller max — and pair with offset on the next call to walk the rest.
</reasoning>
</example>`
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
	d, src := g.gate.CheckPath(context.Background(), "Grep", searchPermissionInput(in), searchScopePath(in))
	return mapDecision(d), src
}

func searchScopePath(in map[string]any) string {
	root := strFromAny(in["root"])
	if root == "" {
		return "."
	}
	return root
}

// searchPermissionInput keeps both the directory scope and query visible to
// substring rules and secret-path checks. Newline is deliberately simple and
// unambiguous while preserving the existing pattern-only payload when root is
// omitted (so saved rules continue to match).
func searchPermissionInput(in map[string]any) string {
	pattern := strFromAny(in["pattern"])
	root := strFromAny(in["root"])
	if root == "" {
		return pattern
	}
	// Root is a directory. Preserve an explicit separator so a credential
	// directory such as `~/.ssh` matches the secret fragment `.ssh/` even
	// when the caller omitted the trailing slash.
	return strings.TrimRight(root, "/") + "/\n" + pattern
}

// DefaultGrepLimit is the default cap when callers don't pass `max`.
// Mirrors claude-code GrepTool.ts:108 DEFAULT_HEAD_LIMIT = 250.
// Generous for exploratory searches, prevents context bloat from
// minified or generated code.
const DefaultGrepLimit = 250

// grepMaxFileSize caps per-file scanning: line-scanning a multi-GB log /
// database / binary is one way a stray Grep used to hang for hours.
const grepMaxFileSize = 5 << 20 // 5 MiB

// grepWalkTimeout is a wall-clock safety budget on the whole walk, independent
// of (and in addition to) the caller's context. A Grep launched from $HOME
// once ran for 5+ hours enumerating ~/Library; this bounds the worst case even
// when the context has no deadline.
const grepWalkTimeout = 20 * time.Second

func (Grep) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if ctx == nil {
		ctx = context.Background() // defensive: ctx.Done() below must not panic
	}
	patStr, _ := in["pattern"].(string)
	if patStr == "" {
		// 2026-05-22: rich error + redirect hint, same approach as
		// Glob/Read/LS. Common confusions: model passed `query` /
		// `search` thinking it was the field name, or passed
		// `command`/`cmd` (wanted Bash), or `path` only (wanted to
		// see directory listing, should use LS).
		hint := ""
		if q, _ := in["query"].(string); q != "" {
			hint = "\n\nYou passed `query`. The argument name is `pattern`. Try Grep({pattern: \"" + q + "\"})."
		} else if q, _ := in["search"].(string); q != "" {
			hint = "\n\nYou passed `search`. The argument name is `pattern`. Try Grep({pattern: \"" + q + "\"})."
		} else if c, _ := in["command"].(string); c != "" {
			hint = "\n\nYou passed `command`. That's the Bash tool's input — call Bash if you want to run `grep` directly."
		}
		return &tools.Result{
			Output:  "Grep: `pattern` is required (a regex like \"func.*Error\" or a literal like \"TODO\"). Grep searches text WITHIN files; for filename patterns use Glob, for listing dir use LS." + hint,
			IsError: true,
		}, nil
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

	// Out-of-worktree clamp: same rationale as Glob (scope.go). When the cwd
	// is not inside a git work tree, cap BOTH walk depth and the number of
	// files scanned so a stray Grep("foo") from $HOME doesn't enumerate every
	// cached file under ~/Library. (Pre-2026-06: only depth was honored — the
	// item cap was discarded — so a low-hit search from $HOME walked the whole
	// home tree for hours.)
	rootAbs, _ := filepath.Abs(root)
	rootClean := filepath.Clean(rootAbs)
	walkDepthCap, walkItemCap := walkBudget(rootClean)

	deadline := time.Now().Add(grepWalkTimeout)
	filesScanned := 0
	budgetHit := false

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Cancellable + time-bounded. The callback runs per entry, so this is
		// the natural place to honor cancellation and the wall-clock budget —
		// previously Execute ignored ctx entirely, so an in-flight walk could
		// not be stopped.
		select {
		case <-ctx.Done():
			budgetHit = true
			return filepath.SkipAll
		default:
		}
		if time.Now().After(deadline) {
			budgetHit = true
			return filepath.SkipAll
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
		// Only search regular files. Skips symlinks, FIFOs, sockets, and
		// device files — os.Open on a FIFO blocks forever waiting for a
		// writer, which is the other way Grep used to hang.
		if !d.Type().IsRegular() {
			return nil
		}
		if globPat != "" {
			ok, _ := doublestarMatch(globPat, path)
			if !ok {
				return nil
			}
		}
		// Out-of-worktree file budget: stop after touching walkItemCap files
		// so a search that matches little can't enumerate an entire home dir.
		if walkItemCap > 0 {
			filesScanned++
			if filesScanned > walkItemCap {
				budgetHit = true
				return filepath.SkipAll
			}
		}
		// Skip oversized files (logs / databases / binaries). Line-scanning a
		// multi-GB file is slow and pollutes results with binary noise.
		if info, ierr := d.Info(); ierr == nil && info.Size() > grepMaxFileSize {
			return nil
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
		if budgetHit {
			return &tools.Result{Output: "(no matches; search stopped early by the walk budget — pass a narrower `root` inside your project, this looks like an out-of-worktree / $HOME search)"}, nil
		}
		return &tools.Result{Output: "(no matches)"}, nil
	}
	// Pagination footer: only emitted when truncation actually happened.
	// Mirrors GrepTool.ts:121-127 — silence in the common "we have all
	// the results" case avoids polluting context with a useless line.
	if limitHit {
		fmt.Fprintf(&b, "\n[truncated at %d matches; pass offset=%d for the next page]\n", max, offset+max)
	} else if budgetHit {
		fmt.Fprintf(&b, "\n[partial results: walk budget reached (%d files / %s) — narrow `root` to your project for a complete search]\n", walkItemCap, grepWalkTimeout)
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
