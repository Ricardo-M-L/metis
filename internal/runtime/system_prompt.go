package runtime

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// SystemPromptFileName is the filename Metis looks for under ~/.metis/
// when assembling the agent's system prompt. Mirrors claude-code's
// `~/.claude/CLAUDE.md` convention so users moving between the two
// agents can keep similar habits.
const SystemPromptFileName = "system.md"

// AssembleSystemPrompt joins the built-in default system prompt with
// (a) the runtime environment block and (b) the user's optional
// ~/.metis/system.md addendum.
//
// Order is intentional:
//
//  1. base                       — agent identity / tool primer
//  2. <env>...</env> block       — concrete cwd / HOME / OS so the
//     LLM doesn't hallucinate paths like
//     /home/user/.claude on macOS (real
//     bug report 2026-04-30, MiniMax kept
//     guessing Linux paths)
//  3. user's system.md addendum  — wins last so "always reply in
//     Chinese" / "use shadcn" etc.
//     override behaviour without losing
//     the core primer
//
// The env block uses claude-code's `<env>...</env>` tag shape so models
// trained on that surface pattern-match the same way.
func AssembleSystemPrompt(base string) string {
	out := base + "\n\n" + buildEnvBlock()
	addendum := loadSystemPromptAddendum()
	if addendum != "" {
		out += "\n\n" + addendum
	}
	return out
}

// buildEnvBlock collects the runtime facts the LLM should see verbatim
// before answering anything path-y. Cheap to compute (a few syscalls)
// so we do it on every chat boot rather than caching — that way a
// /clear after `cd otherproj` picks up the new cwd.
func buildEnvBlock() string {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	uname := ""
	if u, err := user.Current(); err == nil {
		uname = u.Username
	}
	host, _ := os.Hostname()

	var b strings.Builder
	b.WriteString("<env>\n")
	if cwd != "" {
		fmt.Fprintf(&b, "Working directory: %s\n", cwd)
	}
	if home != "" {
		fmt.Fprintf(&b, "Home directory: %s\n", home)
	}
	if uname != "" {
		fmt.Fprintf(&b, "User: %s\n", uname)
	}
	if host != "" {
		fmt.Fprintf(&b, "Hostname: %s\n", host)
	}
	fmt.Fprintf(&b, "Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Today's date: %s\n", time.Now().Format("2006-01-02"))
	b.WriteString("</env>")
	return b.String()
}

func loadSystemPromptAddendum() string {
	path := filepath.Join(config.Home(), SystemPromptFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		return "" // missing / unreadable → no addendum
	}
	s := strings.TrimSpace(string(b))
	return s
}
