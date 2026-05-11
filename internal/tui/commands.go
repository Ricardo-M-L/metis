package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/runtime"
)

// REPLCommand is a built-in command that runs directly in the REPL, not via LLM.
type REPLCommand struct {
	Name        string
	Aliases     []string
	Description string
	Handler     func(r *REPL, args string) string
}

type REPLCommandRegistry struct {
	commands []REPLCommand
	index    map[string]*REPLCommand
}

func NewREPLCommandRegistry() *REPLCommandRegistry {
	return &REPLCommandRegistry{index: make(map[string]*REPLCommand)}
}

func (r *REPLCommandRegistry) Register(c REPLCommand) {
	r.commands = append(r.commands, c)
	r.index[c.Name] = &c
	for _, a := range c.Aliases {
		r.index[a] = &c
	}
}

func (r *REPLCommandRegistry) Get(name string) *REPLCommand {
	return r.index[name]
}

func (r *REPLCommandRegistry) All() []REPLCommand {
	return r.commands
}

// BuildREPLCommands creates the full command registry for the REPL.
func BuildREPLCommands() *REPLCommandRegistry {
	r := NewREPLCommandRegistry()

	// === Core ===
	r.Register(REPLCommand{Name: "help", Aliases: []string{"h", "?"}, Description: "show this help", Handler: cmdHelp})
	r.Register(REPLCommand{Name: "quit", Aliases: []string{"q", "exit", "bye"}, Description: "exit metis", Handler: cmdQuit})
	r.Register(REPLCommand{Name: "clear", Aliases: []string{"reset", "cls"}, Description: "clear conversation history", Handler: cmdClear})

	// === Model ===
	r.Register(REPLCommand{Name: "model", Aliases: []string{"m"}, Description: "show or switch model (e.g. model claude-opus-4-7)", Handler: cmdModel})
	r.Register(REPLCommand{Name: "effort", Description: "set reasoning effort: low | medium | high | off", Handler: cmdEffort})
	r.Register(REPLCommand{Name: "fast", Description: "fast mode: on | off | toggle (effort=low + halved tokens)", Handler: cmdFast})
	r.Register(REPLCommand{Name: "theme", Description: "switch color theme: dark | light | dark-daltonized", Handler: cmdTheme})
	r.Register(REPLCommand{Name: "vim", Description: "vim mode: on | off | toggle (modal input — hjkl in NORMAL)", Handler: cmdVim})
	r.Register(REPLCommand{Name: "voice", Description: "voice input: start | stop | toggle (records mic, transcribes via whisper)", Handler: cmdVoice})
	r.Register(REPLCommand{Name: "share", Description: "share session over local HTTP+SSE: start | stop (URL printed for IDE/browser clients)", Handler: cmdShare})

	// === Productivity / inspection ===
	r.Register(REPLCommand{Name: "agents", Description: "list sub-agents currently in flight (Agent tool)", Handler: cmdAgents})
	r.Register(REPLCommand{Name: "files", Description: "show workspace file index (used by @-mention)", Handler: cmdFiles})
	// `/context` deliberately NOT registered as a REPLCommand any more
	// (claude-code parity, 2026-05-11): the slash-signal path
	// (SignalContext → renderContext) produces a much richer grid +
	// per-category breakdown + MCP Loaded/Available split. REPLCommands
	// take precedence over slash signals (slash_e2e_test.go:35), so
	// keeping cmdContext registered would shadow the new renderer.
	// cmdContext itself stays in this file so a future caller that
	// wants a one-line context summary can still reach it.
	r.Register(REPLCommand{Name: "memory", Description: "memory ops: read | write | search | clear", Handler: cmdMemory})
	r.Register(REPLCommand{Name: "recap", Description: "show structural recap of the just-finished turn", Handler: cmdRecap})
	r.Register(REPLCommand{Name: "replay", Description: "re-run the last turn with the same prompt (no edits)", Handler: cmdReplay})
	r.Register(REPLCommand{Name: "tasks", Description: "list session todos (TodoRead)", Handler: cmdTasks})
	r.Register(REPLCommand{Name: "ide", Description: "show IDE / remote bridge status (/share + ACP)", Handler: cmdIDE})
	r.Register(REPLCommand{Name: "review", Description: "request a code review of the current branch / staged changes", Handler: cmdReview})
	r.Register(REPLCommand{Name: "bug", Description: "compose a bug report (opens editor or pastes session log)", Handler: cmdBug})
	r.Register(REPLCommand{Name: "lessons", Description: "show recent turn outcomes from ~/.metis/learned.jsonl (continuous-learning v0)", Handler: cmdLessons})
	r.Register(REPLCommand{Name: "rename", Aliases: []string{"title"}, Description: "rename current session: rename <new title>", Handler: cmdRename})

	// === Git ===
	r.Register(REPLCommand{Name: "git", Description: "git operations: status|diff|commit|log|branch|checkout|stash|fetch", Handler: cmdGit})
	r.Register(REPLCommand{Name: "commit", Description: "git commit (-m 'message')", Handler: cmdGitCommit})
	r.Register(REPLCommand{Name: "diff", Description: "git diff (--cached for staged)", Handler: cmdGitDiff})
	r.Register(REPLCommand{Name: "log", Description: "git log (--stat for details, -n <count>)", Handler: cmdGitLog})
	// Note: Name is "gbr" (git branch). /branch must fall through to
	// slash.Registry's SignalBranch — fork the current session preserving
	// history. This was bug §28.18 — VNC b9-04 showed /branch reporting
	// "git branch: exit status 128: fatal: not a git repository" because
	// BuildREPLCommands' "branch" entry shadowed the slash-registry fork.
	r.Register(REPLCommand{Name: "gbr", Description: "git branch (alias for /gbr; '/branch' forks session)", Handler: cmdGitBranch})
	r.Register(REPLCommand{Name: "checkout", Description: "git checkout <branch>", Handler: cmdGitCheckout})
	r.Register(REPLCommand{Name: "stash", Description: "git stash (push|pop|list)", Handler: cmdGitStash})
	r.Register(REPLCommand{Name: "fetch", Description: "git fetch (--all for all remotes)", Handler: cmdGitFetch})
	// Note: Name is "gst" / "st" (git status), NOT "status".
	// "/status" must fall through to slash.Registry's SignalStatus
	// (session info — turn count / model / mode / sessionID) which the
	// user expects. This was bug §28.13 — VNC r4-20-status.png showed
	// /status reporting "fatal: not a git repository" because BuildREPL-
	// Commands' "status" entry shadowed the slash-registry version.
	r.Register(REPLCommand{Name: "gst", Aliases: []string{"st"}, Description: "git status (alias 'st')", Handler: cmdGitStatus})

	// === Tools ===
	r.Register(REPLCommand{Name: "tools", Aliases: []string{"t"}, Description: "list available tools", Handler: cmdTools})

	// === Skills ===
	r.Register(REPLCommand{Name: "skills", Aliases: []string{"sk"}, Description: "skills: list | install | remove | info | edit | enable | disable | create | search (local)", Handler: cmdSkills})
	r.Register(REPLCommand{Name: "skill", Description: "alias of /skills (singular form; /skill search hits github)", Handler: cmdSkill})

	// === MCP ===
	r.Register(REPLCommand{Name: "mcp", Description: "MCP ops: list | add | remove | start | enable | disable | edit | test | logs | reload", Handler: cmdMCP})
	r.Register(REPLCommand{Name: "cu", Description: "computer-use (metis-cu) ops: enable | disable | status", Handler: cmdCU})

	// === Session ===
	r.Register(REPLCommand{Name: "session", Aliases: []string{"sid"}, Description: "show current session id", Handler: cmdSession})
	r.Register(REPLCommand{Name: "sessions", Aliases: []string{"ls"}, Description: "list saved sessions", Handler: cmdSessions})
	r.Register(REPLCommand{Name: "export", Description: "export current session to a file", Handler: cmdExport})

	// === Permissions ===
	r.Register(REPLCommand{Name: "mode", Description: "show or set permission mode (ask|auto|bypass|deny)", Handler: cmdMode})
	r.Register(REPLCommand{Name: "allow", Description: "allow a tool permanently (e.g. allow Bash)", Handler: cmdAllow})

	// === System ===
	r.Register(REPLCommand{Name: "compact", Description: "force context compaction now", Handler: cmdCompact})
	r.Register(REPLCommand{Name: "config", Aliases: []string{"cfg"}, Description: "open config in $EDITOR", Handler: cmdConfig})
	r.Register(REPLCommand{Name: "env", Description: "show environment info (OS, arch, CPU, memory)", Handler: cmdEnv})
	r.Register(REPLCommand{Name: "doctor", Description: "diagnose metis installation", Handler: cmdDoctor})

	// === Phase C: claude-code parity slashes ===
	r.Register(REPLCommand{Name: "copy", Description: "copy last N assistant replies to clipboard (default 1)", Handler: cmdCopy})
	r.Register(REPLCommand{Name: "commit-push-pr", Aliases: []string{"cpp"}, Description: "git add -A → commit -m <msg> → push → gh pr create --fill", Handler: cmdCommitPushPR})
	r.Register(REPLCommand{Name: "insights", Description: "summarize sessions in last N days: /insights [--days=N] (default 7)", Handler: cmdInsights})
	r.Register(REPLCommand{Name: "output-style", Description: "output verbosity: full | streamlined | minimal", Handler: cmdOutputStyle})
	r.Register(REPLCommand{Name: "break-cache", Description: "explain how to force a fresh prompt-cache write", Handler: cmdBreakCache})
	r.Register(REPLCommand{Name: "security-review", Description: "OWASP-flavored review nudge (analog of /review for security)", Handler: cmdSecurityReview})
	r.Register(REPLCommand{Name: "feedback", Description: "alias of /bug — file a metis issue", Handler: cmdFeedback})

	// === Phase F: discoverability slashes ===
	r.Register(REPLCommand{Name: "thinkback", Description: "show the most recent assistant turn's extended-thinking trace", Handler: cmdThinkback})
	r.Register(REPLCommand{Name: "ultraplan", Description: "deep-plan nudge: bumps effort=high and pre-loads the planning frame", Handler: cmdUltraplan})
	r.Register(REPLCommand{Name: "onboarding", Description: "first-run setup recap (auth, config, /init, talk)", Handler: cmdOnboarding})

	// === Info ===
	r.Register(REPLCommand{Name: "version", Aliases: []string{"v", "--version"}, Description: "show version", Handler: cmdVersion})
	r.Register(REPLCommand{Name: "login", Description: "set up provider credentials (delegates to metis auth login)", Handler: cmdLogin})
	r.Register(REPLCommand{Name: "logout", Description: "remove a stored provider credential", Handler: cmdLogout})
	r.Register(REPLCommand{Name: "init", Description: "scaffold a CLAUDE.md for this repo (claude-code parity)", Handler: cmdInit})
	r.Register(REPLCommand{Name: "statusline", Description: "show + customize the bottom status line", Handler: cmdStatusLine})
	r.Register(REPLCommand{Name: "cost", Description: "show token usage for current session", Handler: cmdCost})
	r.Register(REPLCommand{Name: "tokens", Description: "show last API call's raw token breakdown (input/output/cache)", Handler: cmdTokens})
	r.Register(REPLCommand{Name: "usage", Description: "show API rate limit info", Handler: cmdUsage})
	r.Register(REPLCommand{Name: "debug", Description: "show debug info (session, model, messages, compact)", Handler: cmdDebug})

	// === Debug ===
	r.Register(REPLCommand{Name: "stack", Description: "show panic stack trace", Handler: cmdStack})

	return r
}

// =============================================================================
// CORE
// =============================================================================

func cmdHelp(r *REPL, args string) string {
	seen := make(map[string]bool)
	rows := make([]infoRow, 0, 64)
	for _, c := range r.cmds.All() {
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		rows = append(rows, infoRow{Key: "/" + c.Name, Value: c.Description})
	}
	rows = append(rows, infoRow{Key: "", Value: ""})
	rows = append(rows, infoRow{Key: "", Value: "git/diff/commit/log/branch/checkout — git shortcuts"})
	rows = append(rows, infoRow{Key: "", Value: "skill install <name> — install a skill"})
	rows = append(rows, infoRow{Key: "", Value: "mcp list — list MCP servers"})
	rows = append(rows, infoRow{Key: "", Value: "Or type anything else to chat with the LLM."})
	return renderInfoBox("Metis Commands", rows)
}

func cmdQuit(r *REPL, args string) string {
	r.shouldQuit = true
	return "goodbye!"
}

func cmdClear(r *REPL, args string) string {
	r.Loop.Reset()
	return "(history cleared)"
}

// =============================================================================
// MODEL
// =============================================================================

func cmdModel(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "model: " + r.model
	}
	r.model = args
	r.Loop.Model = args
	return "model set to: " + args
}

// cmdShare starts or stops the localhost HTTP+SSE bridge so an
// external client (IDE extension, browser, mobile companion) can
// cmdAgents lists sub-agents currently dispatched via the Agent tool.
// Empty when no Agent tool calls are in flight (the common case).
func cmdAgents(r *REPL, args string) string {
	// SubAgents lives on Model, not REPL. The REPL handler returns
	// a string only — for richer rendering the chat surface would
	// hook in directly. v1: just echo a hint.
	return "agents: spawn via the Agent tool — list shown as ◇ pills in the status bar"
}

// cmdFiles dumps the @-mention file index so the user can verify
// what's pickable via Tab completion.
func cmdFiles(r *REPL, args string) string {
	// We can't import internal/tui from the REPL handler context
	// directly (already in tui pkg), so call the index helper.
	files := atMentionIndex()
	if len(files) == 0 {
		return "files: index empty (cwd unreadable?)"
	}
	const maxShow = 30
	out := fmt.Sprintf("files: %d indexed (showing first %d)\n", len(files), maxShow)
	for i, f := range files {
		if i >= maxShow {
			out += fmt.Sprintf("  ... %d more\n", len(files)-maxShow)
			break
		}
		out += "  " + f + "\n"
	}
	return strings.TrimRight(out, "\n")
}

// cmdContext shows current context-window usage. Calculates percent
// of max-context tokens consumed by current history + system prompt.
func cmdContext(r *REPL, args string) string {
	// Use LastIn (the input tokens of the most recent API call) rather
	// than session-cumulative total. Context-window pressure is about
	// what was *just* sent to the LLM (system + history + current msg),
	// NOT API spend across the whole session — the latter conflates two
	// different concepts and produces nonsensical percentages like 200%.
	used := r.totalTokens.LastIn()
	maxCtx := 1_000_000
	if r.Loop != nil && r.Loop.Provider != nil {
		if cap := r.Loop.Provider.MaxContextTokens(); cap > 0 {
			maxCtx = cap
		}
	}
	pct := float64(used) / float64(maxCtx) * 100
	rows := []infoRow{
		{Key: "in last call", Value: fmtThousands(used) + " tokens"},
		{Key: "max", Value: "~" + fmtThousands(maxCtx) + " tokens"},
		{Key: "utilization", Value: fmt.Sprintf("%.1f%%", pct)},
	}
	if pct >= 75 {
		rows = append(rows, infoRow{Key: "", Value: ""})
		rows = append(rows, infoRow{Key: "", Value: "context pressure high — /compact to reclaim"})
	}
	return renderInfoBox("Context Window", rows)
}

// cmdMemory is a thin wrapper that delegates to the memory tool.
// Real ops happen via the agent loop's Memory tool — this command
// just gives the user a "what's in memory?" quick view.
func cmdMemory(r *REPL, args string) string {
	// Show the live memory state — block names + sizes + a brief render.
	// Earlier this returned only a usage hint, which felt like the
	// command was broken. The Memory tool (called by the LLM) handles
	// CRUD; this slash is the read-only "what's in memory right now"
	// view, equivalent to opening ~/.metis/memories/MEMORY.md by hand.
	if r.Loop == nil || r.Loop.Memory == nil {
		return "memory: not initialized (this is a metis bug — please report)"
	}
	core := r.Loop.Memory.Core()
	if core == nil {
		return "memory: core block store unavailable"
	}
	stats := core.Stats()
	if len(stats) == 0 {
		return renderInfoBox("Memory", []infoRow{
			{Key: "", Value: "no blocks (empty)"},
			{Key: "", Value: ""},
			{Key: "", Value: "Memory tool: action=read|search|add|replace|remove|archive|stats"},
		})
	}
	// Stable order: sort block names alphabetically.
	names := make([]string, 0, len(stats))
	for k := range stats {
		names = append(names, k)
	}
	sort.Strings(names)
	rows := make([]infoRow, 0, len(stats)+3)
	for _, name := range names {
		s := stats[name]
		rows = append(rows, infoRow{
			Key:   name,
			Value: fmt.Sprintf("%d / %d chars", s.Used, s.Limit),
			Hint:  fmt.Sprintf("%.1f%%", s.Pct),
		})
	}
	rows = append(rows, infoRow{Key: "", Value: ""})
	rows = append(rows, infoRow{Key: "", Value: "Memory tool: action=read|search|add|replace|remove|archive|stats"})
	return renderInfoBox(fmt.Sprintf("Memory · %d block(s)", len(stats)), rows)
}

// cmdRecap re-emits the most recent turn-end recap line. Useful when
// scrolled away.
func cmdRecap(r *REPL, args string) string {
	hist := r.Loop.History()
	if len(hist) == 0 {
		return "recap: no turns yet"
	}
	return "recap: scroll up to see ※ recap line of the last turn (auto-generated at turn end)"
}

// cmdReplay re-runs the last user prompt by appending the same text
// to the loop. Caller (chat surface) interprets and submits.
func cmdReplay(r *REPL, args string) string {
	hist := r.Loop.History()
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == llm.RoleUser && len(hist[i].Content) > 0 {
			text := hist[i].Content[0].Text
			return "replay: copy the prompt and re-submit:\n  " + text
		}
	}
	return "replay: no prior user prompt found"
}

// cmdTasks reads the TodoRead store for the current session and
// renders the items inline.
func cmdTasks(r *REPL, args string) string {
	if r.SessionID == "" {
		return "tasks: no session id"
	}
	n := tasksRunningCount(r.SessionID)
	if n == 0 {
		return "tasks: no in-progress / pending todos"
	}
	return fmt.Sprintf("tasks: %d in flight (TodoWrite from agent loop populates this)", n)
}

// cmdIDE shows IDE / remote-bridge status so the user knows whether
// /share is running and what address external clients should hit.
func cmdIDE(r *REPL, args string) string {
	if addr := bridgeCurrentAddr(); addr != "" {
		return "ide: bridge running on http://" + addr
	}
	return "ide: bridge off — start with /share to expose a localhost SSE endpoint"
}

// cmdReview emits a system-prompt-style nudge asking the model to
// review staged changes. The actual review happens in the next turn
// via Bash + LLM analysis.
func cmdReview(r *REPL, args string) string {
	target := strings.TrimSpace(args)
	if target == "" {
		target = "staged changes (git diff --cached)"
	}
	return "review: prompt the model with: 'review " + target + " for bugs, style, security'"
}

// cmdBug opens / collects info for a metis bug report.
func cmdBug(r *REPL, args string) string {
	return "bug: file at https://github.com/Ricardo-M-L/metis/issues — include `metis version` + recent transcript"
}

// cmdRename writes a new title onto the current session. Persists
// via session.Store.SetTitle which append-only rewrites the header
// row in the JSONL file. /sessions and /session both reflect the
// new title on the next read.
func cmdRename(r *REPL, args string) string {
	title := strings.TrimSpace(args)
	if title == "" {
		return "rename: usage `/rename <new title>` (current title shown via /session)"
	}
	if r.Session == nil || r.SessionID == "" {
		return "rename: no active session store"
	}
	if err := r.Session.SetTitle(r.SessionID, title); err != nil {
		return "rename: " + err.Error()
	}
	return "rename: session title set to " + title
}

// cmdLessons surfaces the continuous-learning log — a structural
// record of past turns (tools, files, outcomes). The log itself is
// populated by the chat surface at turn-end via runtime.AppendLearned.
func cmdLessons(r *REPL, args string) string {
	n := 10
	if v := strings.TrimSpace(args); v != "" {
		// allow /lessons 25
		if parsed, err := fmt.Sscanf(v, "%d", &n); err == nil && parsed == 1 && n > 0 && n < 1000 {
			// ok
		} else {
			n = 10
		}
	}
	return runtime.SummarizeLearned(n)
}

// follow the active session. claude-code's teleport equivalent —
// minus the private protocol, plus actual usability for non-claude-
// shipped tooling.
func cmdShare(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "stop", "off":
		stopBridge()
		return "share: stopped"
	case "", "start", "on":
		if cur := bridgeCurrentAddr(); cur != "" {
			return "share: already running on http://" + cur
		}
		// Make a buffered channel so a quick burst of POSTs doesn't
		// stall the HTTP handler. Drained by the chat surface (TODO
		// once Model wires it; today the channel is registered but
		// the chat surface doesn't yet consume — read-only mode is
		// useful enough for v1).
		ch := make(chan string, 8)
		addr, err := startBridge(ch)
		if err != nil {
			return "share: " + err.Error()
		}
		return fmt.Sprintf("share: http://%s — endpoints: /transcript /events /message /health", addr)
	}
	return "share: unknown arg " + args
}

// cmdVoice starts or stops mic recording. On stop, the captured WAV
// is uploaded to OpenAI's Whisper endpoint and the transcription
// becomes the next status message — the user can paste it into the
// input via Ctrl+V or just read it. Inline insertion into the
// textarea is handled by voiceCallback wired in NewModel; the
// command path here is the explicit "I want voice" trigger.
func cmdVoice(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "", "toggle", "t":
		if voiceActive() {
			text, err := stopVoiceRecording()
			if err != nil {
				return "voice: " + err.Error()
			}
			return "voice: " + text
		}
		if err := startVoiceRecording(); err != nil {
			return "voice: " + err.Error()
		}
		return "voice: recording (max 30s — call /voice again to stop)"
	case "start", "on":
		if err := startVoiceRecording(); err != nil {
			return "voice: " + err.Error()
		}
		return "voice: recording (max 30s)"
	case "stop", "off":
		text, err := stopVoiceRecording()
		if err != nil {
			return "voice: " + err.Error()
		}
		return "voice: " + text
	}
	// Echoing back arbitrary user text into the warning ("unknown arg
	// 这个命令怎么使用") was confusing — the user thought `/voice` couldn't
	// answer free-form questions. Show the actual usage instead. claude-
	// code's voice command is a pure toggle; metis exposes start/stop
	// as well, hence the extra hint.
	return "voice: usage — /voice (toggle) | /voice start | /voice stop"
}

// cmdVim toggles modal-input ("vim") mode. NORMAL intercepts hjkl /
// 0/$/x for cursor + edit; INSERT behaves like the default. State is
// package-level (vimModeState) so this REPL handler can flip it
// without holding a Model reference. /vim status shows current mode.
func cmdVim(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "":
		state := "off"
		if vimModeState != vimOff {
			state = vimModeState
		}
		return "vim mode: " + state + " — use: vim on | vim off | vim toggle"
	case "on", "true", "1", "yes":
		vimModeState = vimInsert
		return "vim mode: on (INSERT) — ESC to NORMAL, hjkl to move"
	case "off", "false", "0", "no":
		vimModeState = vimOff
		return "vim mode: off"
	case "toggle", "t":
		if vimModeState == vimOff {
			vimModeState = vimInsert
			return "vim mode: on (INSERT)"
		}
		vimModeState = vimOff
		return "vim mode: off"
	}
	return "vim: unknown arg " + args + " — use: on | off | toggle"
}

// cmdTheme switches the chat-surface color palette. Without args it
// lists available themes + the current selection. With an arg it
// tries to switch — unknown names get a friendly fallback hint.
func cmdTheme(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" {
		names := ThemeNames()
		return fmt.Sprintf("theme: %s — available: %s, auto:<provider>",
			currentTheme.Name, strings.Join(names, ", "))
	}
	// `/theme auto` — retint the active palette using the current
	// provider id (anthropic / openai / kimi / ...). Falls back to
	// the active theme unchanged when the provider is unknown.
	if arg == "auto" {
		provider := ""
		if r != nil && r.Loop != nil && r.Loop.Provider != nil {
			provider = r.Loop.Provider.Name()
		}
		ApplyProviderTint(provider)
		initStyles()
		return "theme: " + currentTheme.Name
	}
	if strings.HasPrefix(arg, "auto:") {
		provider := strings.TrimPrefix(arg, "auto:")
		ApplyProviderTint(provider)
		initStyles()
		return "theme: " + currentTheme.Name
	}
	if name := SwitchTheme(arg); name != "" {
		return "theme: " + name
	}
	return fmt.Sprintf("unknown theme %q — available: %s, auto, auto:<provider>",
		arg, strings.Join(ThemeNames(), ", "))
}

// cmdEffort sets the reasoning intensity dial used for subsequent turns.
// Empty arg → show current state in a box with the option list; non-empty
// arg → apply the new setting and confirm with a one-liner.
//
// The box layout was added after the user noted slash output read as
// flat plain text next to claude-code's bordered panels — the readback
// path now matches /context, /cost, /memory, /status formatting.
func cmdEffort(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" {
		cur := string(r.Loop.Effort)
		if cur == "" {
			cur = "default (provider decides)"
		}
		return renderInfoBox("Reasoning Effort", []infoRow{
			{Key: "current", Value: cur},
			{Key: "", Value: ""},
			{Key: "/effort low", Value: "small thinking budget, faster"},
			{Key: "/effort medium", Value: "balanced"},
			{Key: "/effort high", Value: "deep reasoning, slower & costlier"},
			{Key: "/effort off", Value: "clear override (use provider default)"},
		})
	}
	if arg == "off" || arg == "default" || arg == "none" {
		r.Loop.Effort = llm.EffortDefault
		return "effort: cleared (using provider default)"
	}
	e, ok := llm.ParseEffort(arg)
	if !ok {
		return "(unknown effort: " + args + " — use: effort low|medium|high|off)"
	}
	r.Loop.Effort = e
	switch e {
	case llm.EffortLow:
		return "effort: low — small thinking budget, faster"
	case llm.EffortMedium:
		return "effort: medium — balanced"
	case llm.EffortHigh:
		return "effort: high — deep reasoning, slower & costlier"
	}
	return "effort: " + string(e)
}

// cmdFast toggles a per-request "be quick" override. While on it forces
// effort=low and halves the request's max_tokens, regardless of /effort.
func cmdFast(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "":
		state := "off"
		if r.Loop.Fast {
			state = "on"
		}
		return "fast mode: " + state + " — use: fast on | fast off | fast toggle"
	case "on", "true", "1", "yes":
		r.Loop.Fast = true
		return "fast mode: on (effort=low, max_tokens halved for next turn)"
	case "off", "false", "0", "no":
		r.Loop.Fast = false
		return "fast mode: off"
	case "toggle", "t":
		r.Loop.Fast = !r.Loop.Fast
		if r.Loop.Fast {
			return "fast mode: on"
		}
		return "fast mode: off"
	}
	return "(fast mode — use: fast on | fast off | fast toggle)"
}

// =============================================================================
// GIT
// =============================================================================

func cmdGit(r *REPL, args string) string {
	return runGitCmd(args, "status")
}

func cmdGitStatus(r *REPL, args string) string {
	return runGitCmd(args, "status")
}

func cmdGitDiff(r *REPL, args string) string {
	staged := strings.Contains(args, "--cached") || strings.Contains(args, "-c")
	gitArgs := []string{"diff"}
	if staged {
		gitArgs = []string{"diff", "--cached"}
	}
	return runGitCmd(strings.TrimSpace(args), gitArgs...)
}

func cmdGitLog(r *REPL, args string) string {
	stat := strings.Contains(args, "--stat")
	n := "10"
	if idx := strings.Index(args, "-n"); idx >= 0 {
		parts := strings.Fields(strings.TrimSpace(args[idx+2:]))
		if len(parts) > 0 {
			n = parts[0]
		}
	}
	if stat {
		return runGitCmd(args, "log", "--stat", "-n", n)
	}
	return runGitCmd(args, "log", "-n", n, "--oneline")
}

func cmdGitBranch(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if strings.Contains(args, "-a") {
		return runGitCmd(args, "branch", "-a")
	}
	if strings.HasPrefix(args, "-c ") {
		name := strings.TrimSpace(strings.TrimPrefix(args, "-c"))
		if name == "" {
			return "(branch name required: branch -c <name>)"
		}
		return runGitCmd(args, "branch", name)
	}
	return runGitCmd(args, "branch")
}

func cmdGitCheckout(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "(branch name required: checkout <branch>)"
	}
	return runGitCmd(args, "checkout", args)
}

func cmdGitStash(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "push" {
		msg := "metis stash " + time.Now().Format(time.RFC3339)
		return runGitCmd(args, "stash", "push", "-m", msg)
	}
	if args == "pop" {
		return runGitCmd(args, "stash", "pop")
	}
	if args == "list" {
		return runGitCmd(args, "stash", "list")
	}
	return "(stash: use: stash push | stash pop | stash list)"
}

func cmdGitFetch(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "--all" || args == "-a" {
		return runGitCmd(args, "fetch", "--all")
	}
	if args != "" {
		return runGitCmd(args, "fetch", args)
	}
	return runGitCmd(args, "fetch")
}

func cmdGitCommit(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	msg := extractFlag(args, "-m")
	if msg == "" {
		diff, _ := runGitCmdOut("diff", "--staged")
		if len(diff) == 0 {
			return "(nothing staged to commit)"
		}
		return "(no commit message; use: commit -m 'your message')"
	}
	return runGitCmd(args, "commit", "-m", msg)
}

func runGitCmd(rest string, gitArgs ...string) string {
	out, err := runGitCmdOut(gitArgs...)
	if err != nil {
		return "git " + strings.Join(gitArgs, " ") + ": " + err.Error()
	}
	if len(out) == 0 {
		return "(ok)"
	}
	return string(out)
}

func runGitCmdOut(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		return nil, fmt.Errorf("%s: %s", err, stderr.String())
	}
	return out, err
}

func extractFlag(args, flag string) string {
	parts := strings.Split(args, flag)
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(parts[1]), " ", 2)[0])
}

// =============================================================================
// TOOLS
// =============================================================================

func cmdTools(r *REPL, args string) string {
	return renderToolsList(r.Loop)
}

// =============================================================================
// SKILLS
// =============================================================================

// cmdSkills lives in cmd_skills_extra.go (full subcommand dispatcher).
// /skill (singular) keeps the historical narrow surface: list / install /
// uninstall / search-on-github. Users typing the plural form get the
// extended Phase-A surface.

func cmdSkill(r *REPL, args string) string {
	parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
	sub := strings.TrimSpace(parts[0])
	switch sub {
	case "list", "ls":
		return r.handleSkillList()
	case "install":
		if len(parts) < 2 {
			return "usage: skill install <name>"
		}
		return r.handleSkillInstall(strings.TrimSpace(parts[1]))
	case "uninstall", "remove", "rm":
		if len(parts) < 2 {
			return "usage: skill uninstall <name>"
		}
		return r.handleSkillUninstall(strings.TrimSpace(parts[1]))
	case "search":
		if len(parts) < 2 {
			return "usage: skill search <query>"
		}
		return r.handleSkillSearch(strings.TrimSpace(parts[1]))
	case "":
		return r.handleSkillList()
	default:
		return "skill: unknown '" + sub + "'. usage: skill list | install <name> | uninstall <name> | search <query>"
	}
}

// =============================================================================
// MCP
// =============================================================================

func cmdMCP(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return r.handleMCPList()
	}
	parts := strings.Fields(args)
	sub := parts[0]
	switch sub {
	case "list", "ls":
		return r.handleMCPList()
	case "add":
		// Usage: mcp add <name> <command> [args...]
		if len(parts) < 3 {
			return "usage: mcp add <name> <command> [args...]"
		}
		return r.handleMCPAdd(parts[1], parts[2], parts[3:])
	case "remove", "rm":
		if len(parts) < 2 {
			return "usage: mcp remove <name>"
		}
		return r.handleMCPRemove(parts[1])
	case "start":
		if len(parts) < 2 {
			return "usage: mcp start <name>"
		}
		return r.handleMCPStart(parts[1])
	case "enable":
		if len(parts) < 2 {
			return "usage: mcp enable <name>"
		}
		return r.handleMCPEnable(parts[1])
	case "disable":
		if len(parts) < 2 {
			return "usage: mcp disable <name>"
		}
		return r.handleMCPDisable(parts[1])
	case "edit":
		// `/mcp edit` (no name) opens mcp.toml whole; `/mcp edit <name>`
		// pre-validates the name first so a typo doesn't waste the
		// editor round-trip.
		var name string
		if len(parts) >= 2 {
			name = parts[1]
		}
		return r.handleMCPEdit(name)
	case "test":
		if len(parts) < 2 {
			return "usage: mcp test <name>"
		}
		return r.handleMCPTest(parts[1])
	case "logs":
		if len(parts) < 2 {
			return "usage: mcp logs <name>"
		}
		return r.handleMCPLogs(parts[1])
	case "reload":
		return r.handleMCPReload()
	}
	return "mcp: unknown '" + sub + "'. usage: mcp list | add <name> <cmd> [args] | remove <name> |\n" +
		"  start <name> | enable <name> | disable <name> | edit [<name>] | test <name> | logs <name> | reload"
}

func (r *REPL) handleMCPList() string {
	reg, err := runtime.LoadMCP()
	if err != nil {
		return "mcp: " + err.Error()
	}
	if len(reg.Servers) == 0 {
		return "(no MCP servers — try `mcp add <name> <command>`)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d MCP server(s) registered (in %s):\n", len(reg.Servers), runtime.MCPPath())
	for _, s := range reg.Servers {
		state := "enabled"
		if s.Disabled {
			state = "disabled"
		}
		fmt.Fprintf(&b, "  %-12s %s  [%s] %s %s\n", s.Name, state, s.Command,
			"args:", strings.Join(s.Args, " "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r *REPL) handleMCPAdd(name, command string, args []string) string {
	reg, err := runtime.LoadMCP()
	if err != nil {
		return "mcp: " + err.Error()
	}
	existed := runtime.FindMCPServer(reg, name) != nil
	if err := runtime.AddMCPServer(reg, name, command, args); err != nil {
		return "mcp: " + err.Error()
	}
	if err := runtime.SaveMCP(reg); err != nil {
		return "mcp: save: " + err.Error()
	}
	// User-confusion guardrail: if the "command" they typed looks like
	// a URL, they almost certainly meant to register an HTTP MCP server
	// — but `/mcp add` only knows the stdio path. Without a warning we
	// silently store `command = "https://..."` and the user wonders why
	// "no such file or directory" comes back on launch. Same intent as
	// Claude Code's looksLikeUrl check in addCommand.ts.
	if hint := looksLikeURLHint(command, name); hint != "" {
		base := "(replaced MCP server: " + name + ")"
		if !existed {
			base = "(added MCP server: " + name + " — run `mcp start " + name + "` to spawn)"
		}
		return base + "\n" + hint
	}
	if existed {
		return "(replaced MCP server: " + name + ")"
	}
	return "(added MCP server: " + name + " — run `mcp start " + name + "` to spawn)"
}

// looksLikeURLHint returns a non-empty warning string when `command`
// looks like a URL that the user probably wanted to register as an
// HTTP server, not a stdio command. Returns "" when the command is
// almost certainly a real binary path / npx invocation / etc.
func looksLikeURLHint(command, name string) string {
	c := strings.ToLower(command)
	switch {
	case strings.HasPrefix(c, "http://"),
		strings.HasPrefix(c, "https://"),
		strings.HasSuffix(c, "/sse"),
		strings.HasSuffix(c, "/mcp"):
		// fall through
	default:
		return ""
	}
	return "  warning: command looks like a URL — `/mcp add` only registers stdio servers.\n" +
		"  for an HTTP MCP server, edit ~/.metis/mcp.toml directly:\n" +
		"    [[servers]]\n" +
		"    name = \"" + name + "\"\n" +
		"    url  = \"" + command + "\"\n" +
		"    [servers.headers]\n" +
		"      Authorization = \"Bearer ${YOUR_TOKEN}\""
}

func (r *REPL) handleMCPRemove(name string) string {
	reg, err := runtime.LoadMCP()
	if err != nil {
		return "mcp: " + err.Error()
	}
	if !runtime.RemoveMCPServer(reg, name) {
		return "(no MCP server named: " + name + ")"
	}
	if err := runtime.SaveMCP(reg); err != nil {
		return "mcp: save: " + err.Error()
	}
	return "(removed MCP server: " + name + ")"
}

// handleMCPStart launches a registered MCP server and grafts its tools onto
// the live registry. Returned tools are visible to the LLM on the next turn.
//
// We intentionally don't track the spawned *Server pointer here — REPL has
// no shutdown hook today; future Round can plumb cleanup via the runtime
// struct that owns mcp child processes. For now, the OS reaps subprocesses
// on metis exit.
func (r *REPL) handleMCPStart(name string) string {
	reg, err := runtime.LoadMCP()
	if err != nil {
		return "mcp: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv, err := runtime.LaunchMCPServer(ctx, reg, name, r.Loop.Registry)
	if err != nil {
		return "mcp start: " + err.Error()
	}
	tools := srv.Tools()
	return fmt.Sprintf("(MCP server %q started · %d tools registered)", name, len(tools))
}

// =============================================================================
// SESSION
// =============================================================================

func cmdSession(r *REPL, args string) string {
	return renderCurrentSession(r.Session, r.SessionID, r.Loop, r.model, string(r.Gate.Mode()))
}

func cmdSessions(r *REPL, args string) string {
	return renderSessionsList(r.Session, 20)
}

func cmdExport(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	var path string
	if args == "" {
		// Default destination: ~/.metis/exports/session-<id>-<unix>.jsonl.
		// Earlier this returned a usage hint, which felt like the command
		// was broken — users expect "/export" to actually export. Pass an
		// explicit path only when you want to override.
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".metis", "exports")
		_ = os.MkdirAll(dir, 0o755)
		sid := r.SessionID
		if sid == "" {
			sid = "untitled"
		}
		path = filepath.Join(dir, fmt.Sprintf("session-%s-%d.jsonl", sid, time.Now().Unix()))
	} else {
		path = args
	}
	hist := r.Loop.History()
	data, err := json.MarshalIndent(hist, "", "  ")
	if err != nil {
		return "export failed: " + err.Error()
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "export failed: " + err.Error()
	}
	return "exported " + fmt.Sprintf("%d messages", len(hist)) + " to " + path
}

// =============================================================================
// PERMISSIONS
// =============================================================================

func cmdMode(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "mode: " + string(r.Gate.Mode())
	}
	r.Gate.SetMode(permission.Mode(args))
	return "mode set to: " + args
}

func cmdAllow(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "usage: allow <tool-name> (e.g. allow Bash)"
	}
	r.Gate.AppendRules(permission.Rule{
		Tool: args, Verb: permission.DecisionAllow, Source: "interactive",
	})
	return "allowed: " + args
}

// =============================================================================
// SYSTEM
// =============================================================================

func cmdCompact(r *REPL, args string) string {
	// Force a compact NOW, regardless of ShouldCompact threshold. This
	// was bug §28.20 — earlier this returned only a hint string, while
	// the slash registry's SignalCompact path also no-op'd. Now we
	// actually invoke Compactor.Compact on the live history.
	if r.Loop == nil || r.Loop.Compactor == nil {
		return "compact: compactor not configured (provider may not support it)"
	}
	hist := r.Loop.History()
	before := len(hist)
	if before <= 2 {
		return fmt.Sprintf("compact: nothing to compact (only %d messages)", before)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	compacted, err := r.Loop.Compactor.Compact(ctx, hist)
	if err != nil {
		return "compact failed: " + err.Error()
	}
	if len(compacted) >= before {
		return fmt.Sprintf("compact: no reduction (%d → %d messages)", before, len(compacted))
	}
	r.Loop.Restore(compacted)
	return fmt.Sprintf("compact: %d → %d messages (saved %d)", before, len(compacted), before-len(compacted))
}

func cmdConfig(r *REPL, args string) string {
	cfgPath := filepath.Join(config.Home(), "config.toml")
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, cfgPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
	return "(config updated — restart metis for changes to take effect)"
}

func cmdEnv(r *REPL, args string) string {
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("  OS:       %s/%s\n", goruntime.GOOS, goruntime.GOARCH))
	b.WriteString(fmt.Sprintf("  Go:       %s\n", goruntime.Version()))
	b.WriteString(fmt.Sprintf("  CPUs:     %d\n", goruntime.NumCPU()))
	b.WriteString(fmt.Sprintf("  GoMem:    %.1f MB\n", float64(m.Alloc)/1e6))
	b.WriteString(fmt.Sprintf("  GoHeap:   %.1f MB\n", float64(m.Sys)/1e6))
	b.WriteString(fmt.Sprintf("  CWD:      %s\n", cwd()))
	return b.String()
}

func cwd() string {
	dir, _ := os.Getwd()
	return dir
}

func cmdDoctor(r *REPL, args string) string {
	check := func(path, label string) infoRow {
		if _, err := os.Stat(path); err == nil {
			return infoRow{Key: label, Value: path, Hint: "ok"}
		}
		return infoRow{Key: label, Value: path, Hint: "missing"}
	}
	rows := []infoRow{
		check(filepath.Join(config.Home(), "config.toml"), "config"),
		check(filepath.Join(config.Home(), "skills"), "skills dir"),
		check(filepath.Join(config.Home(), "sessions"), "session dir"),
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
		if v := os.Getenv(k); v != "" {
			rows = append(rows, infoRow{Key: k, Value: "set", Hint: v[:4] + "…"})
		}
	}
	gitHint := "not found"
	if _, err := exec.LookPath("git"); err == nil {
		gitHint = "ok"
	}
	rows = append(rows, infoRow{Key: "git", Value: gitHint})
	goHint := "not found"
	if _, err := exec.LookPath("go"); err == nil {
		goHint = "ok"
	}
	rows = append(rows, infoRow{Key: "go", Value: goHint})
	rows = append(rows, infoRow{Key: "binary", Value: os.Args[0]})
	return renderInfoBox("Metis Doctor", rows)
}

// =============================================================================
// INFO
// =============================================================================

func cmdVersion(r *REPL, args string) string {
	return renderVersion()
}

// cmdLogin / cmdLogout — slash entries that delegate to the top-level
// `metis auth` family. The auth wizard runs its own bubbletea program
// which can't safely re-enter the chat surface's program, so we surface
// a clear "exit + run from shell" hint rather than crash. Adding these
// slashes lets users discover credential setup from inside chat (CC
// parity — they have /login and /logout in the slash registry too).
func cmdLogin(r *REPL, args string) string {
	provider := strings.TrimSpace(args)
	if provider == "" {
		return "login: exit chat (Ctrl-D or /quit) and run `metis auth login`\n" +
			"  · interactive wizard picks provider + stores key in ~/.metis/auth.json\n" +
			"  · or one-shot: `metis auth login <provider>` (anthropic | openai | gemini)"
	}
	return "login: exit chat and run `metis auth login " + provider + "`\n" +
		"  · sets the credential for " + provider + " then resume metis"
}

func cmdLogout(r *REPL, args string) string {
	provider := strings.TrimSpace(args)
	if provider == "" {
		return "logout: exit chat and run `metis auth logout <provider>`\n" +
			"  · removes that provider's stored key from ~/.metis/auth.json\n" +
			"  · use `metis auth list` first to see what's stored"
	}
	return "logout: exit chat and run `metis auth logout " + provider + "`"
}

// cmdInit scaffolds a CLAUDE.md in cwd for this project. Cross-tool
// convention (claude-code, Cursor, Aider all read it). Use --force to
// overwrite. Detects project type from sentinel files (go.mod /
// package.json / Cargo.toml / pyproject.toml) and pre-fills build /
// test commands. The user is expected to fill in the conventions
// section by hand.
func cmdInit(r *REPL, args string) string {
	output, _ := runInitCommand(args)
	return output
}

// cmdStatusLine documents the current status-line layout and points at
// the config knobs that customize it. Full customization (preset
// layouts, custom format strings) is a separate refactor — for now
// this is the discoverability hook so users can find the configuration
// surface from inside chat (CC parity for /statusline as a discovery
// command, even if the editing UX is config-file rather than TUI).
func cmdStatusLine(r *REPL, args string) string {
	return renderInfoBox("Status Line", []infoRow{
		{Key: "current layout", Value: "elapsed · effort · mode · subagents · tokens"},
		{Key: "", Value: ""},
		{Key: "Customize via:", Value: ""},
		{Key: "  ~/.metis/config.toml", Value: "[ui] section — set theme + statusline knobs"},
		{Key: "  METIS_THEME env", Value: "override theme without editing the file"},
		{Key: "", Value: ""},
		{Key: "Available pieces:", Value: ""},
		{Key: "  elapsed", Value: "wall-clock for the current turn"},
		{Key: "  effort", Value: "reasoning effort glyph (low/med/high/off)"},
		{Key: "  mode", Value: "permission mode (auto/ask/bypass/plan/deny)"},
		{Key: "  subagents", Value: "active Agent tool sub-agent pills"},
		{Key: "  tokens", Value: "context-window load + percent (right-aligned)"},
		{Key: "  spinner", Value: "↑in / ↓out per-turn breakdown"},
		{Key: "", Value: ""},
		{Key: "", Value: "Full preset picker (/statusline cycle) — coming in a future release."},
	})
}

func cmdCost(r *REPL, args string) string {
	// Mirror renderCost (render_info.go) so the REPL fast-path and the
	// slash.Registry path produce identical output.
	in := r.totalTokens.in
	out := r.totalTokens.out
	cacheCreate := r.totalTokens.CacheCreate()
	cacheRead := r.totalTokens.CacheRead()
	total := in + out
	priceIn, priceOut := guessPriceUSDPerM(r.Loop.Model)
	// Cache reads bill at 10% of fresh input on Anthropic; cache_create
	// at 125%. Estimate the savings: (read × 0.9 × priceIn) is how much
	// cheaper this session was versus paying full input rate. Useful
	// to show users "your /memory + addendum sectioning earned you $X".
	costUSD := float64(in)*priceIn/1_000_000 + float64(out)*priceOut/1_000_000
	cacheSavingsUSD := float64(cacheRead) * 0.9 * priceIn / 1_000_000
	rows := []infoRow{
		{Key: "input tokens", Value: fmtThousands(in)},
		{Key: "output tokens", Value: fmtThousands(out)},
		{Key: "cache_read", Value: fmtThousands(cacheRead), Hint: "served from prompt cache"},
		{Key: "cache_create", Value: fmtThousands(cacheCreate), Hint: "written to prompt cache"},
		{Key: "total tokens", Value: fmtThousands(total)},
		{Key: "cache hit rate", Value: fmt.Sprintf("%.1f%%", r.totalTokens.CacheHitRate()*100)},
		{Key: "est. cost", Value: fmt.Sprintf("$%.4f", costUSD), Hint: "real billing on provider"},
		{Key: "est. cache savings", Value: fmt.Sprintf("$%.4f", cacheSavingsUSD), Hint: "vs. paying full input rate for cache_read"},
	}
	return renderInfoBox("Session Cost · "+r.Loop.Model, rows)
}

func cmdUsage(r *REPL, args string) string {
	return "(rate limit info: depends on your API provider — check your provider dashboard)"
}

// cmdTokens surfaces the most recent API call's raw token breakdown +
// the session cumulative breakdown. The user reported the bottom-right
// percentage looking inflated and asked "is this counting wrong?";
// this command lets them see EXACTLY what the provider returned for
// each field, so they can disambiguate "metis math bug" from "provider
// reported it that way" from "long history naturally inflates input".
//
// Layout matches /cost so the two read consistently.
func cmdTokens(r *REPL, args string) string {
	t := &r.totalTokens
	rows := []infoRow{
		{Key: "── most recent API call ──", Value: ""},
		{Key: "input_tokens", Value: fmtThousands(t.LastIn()), Hint: "fresh tokens this round"},
		{Key: "output_tokens", Value: fmtThousands(t.LastOut()), Hint: "tokens the model produced"},
		{Key: "cache_creation", Value: fmtThousands(t.LastCacheCreate()), Hint: "tokens written to prompt cache"},
		{Key: "cache_read", Value: fmtThousands(t.LastCacheRead()), Hint: "tokens served from prompt cache"},
		{Key: "── derived ──", Value: ""},
		{Key: "per-turn cost", Value: fmtThousands(t.LastTotal()), Hint: "input + output (spinner row)"},
		{Key: "context load", Value: fmtThousands(t.ContextUsage()), Hint: "input + cache_create + cache_read (bottom-right)"},
		{Key: "── session cumulative ──", Value: ""},
		{Key: "input total", Value: fmtThousands(t.Input())},
		{Key: "output total", Value: fmtThousands(t.Output())},
		{Key: "cache_create total", Value: fmtThousands(t.CacheCreate())},
		{Key: "cache_read total", Value: fmtThousands(t.CacheRead())},
		{Key: "cache hit rate", Value: fmt.Sprintf("%.1f%%", t.CacheHitRate()*100), Hint: "cache_read / (read+create+input)"},
	}
	return renderInfoBox("Token Breakdown", rows)
}

func cmdDebug(r *REPL, args string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("  session:  %s\n", r.SessionID))
	b.WriteString(fmt.Sprintf("  model:    %s\n", r.Loop.Model))
	b.WriteString(fmt.Sprintf("  messages: %d\n", len(r.Loop.History())))
	b.WriteString(fmt.Sprintf("  compact:  %v\n", r.Loop.Compactor != nil))
	if r.skillDir != "" {
		b.WriteString(fmt.Sprintf("  skillDir: %s\n", r.skillDir))
	}
	b.WriteString(fmt.Sprintf("  mode:     %s\n", r.Gate.Mode()))
	b.WriteString(fmt.Sprintf("  tools:    %d\n", len(r.Loop.Registry.All())))
	return b.String()
}

func cmdStack(r *REPL, args string) string {
	return string(debug.Stack())
}

// =============================================================================
// SKILL HELPERS
// =============================================================================

func (r *REPL) handleSkillList() string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	entries, err := os.ReadDir(r.skillDir)
	if err != nil {
		return "(no skills: " + err.Error() + ")"
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		b.WriteString("  " + name + "\n")
	}
	if b.Len() == 0 {
		return "(no skills — use 'skill install <name>' to add)"
	}
	return b.String()
}

func (r *REPL) handleSkillInstall(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	name = sanitize(name)
	skillJSON := fmt.Sprintf(`{
  "name": %q,
  "description": "user-installed skill",
  "category": "custom",
  "prompt": "",
  "tools": [],
  "tags": [],
  "uses": 0
}`, name)
	path := filepath.Join(r.skillDir, name+".json")
	if _, err := os.Stat(path); err == nil {
		return "(skill already exists: " + name + ")"
	}
	if err := os.WriteFile(path, []byte(skillJSON), 0o644); err != nil {
		return "install failed: " + err.Error()
	}
	return "installed: " + name + "\nedit: " + path
}

func (r *REPL) handleSkillUninstall(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	name = sanitize(name)
	path := filepath.Join(r.skillDir, name+".json")
	if _, err := os.Stat(path); err != nil {
		return "(skill not found: " + name + ")"
	}
	if err := os.Remove(path); err != nil {
		return "uninstall failed: " + err.Error()
	}
	return "uninstalled: " + name
}

// handleSkillSearch hits GitHub's code search and prints the top matches.
// Each row is formatted so the user can copy-paste straight into
// `skill install <ref>`.
//
// Network call goes through the default GitHub source. Hits are capped at
// 10 to keep the chat output readable; raise via `/skill search <q> 30`
// is intentionally NOT supported here — the user can fall back to the
// browser if they need deeper paging.
func (r *REPL) handleSkillSearch(query string) string {
	if query == "" {
		return "usage: skill search <query>"
	}
	src := skills.NewGitHubSource()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hits, err := src.Search(ctx, query, 10)
	if err != nil {
		return "skill search: " + err.Error()
	}
	if len(hits) == 0 {
		return "(no skills found for: " + query + ")"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) on github:\n", len(hits))
	for _, h := range hits {
		desc := h.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "  %s — %s\n", h.Ref, desc)
	}
	b.WriteString("\ninstall any with: skill install <ref>")
	return strings.TrimRight(b.String(), "\n")
}

func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' || r == 0 {
			return -1
		}
		return r
	}, name)
}

// tokenTracker tracks token usage with two distinct concepts:
//
//   - Session cumulative (in / out) — for `/cost` billing summaries.
//     Every API call adds to these; they only grow.
//
//   - Most-recent API call (lastIn / lastOut / lastCacheCreate /
//     lastCacheRead) — overwritten on every API call. Two distinct
//     status displays read from these:
//
//     (a) Spinner row "↓ 38123 tokens"  →  LastTotal() == lastIn + lastOut
//     This is the per-turn cost: what the most recent round trip
//     actually consumed (input + output).
//
//     (b) Bottom-right status bar "38123 tokens (19%)"  →  ContextUsage()
//     This is the context-window load: lastIn + lastCacheCreate
//
//   - lastCacheRead. Mirrors claude-code's statusline formula
//     (input-side only, including prompt-cache contribution; see
//     https://code.claude.com/docs/en/statusline.md). Output is
//     NOT included — it isn't part of the in-flight context.
//
// The two numbers diverge in two ways:
//   - ContextUsage adds cache (CC parity); LastTotal does not.
//   - LastTotal adds output; ContextUsage does not.
//
// `dispIn/dispOut` are smoothed values for animation.
type tokenTracker struct {
	in, out                        int // session cumulative input/output
	cacheCreate, cacheRead         int // session cumulative cache (for /cost transparency)
	lastIn, lastOut                int // most recent API call (per-turn cost)
	lastCacheCreate, lastCacheRead int // most recent API call (cache portion of input)
	dispIn, dispOut                int
}

// add records a per-iteration usage report. `in`/`out` accumulate
// session-wide for /cost; the `last*` fields overwrite each call so the
// status bar reflects only the most recent round-trip. Cache fields are
// 0 for providers without prompt caching (Gemini / OpenAI / non-cached
// Anthropic requests).
func (t *tokenTracker) add(in, out, cacheCreate, cacheRead int) {
	t.in += in
	t.out += out
	t.cacheCreate += cacheCreate
	t.cacheRead += cacheRead
	// Last-* trackers represent "the most recent API call that
	// REPORTED usage" — don't overwrite them when an event carries
	// a zero. Mid-turn the agent emits multiple EventTokenUpdate
	// events, some of which (tool_result echo, streaming
	// summary-only) report 0 input usage; if we let those overwrite
	// last* the bottom-right context bar blanks out mid-turn.
	// Feedback 2026-05-05: "right-side token count disappears
	// during a tool run".
	if in > 0 || cacheCreate > 0 || cacheRead > 0 {
		t.lastIn = in
		t.lastCacheCreate = cacheCreate
		t.lastCacheRead = cacheRead
	}
	if out > 0 {
		t.lastOut = out
	}
}

// LastIn / LastOut / LastCacheCreate / LastCacheRead expose the most
// recent API call's raw usage breakdown. /tokens uses these to surface
// the exact provider numbers so a confused user can see whether the
// bottom-right counter's percentage is genuine context-window load or
// a parsing bug.
func (t *tokenTracker) LastIn() int          { return t.lastIn }
func (t *tokenTracker) LastOut() int         { return t.lastOut }
func (t *tokenTracker) LastCacheCreate() int { return t.lastCacheCreate }
func (t *tokenTracker) LastCacheRead() int   { return t.lastCacheRead }

// CacheCreate / CacheRead expose the session-cumulative cache totals
// (parallel to Input() / Output() for non-cache fields). /cost uses
// these to call out how much of the spend was prompt-cache vs fresh.
func (t *tokenTracker) CacheCreate() int { return t.cacheCreate }
func (t *tokenTracker) CacheRead() int   { return t.cacheRead }

// CacheHitRate is the fraction of input tokens served from prompt
// cache: cacheRead / (cacheRead + cacheCreate + in). Range [0, 1];
// returns 0 when there's no cache activity at all (avoids
// 0/0 = NaN). Mirrors claude-code's tengu_compact.cacheHitRate
// metric — it's the single number that tells you "is my caching
// actually working?".
//
// Anchor at total cacheable input (everything that COULD have been
// cached) instead of input alone, so a request with no cache
// participation reads as 0.0 rather than blowing past 1.0 when input
// is tiny.
func (t *tokenTracker) CacheHitRate() float64 {
	denom := t.cacheRead + t.cacheCreate + t.in
	if denom == 0 {
		return 0
	}
	return float64(t.cacheRead) / float64(denom)
}

// LastTotal is the most recent API call's input+output combined — the
// per-turn cost. Spinner row uses this to surface what the just-finished
// round trip consumed.
func (t *tokenTracker) LastTotal() int { return t.lastIn + t.lastOut }

// ContextUsage is the most recent API call's input-side total including
// prompt-cache tokens. Mirrors claude-code's statusline `used_percentage`
// numerator (input + cache_creation + cache_read). Bottom-right status
// bar uses this to show context-window load — distinct from per-turn
// cost (which still includes output).
func (t *tokenTracker) ContextUsage() int {
	return t.lastIn + t.lastCacheCreate + t.lastCacheRead
}

// Reset zeroes both raw and displayed counters. Called by /clear and /new
// when the conversation is being thrown away so the displayed total
// matches the new (empty) history. claude-code resets too — a /clear
// session shouldn't carry forward the old session's API spend.
func (t *tokenTracker) Reset() {
	t.in = 0
	t.out = 0
	t.cacheCreate = 0
	t.cacheRead = 0
	t.lastIn = 0
	t.lastOut = 0
	t.lastCacheCreate = 0
	t.lastCacheRead = 0
	t.dispIn = 0
	t.dispOut = 0
}

// Animate nudges the displayed counters one step closer to the actual
// counts. Safe to call on every spinner tick — it's a no-op when the
// gap is zero.
func (t *tokenTracker) Animate() {
	t.dispIn = animateOne(t.dispIn, t.in)
	t.dispOut = animateOne(t.dispOut, t.out)
}

// Total returns what the UI should display right now (smoothed).
func (t *tokenTracker) Total() int { return t.dispIn + t.dispOut }

// Input / Output expose the raw cumulative counts (not the smoothed
// display values) so info renderers (/cost, /stats) can compute exact
// totals without waiting for animation to converge.
func (t *tokenTracker) Input() int  { return t.in }
func (t *tokenTracker) Output() int { return t.out }

// Snap forces displayed to actual — used at end of turn so the final
// number lands exactly on the wire-reported total.
func (t *tokenTracker) Snap() {
	t.dispIn = t.in
	t.dispOut = t.out
}

func animateOne(current, target int) int {
	gap := target - current
	if gap < 0 {
		// Allow downward jumps (post-compaction) to land immediately —
		// the user just saw a "compacted N → M" event, the smaller
		// number should match.
		return target
	}
	if gap == 0 {
		return target
	}
	step := 0
	switch {
	case gap < 70:
		step = 3
	case gap < 200:
		step = gap * 12 / 100
		if step < 3 {
			step = 3
		}
	default:
		step = 50
	}
	if step > gap {
		step = gap
	}
	return current + step
}
