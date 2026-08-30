package builtin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Grep struct {
	tools.BaseTool
	gate       *permission.Gate
	sandbox    *sandbox.Manager
	authorizer *invocationAuthorizer[grepPathBinding]
	// Test seams used to make path swaps deterministic.
	afterRootOpen func()
	afterFileOpen func(string)
}

type grepPathBinding struct {
	target      approvedExistingPath
	inputDigest string
}

// NewGrep preserves the legacy unsandboxed construction path used by tests
// and embedders. Runtime registration uses NewGrepWithSandbox.
func NewGrep(gate *permission.Gate) Grep {
	return Grep{gate: gate, authorizer: newInvocationAuthorizer[grepPathBinding]()}
}

func NewGrepWithSandbox(gate *permission.Gate, manager *sandbox.Manager) Grep {
	return Grep{gate: gate, sandbox: manager, authorizer: newInvocationAuthorizer[grepPathBinding]()}
}

func (g Grep) WithSandbox(manager *sandbox.Manager) Grep {
	g.sandbox = manager
	return g
}

func (g Grep) SandboxManager() *sandbox.Manager { return g.sandbox }

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

func (g Grep) PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error {
	root := resolvePathAgainstAgentCWD(ctx, searchScopePath(in))
	target, err := prepareExistingPath(root, true)
	if err != nil {
		return err
	}
	if !target.matchesCurrent(target.targetInfo) {
		return errors.New("Grep target changed during permission preparation")
	}
	g.authorizer.record(ctx, grepPathBinding{target: target, inputDigest: grepApprovalKey(in, root)})
	return nil
}

func (g Grep) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	root := resolvePathAgainstAgentCWD(ctx, searchScopePath(in))
	d, src := permission.DecisionAllow, ""
	if g.gate != nil {
		d, src = g.gate.CheckPath(ctx, "Grep", searchPermissionInput(in), root)
	}
	if d != permission.DecisionDeny {
		if err := g.PrepareAuthorizedInvocation(ctx, in); err != nil {
			return tools.PermissionDeny, security.RedactSubprocessText(err.Error())
		}
	}
	return mapDecision(d), src
}

func grepApprovalKey(in map[string]any, effectiveRoot string) string {
	// Keep abandoned approval records bounded even when an untrusted pattern or
	// glob is huge. Length-prefix strings so distinct tuples cannot collide via
	// separators, and include dynamic types for numeric input fidelity.
	h := sha256.New()
	writeString := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:", len(s))
		_, _ = h.Write([]byte(s))
	}
	writeString(effectiveRoot)
	writeString(searchScopePath(in))
	writeString(strFromAny(in["pattern"]))
	writeString(strFromAny(in["glob"]))
	_, _ = fmt.Fprintf(h, "\x00%T:%v\x00%T:%v", in["max"], in["max"], in["offset"], in["offset"])
	return fmt.Sprintf("%x", h.Sum(nil))
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

func (g Grep) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
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
	logicalRoot := root
	root = resolvePathAgainstAgentCWD(ctx, root)
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
	binding, hasInvocationID, foundBinding := g.authorizer.consume(ctx)
	if hasInvocationID && !foundBinding {
		if _, prepErr := prepareExistingPath(root, true); prepErr != nil {
			return &tools.Result{Output: "Grep denied: " + security.RedactSubprocessText(prepErr.Error()), IsError: true}, nil
		}
		return &tools.Result{Output: "Grep denied: permission binding missing for this invocation", IsError: true}, nil
	}
	if !hasInvocationID {
		target, prepErr := prepareExistingPath(root, true)
		if prepErr != nil {
			return &tools.Result{Output: "Grep denied: " + security.RedactSubprocessText(prepErr.Error()), IsError: true}, nil
		}
		binding = grepPathBinding{target: target, inputDigest: grepApprovalKey(in, root)}
		if g.gate != nil {
			decision, source := g.gate.CheckPath(ctx, "Grep", searchPermissionInput(in), root)
			if decision != permission.DecisionAllow {
				return &tools.Result{Output: "Grep denied: " + security.RedactSubprocessText(source), IsError: true}, nil
			}
		}
	}
	if binding.inputDigest != grepApprovalKey(in, root) {
		return &tools.Result{Output: "Grep denied: invocation input changed after permission check", IsError: true}, nil
	}
	rootHandle, resolvedRoot, err := openPinnedReadRoot(root, g.afterRootOpen)
	if err != nil {
		return &tools.Result{Output: "Grep denied: " + security.RedactSubprocessText(err.Error()), IsError: true}, nil
	}
	defer rootHandle.Close()
	openedRootInfo, statErr := rootHandle.Stat(".")
	if statErr != nil || resolvedRoot != binding.target.resolvedPath || !os.SameFile(binding.target.targetInfo, openedRootInfo) {
		return &tools.Result{Output: "Grep denied: search root changed after permission check", IsError: true}, nil
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
	rootClean := resolvedRoot
	walkDepthCap, walkItemCap := walkBudgetWithSandbox(ctx, rootClean, g.sandbox)

	deadline := time.Now().Add(grepWalkTimeout)
	filesScanned := 0
	budgetHit := false
	credentialFilesSkipped := 0
	var pathChangedErr error

	err = fs.WalkDir(rootHandle.FS(), ".", func(relPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		path := logicalRoot
		actualPath := root
		if relPath != "." {
			path = filepath.Join(logicalRoot, filepath.FromSlash(relPath))
			actualPath = filepath.Join(root, filepath.FromSlash(relPath))
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
				if relPath != "." {
					depth := strings.Count(filepath.FromSlash(relPath), string(filepath.Separator)) + 1
					if depth >= walkDepthCap {
						return filepath.SkipDir
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
		// A broad project Grep must not accidentally surface .env/package
		// registry/cloud credentials merely because the caller named the parent
		// directory rather than the sensitive file itself.
		if permission.IsSecretReadPath(actualPath) {
			credentialFilesSkipped++
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
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() || info.Size() > grepMaxFileSize {
			return nil
		}
		f, err := rootHandle.Open(relPath)
		if err != nil {
			return nil
		}
		if g.afterFileOpen != nil {
			g.afterFileOpen(actualPath)
		}
		openedInfo, openErr := f.Stat()
		afterInfo, afterErr := rootHandle.Lstat(relPath)
		if openErr != nil || afterErr != nil || !openedInfo.Mode().IsRegular() ||
			!os.SameFile(info, openedInfo) || !os.SameFile(openedInfo, afterInfo) {
			_ = f.Close()
			pathChangedErr = fmt.Errorf("search file changed while opening: %s", path)
			return filepath.SkipAll
		}
		// Read the already size-bounded, pinned descriptor once. The source-aware
		// redactor below uses the complete snapshot to retain credential context
		// that a line-only Grep result would otherwise lose.
		data, readErr := io.ReadAll(io.LimitReader(f, grepMaxFileSize+1))
		openedAfter, statErr := f.Stat()
		_ = f.Close()
		if readErr != nil {
			return readErr
		}
		if statErr != nil || !os.SameFile(openedInfo, openedAfter) || openedInfo.Size() != openedAfter.Size() ||
			!openedInfo.ModTime().Equal(openedAfter.ModTime()) {
			pathChangedErr = fmt.Errorf("search file changed while reading: %s", path)
			return filepath.SkipAll
		}
		if len(data) > grepMaxFileSize {
			return nil
		}
		fileRedactor := security.NewFileCredentialRedactor(data)

		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		lineno := 0
		sourceLineStart := 0
		for sc.Scan() {
			lineno++
			line := sc.Text()
			currentLineStart := sourceLineStart
			sourceLineStart += len(sc.Bytes())
			if sourceLineStart < len(data) && data[sourceLineStart] == '\r' {
				sourceLineStart++
			}
			if sourceLineStart < len(data) && data[sourceLineStart] == '\n' {
				sourceLineStart++
			}
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
			line = fileRedactor.RedactLineAt(currentLineStart, line)
			fmt.Fprintf(&b, "%s:%d:%s\n", path, lineno, security.RedactSubprocessText(line))
			hits++
		}
		return sc.Err()
	})
	if pathChangedErr != nil {
		return &tools.Result{Output: "Grep denied: " + security.RedactSubprocessText(pathChangedErr.Error()), IsError: true}, nil
	}
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, err
	}
	if hits == 0 && skipped == 0 {
		if credentialFilesSkipped > 0 {
			return &tools.Result{Output: fmt.Sprintf("(no matches; %d credential file(s) skipped)", credentialFilesSkipped)}, nil
		}
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
	if credentialFilesSkipped > 0 {
		fmt.Fprintf(&b, "\n[%d credential file(s) skipped]\n", credentialFilesSkipped)
	}
	return &tools.Result{Output: security.RedactSubprocessText(b.String())}, nil
}

func openPinnedReadRoot(path string, afterOpen func()) (*os.Root, string, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, "", err
	}
	lexicalBefore, err := os.Lstat(absPath)
	if err != nil {
		return nil, "", err
	}
	resolvedBefore, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, "", err
	}
	targetBefore, err := os.Stat(absPath)
	if err != nil {
		return nil, "", err
	}
	if !targetBefore.IsDir() {
		return nil, "", fmt.Errorf("search root is not a directory: %s", path)
	}
	root, err := os.OpenRoot(absPath)
	if err != nil {
		return nil, "", err
	}
	if afterOpen != nil {
		afterOpen()
	}
	openedInfo, openErr := root.Stat(".")
	lexicalAfter, lexicalErr := os.Lstat(absPath)
	resolvedAfter, resolvedErr := filepath.EvalSymlinks(absPath)
	targetAfter, targetErr := os.Stat(absPath)
	if openErr != nil || lexicalErr != nil || resolvedErr != nil || targetErr != nil ||
		!openedInfo.IsDir() || !os.SameFile(targetBefore, openedInfo) ||
		!os.SameFile(lexicalBefore, lexicalAfter) || !os.SameFile(openedInfo, targetAfter) ||
		filepath.Clean(resolvedBefore) != filepath.Clean(resolvedAfter) {
		_ = root.Close()
		return nil, "", errors.Join(openErr, lexicalErr, resolvedErr, targetErr,
			fmt.Errorf("search root changed while opening: %s", path))
	}
	return root, filepath.Clean(resolvedAfter), nil
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
