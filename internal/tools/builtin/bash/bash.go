package bash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/shellguard"
	"github.com/Ricardo-M-L/metis/internal/spill"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// AutoBackgroundThreshold — wall-clock duration after which a still-
// running foreground Bash gets promoted to a background job. The
// foreground turn unblocks; the command keeps executing and the model
// can poll via JobOutput / get a job_notification when it finishes.
//
// Mirrors claude-code's ASSISTANT_BLOCKING_BUDGET_MS (15s on their
// side). 60s on metis is the user-chosen middle ground: long enough
// that npm-install / go-test sized commands still complete in the
// foreground, short enough that runaway loops don't lock the agent.
var AutoBackgroundThreshold = 60 * time.Second

type Bash struct {
	tools.BaseTool
	gate       *permission.Gate
	settings   config.ToolBashSettings
	classifier *BashClassifier
	sandbox    *sandbox.Manager

	// sandboxInitErr preserves constructor failures for the legacy New API,
	// whose value-returning signature cannot surface an error. Execute checks
	// it before spawning anything, so an invalid/enabled-but-unavailable
	// sandbox always fails closed.
	sandboxInitErr error

	// Jobs is the process-wide background job pool. nil disables the
	// auto-background path entirely (foreground commands still run,
	// they just hit the existing timeout instead of being adopted).
	// Populated by RegisterWithJobs from runtime/agent_loop.go.
	Jobs *jobs.Registry
}

func (b *Bash) classifierFor() *BashClassifier {
	if b.classifier == nil {
		b.classifier = NewBashClassifier()
	}
	return b.classifier
}

func (Bash) Name() string { return "Bash" }

// ShortDescription is the curated 1-2 sentence form shipped to
// sub-agents and METIS_SIMPLE boots in lieu of the full multi-section
// Description(). Hand-tuned to fit ~250 chars while still naming the
// two biggest tool-selection footguns (cat→Read, find→Glob) so the
// model gets at least one tool-redirect hint even when the full
// `# Tool selection` table from base.md is skipped.
func (Bash) ShortDescription() string {
	return `Execute a shell command. stdout+stderr merge, truncated at a byte cap; cwd persists across calls in a turn, env vars do NOT. Prefer dedicated tools where possible (Read NOT cat, Glob NOT find, Grep NOT grep -r, Edit NOT sed). Pass run_in_background=true for dev servers, watchers, long builds.`
}

func (Bash) Description() string {
	return `Execute a shell command in the user's environment. stdout+stderr merge into one stream, truncated at a byte cap. cwd persists between calls in the same turn; shell state (env vars, aliases) does NOT — use absolute paths and re-export vars if needed.

Use Bash for:
  - Operations no other tool covers: git (status/diff/log/commit), package managers (go, npm, pip, cargo), test runners, build commands, system queries (uname, df, ps), curl one-offs.
  - Chained logic where a dedicated tool would need multiple round-trips (e.g. "run tests, then if green, commit").

Do NOT use Bash for these — use the dedicated tool, which gives the user a cleaner audit trail and better truncation:
  - Reading files       → use Read (not cat/head/tail/less)
  - Listing directories → use LS (not ls -la). LS gives structured output and rejects file paths cleanly.
  - Editing files       → use Edit (not sed -i / awk -i / ed)
  - Creating files      → use Write (not 'echo > foo' / 'cat <<EOF')
  - Finding files       → use Glob (not find -name)
  - Searching text      → use Grep (not grep -r / rg)
  - Talking to the user → just output text (not echo / printf)

When issuing multiple commands:
  - If the commands are independent and can run in parallel, make multiple Bash tool calls in a single message. Example: "git status" and "git diff" → ONE message with TWO Bash tool_use blocks.
  - If the commands depend on each other and must run sequentially, use a SINGLE Bash call with '&&' to chain them — not N separate Bash calls.
  - Use ';' only when you need to run commands sequentially but DON'T care if earlier commands fail.
  - DO NOT use newlines to separate commands (newlines are ok inside quoted strings).

Path + cwd discipline:
  - Quote paths with spaces: cd "/Users/x/My Folder", NOT cd /Users/x/My Folder.
  - Prefer absolute paths over 'cd' so the cwd stays predictable across calls. Use 'cd' only when the user explicitly asks for it or when an external tool (a build script, npm) requires being run from a specific directory.
  - Never prepend 'cd <current-dir>' to a git command — git already operates on the current working tree, and the combo triggers a permission prompt.
  - Before creating new directories or files, first run 'ls' (or just check) to verify the parent directory exists and is the correct location.

Safety:
  - Never pass --no-verify, --no-gpg-sign, --force-with-lease without explicit user consent; never 'git push --force' to main/master.
  - Never 'rm -rf' or pipe to /dev/sd*; never run a command whose effect you can't reverse without asking first.

Long-running commands: anything that may exceed the timeout (dev server, file watcher, long build, log tail) MUST set run_in_background=true. You'll get a job_id back instantly and can poll via Output or stop via Kill. Note: 'sleep N' is detected and auto-rejected from background mode — pick a real command.

Always pass description: a 5-10 word phrase like "run tests" or "git status before commit". It's shown in the audit trail and helps the user see why each shell call exists.`
}
func (Bash) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"command", "description"},
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command. Quote any path containing spaces. Use absolute paths since shell env does not persist across calls.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "5-10 word summary in active voice, e.g. 'Run gofmt on changed Go files'. NOT 'Running...' or 'This will...'. Required.",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "Override the default timeout (ms). Max 600000 (10 min). For anything longer, use run_in_background.",
			},
			"run_in_background": map[string]any{
				"type":        "boolean",
				"description": "True for commands that don't terminate quickly: dev servers, file watchers, long builds, log tails. Returns job_id immediately; read output with Output, terminate with Kill. 'sleep N' is auto-rejected — pick a real command.",
			},
		},
	}
}

// Concurrency for Bash is input-dependent — claude-code's pattern.
// Read-only commands (`ls`, `cat`, `grep`, `git status`, ...) declare
// Safe so they can fan out alongside Read/Grep in the parallel batch;
// anything that mutates state stays Exclusive. The classifier shells
// out to the same shell-quote parser used by the permission gate so
// the safe-list matches what the user already approved at install
// time.
//
// Failing closed: any parse error or unknown command keyword maps
// back to Exclusive — better to serialize than to corrupt state.
func (b Bash) Concurrency(in map[string]any) tools.Concurrency {
	cmd, _ := in["command"].(string)
	if cmd == "" {
		return tools.ConcurrencyExclusive
	}
	if isReadOnlyCommand(cmd) {
		return tools.ConcurrencySafe
	}
	return tools.ConcurrencyExclusive
}

// IsReadOnly mirrors Concurrency for Snip purposes: a `cat foo.go`
// tool_result can be aggressively truncated; a `make build` cannot
// because the model may rely on the full output to debug failures.
func (b Bash) IsReadOnly(in map[string]any) bool {
	cmd, _ := in["command"].(string)
	if cmd == "" {
		return false
	}
	return isReadOnlyCommand(cmd)
}

// IsDestructive flags unrecoverable shell ops: rm -rf, dd, mkfs,
// shred, kill -9 init. Used for stricter ASK colouring — the model
// already failed bash_security_rules' classifier if these are
// reaching the gate at all, but TUI shows extra friction either way.
func (b Bash) IsDestructive(in map[string]any) bool {
	cmd, _ := in["command"].(string)
	c := strings.ToLower(cmd)
	keywords := []string{
		"rm -rf", "rm -fr", "dd if=", "mkfs", "shred", " > /dev/sd",
		"git push --force", "git push -f", "drop table", "drop database",
	}
	for _, k := range keywords {
		if strings.Contains(c, k) {
			return true
		}
	}
	return false
}

// readOnlyCommands is the conservative safe-list of binaries whose
// invocation does not mutate filesystem / process / network state.
// Adapted from claude-code's safelist; trimmed to commands that ship
// on every macOS / Linux box without flag analysis.
var readOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"find": true, "fd": true, "tree": true,
	"stat": true, "file": true, "du": true, "df": true,
	"echo": true, "printf": true, "true": true, "false": true,
	"pwd": true, "whoami": true, "id": true, "groups": true,
	"date": true, "uname": true, "hostname": true, "uptime": true,
	"which": true, "type": true, "command": true, "whence": true,
	"env": true, "printenv": true,
	"ps": true, "top": true, "htop": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
	"sort": true, "uniq": true, "tr": true, "cut": true, "awk": true,
	"sed":  false, // sed -i mutates; classify as exclusive even for read-only modes
	"diff": true, "cmp": true,
	"go":  true, // bare `go` covered below — only safe subcommands
	"git": true, // ditto — only safe subcommands
}

// readOnlyGoSubcommands is the per-binary subcommand allowlist for
// commands that have both read-only and mutating modes. Bare `go list`
// or `go env` → Safe, `go build` / `go install` → Exclusive.
var readOnlyGoSubcommands = map[string]bool{
	"list": true, "env": true, "version": true, "help": true,
	"vet": true, "doc": true, "tool": false, // go tool can do a lot, conservative
}

// readOnlyGitSubcommands — same idea for git.
var readOnlyGitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"branch": true, "tag": true, "describe": true, "blame": true,
	"config": false, // git config can mutate; just classify exclusive
	"remote": true, "ls-files": true, "ls-tree": true, "rev-parse": true,
	"rev-list": true, "shortlog": true, "reflog": true,
}

// isReadOnlyCommand classifies a shell command line. Splits on common
// command separators (`;`, `&&`, `||`, `|`) — every segment must be
// safe, otherwise the whole line is Exclusive. Pipes count: `cat foo
// | grep bar` is two read-only commands and stays safe.
func isReadOnlyCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	// Sub-shells / process substitution are write-side risks. Check
	// these BEFORE the bare-redirection check so `<(...)` doesn't get
	// misread as input redirection (it's not).
	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "<(") || strings.Contains(cmd, ">(") {
		return false
	}
	// Reject any I/O redirection (>, >>, <) and command substitution
	// (backtick) and variable expansion ($) — all write-side operators
	// or arbitrary-execution vectors.
	if strings.ContainsAny(cmd, ">$`<") {
		return false
	}
	// Split on simple separators. We don't try to parse quoting — if a
	// suspicious char like `;` lives inside a quoted string the whole
	// line is conservatively Exclusive (which is the safe direction).
	for _, seg := range splitOnAny(cmd, []string{";", "&&", "||", "|"}) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if !isReadOnlySegment(seg) {
			return false
		}
	}
	return true
}

// isReadOnlySegment classifies a single command (no shell operators).
// First word is the binary; subsequent args ignored except for the
// `git`/`go` subcommand checks where we look at the second word.
func isReadOnlySegment(seg string) bool {
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return false
	}
	// Drop env-var prefix (`FOO=bar cmd ...`) — find the first non-env
	// word. Safe since all env-var assignments don't run binaries.
	cmd := ""
	for _, f := range fields {
		if !strings.Contains(f, "=") || strings.HasPrefix(f, "=") {
			cmd = f
			break
		}
		// looks like FOO=bar — keep walking
	}
	if cmd == "" {
		return false
	}
	// Strip leading path: "/usr/bin/ls" → "ls"
	if i := strings.LastIndex(cmd, "/"); i >= 0 {
		cmd = cmd[i+1:]
	}
	safe, known := readOnlyCommands[cmd]
	if !known {
		return false
	}
	if !safe {
		return false
	}
	// Per-binary subcommand check.
	if cmd == "go" && len(fields) >= 2 {
		sub := fields[1]
		// skip any leading FOO=bar args
		for i, f := range fields {
			if !strings.Contains(f, "=") {
				if i+1 < len(fields) {
					sub = fields[i+1]
				}
				break
			}
		}
		ok, kn := readOnlyGoSubcommands[sub]
		return kn && ok
	}
	if cmd == "git" && len(fields) >= 2 {
		sub := fields[1]
		ok, kn := readOnlyGitSubcommands[sub]
		return kn && ok
	}
	return true
}

// splitOnAny splits s on any of the given separators (multi-char ok).
// Used by isReadOnlyCommand to fan out a pipeline into segments. We
// could use a regex but the input is short and the separators are
// fixed; a manual scan is simpler to reason about.
func splitOnAny(s string, seps []string) []string {
	out := []string{s}
	for _, sep := range seps {
		var next []string
		for _, piece := range out {
			next = append(next, strings.Split(piece, sep)...)
		}
		out = next
	}
	return out
}
func (b Bash) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	cmd, _ := in["command"].(string)
	if err := shellguard.Check(cmd); err != nil {
		return tools.PermissionDeny, err.Error()
	}
	// CC-style adversarial-input checks (Task #73): IFS injection,
	// /proc/environ exfil, zero-width unicode spoofing, etc. These
	// run BEFORE the user-permission gate because no permission level
	// (even bypass) should let an obviously-adversarial command run —
	// the model has been jailbroken or got prompt-injected upstream.
	if r := CheckCommand(cmd); !r.Allow {
		return tools.PermissionDeny, "bash-security rule #" + itoa(r.RuleID) + ": " + r.Reason
	}
	// Always consult the gate before applying sandbox auto-allow. Plan and
	// dontAsk are hard denials, and explicit deny rules must not be bypassed
	// merely because Seatbelt is enabled. Auto-allow only replaces an ASK;
	// the kernel sandbox remains the approval boundary for that prompt.
	d, src := b.gate.Check(context.Background(), "Bash", cmd)
	if d == permission.DecisionAsk && b.sandbox != nil && b.sandbox.AutoAllow() {
		return tools.PermissionAllow, "sandbox auto-allow"
	}
	return mapDecision(d), src
}

// itoa is a tiny no-import-strconv helper for the bash-security rule
// IDs (always 1..23, single or double digit).
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func (b Bash) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	cmd, _ := in["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("command is required")
	}
	// Execute repeats the non-bypassable process guard because Fork and other
	// internal callers may intentionally skip Tool.CanUse.
	if err := shellguard.Check(cmd); err != nil {
		return &tools.Result{Output: "[blocked] " + err.Error(), IsError: true}, nil
	}

	timeout := time.Duration(b.settings.TimeoutSeconds) * time.Second
	if to, ok := in["timeout_ms"].(float64); ok && to > 0 {
		timeout = time.Duration(to) * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	for _, deny := range b.settings.Denylist {
		if strings.Contains(cmd, deny) {
			return nil, errors.New("command matches denylist: " + deny)
		}
	}

	// Soft-sandbox policy: allow/deny lists from [sandbox.bash].
	if err := applyBashPolicy(cmd, b.settings.Sandbox); err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}

	// Structured (cmd, sub-cmd, flag) tuple blocker — catches
	// "individually-fine tokens, dangerous together" patterns like
	// `go test -exec "..."` and `npm install --global`. Hardcoded;
	// not configurable via permission prompt because the model could
	// rationalise away "yes please install --global, it's needed".
	if err := applyBashArgsBlocker(cmd); err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}

	// Classify command and flag dangerous operations.
	class := b.classifierFor().Classify(cmd)
	if class.Class == ClassDangerous {
		return &tools.Result{
			Output:  "[⚠️ blocked] command classified as dangerous: " + class.Reason + "\n\nCommand: " + cmd + "\n\nTo execute anyway, split into smaller safe commands or use a different approach.",
			IsError: true,
		}, nil
	}
	if class.Class == ClassSystem {
		return &tools.Result{
			Output:  "[ℹ️ system command] " + class.Reason + "\n\nCommand: " + cmd,
			IsError: false,
		}, nil
	}

	// Reject bare-sleep patterns that would just sit around blocking
	// the foreground turn for nothing useful. Mirrors claude-code's
	// detectBlockedSleepPattern (BashTool.tsx:322): standalone `sleep
	// N` (N≥2) and `sleep N && check` are both pointless — the model
	// is essentially polling, which a real signal (file watch, MCP
	// event, or background-task notification) would handle better.
	// Sub-2s sleeps and sleeps inside pipelines / subshells are fine.
	if blocked := detectBlockedSleepPattern(cmd); blocked != "" {
		return &tools.Result{
			Output: fmt.Sprintf(
				"[blocked sleep pattern] %s\n\n"+
					"Bare `sleep N` (N ≥ 2 seconds) is rejected as a polling primitive. "+
					"Better alternatives: "+
					"(1) use the Monitor tool to watch a file/log for a specific pattern — "+
					"its event arrives the moment the condition fires, not on a polling tick; "+
					"(2) for a long-running command, pass run_in_background=true and check progress via Output; "+
					"(3) for deliberate sub-second pacing, `sleep 0.5` etc. is allowed.",
				blocked,
			),
			IsError: true,
		}, nil
	}

	wantBackground := false
	if v, ok := in["run_in_background"].(bool); ok {
		wantBackground = v
	}

	// Explicit run_in_background=true: skip the foreground race and
	// hand the command straight to the job pool. We still build the
	// *exec.Cmd here (so the env / sandbox policy is applied
	// uniformly) — Spawn just adopts what we built.
	if wantBackground {
		return b.executeBackground(ctx, cmd)
	}

	return b.executeForegroundWithBgFallback(ctx, cmd, timeout)
}

// executeForegroundWithBgFallback runs cmd in the foreground but with
// a 60s race: if the command outlives AutoBackgroundThreshold, it's
// promoted to a background job (cmd keeps running, output keeps
// growing) and the model gets a "moved to background" reply with a
// job ID. Otherwise this is the existing pre-2026-05-09 behavior:
// capped buffer, hit timeout, return.
func (b Bash) executeForegroundWithBgFallback(ctx context.Context, cmdStr string, timeout time.Duration) (*tools.Result, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	// NOTE: cancel is intentionally NOT deferred at function-exit when
	// the command gets promoted to a background job — Adopt takes
	// ownership of cancel so JobKill can use it. Promotion path
	// rebinds `cancel = func(){}` so the deferred runs, no-op.
	canceled := false
	defer func() {
		if !canceled {
			cancel()
		}
	}()

	shell := b.settings.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	// jobs.OOMWrappedCommand: on Linux wraps in sh -c that bumps
	// /proc/self/oom_score_adj to 1000 so the kernel always picks the
	// bash subprocess (and not metis itself) when memory is tight.
	// On macOS/Windows it's a plain `shell -c cmdStr` (no /proc here).
	// See internal/jobs/oom_linux.go.
	exe := jobs.OOMWrappedCommand(cctx, shell, cmdStr)
	exe.Env = b.commandEnv(os.Environ())
	// G.2 (2026-05-12): when a sub-agent was spawned with `cwd:"..."`
	// or `isolation:"worktree"`, the Agent tool stamps the effective
	// cwd into context. Bash threads it through to exec.Cmd.Dir so the
	// child process inherits the sub-agent's working directory rather
	// than the parent metis's cwd. No-op for parent-agent calls
	// (CwdFromContext returns "" when no override is set).
	if cwd := agent.CwdFromContext(ctx); cwd != "" {
		exe.Dir = cwd
	}
	// Enter the runtime-owned OS sandbox after Dir and Env are finalised.
	// Wrap failures are ordinary tool errors and always fail closed.
	if wrapped, err := b.wrapCommand(exe); err != nil {
		return &tools.Result{
			Output:  "sandbox wrap failed: " + err.Error(),
			IsError: true,
		}, nil
	} else {
		exe = wrapped
	}
	// Put the bash leader + its children in their own process group so
	// kill-on-promote (Adopt path) tree-kills cleanly. Effectively a
	// no-op when the cmd never gets adopted — but cheap enough to do
	// universally rather than guess at adoption time.
	jobs.ApplyProcessGroup(exe)

	maxBytes := b.settings.MaxOutputBytes
	if maxBytes <= 0 {
		// In-process default — matches the config default
		// (config.go::DefaultConfig).
		// Keep these two in sync. Smaller than the previous 1 MiB so
		// a single chatty Bash call doesn't poison history with 250k
		// tokens of build log.
		maxBytes = 32 * 1024
	}
	cappedBuf := newCappedWriter(maxBytes)

	// If the job pool is wired up, also tee output to disk so we can
	// adopt the cmd into a Job without losing anything. If the pool
	// isn't wired (e.g. very early init / tests), we skip the disk
	// half and the auto-bg promotion path becomes a no-op.
	var diskOut *jobs.DiskOutput
	canPromote := b.Jobs != nil && AutoBackgroundThreshold > 0 && AutoBackgroundThreshold < timeout
	if canPromote {
		var err error
		diskOut, _, err = b.Jobs.NewDiskOutput()
		if err != nil {
			// Disk write failure shouldn't break Bash entirely — fall
			// back to no-promotion mode. The foreground path still
			// works exactly as before.
			canPromote = false
			diskOut = nil
		}
	}

	if canPromote {
		exe.Stdout = io.MultiWriter(cappedBuf, diskOut.Writer())
		exe.Stderr = io.MultiWriter(cappedBuf, diskOut.Writer())
	} else {
		exe.Stdout = cappedBuf
		exe.Stderr = cappedBuf
	}

	if err := exe.Start(); err != nil {
		if diskOut != nil {
			b.Jobs.CleanupOrphan(diskOut)
		}
		return nil, err
	}
	startedAt := time.Now()

	// Race the sole Cmd.Wait owner against the auto-bg threshold timer.
	// If the timer wins, Adopt receives this result channel and consumes it;
	// Registry must not call Cmd.Wait again for the same process.
	waitCh := make(chan error, 1)
	go func() { waitCh <- exe.Wait() }()

	var bgTimer <-chan time.Time
	if canPromote {
		t := time.NewTimer(AutoBackgroundThreshold)
		defer t.Stop()
		bgTimer = t.C
	}

	select {
	case err := <-waitCh:
		// Foreground completion (the common case). Tear down disk
		// half and return the buffer.
		if diskOut != nil {
			b.Jobs.CleanupOrphan(diskOut)
		}
		out, truncated := cappedBuf.snapshot()
		out = normalizeCapturedOutput(out)
		if truncated {
			out += "\n\n... [output truncated at " + bytesString(maxBytes) + "] ..."
		}
		res := &tools.Result{Output: out}
		if cctx.Err() == context.DeadlineExceeded {
			res.Output = out + "\n\n[command exceeded timeout " + timeout.String() + "]"
			res.IsError = true
			return res, nil
		}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				if ee.ExitCode() == 1 {
					if semantic, handled := interpretSearchExitOne(cmdStr, out); handled {
						if strings.TrimSpace(out) == "" {
							res.Output = semantic
						} else {
							res.Output = out + "\n\n[" + semantic + "]"
						}
						return res, nil
					}
				}
				res.Output = out + "\n\n[exit status " + intStr(ee.ExitCode()) + "]"
				res.IsError = true
				return res, nil
			}
			return nil, err
		}
		return res, nil

	case <-bgTimer:
		// 60s elapsed and command is still running. Promote to job.
		// We hand cancel ownership to Adopt so JobKill can escalate
		// to SIGKILL via context cancel; rebind our own var so the
		// function-exit defer is a no-op.
		jb, err := b.Jobs.Adopt(jobs.AdoptArgs{
			Command:     cmdStr,
			Description: "",
			Cmd:         exe,
			Cancel:      cancel,
			Output:      diskOut,
			StartTime:   startedAt,
			WaitResult:  waitCh,
		})
		if err != nil {
			// Adoption failed — let the cmd finish in the foreground
			// after all. Drain the wait we already started.
			err2 := <-waitCh
			out := cappedBuf.preview()
			res := &tools.Result{Output: out, IsError: err2 != nil}
			return res, nil
		}
		canceled = true // Adopt owns the cancel now
		preview, truncated := cappedBuf.snapshot()
		preview = normalizeCapturedOutput(preview)
		if truncated {
			preview += "\n... [output truncated at " + bytesString(maxBytes) + "] ..."
		}
		msg := fmt.Sprintf(
			"[command moved to background after %s — still running, job_id=%s]\n"+
				"Use Output {job_id: %q} to read more output, Kill {job_id: %q} to stop.\n"+
				"Output captured in foreground:\n%s",
			AutoBackgroundThreshold, jb.ID, jb.ID, jb.ID, preview,
		)
		// IsError=false: this is a normal flow, not an error. The
		// model should treat the job_id as the way forward.
		return &tools.Result{Output: msg}, nil
	}
}

// executeBackground starts cmd directly in the job pool — the
// foreground reply is just "running with job_id=X" and the model
// uses Output / Kill to interact further. Used for the
// explicit run_in_background=true path.
func (b Bash) executeBackground(ctx context.Context, cmdStr string) (*tools.Result, error) {
	if b.Jobs == nil {
		return &tools.Result{
			Output:  "[run_in_background] not available: jobs registry not wired (build error?)",
			IsError: true,
		}, nil
	}
	// Fresh context — background jobs don't share the Execute ctx
	// (which gets canceled when the foreground turn ends). The job
	// outlives the turn intentionally.
	bgCtx, cancel := context.WithCancel(context.Background())
	_ = ctx // intentionally not used; see comment above

	shell := b.settings.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	// Linux OOM-score wrapping (see jobs.OOMWrappedCommand).
	exe := jobs.OOMWrappedCommand(bgCtx, shell, cmdStr)
	exe.Env = b.commandEnv(os.Environ())
	// G.2 — read effective cwd from the ORIGINAL ctx (not bgCtx,
	// which is fresh). The sub-agent ctx the Agent tool stamped lives
	// on the upstream chain; bgCtx is just for cancellation lifetime.
	if cwd := agent.CwdFromContext(ctx); cwd != "" {
		exe.Dir = cwd
	}
	jobs.ApplyProcessGroup(exe)

	// Sandbox the background subprocess the same way the foreground
	// path does. Background jobs outlive the foreground turn but the
	// sandbox restrictions don't expire with the context, so the
	// wrap is just as effective.
	if wrapped, err := b.wrapCommand(exe); err != nil {
		cancel()
		return &tools.Result{
			Output:  "sandbox wrap failed: " + err.Error(),
			IsError: true,
		}, nil
	} else {
		exe = wrapped
	}

	jb, err := b.Jobs.Spawn(jobs.SpawnArgs{
		Command: cmdStr,
		Cmd:     exe,
		Cancel:  cancel,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return &tools.Result{
		Output: fmt.Sprintf(
			"[command running in background, job_id=%s]\n"+
				"Use Output {job_id: %q} to read its output, Kill {job_id: %q} to stop.\n"+
				"You'll receive a <job_notification> when the command exits.",
			jb.ID, jb.ID, jb.ID,
		),
	}, nil
}

// blockedSleepRE matches a leading `sleep N` segment (N integer ≥ 1)
// at the start of a command. We deliberately don't treat float sleeps
// (sleep 0.5) the same way — those are legitimate pacing patterns.
var blockedSleepRE = regexp.MustCompile(`^\s*sleep\s+(\d+)\s*(.*)$`)

// detectBlockedSleepPattern returns a non-empty diagnostic when cmd
// is a bare `sleep N` (N ≥ 2) or `sleep N && rest` / `sleep N; rest`
// pattern. Returns "" when the sleep is fine to execute (sub-2s, in a
// pipeline, in a subshell, in a script).
//
// Why we reject these specifically: they're the model's "polling
// pattern" — a sign it's waiting for something it should be watching
// instead. Catching the polite cases (`sleep 30 && check_status`) at
// the tool boundary forces a redirect to the job-notification or
// file-watch path, which is more responsive AND doesn't burn the
// foreground turn.
func detectBlockedSleepPattern(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	// for / while loop with embedded sleep is a real script
	// (polling with structure), not the bare-sleep anti-pattern.
	if strings.Contains(cmd, "for ") || strings.Contains(cmd, "while ") {
		return ""
	}
	// Sleep INSIDE a subshell or heredoc is allowed (real script).
	// We check by looking at the FIRST non-whitespace token: only
	// reject when `sleep` IS the leading command. Previous version
	// rejected via ContainsAny(cmd, "|()<>") which over-matched —
	// `sleep 60 && find ... | wc -l` got passed because the `|`
	// belonged to the *trailing* find pipeline, not the leading
	// sleep (image #50 session 5d9a38e5 repro 2026-05-21). The
	// post-chain pipeline doesn't change the fact that the model
	// is polling on `sleep N &&`.
	if strings.HasPrefix(cmd, "(") || strings.HasPrefix(cmd, "{") {
		return ""
	}
	m := blockedSleepRE.FindStringSubmatch(cmd)
	if m == nil {
		return ""
	}
	secs := 0
	for _, c := range m[1] {
		secs = secs*10 + int(c-'0')
	}
	if secs < 2 {
		return ""
	}
	rest := strings.TrimSpace(m[2])
	if rest == "" {
		return fmt.Sprintf("standalone `sleep %d`", secs)
	}
	// `sleep N && check` / `sleep N; check` — explicit chain. The
	// chained tail can have anything (pipelines, redirects); the
	// rejection is about the LEADING sleep, not the tail.
	if strings.HasPrefix(rest, "&&") || strings.HasPrefix(rest, ";") || strings.HasPrefix(rest, "||") {
		return fmt.Sprintf("`sleep %d` followed by chained command", secs)
	}
	// `sleep 5 | something` — also a polling-then-pipe pattern. The
	// `|` immediately after sleep is the same anti-pattern (model
	// waits then processes); we reject so the model picks a real
	// signal instead.
	if strings.HasPrefix(rest, "|") {
		return fmt.Sprintf("`sleep %d` piped into another command", secs)
	}
	// e.g. `sleep 5 anything-else` (rare); reject to be safe.
	return fmt.Sprintf("`sleep %d` with trailing args", secs)
}

// cappedWriter bounds an in-memory output preview to `max` bytes, but
// — unlike a plain head cap — it also retains the LAST `tailMax` bytes
// in a ring. When the captured stream is truncated and that tail
// carries error output (compiler / test / stack-trace failures land at
// the very end), preview() returns head + tail so the model still sees
// the verdict. Mirrors MiMo-Code's error-aware truncation; pure-head
// capping (the prior behavior, also Claude Code's) went blind there.
type cappedWriter struct {
	// mu guards all fields: os/exec drives a non-*os.File Stdout and Stderr
	// from TWO separate copier goroutines, and this writer is shared as both
	// (bash.go ~537), so Write is called concurrently → data race without it.
	mu        sync.Mutex
	head      []byte
	headMax   int
	tail      []byte // ring of up to tailMax most-recent bytes
	tailMax   int
	truncated bool
}

func newCappedWriter(max int) *cappedWriter {
	// Head keeps the FULL cap (no regression for ordinary output, which
	// stays head-only). The tail ring is an ADDITIONAL 30% reserve,
	// surfaced only when truncation happens AND the tail looks like a
	// failure — so error output costs a little extra, ordinary output
	// costs nothing.
	return &cappedWriter{headMax: max, tailMax: max * 3 / 10}
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if len(c.head) < c.headMax {
		room := c.headMax - len(c.head)
		if room >= len(p) {
			c.head = append(c.head, p...)
			return n, nil
		}
		c.head = append(c.head, p[:room]...)
		p = p[room:]
	}
	if len(p) > 0 {
		c.truncated = true
		c.pushTail(p)
	}
	return n, nil
}

// pushTail keeps only the last tailMax bytes seen so far.
func (c *cappedWriter) pushTail(p []byte) {
	if len(p) >= c.tailMax {
		c.tail = append(c.tail[:0], p[len(p)-c.tailMax:]...)
		return
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > c.tailMax {
		c.tail = c.tail[len(c.tail)-c.tailMax:]
	}
}

// preview renders the in-memory output. Full head when not truncated;
// head + tail when the dropped tail looks like a failure; head only
// otherwise.
func (c *cappedWriter) preview() string {
	out, _ := c.snapshot()
	return out
}

// snapshot returns a consistent preview + truncation bit while a promoted
// command may still be writing. Reading truncated separately after preview
// would race the os/exec copier goroutines.
func (c *cappedWriter) snapshot() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.truncated {
		return string(c.head), false
	}
	if spill.HasErrorMarker(string(c.tail)) {
		return string(c.head) +
			"\n\n... [middle omitted; tail kept below — it contains error output] ...\n\n" +
			string(c.tail), true
	}
	return string(c.head), true
}
