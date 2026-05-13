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
	"os/exec"
	"regexp"
	"strings"
	"time"

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
		return runInlineShell(ctx, sub[1], cwd)
	})
	body = inlineShellBacktickRE.ReplaceAllStringFunc(body, func(match string) string {
		sub := inlineShellBacktickRE.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		return runInlineShell(ctx, sub[1], cwd)
	})
	return body
}

// runInlineShell executes one shell command via `sh -c` with a per-
// invocation timeout and byte cap. Returns the (trimmed) stdout on
// success or a "[shell error: ...]" sentinel on failure.
func runInlineShell(ctx context.Context, cmd, cwd string) string {
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
	// WaitDelay (Go 1.20+) bounds the post-cancel wait when a shell
	// child outlives its parent and keeps pipe FDs open (Linux
	// reparents `sleep` to init after the shell is killed; c.Wait
	// would otherwise hang forever waiting on the pipe). With
	// WaitDelay set, Wait force-closes pipes and returns. macOS
	// shells happen to propagate the kill, so this only manifests
	// on Linux CI runners — TestExpandInlineShell_TimeoutBounded
	// failed there pre-2026-05-13.
	c.WaitDelay = 2 * time.Second
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
