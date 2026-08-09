package skills

// inline_shell.go — SKILL.md inline-shell expansion at invoke time.
//
// Two recognised forms (mirror claude-code's executeShellCommandsInPrompt):
//
//	!`cmd …`            — single-line, runs cmd, replaces with stdout
//	```!
//	cmd 1
//	cmd 2
//	```                 — multi-line shell block (joined with newlines)
//
// Skills can use these to splice live data into the prompt without the
// model having to call Bash itself (e.g. embedding `git status`, the
// current timestamp, or a remote /healthz response into the body).
//
// Trust gate: callers must check `ShouldRunInlineShell(trust)` before
// expanding. Community / MCP-source skills are NOT trusted to embed
// arbitrary shell — that would let a third-party manifest pull
// secrets / poison the prompt at invoke time. claude-code's
// loadSkillsDir.ts has the same carve-out (MCP-source skills bypass
// shell execution).

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/sandbox"

	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// inlineShellTimeout caps any one inline-shell invocation so a wedged
// command (curl on a dead endpoint, `read`, infinite loop) can't hang
// the agent. 10 s mirrors claude-code's default and is plenty for the
// typical `date`, `git rev-parse`, `cat metadata.json` use cases.
const inlineShellTimeout = 10 * time.Second

// inlineShellMaxBytes caps captured stdout per invocation. A skill
// pulling `cat huge.log` shouldn't blow the prompt budget. 8 KiB =
// roughly ~2k tokens, enough for normal status output, hard cap
// enough to stop runaway payloads.
const inlineShellMaxBytes = 8 * 1024

// Compiled patterns. The fenced form is intentionally greedy-shortest
// so two ```! blocks on the same body don't merge.
var (
	inlineShellBacktickRE = regexp.MustCompile("!`([^`\\n]+)`")
	inlineShellFencedRE   = regexp.MustCompile("(?s)```!\\n(.*?)\\n```")
)

// ShouldRunInlineShell reports whether the loader's trust posture for
// this skill is high enough to safely execute inline-shell blocks.
// builtin / trusted / user / project are allowed; community / mcp
// and unknown trust levels are NOT.
func ShouldRunInlineShell(trust string) bool {
	switch trust {
	case pubskill.TrustBuiltin, pubskill.TrustTrusted, pubskill.TrustUser, pubskill.TrustProject:
		return true
	}
	return false
}

// ExpandInlineShell runs every recognised shell block in body and
// returns a new body string with each block replaced by the command's
// trimmed stdout. ctx bounds the *individual* command (we wrap an
// inlineShellTimeout-derived sub-ctx per call).
//
// Errors do not abort expansion — a failing command is replaced with
// a clearly-marked "[shell error: ...]" placeholder so the rest of the
// skill body still works. This matches claude-code: a flaky `gh pr
// list` shouldn't make the whole skill unusable.
//
// cwd is the directory the commands run from. Pass the skill's own
// directory (filepath.Dir(sk.LocalPath)) so `cat ./template.json`
// resolves the way the author expects.
func ExpandInlineShell(ctx context.Context, body, cwd string) string {
	return ExpandInlineShellWithSandbox(ctx, body, cwd, nil)
}

// ExpandInlineShellWithSandbox expands trusted skill shell blocks through the
// runtime's shared OS sandbox. Keeping the old wrapper preserves embedders that
// intentionally execute outside a Metis runtime.
func ExpandInlineShellWithSandbox(ctx context.Context, body, cwd string, manager *sandbox.Manager) string {
	if body == "" || !strings.Contains(body, "!") {
		return body
	}
	// Fenced first — its delimiters are longer than the backtick form,
	// so a naive backtick pass over a fenced block would partially
	// consume it. Fenced first means the backtick pass only sees
	// what's left.
	body = inlineShellFencedRE.ReplaceAllStringFunc(body, func(match string) string {
		sub := inlineShellFencedRE.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		return runInlineShell(ctx, sub[1], cwd, manager)
	})
	body = inlineShellBacktickRE.ReplaceAllStringFunc(body, func(match string) string {
		sub := inlineShellBacktickRE.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		return runInlineShell(ctx, sub[1], cwd, manager)
	})
	return body
}

// runInlineShell executes one shell command via `sh -c` with a per-
// invocation timeout and byte cap. Returns the (trimmed) stdout on
// success or a "[shell error: ...]" sentinel on failure.
func runInlineShell(ctx context.Context, cmd, cwd string, manager *sandbox.Manager) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	subCtx, cancel := context.WithTimeout(ctx, inlineShellTimeout)
	defer cancel()
	c := exec.CommandContext(subCtx, "sh", "-c", cmd)
	if cwd != "" {
		c.Dir = cwd
	}
	// Linux gotcha: when ctx fires, exec.CommandContext sends SIGKILL
	// to `sh`, but `sh -c "sleep 30"`'s sleep grandchild is reparented
	// to init and keeps the inherited stdout pipe FD open — so c.Wait
	// blocks forever on the pipe read. macOS shells happen to
	// propagate the kill, masking the issue (the only CI failure
	// surface). Fix is two-layered:
	//   1. Setpgid puts the shell + all its descendants in a fresh
	//      process group, and Cancel overrides the default SIGKILL
	//      target with a group-wide kill (`syscall.Kill(-pid, ...)`).
	//      That dispatches the kill to `sleep` too, so the pipe
	//      writer closes promptly and Wait returns.
	//   2. WaitDelay=500ms is a belt-and-braces: if anything in the
	//      group survives Setpgid (unlikely), the I/O goroutines
	//      still force-close after 500ms.
	configureInlineShellCancellation(c)
	c.WaitDelay = 500 * time.Millisecond
	if manager != nil {
		c.Env = manager.FilterEnv(os.Environ(), false)
		wrapped, err := manager.Wrap(c, sandbox.Request{Cwd: cwd})
		if err != nil {
			return "[shell sandbox error: " + err.Error() + "]"
		}
		c = wrapped
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	out := stdout.Bytes()
	if len(out) > inlineShellMaxBytes {
		out = append(out[:inlineShellMaxBytes], []byte("\n[... truncated, output exceeded 8 KiB cap]")...)
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if err != nil {
		// Surface a tiny stderr snippet so the model knows the failure
		// mode without paying for the whole error stream.
		errSnippet := strings.TrimSpace(stderr.String())
		if len(errSnippet) > 200 {
			errSnippet = errSnippet[:200] + "…"
		}
		if errSnippet != "" {
			return "[shell error: " + errSnippet + "]"
		}
		return "[shell error: " + err.Error() + "]"
	}
	return trimmed
}
