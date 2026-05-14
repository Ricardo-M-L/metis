package slash

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/fun"
)

// Cmd is a callable slash command.
type Cmd struct {
	Name        string
	Aliases     []string
	Description string
	Handler     Handler
}

// Handler executes the command.
type Handler func(args string) (display string, control Signal)

// Signal tells the REPL what to do.
type Signal int

const (
	SignalNone Signal = iota
	SignalQuit
	SignalClear
	SignalCompact
	SignalReload
	SignalPlan
	SignalBypass
	SignalAuto
	SignalNew
	SignalRetry
	SignalUndo
	SignalHistory
	SignalSave
	SignalBranch
	SignalStatus
	SignalLoop
	SignalTitle
	SignalTools
	SignalSessions
	SignalSession
	SignalSkills
	SignalVersion
	SignalAddDir
	SignalRemoveDir
	SignalListDirs
	SignalBtw
	SignalBatch
	SignalCost
	SignalDiff
	SignalDoctor
	SignalStats
	SignalKeybindings
	SignalPermissions
	SignalHooks
	SignalVim
	SignalExport
	SignalReleaseNotes
	SignalTheme
	SignalEffort
	SignalPRComments
	SignalUpgrade
	SignalContext
	SignalResume
	SignalRename
	SignalTag
	// SignalCustomPrompt: handler returns a prompt body that the TUI
	// should treat as the next user message. Used by user-authored
	// slash commands loaded from ~/.metis/commands/*.md (and the
	// project-local .metis/commands/). Mirrors claude-code's
	// `~/.claude/commands/` mechanism.
	SignalCustomPrompt
)

type Registry struct {
	cmds  []Cmd
	index map[string]*Cmd
}

func NewRegistry() *Registry {
	return &Registry{index: make(map[string]*Cmd)}
}

func (r *Registry) Register(c Cmd) {
	r.cmds = append(r.cmds, c)
	cp := &r.cmds[len(r.cmds)-1]
	r.index[c.Name] = cp
	for _, a := range c.Aliases {
		r.index[a] = cp
	}
}

func (r *Registry) All() []Cmd {
	out := append([]Cmd(nil), r.cmds...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Get(name string) (*Cmd, bool) {
	c, ok := r.index[name]
	return c, ok
}

// Parse returns (true, output, signal, args) if input is a slash command,
// (false, "", SignalNone, "") otherwise. `args` is whatever followed the
// command name on the input line — the runtime needs it for signals like
// SignalTitle that carry a payload (`/title my session`).
//
// "Looks like a slash command" is more than `startsWith("/")` — pasted
// absolute paths (`/Users/...`, `/var/log/...`) start with `/` but are
// NOT commands, and surfacing "unknown: /Users/... — try /help" turns
// the user's prompt into noise. We discriminate via IsCommandShape on
// the head token; non-command shapes fall through to the agent as
// plain prompt text. Bug audit 2026-05-10 (image #15).
func (r *Registry) Parse(input string) (handled bool, display string, sig Signal, args string) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return false, "", SignalNone, ""
	}
	input = input[1:]
	name, rest, _ := cut(input, " ")
	if !IsCommandShape(name) {
		// Path-like or otherwise non-command — let the caller treat
		// the original input as plain text.
		return false, "", SignalNone, ""
	}
	c, ok := r.index[name]
	if !ok {
		return true, fmt.Sprintf("unknown: /%s — try /help", name), SignalNone, ""
	}
	d, s := c.Handler(rest)
	return true, d, s, rest
}

// IsCommandShape reports whether name is a valid slash-command head:
// 1+ chars from [A-Za-z0-9_-]. Empty name is rejected (a bare "/"
// triggers the palette via a different code path, not Parse).
//
// Used by Parse + the TUI palette gate to keep the "is this a real
// slash command?" rule in one place.
func IsCommandShape(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// HelpText renders a column-aligned listing.
func (r *Registry) HelpText() string {
	var b strings.Builder
	all := r.All()
	maxLen := 0
	for _, c := range all {
		if l := len(c.Name); l > maxLen {
			maxLen = l
		}
	}
	for _, c := range all {
		fmt.Fprintf(&b, "  /%-*s  %s\n", maxLen, c.Name, c.Description)
	}
	return b.String()
}

func cut(s, sep string) (before, after string, found bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// RegisterAll installs the full built-in command set.
func RegisterAll(r *Registry, cfg *config.Config) {
	// Core commands
	r.Register(Cmd{Name: "help", Description: "show this help", Handler: func(_ string) (string, Signal) {
		return r.HelpText(), SignalNone
	}})
	r.Register(Cmd{Name: "quit", Aliases: []string{"q", "exit"}, Description: "exit metis", Handler: func(_ string) (string, Signal) {
		return "", SignalQuit
	}})

	// Session commands
	r.Register(Cmd{Name: "new", Aliases: []string{"reset"}, Description: "start a new session", Handler: func(_ string) (string, Signal) {
		return "(starting new session...)", SignalNew
	}})
	r.Register(Cmd{Name: "clear", Description: "clear conversation history", Handler: func(_ string) (string, Signal) {
		return "(history cleared)", SignalClear
	}})
	r.Register(Cmd{Name: "retry", Description: "retry last assistant response", Handler: func(_ string) (string, Signal) {
		return "(retrying last response...)", SignalRetry
	}})
	r.Register(Cmd{Name: "undo", Aliases: []string{"u"}, Description: "remove last user/assistant exchange (incl. tool calls)", Handler: func(_ string) (string, Signal) {
		// Empty display — the actual confirmation is printed by the
		// runtime once it knows whether anything was undone.
		return "", SignalUndo
	}})
	r.Register(Cmd{Name: "history", Aliases: []string{"hist"}, Description: "show conversation history (Esc / q to close)", Handler: func(_ string) (string, Signal) {
		// Display is empty — the runtime renders the screen directly.
		return "", SignalHistory
	}})
	r.Register(Cmd{Name: "save", Description: "fsync the current session to disk", Handler: func(_ string) (string, Signal) {
		return "", SignalSave
	}})
	r.Register(Cmd{Name: "title", Description: "set session title (e.g. /title refactor sprint)", Handler: func(args string) (string, Signal) {
		if strings.TrimSpace(args) == "" {
			return "(title: type `/title <text>` to set; current title is shown in /sessions)", SignalNone
		}
		// Display empty so the runtime can confirm with the actual id +
		// stored title once persistence succeeds.
		return "", SignalTitle
	}})
	r.Register(Cmd{Name: "branch", Aliases: []string{"fork"}, Description: "fork the current session to a new id (preserves history)", Handler: func(_ string) (string, Signal) {
		return "", SignalBranch
	}})
	r.Register(Cmd{Name: "add-dir", Description: "add a directory to the agent's accessible scope (`/add-dir <path>`)", Handler: func(args string) (string, Signal) {
		args = strings.TrimSpace(args)
		if args == "" {
			return "", SignalListDirs
		}
		return "", SignalAddDir
	}})
	r.Register(Cmd{Name: "rm-dir", Aliases: []string{"remove-dir"}, Description: "remove a directory previously added (`/rm-dir <path>`)", Handler: func(args string) (string, Signal) {
		if strings.TrimSpace(args) == "" {
			return "usage: /rm-dir <path>", SignalNone
		}
		return "", SignalRemoveDir
	}})
	r.Register(Cmd{Name: "btw", Description: "ask a side question without disturbing the main turn (`/btw <question>`)", Handler: func(args string) (string, Signal) {
		if strings.TrimSpace(args) == "" {
			return "usage: /btw <question>", SignalNone
		}
		return "", SignalBtw
	}})
	r.Register(Cmd{Name: "batch", Description: "research → plan → spawn N worktree sub-agents to execute (`/batch <task>`)", Handler: func(args string) (string, Signal) {
		if strings.TrimSpace(args) == "" {
			return "usage: /batch <task description>", SignalNone
		}
		return "", SignalBatch
	}})

	// --- info / toggle commands (claude-code parity) ---
	r.Register(Cmd{Name: "cost", Aliases: []string{"usage"}, Description: "show token usage and estimated cost for the session", Handler: func(_ string) (string, Signal) {
		return "", SignalCost
	}})
	r.Register(Cmd{Name: "diff", Description: "show git diff for the working tree", Handler: func(_ string) (string, Signal) {
		return "", SignalDiff
	}})
	r.Register(Cmd{Name: "doctor", Description: "run a health check (API key, MCP, tools, cwd)", Handler: func(_ string) (string, Signal) {
		return "", SignalDoctor
	}})
	r.Register(Cmd{Name: "stats", Description: "show session statistics (turns, tool calls, tokens)", Handler: func(_ string) (string, Signal) {
		return "", SignalStats
	}})
	r.Register(Cmd{Name: "keybindings", Aliases: []string{"keys"}, Description: "list TUI keybindings", Handler: func(_ string) (string, Signal) {
		return "", SignalKeybindings
	}})
	r.Register(Cmd{Name: "permissions", Aliases: []string{"perms"}, Description: "list active permission rules", Handler: func(_ string) (string, Signal) {
		return "", SignalPermissions
	}})
	r.Register(Cmd{Name: "hooks", Description: "list loaded lifecycle hooks", Handler: func(_ string) (string, Signal) {
		return "", SignalHooks
	}})
	r.Register(Cmd{Name: "vim", Description: "toggle vim-style modal editing for the input box", Handler: func(_ string) (string, Signal) {
		return "", SignalVim
	}})
	r.Register(Cmd{Name: "export", Description: "export the current session as JSONL to stdout (mirrored to ~/.metis/exports/)", Handler: func(_ string) (string, Signal) {
		return "", SignalExport
	}})
	r.Register(Cmd{Name: "release-notes", Aliases: []string{"changelog", "whatsnew"}, Description: "show recent metis release notes", Handler: func(_ string) (string, Signal) {
		return "", SignalReleaseNotes
	}})

	// --- /fun — opt-in user-delight commands (music / pet / break).
	// All side effects (mpv spawn, state files) live in internal/fun;
	// this handler is a thin string dispatcher returning display text.
	r.Register(Cmd{Name: "fun", Description: "opt-in delight commands (`/fun lofi`, `/fun music status`, etc.)", Handler: func(args string) (string, Signal) {
		return fun.Dispatch(args), SignalNone
	}})

	// --- P1: theme / effort / pr_comments / upgrade / context ---
	r.Register(Cmd{Name: "theme", Description: "cycle TUI color theme (`/theme [dark|light|auto]`)", Handler: func(_ string) (string, Signal) {
		return "", SignalTheme
	}})
	r.Register(Cmd{Name: "effort", Description: "show or set reasoning effort (`/effort [low|medium|high]`)", Handler: func(_ string) (string, Signal) {
		return "", SignalEffort
	}})
	r.Register(Cmd{Name: "pr_comments", Aliases: []string{"prc"}, Description: "fetch PR review comments via gh CLI (`/pr_comments <number>`)", Handler: func(args string) (string, Signal) {
		if strings.TrimSpace(args) == "" {
			return "usage: /pr_comments <pr-number>", SignalNone
		}
		return "", SignalPRComments
	}})
	r.Register(Cmd{Name: "upgrade", Description: "check for a newer metis release (`metis update --check` wrapper)", Handler: func(_ string) (string, Signal) {
		return "", SignalUpgrade
	}})
	r.Register(Cmd{Name: "context", Description: "show context window utilization for the current session", Handler: func(_ string) (string, Signal) {
		return "", SignalContext
	}})
	// --- P2 lightweight wrappers ---
	r.Register(Cmd{Name: "resume", Description: "show how to resume a session (use `metis chat --resume <id>`)", Handler: func(_ string) (string, Signal) {
		return "", SignalResume
	}})
	r.Register(Cmd{Name: "rename", Description: "rename the current session (alias of /title)", Handler: func(args string) (string, Signal) {
		args = strings.TrimSpace(args)
		if args == "" {
			return "usage: /rename <new title>", SignalNone
		}
		return "", SignalTitle // reuse existing title pipeline
	}})
	r.Register(Cmd{Name: "tag", Description: "tag the current session with a label (`/tag <label>`)", Handler: func(args string) (string, Signal) {
		args = strings.TrimSpace(args)
		if args == "" {
			return "usage: /tag <label>", SignalNone
		}
		return "", SignalTag
	}})
	// /usage is just an alias of /cost — claude-code parity. We register
	// a separate Cmd so /help shows the alias, but route the same signal.
	r.Register(Cmd{Name: "usage", Description: "alias of /cost", Handler: func(_ string) (string, Signal) {
		return "", SignalCost
	}})

	// Mode commands
	r.Register(Cmd{Name: "plan", Aliases: []string{"p"}, Description: "switch to plan mode (show plan before executing)", Handler: func(_ string) (string, Signal) {
		return "(plan mode: on — /auto to exit)", SignalPlan
	}})
	r.Register(Cmd{Name: "auto", Aliases: []string{"a"}, Description: "auto mode (read-only tools auto-allow)", Handler: func(_ string) (string, Signal) {
		return "(mode: auto)", SignalAuto
	}})
	r.Register(Cmd{Name: "bypass", Aliases: []string{"yolo"}, Description: "bypass mode (no permission prompts)", Handler: func(_ string) (string, Signal) {
		return "(mode: bypass — WARNING: approves all)", SignalBypass
	}})
	r.Register(Cmd{Name: "compact", Description: "force context compaction", Handler: func(_ string) (string, Signal) {
		return "(compaction triggered)", SignalCompact
	}})

	// Loop — sugar wrapper over /cron with the "loop:" name prefix so a
	// user can list / stop their loops without scanning every cron job.
	// `/loop <every> <prompt>` creates; `/loop list` / `/loop stop <id|all>`
	// manage. The same persistence + pause/resume guarantees as /cron.
	r.Register(Cmd{Name: "loop", Description: "autopilot prompts: <every> <prompt> | list | stop <id|all>", Handler: func(args string) (string, Signal) {
		return handleLoopCommand(cfg, args), SignalNone
	}})

	// Info commands
	r.Register(Cmd{Name: "status", Description: "show session info", Handler: func(_ string) (string, Signal) {
		return "(status: see REPL)", SignalStatus
	}})
	r.Register(Cmd{Name: "session", Aliases: []string{"sid"}, Description: "show current session id + title + turn count", Handler: func(_ string) (string, Signal) {
		return "", SignalSession
	}})
	r.Register(Cmd{Name: "sessions", Aliases: []string{"ls"}, Description: "list recent saved sessions", Handler: func(_ string) (string, Signal) {
		return "", SignalSessions
	}})
	r.Register(Cmd{Name: "model", Aliases: []string{"m"}, Description: "show current model", Handler: func(_ string) (string, Signal) {
		return "model: " + cfg.Provider.Default, SignalNone
	}})

	// Tools & Skills
	r.Register(Cmd{Name: "tools", Aliases: []string{"t"}, Description: "list registered tools (Read / Bash / Glob / …)", Handler: func(_ string) (string, Signal) {
		return "", SignalTools
	}})
	r.Register(Cmd{Name: "toolsets", Description: "show available toolsets", Handler: func(_ string) (string, Signal) {
		return "(toolsets: file, terminal, web, memory, skills, cronjob, delegation, web)", SignalNone
	}})
	r.Register(Cmd{Name: "skills", Aliases: []string{"sk"}, Description: "list installed skills under ~/.metis/skills", Handler: func(_ string) (string, Signal) {
		return "", SignalSkills
	}})
	r.Register(Cmd{Name: "memory", Description: "auto-memory: list | show <file> | rm <file> | path", Handler: func(args string) (string, Signal) {
		return handleMemoryCommand(args), SignalNone
	}})
	r.Register(Cmd{Name: "reload", Description: "reload tools and skills", Handler: func(_ string) (string, Signal) {
		return "(reloading...)", SignalReload
	}})

	// Cron — list/add/rm/pause/resume scheduled prompts
	r.Register(Cmd{Name: "cron", Description: "scheduled prompts: list | add <every> <prompt> | rm <id> | pause/resume <id>", Handler: func(args string) (string, Signal) {
		out, ok := handleCronCommand(cfg, args)
		if !ok {
			// Empty / help path stays SignalNone so the runtime just
			// echoes the usage hint we returned.
			return out, SignalNone
		}
		return out, SignalNone
	}})

	// Misc
	r.Register(Cmd{Name: "abort", Description: "abort the current turn", Handler: func(_ string) (string, Signal) {
		return "(abort: Ctrl+C)", SignalNone
	}})
	r.Register(Cmd{Name: "edit", Description: "rewind to before the last reply (alias for /undo)", Handler: func(_ string) (string, Signal) {
		// True in-place edit of an assistant message would require a
		// text-overlay screen + a way to re-stream from the rewritten
		// turn. /undo gets you 90% there: it pops the assistant reply
		// (and any tool-loop bundle) so your previous prompt is the
		// next thing to send — you just retype with edits.
		return "", SignalUndo
	}})
	r.Register(Cmd{Name: "submit", Aliases: []string{"s"}, Description: "submit and finalize", Handler: func(_ string) (string, Signal) {
		return "(submit: use /plan to preview first)", SignalNone
	}})
	r.Register(Cmd{Name: "config", Aliases: []string{"cfg"}, Description: "open config in editor", Handler: func(_ string) (string, Signal) {
		return "(config: ~/.metis/config.toml)", SignalNone
	}})
	r.Register(Cmd{Name: "version", Aliases: []string{"v"}, Description: "show metis version", Handler: func(_ string) (string, Signal) {
		return "", SignalVersion
	}})
	r.Register(Cmd{Name: "agents", Description: "show active agents", Handler: func(_ string) (string, Signal) {
		return "(agents: single agent mode)", SignalNone
	}})
}
