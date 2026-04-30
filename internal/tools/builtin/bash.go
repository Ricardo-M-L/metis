package builtin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Bash struct {
	gate       *permission.Gate
	settings   config.ToolBashSettings
	classifier *BashClassifier
}

func (b *Bash) classifierFor() *BashClassifier {
	if b.classifier == nil {
		b.classifier = NewBashClassifier()
	}
	return b.classifier
}

func (Bash) Name() string { return "Bash" }
func (Bash) Description() string {
	return "Execute a shell command. Output is captured (stdout+stderr merged) and truncated to a configurable byte cap. Long-running commands hit a timeout."
}
func (Bash) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"command"},
		"properties": map[string]any{
			"command":     map[string]any{"type": "string", "description": "shell command to execute"},
			"description": map[string]any{"type": "string", "description": "5-10 word summary of what this does"},
			"timeout_ms":  map[string]any{"type": "integer", "description": "override the default timeout"},
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
	d, src := b.gate.Check(context.Background(), "Bash", cmd)
	return mapDecision(d), src
}

func (b Bash) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	cmd, _ := in["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("command is required")
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

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := b.settings.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	exe := exec.CommandContext(cctx, shell, "-c", cmd)
	// Soft sandbox: filter sensitive env (API keys, tokens) and apply
	// network policy. Pass-through if dangerously_inherit_env is set.
	childEnv := filterEnv(os.Environ(), b.settings.Sandbox.DangerouslyInheritEnv)
	childEnv = applyBashNetworkPolicy(childEnv, b.settings.Sandbox)
	exe.Env = childEnv
	var buf bytes.Buffer
	maxBytes := b.settings.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	w := &cappedWriter{w: &buf, max: maxBytes}
	exe.Stdout = w
	exe.Stderr = w

	err := exe.Run()
	out := buf.String()
	if w.truncated {
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
			res.Output = out + "\n\n[exit status " + intStr(ee.ExitCode()) + "]"
			res.IsError = true
			return res, nil
		}
		return nil, err
	}
	return res, nil
}

type cappedWriter struct {
	w         io.Writer
	max       int
	written   int
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	remain := c.max - c.written
	if remain <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remain {
		_, err := c.w.Write(p[:remain])
		c.written = c.max
		c.truncated = true
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.written += n
	return n, err
}
