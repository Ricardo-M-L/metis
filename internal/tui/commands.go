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
	r.Register(REPLCommand{Name: "context", Description: "show context-window usage: tokens / max / percentage", Handler: cmdContext})
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
	r.Register(REPLCommand{Name: "branch", Description: "git branch (-a for all, -c <name> to create)", Handler: cmdGitBranch})
	r.Register(REPLCommand{Name: "checkout", Description: "git checkout <branch>", Handler: cmdGitCheckout})
	r.Register(REPLCommand{Name: "stash", Description: "git stash (push|pop|list)", Handler: cmdGitStash})
	r.Register(REPLCommand{Name: "fetch", Description: "git fetch (--all for all remotes)", Handler: cmdGitFetch})
	r.Register(REPLCommand{Name: "status", Aliases: []string{"st"}, Description: "git status", Handler: cmdGitStatus})

	// === Tools ===
	r.Register(REPLCommand{Name: "tools", Aliases: []string{"t"}, Description: "list available tools", Handler: cmdTools})

	// === Skills ===
	r.Register(REPLCommand{Name: "skills", Aliases: []string{"sk"}, Description: "list installed skills", Handler: cmdSkills})
	r.Register(REPLCommand{Name: "skill", Description: "skill ops: list | install <name> | uninstall <name> | search <query>", Handler: cmdSkill})

	// === MCP ===
	r.Register(REPLCommand{Name: "mcp", Description: "MCP ops: list | add <name> <cmd> | remove <name> | start <name>", Handler: cmdMCP})

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

	// === Info ===
	r.Register(REPLCommand{Name: "version", Aliases: []string{"v", "--version"}, Description: "show version", Handler: cmdVersion})
	r.Register(REPLCommand{Name: "cost", Description: "show token usage for current session", Handler: cmdCost})
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
	var b strings.Builder
	b.WriteString("metis commands:\n\n")
	seen := make(map[string]bool)
	for _, c := range r.cmds.All() {
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		b.WriteString(fmt.Sprintf("  %-14s  %s\n", c.Name, c.Description))
	}
	b.WriteString("\nTip: git/diff/commit/log/branch/checkout — git shortcuts\n      skill install <name> — install a skill\n      mcp list — list MCP servers\n")
	b.WriteString("\nOr type anything else to chat with the LLM.\n")
	return b.String()
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
	tot := r.totalTokens.Total()
	// Conservative model defaults — exact value depends on model in
	// use. Anthropic claude-opus-4-7 = 1M; sonnet = 200k; haiku = 200k.
	maxCtx := 1_000_000
	pct := float64(tot) / float64(maxCtx) * 100
	return fmt.Sprintf("context: %d tokens used / ~%d max ≈ %.1f%%", tot, maxCtx, pct)
}

// cmdMemory is a thin wrapper that delegates to the memory tool.
// Real ops happen via the agent loop's Memory tool — this command
// just gives the user a "what's in memory?" quick view.
func cmdMemory(r *REPL, args string) string {
	arg := strings.TrimSpace(args)
	if arg == "" {
		return "memory: read | write <text> | search <query> | clear (delegates to the Memory tool — model decides)"
	}
	return "memory: queue this in your next turn — say 'check memory for ...' or 'remember: ...'"
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
	return "voice: unknown arg " + args
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
		return fmt.Sprintf("theme: %s — available: %s", currentTheme.Name, strings.Join(names, ", "))
	}
	if name := SwitchTheme(arg); name != "" {
		return "theme: " + name
	}
	return fmt.Sprintf("unknown theme %q — available: %s", arg, strings.Join(ThemeNames(), ", "))
}

// cmdEffort sets the reasoning intensity dial used for subsequent turns.
// Empty arg → show current state; "off" / "default" → clear the override
// and let the provider/model defaults apply.
func cmdEffort(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" {
		cur := string(r.Loop.Effort)
		if cur == "" {
			cur = "(default — provider decides)"
		}
		return "effort: " + cur + " — use: effort low|medium|high|off"
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

func cmdSkills(r *REPL, args string) string {
	return renderSkillsList(r.skillDir)
}

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
	}
	return "mcp: unknown '" + sub + "'. usage: mcp list | add <name> <cmd> [args] | remove <name> | start <name>"
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
	if existed {
		return "(replaced MCP server: " + name + ")"
	}
	return "(added MCP server: " + name + " — run `mcp start " + name + "` to spawn)"
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
	if args == "" {
		return "usage: export [path] — exports current session as JSON"
	}
	path := args
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
	return "(compaction will trigger automatically at next threshold — or use /compact in the session)"
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
	var b strings.Builder
	b.WriteString("metis doctor\n\n")

	// Config file
	cfgPath := filepath.Join(config.Home(), "config.toml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		b.WriteString("  [MISSING] config: " + cfgPath + "\n")
	} else {
		b.WriteString("  [OK]      config: " + cfgPath + "\n")
	}

	// Skills dir
	skDir := filepath.Join(config.Home(), "skills")
	if _, err := os.Stat(skDir); os.IsNotExist(err) {
		b.WriteString("  [WARN]    skills dir: " + skDir + " (not created)\n")
	} else {
		b.WriteString("  [OK]      skills dir: " + skDir + "\n")
	}

	// Session dir
	sesDir := filepath.Join(config.Home(), "sessions")
	if _, err := os.Stat(sesDir); os.IsNotExist(err) {
		b.WriteString("  [WARN]    session dir: " + sesDir + " (not created)\n")
	} else {
		b.WriteString("  [OK]      session dir: " + sesDir + "\n")
	}

	// API key
	apiKeys := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"}
	for _, k := range apiKeys {
		if v := os.Getenv(k); v != "" {
			b.WriteString("  [OK]      " + k + ": set (" + v[:4] + "...)\n")
		}
	}

	// Git
	if _, err := exec.LookPath("git"); err != nil {
		b.WriteString("  [WARN]    git: not found\n")
	} else {
		b.WriteString("  [OK]      git: found\n")
	}

	// Go
	if _, err := exec.LookPath("go"); err != nil {
		b.WriteString("  [WARN]    go: not found\n")
	} else {
		b.WriteString("  [OK]      go: found\n")
	}

	b.WriteString("\n  Binary:   " + os.Args[0] + "\n")
	return b.String()
}

// =============================================================================
// INFO
// =============================================================================

func cmdVersion(r *REPL, args string) string {
	return renderVersion()
}

func cmdCost(r *REPL, args string) string {
	tokens := r.totalTokens
	return fmt.Sprintf("session tokens:\n  input:  %d\n  output: %d\n  total:  %d",
		tokens.in, tokens.out, tokens.in+tokens.out)
}

func cmdUsage(r *REPL, args string) string {
	return "(rate limit info: depends on your API provider — check your provider dashboard)"
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

// tokenTracker tracks session-cumulative token consumption with a
// smoothed display value so the counter doesn't jump in big chunks.
//
// Semantics (matches claude-code's StatusLine):
//
//   - Both `in` and `out` accumulate across every iteration of every
//     turn. claude-code's cost-tracker.ts does
//     `modelUsage.inputTokens += usage.input_tokens` per API call; the
//     bottom-right "tokens" total is `sumBy(modelUsage, 'inputTokens')
//
//   - sumBy(..., 'outputTokens')`. We mirror that.
//
//   - The number is "API spend" (every billed request adds its full
//     input_tokens), not "current context window size". Two API calls
//     that each send 1k of history → counter shows ~2k, even though
//     the live history is still 1k. That's the right model: the user
//     wants to see what they've consumed, and a single big tool turn
//     can quietly cost 5x what the visible history implies.
//
// `dispIn/dispOut` are the smoothed numbers actually rendered. Animate()
// is called on each spinner tick to nudge displayed toward truth via a
// piecewise step:
//
//	gap < 70   → +3 per tick (tiny gaps feel ticky)
//	gap < 200  → +12% per tick (mid-range eases out)
//	gap >= 200 → +50 per tick (catches big bursts without lag)
//
// Numbers tuned to claude-code's TokenCounter animation.
type tokenTracker struct {
	in, out         int
	dispIn, dispOut int
}

// add records a per-iteration usage report. Both axes accumulate —
// matching claude-code's cost-tracker behaviour where each API call's
// usage is += into the running totals.
func (t *tokenTracker) add(in, out int) {
	t.in += in
	t.out += out
}

// Reset zeroes both raw and displayed counters. Called by /clear and /new
// when the conversation is being thrown away so the displayed total
// matches the new (empty) history. claude-code resets too — a /clear
// session shouldn't carry forward the old session's API spend.
func (t *tokenTracker) Reset() {
	t.in = 0
	t.out = 0
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
