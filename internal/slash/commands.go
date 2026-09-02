package slash

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/fun"
)

// Cmd is a callable slash command.
type Cmd struct {
	Name         string
	Aliases      []string
	Description  string
	ArgumentHint string
	Source       string
	Category     string
	Handler      Handler

	// Visible and Enabled are optional live predicates used by discovery
	// surfaces. A nil predicate means true, preserving compatibility for all
	// existing registrations. Dispatch remains independent: phase one unifies
	// metadata without forcing every handler into a new execution registry.
	Visible func() bool
	Enabled func() bool

	// AllowedTools / Model carry per-command frontmatter requests from
	// user-authored commands (~/.metis/commands/*.md). Empty = no request.
	// Trusted AllowedTools become one-turn pre-approvals. Model is validated
	// against the already active model; Metis does not silently rebuild the
	// provider, and a mismatch tells the user to run /model explicitly.
	AllowedTools []string
	Model        string
	// Trusted is true only for commands loaded from the user's Metis home.
	// Project-local commands are prompt templates but cannot use frontmatter
	// to auto-approve tools or influence model selection until Metis has a project-trust
	// decision comparable to Claude Code's trust boundary.
	Trusted bool

	// Custom is true for user-authored commands loaded from
	// ~/.metis/commands/*.md (and project .metis/commands/). The
	// SlashCommand tool only lets the model invoke Custom commands —
	// built-in TUI commands (quit/clear/compact/…) need Signals a tool
	// can't honor, so they're refused.
	Custom bool
}

// IsVisible and IsEnabled apply the nil-means-true compatibility default.
func (c Cmd) IsVisible() bool { return c.Visible == nil || c.Visible() }
func (c Cmd) IsEnabled() bool { return c.Enabled == nil || c.Enabled() }

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
	SignalAcceptEdits
	SignalBypassPermissions
	SignalFullAccess
	SignalDefault
	SignalDontAsk
	SignalNew
	SignalRetry
	SignalUndo
	// SignalRewind: restore the working tree to the pre-edit snapshot of
	// the last edit-turn AND undo the conversation to that point — the
	// unified code+conversation rewind. Handled by the TUI calling
	// loop.Rewind().
	SignalRewind
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
	// SignalThinkingDisplay carries the user's transcript-mode
	// preference for reasoning blocks. Args = "show" / "hide" /
	// "auto" — TUI's m.thinkingDisplay gets set to this verbatim.
	// Distinct signal (rather than reusing SignalCustomPrompt) so the
	// TUI can route to the Model field directly without queuing the
	// args as a user message.
	SignalThinkingDisplay
)

type Registry struct {
	mu       sync.RWMutex
	cmds     []Cmd
	index    map[string]*Cmd
	reserved map[string]struct{}
}

func NewRegistry() *Registry {
	return &Registry{index: make(map[string]*Cmd), reserved: make(map[string]struct{})}
}

func (r *Registry) Register(c Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.Source == "" {
		if c.Custom {
			c.Source = "custom"
		} else {
			c.Source = "slash"
		}
	}
	if c.Category == "" {
		if c.Custom {
			c.Category = "custom"
		} else {
			c.Category = "built-in"
		}
	}
	r.cmds = append(r.cmds, c)
	r.rebuildIndexLocked()
}

// rebuildIndexLocked makes registration replacement and alias collisions
// deterministic. The latest canonical registration wins; aliases from a
// replaced registration disappear; and an alias can never shadow a real
// canonical name. Rebuilding also refreshes pointers after slice growth.
func (r *Registry) rebuildIndexLocked() {
	latest := make(map[string]int, len(r.cmds))
	for i := range r.cmds {
		latest[r.cmds[i].Name] = i
	}
	r.index = make(map[string]*Cmd, len(r.cmds)*2)
	for name, i := range latest {
		r.index[name] = &r.cmds[i]
	}
	for i := range r.cmds {
		cmd := &r.cmds[i]
		if latest[cmd.Name] != i {
			continue
		}
		for _, alias := range cmd.Aliases {
			if _, canonical := latest[alias]; canonical {
				continue
			}
			r.index[alias] = cmd
		}
	}
}

func (r *Registry) All() []Cmd {
	return r.Catalog()
}

// Catalog returns the effective canonical registrations after applying the
// registry's last-write-wins name and alias rules. This matters for project
// custom commands and runtime registrations, where keeping stale appended
// entries would make discovery disagree with Resolve and Parse.
func (r *Registry) Catalog() []Cmd {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byName := make(map[string]*Cmd, len(r.index))
	for _, cmd := range r.index {
		if cmd == nil || r.index[cmd.Name] != cmd {
			continue
		}
		byName[cmd.Name] = cmd
	}
	out := make([]Cmd, 0, len(byName))
	for _, cmd := range byName {
		cp := *cmd
		cp.Aliases = make([]string, 0, len(cmd.Aliases))
		for _, alias := range cmd.Aliases {
			if r.index[alias] == cmd {
				cp.Aliases = append(cp.Aliases, alias)
			}
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Get(name string) (*Cmd, bool) {
	return r.Resolve(name)
}

// Resolve maps a canonical name or alias to its canonical command.
func (r *Registry) Resolve(name string) (*Cmd, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.index[name]
	return c, ok
}

func (r *Registry) CanonicalName(name string) (string, bool) {
	cmd, ok := r.Resolve(name)
	if !ok {
		return "", false
	}
	return cmd.Name, true
}

// Reserve prevents a user-authored custom command from claiming a name owned
// by another dispatcher (notably the legacy TUI REPL registry). Reserved names
// do not become callable slash commands by themselves.
func (r *Registry) Reserve(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			r.reserved[name] = struct{}{}
		}
	}
}

func (r *Registry) IsReserved(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.reserved[name]
	return ok
}

// RemoveCustom drops every command loaded from commands/*.md and rebuilds the
// alias index. It lets /reload reflect edits and deletions instead of stacking
// stale closures for the lifetime of the process.
func (r *Registry) RemoveCustom() {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := make([]Cmd, 0, len(r.cmds))
	for _, cmd := range r.cmds {
		if !cmd.Custom {
			kept = append(kept, cmd)
		}
	}
	r.cmds = kept
	r.rebuildIndexLocked()
}

// RemoveSource drops every command contributed by one runtime source. MCP
// reconnect uses this before registering the newly discovered prompt set so a
// removed prompt cannot retain a closure to the closed prior server.
func (r *Registry) RemoveSource(source string) {
	if strings.TrimSpace(source) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := make([]Cmd, 0, len(r.cmds))
	for _, cmd := range r.cmds {
		if cmd.Source != source {
			kept = append(kept, cmd)
		}
	}
	r.cmds = kept
	r.rebuildIndexLocked()
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
	c, ok := r.Get(name)
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
		if !c.IsVisible() || !c.IsEnabled() {
			continue
		}
		label := c.Name
		if c.ArgumentHint != "" {
			label += " " + c.ArgumentHint
		}
		if l := len(label); l > maxLen {
			maxLen = l
		}
	}
	for _, c := range all {
		if !c.IsVisible() || !c.IsEnabled() {
			continue
		}
		label := c.Name
		if c.ArgumentHint != "" {
			label += " " + c.ArgumentHint
		}
		fmt.Fprintf(&b, "  /%-*s  %s\n", maxLen, label, c.Description)
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
	// /debug — claude-code parity. Lives in a sibling file (debug.go)
	// because the handler synthesises a multi-paragraph prompt that
	// gets injected as the next user message (SignalCustomPrompt),
	// not a string for the REPL to render directly.
	RegisterDebugCommand(r)

	// /review — same sibling-file pattern (review.go). Builds a
	// code-review prompt against a local diff (staged by default) or
	// a PR via `gh`, then SignalCustomPrompt routes it through the
	// agent loop. Modeled on claude-code's /review and codex's
	// review_request.rs.
	RegisterReviewCommand(r)
	RegisterInitCommand(r)
	RegisterArtifactCommands(r)

	// Core commands
	r.Register(Cmd{Name: "help", Description: "show this help", Handler: func(_ string) (string, Signal) {
		return r.HelpText(), SignalNone
	}})
	r.Register(Cmd{Name: "quit", Aliases: []string{"q", "exit"}, Description: "exit metis", Handler: func(_ string) (string, Signal) {
		return "", SignalQuit
	}})

	// Session commands
	r.Register(Cmd{Name: "clear", Aliases: []string{"new", "reset"}, Description: "start a new session", Category: "session", Handler: func(_ string) (string, Signal) {
		return "(starting new session...)", SignalNew
	}})
	r.Register(Cmd{Name: "clear-history", Description: "clear conversation history without creating a new session", Category: "session", Handler: func(_ string) (string, Signal) {
		return "(history cleared)", SignalClear
	}})
	r.Register(Cmd{Name: "retry", Description: "retry last assistant response", Handler: func(_ string) (string, Signal) {
		return "", SignalRetry
	}})
	r.Register(Cmd{Name: "undo", Aliases: []string{"u"}, Description: "remove last user/assistant exchange (incl. tool calls)", Handler: func(_ string) (string, Signal) {
		// Empty display — the actual confirmation is printed by the
		// runtime once it knows whether anything was undone.
		return "", SignalUndo
	}})
	r.Register(Cmd{Name: "rewind", Description: "restore from a checkpoint (chooser in TUI; latest edit in plain REPL)", ArgumentHint: "[last]", Category: "session", Handler: func(_ string) (string, Signal) {
		// Empty display — the runtime reports what was rewound.
		return "", SignalRewind
	}})
	r.Register(Cmd{Name: "history", Aliases: []string{"hist"}, Description: "show conversation history (Esc / q to close)", Handler: func(_ string) (string, Signal) {
		// Display is empty — the runtime renders the screen directly.
		return "", SignalHistory
	}})
	r.Register(Cmd{Name: "save", Description: "fsync the current session to disk", Handler: func(_ string) (string, Signal) {
		return "", SignalSave
	}})
	r.Register(Cmd{
		Name:        "thinking",
		Description: "control how extended-thinking blocks render: /thinking [show|hide|auto]",
		Handler: func(args string) (string, Signal) {
			arg := strings.TrimSpace(strings.ToLower(args))
			switch arg {
			case "":
				return "usage: /thinking [show|hide|auto]\n" +
					"  show — always render thinking fully expanded\n" +
					"  hide — drop all thinking + redacted_thinking rows\n" +
					"  auto — compact live/history preview (default)", SignalNone
			case "show", "hide", "auto":
				return "", SignalThinkingDisplay
			default:
				return "unknown mode: " + arg + " (expected show|hide|auto)", SignalNone
			}
		},
	})
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
	r.Register(Cmd{Name: "diff", Aliases: []string{"diff-view", "dv"}, Description: "review uncommitted changes in the interactive diff viewer", Category: "git", Handler: func(_ string) (string, Signal) {
		return "", SignalDiff
	}})
	r.Register(Cmd{Name: "doctor", Description: "run a health check (API key, MCP, tools, cwd)", Handler: func(_ string) (string, Signal) {
		return "", SignalDoctor
	}})
	r.Register(Cmd{Name: "stats", Description: "show usage and activity across sessions", Category: "session", Handler: func(_ string) (string, Signal) {
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
	r.Register(Cmd{Name: "export", Description: "export the current conversation to a readable text file", Handler: func(_ string) (string, Signal) {
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
	r.Register(Cmd{Name: "update", Description: "check for a newer Metis release", Category: "system", Handler: func(_ string) (string, Signal) {
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
	r.Register(Cmd{Name: "plan", Aliases: []string{"p"}, Description: "enter plan mode, show the current plan, or edit/update it", ArgumentHint: "[open|description]", Category: "mode", Handler: func(_ string) (string, Signal) {
		return "(mode: plan)", SignalPlan
	}})
	r.Register(Cmd{Name: "acceptEdits", Description: "accept file edits without prompting; ask for other state changes", Handler: func(_ string) (string, Signal) {
		return "(mode: acceptEdits)", SignalAcceptEdits
	}})
	r.Register(Cmd{Name: "default", Aliases: []string{"ask"}, Description: "default mode (ask before state changes)", Handler: func(_ string) (string, Signal) {
		return "(mode: default)", SignalDefault
	}})
	r.Register(Cmd{Name: "bypassPermissions", Aliases: []string{"bypass", "yolo"}, Description: "bypass ordinary approvals while keeping the credential boundary", Handler: func(_ string) (string, Signal) {
		return "(mode: bypassPermissions — WARNING: approves tools)", SignalBypassPermissions
	}})
	r.Register(Cmd{Name: "fullAccess", Aliases: []string{"full"}, Description: "full host access (dangerous; no approvals or sandbox)", Handler: func(_ string) (string, Signal) {
		return "(mode: fullAccess — WARNING: approvals and sandbox disabled)", SignalFullAccess
	}})
	r.Register(Cmd{Name: "dontAsk", Aliases: []string{"deny"}, Description: "deny actions that would otherwise prompt", Handler: func(_ string) (string, Signal) {
		return "(mode: dontAsk)", SignalDontAsk
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
		return "", SignalStatus
	}})
	r.Register(Cmd{Name: "session-info", Aliases: []string{"sid"}, Description: "show current local session id + title + turn count", Category: "session", Handler: func(_ string) (string, Signal) {
		return "", SignalSession
	}})
	r.Register(Cmd{Name: "sessions", Aliases: []string{"ls"}, Description: "list recent saved sessions", Handler: func(_ string) (string, Signal) {
		return "", SignalSessions
	}})
	r.Register(Cmd{Name: "model", Aliases: []string{"m"}, Description: "show current model", Handler: func(_ string) (string, Signal) {
		return "model: " + cfg.Provider.Default, SignalNone
	}})

	// Tools & Skills
	r.Register(Cmd{Name: "tools", Aliases: []string{"t", "toolsets"}, Description: "list registered tools (Read / Bash / Glob / …)", Handler: func(_ string) (string, Signal) {
		return "", SignalTools
	}})
	r.Register(Cmd{Name: "skills", Aliases: []string{"sk"}, Description: "list installed skills under ~/.metis/skills", Handler: func(_ string) (string, Signal) {
		return "", SignalSkills
	}})
	r.Register(Cmd{Name: "memory", Description: "auto-memory: list | show <file> | rm <file> | path", Handler: func(args string) (string, Signal) {
		return handleMemoryCommand(args), SignalNone
	}})
	r.Register(Cmd{Name: "reload", Description: "reload disk-backed skills and custom commands", Handler: func(_ string) (string, Signal) {
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
	r.Register(Cmd{Name: "config", Aliases: []string{"cfg"}, Description: "view and edit Metis settings", Category: "system", Handler: func(_ string) (string, Signal) {
		return "(config: ~/.metis/config.toml)", SignalNone
	}})
	r.Register(Cmd{Name: "version", Aliases: []string{"v"}, Description: "show metis version", Handler: func(_ string) (string, Signal) {
		return "", SignalVersion
	}})
	r.Register(Cmd{Name: "agents", Description: "show active agents", Handler: func(_ string) (string, Signal) {
		return "(agents: single agent mode)", SignalNone
	}})
}
