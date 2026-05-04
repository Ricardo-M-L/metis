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
	"github.com/Ricardo-M-L/metis/internal/llm"
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
	// Layout (top → bottom):
	//
	//   base
	//   <<<__METIS_CACHE_BOUNDARY__>>>     ← split point for prompt cache
	//   <env>...</env>                     ← cwd, hostname, today's date
	//   <project_context>...</               ← CLAUDE.md / AGENTS.md / METIS.md
	//   <user-addendum>                      ← ~/.metis/system.md
	//
	// Why split here: `base` is the agent identity + tool primer — it's
	// stable across sessions, users, and time. Everything below the
	// boundary changes per-call (cwd from `cd elsewhere`, today's date,
	// project file edits). The Anthropic provider's buildSystemBlocks
	// reads this marker and emits `[static (cache_control), dynamic]`
	// so the static prefix gets ~10% billing on cache hit.
	out := base + "\n\n" + llm.SystemPromptCacheBoundary + "\n\n" + buildEnvBlock()
	if proj := loadProjectContext(); proj != "" {
		out += "\n\n" + proj
	}
	addendum := loadSystemPromptAddendum()
	if addendum != "" {
		out += "\n\n" + addendum
	}
	return out
}

// loadProjectContext checks cwd for any of the conventional project-
// context filenames and returns the first hit's body wrapped in a
// labeled block. Search order matches the de-facto priority across
// LLM-CLI tools: CLAUDE.md is most common (claude-code), AGENTS.md
// is the OpenAI/codex convention, METIS.md is metis-specific.
//
// `.metis/CLAUDE.md` is also checked so users who want the file out
// of the repo root can stash it under .metis/.
func loadProjectContext() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	candidates := []string{
		"CLAUDE.md",
		"AGENTS.md",
		"METIS.md",
		filepath.Join(".metis", "CLAUDE.md"),
		filepath.Join(".claude", "CLAUDE.md"),
	}
	for _, name := range candidates {
		path := filepath.Join(cwd, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body := strings.TrimSpace(string(b))
		if body == "" {
			continue
		}
		// Wrap in a labeled block so the LLM knows where this content
		// came from (helps it cite "per CLAUDE.md..." in answers).
		return "<project_context source=\"" + name + "\">\n" + body + "\n</project_context>"
	}
	return ""
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
