package runtime

// Git workspace context injection — mirrors claude-code's getGitStatus
// attachment (restored-src/src/context.ts:124-142): at session boot the
// model receives the current branch, the porcelain status list and the
// last few commits, so "what state is the working tree in?" never costs
// a tool round-trip. The env block already carries the branch name;
// this section adds the change list + recent history.
//
// Snapshot semantics: computed once per boot (same lifecycle as
// buildEnvBlock), explicitly labelled as a point-in-time snapshot so
// the model doesn't treat it as live after it starts editing files.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// gitContextTimeout bounds the TOTAL time spent on git subprocesses.
	// A cold NFS mount or a gc-ing monorepo must not stall chat boot.
	gitContextTimeout = 500 * time.Millisecond

	// gitContextMaxBytes caps the rendered block. A repo with 4k dirty
	// files would otherwise dwarf the base prompt.
	gitContextMaxBytes = 2048
)

// buildGitContext returns the <git_status> block, or "" when the cwd
// isn't a git repository (or git is too slow / missing — fail silent,
// the env block's branch line degrades the same way).
func buildGitContext() string {
	ctx, cancel := context.WithTimeout(context.Background(), gitContextTimeout)
	defer cancel()

	branch := gitOut(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return "" // not a repo
	}
	status := gitOut(ctx, "status", "--porcelain")
	commits := gitOut(ctx, "log", "--oneline", "-5")

	var b strings.Builder
	b.WriteString("<git_status>\n")
	b.WriteString("This is a snapshot of the git status at the start of the conversation; it does NOT update as you work.\n")
	fmt.Fprintf(&b, "Current branch: %s\n", branch)
	b.WriteString("\nStatus:\n")
	if status == "" {
		b.WriteString("(clean)\n")
	} else {
		b.WriteString(status)
		b.WriteString("\n")
	}
	if commits != "" {
		b.WriteString("\nRecent commits:\n")
		b.WriteString(commits)
		b.WriteString("\n")
	}
	b.WriteString("</git_status>")

	out := b.String()
	if len(out) > gitContextMaxBytes {
		cut := gitContextMaxBytes
		for cut > 0 && out[cut]&0xC0 == 0x80 { // don't split a UTF-8 rune
			cut--
		}
		out = out[:cut] + "\n... (git status truncated)\n</git_status>"
	}
	return out
}

// gitOut runs one git command under the shared deadline and returns
// trimmed stdout, or "" on any error (not a repo, timeout, no git).
func gitOut(ctx context.Context, args ...string) string {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
