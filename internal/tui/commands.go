package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/mcpoauth"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/themes"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// REPLCommand is a built-in command that runs directly in the REPL, not via LLM.
type REPLCommand struct {
	Name         string
	Aliases      []string
	Description  string
	ArgumentHint string
	Source       string
	Category     string
	Handler      func(r *REPL, args string) string

	// Optional live discovery predicates. Nil means true so existing command
	// registrations remain visible and enabled by default.
	Visible func() bool
	Enabled func() bool
}

func (c REPLCommand) IsVisible() bool { return c.Visible == nil || c.Visible() }
func (c REPLCommand) IsEnabled() bool { return c.Enabled == nil || c.Enabled() }

type REPLCommandRegistry struct {
	commands []REPLCommand
	index    map[string]*REPLCommand
}

func NewREPLCommandRegistry() *REPLCommandRegistry {
	return &REPLCommandRegistry{index: make(map[string]*REPLCommand)}
}

func (r *REPLCommandRegistry) Register(c REPLCommand) {
	if c.Source == "" {
		c.Source = "repl"
	}
	if c.Category == "" {
		c.Category = "built-in"
	}
	r.commands = append(r.commands, c)
	r.rebuildIndex()
}

func (r *REPLCommandRegistry) rebuildIndex() {
	latest := make(map[string]int, len(r.commands))
	for i := range r.commands {
		latest[r.commands[i].Name] = i
	}
	r.index = make(map[string]*REPLCommand, len(r.commands)*2)
	for name, i := range latest {
		r.index[name] = &r.commands[i]
	}
	for i := range r.commands {
		cmd := &r.commands[i]
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

func (r *REPLCommandRegistry) Get(name string) *REPLCommand {
	return r.Resolve(name)
}

// Resolve maps a canonical name or alias to the canonical command entry.
func (r *REPLCommandRegistry) Resolve(name string) *REPLCommand {
	return r.index[name]
}

func (r *REPLCommandRegistry) CanonicalName(name string) (string, bool) {
	cmd := r.Resolve(name)
	if cmd == nil {
		return "", false
	}
	return cmd.Name, true
}

func (r *REPLCommandRegistry) All() []REPLCommand {
	return r.commands
}

// Catalog returns only the registrations that currently own their canonical
// name, with aliases filtered through the same last-write-wins index used by
// dispatch.
func (r *REPLCommandRegistry) Catalog() []REPLCommand {
	byName := make(map[string]*REPLCommand, len(r.index))
	for _, cmd := range r.index {
		if cmd == nil || r.index[cmd.Name] != cmd {
			continue
		}
		byName[cmd.Name] = cmd
	}
	out := make([]REPLCommand, 0, len(byName))
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

// BuildREPLCommands creates the full command registry for the REPL.
func BuildREPLCommands() *REPLCommandRegistry {
	r := NewREPLCommandRegistry()

	// === Core ===
	r.Register(REPLCommand{Name: "help", Aliases: []string{"h", "?"}, Description: "show this help", Handler: cmdHelp})
	r.Register(REPLCommand{Name: "quit", Aliases: []string{"q", "exit", "bye"}, Description: "exit metis", Handler: cmdQuit})
	r.Register(REPLCommand{Name: "clear-history", Aliases: []string{"cls"}, Description: "clear conversation history without creating a new session", Category: "session", Handler: cmdClear})

	// === Model ===
	r.Register(REPLCommand{Name: "model", Aliases: []string{"m", "models"}, Description: "switch provider/model (OpenCode-style picker)", ArgumentHint: "[provider/model|model]", Category: "model", Handler: cmdModel})
	r.Register(REPLCommand{Name: "provider", Aliases: []string{"providers"}, Description: "show or switch configured API provider", ArgumentHint: "[provider[/model]]", Category: "model", Handler: cmdProvider})
	r.Register(REPLCommand{Name: "effort", Description: "set reasoning effort: low | medium | high | off", Handler: cmdEffort})
	r.Register(REPLCommand{Name: "quick", Description: "quick output: effort=low with max_tokens halved", ArgumentHint: "[on|off|toggle]", Category: "model", Handler: cmdQuick})
	r.Register(REPLCommand{Name: "theme", Description: "switch color theme: dark | light | dark-daltonized | nord | solarized-dark", Handler: cmdTheme})
	r.Register(REPLCommand{Name: "vim", Description: "vim mode: on | off | toggle (modal input — hjkl in NORMAL)", Handler: cmdVim})
	// /voice hidden from the palette 2026-05-23 — feature requires
	// an OpenAI API key in ~/.metis/auth.json for Whisper transcription,
	// and most users hit "voice: openai api key required..." on first
	// use. Code (voice.go + cmdVoice handler) retained so we can re-
	// enable cleanly once the alternative transcription paths land
	// (Gemini audio in, macOS native Dictation, etc — see voice.go
	// header for the three candidate approaches). To re-enable without
	// rebuilding: uncomment this Register line.
	// r.Register(REPLCommand{Name: "voice", Description: "voice input: start | stop | toggle (records mic, transcribes via whisper)", Handler: cmdVoice})
	r.Register(REPLCommand{Name: "share", Description: "share session over local HTTP+SSE: start | stop (URL printed for IDE/browser clients)", Handler: cmdShare})

	// === Productivity / inspection ===
	r.Register(REPLCommand{Name: "files", Description: "show files currently loaded in this conversation context", Category: "context", Handler: cmdFiles})
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
	r.Register(REPLCommand{Name: "todos", Description: "list the session checklist (TodoRead)", Category: "session", Handler: cmdTodos})
	r.Register(REPLCommand{Name: "tasks", Description: "manage background jobs for this session", ArgumentHint: "[list|output <id>|stop <id>]", Category: "session", Handler: cmdBackgroundTasks})
	r.Register(REPLCommand{Name: "ide", Description: "show IDE / remote bridge status (/share + ACP)", Handler: cmdIDE})
	// /review is owned by the slash registry (internal/slash/review.go)
	// — see the 2026-07-28 cleanup that removed the legacy cmdReview
	// REPL shadow. The slash handler emits a SignalCustomPrompt that
	// keybind_submit routes back into the agent loop.
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
	r.Register(REPLCommand{Name: "tools", Aliases: []string{"t", "toolsets"}, Description: "list available tools", Handler: cmdTools})

	// === Skills ===
	// /skills is the canonical form; the singular /skill alias was
	// removed on 2026-05-22 (user request — image #51): the duplicate
	// row cluttered the slash-command autocomplete picker without
	// adding new functionality. The handler cmdSkill stays in the
	// codebase as cmdSkills's implementation, just not registered
	// under its own name.
	r.Register(REPLCommand{Name: "skills", Aliases: []string{"sk"}, Description: "skills: list | install | remove | info | edit | enable | disable | create | search (local)", Handler: cmdSkills})

	// === MCP ===
	r.Register(REPLCommand{Name: "mcp", Description: "MCP ops: list | add | remove | start | login | enable | disable | edit | test | logs | reload", Handler: cmdMCP})
	r.Register(REPLCommand{Name: "cu", Description: "computer-use (metis-cu) ops: enable | disable | status", Handler: cmdCU})

	// === Session ===
	r.Register(REPLCommand{Name: "session", Description: "show or control the local read-only sharing session", ArgumentHint: "[status|start|stop]", Category: "session", Handler: cmdSessionShare})
	r.Register(REPLCommand{Name: "session-info", Aliases: []string{"sid"}, Description: "show current local session id, title, turns, and model", Category: "session", Handler: cmdSessionInfo})
	r.Register(REPLCommand{Name: "sessions", Aliases: []string{"ls"}, Description: "list saved sessions", Handler: cmdSessions})
	r.Register(REPLCommand{Name: "export", Description: "export the current conversation to a readable text file", Handler: cmdExport})

	// === Permissions ===
	r.Register(REPLCommand{Name: "mode", Description: "show or set permission mode (default|acceptEdits|plan|dontAsk|bypassPermissions|fullAccess)", Handler: cmdMode})
	r.Register(REPLCommand{Name: "allow", Description: "allow a tool permanently (e.g. allow Bash)", Handler: cmdAllow})
	r.Register(REPLCommand{Name: "sandbox", Description: "OS command sandbox: status | doctor | reset | off | permissions | auto-allow", Handler: cmdSandbox})

	// === System ===
	r.Register(REPLCommand{Name: "compact", Description: "force context compaction now", Handler: cmdCompact})
	r.Register(REPLCommand{Name: "ctx", Description: "show compaction state: cap, threshold, current tokens, trigger distance", Handler: cmdCtx})
	r.Register(REPLCommand{Name: "config", Aliases: []string{"cfg"}, Description: "view and edit Metis settings", Category: "system", Handler: cmdConfig})
	r.Register(REPLCommand{Name: "env", Description: "show environment info (OS, arch, CPU, memory)", Handler: cmdEnv})
	r.Register(REPLCommand{Name: "doctor", Description: "diagnose metis installation", Handler: cmdDoctor})

	// === Phase C: interactive helper slashes ===
	r.Register(REPLCommand{Name: "copy", Description: "copy the Nth-latest assistant reply", ArgumentHint: "[N]", Category: "session", Handler: cmdCopy})
	r.Register(REPLCommand{Name: "commit-push-pr", Aliases: []string{"cpp"}, Description: "git add -A → commit -m <msg> → push → gh pr create --fill", Handler: cmdCommitPushPR})
	r.Register(REPLCommand{Name: "insights", Description: "summarize sessions in last N days: /insights [--days=N] (default 7)", Handler: cmdInsights})
	r.Register(REPLCommand{Name: "output-style", Description: "output verbosity: full | streamlined | minimal", Handler: cmdOutputStyle})
	r.Register(REPLCommand{Name: "break-cache", Description: "explain how to force a fresh prompt-cache write", Handler: cmdBreakCache})
	r.Register(REPLCommand{Name: "security-review", Description: "OWASP-flavored review nudge (analog of /review for security)", Handler: cmdSecurityReview})
	r.Register(REPLCommand{Name: "feedback", Description: "record a log-only remark (with text) or file a bug report (bare)", ArgumentHint: "[<remark>]", Handler: cmdFeedback})
	r.Register(REPLCommand{Name: "bundle", Description: "manage profile bundles: install <path> | list | remove <name>", ArgumentHint: "<install|list|remove> [arg]", Category: "context", Handler: cmdBundle})

	// === Phase F: discoverability slashes ===
	r.Register(REPLCommand{Name: "think-back", Aliases: []string{"thinkback"}, Description: "show the current year's cross-session activity review", Handler: cmdThinkBack})
	r.Register(REPLCommand{Name: "thoughts", Description: "show the most recent assistant turn's extended-thinking trace", Handler: cmdThoughts})
	r.Register(REPLCommand{Name: "ultraplan", Description: "deep-plan nudge: bumps effort=high and pre-loads the planning frame", Handler: cmdUltraplan})
	r.Register(REPLCommand{Name: "onboarding", Description: "first-run setup recap (auth, config, /init, talk)", Handler: cmdOnboarding})

	// === Desktop Features (Codex parity) ===
	r.Register(REPLCommand{Name: "resume", Aliases: []string{"rs"}, Description: "resume/fork session with search, fork, and fresh start", Handler: cmdResume})
	r.Register(REPLCommand{Name: "diff-view", Aliases: []string{"dv"}, Description: "full-screen git diff viewer with file list", Handler: cmdDiffView})
	r.Register(REPLCommand{Name: "agents", Aliases: []string{"av"}, Description: "inspect sub-agent work in a live tree view", Handler: cmdAgentsView})
	r.Register(REPLCommand{Name: "desktop", Description: "launch native desktop app (macOS/Linux/Windows)", Handler: cmdDesktop})

	// === Info ===
	r.Register(REPLCommand{Name: "version", Aliases: []string{"v", "--version"}, Description: "show version", Handler: cmdVersion})
	r.Register(REPLCommand{Name: "login", Description: "set up provider credentials (delegates to metis auth login)", Handler: cmdLogin})
	r.Register(REPLCommand{Name: "logout", Description: "remove a stored provider credential", Handler: cmdLogout})
	r.Register(REPLCommand{Name: "init", Description: "create a starter CLAUDE.md for this repo (fallback handler)", Handler: cmdInit})
	r.Register(REPLCommand{Name: "statusline", Description: "show + customize the bottom status line", Handler: cmdStatusLine})
	r.Register(REPLCommand{Name: "bg", Description: "background-turn status (alias for Ctrl+B mid-turn)", Handler: cmdBg})
	r.Register(REPLCommand{Name: "cost", Description: "show token usage for current session", Handler: cmdCost})
	r.Register(REPLCommand{Name: "usage", Description: "show API rate limit info", Handler: cmdUsage})

	// === Debug ===
	r.Register(REPLCommand{Name: "stack", Description: "show panic stack trace", Handler: cmdStack})

	return r
}

// =============================================================================
// CORE
// =============================================================================

func cmdHelp(r *REPL, args string) string {
	rows := make([]infoRow, 0, 64)
	for _, c := range effectiveCommandCatalog(r.cmds, r.Slash) {
		key := "/" + c.Name
		if c.ArgumentHint != "" {
			key += " " + c.ArgumentHint
		}
		rows = append(rows, infoRow{Key: key, Value: c.Description})
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
	if err := r.replaceHistory(nil); err != nil {
		return "clear failed: " + err.Error()
	}
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
	// Full provider rebuild — keeps loop.Provider, loop.ContextWindow,
	// and loop.Compactor's effective cap in sync with the new selection.
	// The old code only updated the string fields, which silently broke
	// /model when the new model lived behind a different provider profile
	// (user screenshot 35, 2026-05-17). switchREPLModel falls back to a
	// string-only update when no cfg / profile is available (tests, plain
	// REPL), so existing surfaces stay green.
	err := switchREPLModel(r, args)
	if err != nil {
		return "model switch failed; previous model remains active: " + r.model + "\n(" + err.Error() + ")"
	}
	out := "model set to: " + r.model
	if r.providerName != "" {
		out += "  ·  provider: " + r.providerName
	}
	return out
}

func cmdProvider(r *REPL, args string) string {
	if r == nil {
		return "provider: runtime unavailable"
	}
	args = strings.TrimSpace(args)
	if args == "" {
		providerName := r.providerName
		if providerName == "" && r.cfg != nil {
			providerName = r.cfg.Provider.Default
		}
		if providerName == "" {
			return "provider: not configured"
		}
		return "provider: " + providerName + "  ·  model: " + r.model
	}
	if r.cfg == nil {
		return "provider switch failed; provider configuration unavailable"
	}

	providerName, model := splitConfiguredProviderModel(r.cfg, args)
	ok := providerName != ""
	if !ok {
		if rawProvider, rawModel, qualified := strings.Cut(args, "/"); qualified {
			providerName, _, ok = configuredProviderModel(r.cfg, rawProvider)
			if ok && strings.TrimSpace(rawModel) != "" {
				model = strings.TrimSpace(rawModel)
			} else {
				ok = false
			}
		} else {
			providerName, model, ok = configuredProviderModel(r.cfg, args)
		}
	}
	if !ok {
		known := configuredProviderNames(r.cfg)
		hint := ""
		if len(known) > 0 {
			hint = " Known profiles: " + strings.Join(known, ", ")
		}
		return fmt.Sprintf("provider switch failed; provider %q is not configured.%s", args, hint)
	}

	previous := r.providerName
	if previous == "" {
		previous = r.cfg.Provider.Default
	}
	if err := switchREPLModel(r, providerName+"/"+model); err != nil {
		return "provider switch failed; previous provider remains active: " + previous + "\n(" + err.Error() + ")"
	}
	return "provider set to: " + r.providerName + "  ·  model: " + r.model
}

// switchREPLModel runs the same Provider rebuild Model.switchModel does,
// but from the REPL-side surface where we have r.Loop / r.cfg /
// r.providerName instead of *Model. Same semantics: rebuild Provider,
// swap into loop, refresh Compactor's window math, update tracked
// model + provider name. Returns an error string suitable for surfacing
// in the chat transcript.
func switchREPLModel(r *REPL, newModel string) error {
	if r == nil || r.Loop == nil {
		return fmt.Errorf("repl not fully wired (loop missing)")
	}
	if r.cfg == nil {
		r.model = newModel
		provider, _, _ := r.Loop.ProviderModelSnapshot()
		r.Loop.RebindProviderModel(provider, newModel)
		runtime.RebindLoopRuntime(r.Loop, provider, newModel, r.Loop.System, r.SessionID)
		return nil // string-only swap (test path)
	}
	// `/model <provider>/<model>` is unambiguous for custom profiles. For a
	// bare model ID, resolve an exact configured-profile match before falling
	// back to the current provider. Without this, `/model kimi-k3` changed the
	// label but still sent the request through the previous Ark endpoint.
	newProvName, resolvedModel := splitConfiguredProviderModel(r.cfg, newModel)
	newModel = resolvedModel
	for _, c := range builtinModelChoices {
		if newProvName == "" && c.ID == newModel {
			newProvName = c.Provider
			break
		}
	}
	if newProvName == "" {
		newProvName = configuredProviderForModel(r.cfg, newModel)
	}
	if newProvName == "" {
		newProvName = r.providerName
	}
	if newProvName == "" {
		newProvName = r.cfg.Provider.Default
	}
	if newProvName == "" {
		r.model = newModel
		provider, _, _ := r.Loop.ProviderModelSnapshot()
		r.Loop.RebindProviderModel(provider, newModel)
		runtime.RebindLoopRuntime(r.Loop, provider, newModel, r.Loop.System, r.SessionID)
		return nil // no profile to rebuild against
	}
	pb, err := runtime.BuildProvider(r.cfg, newProvName, newModel)
	if err != nil {
		// Keep labels and transport on the same model/profile. Reporting the
		// requested model as active after a failed rebuild sends the next turn
		// over the old provider with misleading UI state.
		return fmt.Errorf("BuildProvider(%s, %s): %w", newProvName, newModel, err)
	}
	newSystem, newSections := runtime.RebindProviderPrompt(
		r.Loop.System, r.Loop.SystemSections, newProvName, pb.Model,
	)
	newBaseSystem, newBaseSections := runtime.RebindProviderPrompt(
		r.baseSystem, r.baseSystemSections, newProvName, pb.Model,
	)
	r.Loop.RebindProviderRuntime(pb.Provider, pb.Model, pb.MaxOutputTokens, newSystem, newSections)
	r.model = pb.Model
	r.providerName = newProvName
	r.baseSystem = newBaseSystem
	r.baseSystemSections = newBaseSections
	runtime.RebindLoopRuntime(r.Loop, pb.Provider, pb.Model, newSystem, r.SessionID, runtime.LoopRuntimeRebindOptions{
		ProviderName: newProvName,
	})
	getModelState().AddRecent(pb.Model)
	return nil
}

// cmdShare starts or stops the localhost HTTP+SSE bridge so an
// cmdBg reports the live background-turn state via the TUI bridge.
// Falls back to a static hint when invoked outside the TUI (no
// BgTurnSnapshot closure wired). Phase F slash sibling of Ctrl+B —
// the keybind toggles, this command observes.
func cmdBg(r *REPL, args string) string {
	hint := "(Ctrl+B mid-turn: suppress streaming output + free the input. Press again to foreground.)"
	if r == nil || r.BgTurnSnapshot == nil {
		return "bg: no live turn state (headless REPL)\n  " + hint
	}
	st := r.BgTurnSnapshot()
	if !st.IsActive {
		return "bg: no turn running\n  " + hint
	}
	elapsed := time.Since(st.StartTime).Round(time.Second)
	out := fmt.Sprintf("bg: turn ACTIVE · %s elapsed · model=%s", elapsed, st.Model)
	if st.QueuedCount > 0 {
		out += fmt.Sprintf(" · %d prompt(s) queued", st.QueuedCount)
	}
	return out + "\n  " + hint
}

// cmdFiles reports the concrete files represented in the live model context.
// Workspace discovery belongs to @-mention; /files answers the narrower
// question "which files has this conversation actually loaded?".
func cmdFiles(r *REPL, args string) string {
	return renderContextFiles(r)
}

// cmdContext shows current context-window usage. Calculates percent
// of max-context tokens consumed by current history + system prompt.
func cmdContext(r *REPL, args string) string {
	// Use the loop's canonical active-context snapshot when available. It is
	// anchored to the latest provider response and includes local messages
	// appended after that response exactly once. tokenTracker remains the
	// headless fallback; its session-cumulative counters are reserved for /cost.
	used := r.totalTokens.ContextUsage()
	if r.Loop != nil {
		used = r.Loop.EstimateContextTokens()
	}
	maxCtx := 1_000_000
	if r.Loop != nil && r.Loop.Provider != nil {
		if cap := r.Loop.Provider.MaxContextTokens(); cap > 0 {
			maxCtx = cap
		}
	}
	pct := float64(used) / float64(maxCtx) * 100
	if pct > 100 {
		pct = 100
	}
	rows := []infoRow{
		{Key: "active prompt", Value: fmtThousands(used) + " tokens"},
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
	// view, equivalent to opening ~/.metis/memory/MEMORY.md by hand.
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
	if r != nil && r.RecapSnapshot != nil {
		if recap := strings.TrimSpace(r.RecapSnapshot()); recap != "" {
			return "※ recap: " + recap
		}
	}
	return "recap: no structural recap yet (a recap appears after a turn with multiple tool calls)"
}

// cmdReplay re-runs the last user prompt by appending the same text
// to the loop. Caller (chat surface) interprets and submits.
func cmdReplay(r *REPL, args string) string {
	hist := r.Loop.History()
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == llm.RoleUser && len(hist[i].Content) > 0 {
			text := hist[i].Content[0].Text
			// 2026-05-21: if the TUI provides InsertInput, prefill
			// the input box for one-keystroke re-send. Same UX
			// upgrade cmdReview got on 2026-05-18. Falls back to
			// the legacy "copy this" mode for headless runs without
			// the bridge.
			if r != nil && r.InsertInput != nil {
				r.InsertInput(text)
				return "replay: prompt loaded into input — edit if needed, press Enter to re-send"
			}
			return "replay: copy the prompt and re-submit:\n  " + text
		}
	}
	return "replay: no prior user prompt found"
}

// cmdTasks reads the TodoRead store for the current session and
// renders the items inline — content + status + id, not just the
// in-flight count. Pre-2026-05-21 this only returned the count
// (image #35 user complaint: "shows complete but todos below not
// checked"). Now mirrors the TodoRead tool's output format so the
// user sees the same picture the model sees.
func cmdTasks(r *REPL, args string) string {
	if r.SessionID == "" {
		return "tasks: no session id"
	}
	tl, err := tasks.Load(r.SessionID)
	if err != nil {
		return "tasks: " + err.Error()
	}
	if tl == nil || len(tl.Items) == 0 {
		return "tasks: no todos yet — TodoWrite from the agent loop populates this"
	}
	// Render rows as `<icon> <content>` — claude-code's TaskListV2
	// (components/TaskListV2.tsx:303+313) shows icon + subject only,
	// no id, no status word. The id is an implementation detail of
	// TodoWrite dedup; users don't need to see it (they don't pass
	// ids manually).  The status icon already communicates state.
	var b strings.Builder
	pending, inProgress, completed := 0, 0, 0
	for _, it := range tl.Items {
		icon := "○"
		switch it.Status {
		case "completed":
			icon = "●"
			completed++
		case "in_progress":
			icon = "◐"
			inProgress++
		default:
			pending++
		}
		fmt.Fprintf(&b, "%s %s\n", icon, it.Content)
	}
	header := fmt.Sprintf("tasks: %d total · %d done · %d in-progress · %d pending\n\n",
		len(tl.Items), completed, inProgress, pending)
	return header + strings.TrimRight(b.String(), "\n")
}

// cmdIDE shows IDE / remote-bridge status so the user knows whether
// /share is running and what address external clients should hit.
func cmdIDE(r *REPL, args string) string {
	var lines []string
	// IDE MCP attachment (extension hosts the server, metis connects).
	if cwd, err := os.Getwd(); err == nil {
		if lock, ok := runtime.DiscoverIDE(cwd); ok {
			lines = append(lines, fmt.Sprintf("ide: attached to %s MCP at %s (tools: mcp__ide__*)", lock.IDEName, lock.Endpoint()))
		} else {
			lines = append(lines, "ide: no IDE MCP server found (looked in ~/.metis/ide/*.lock)")
		}
	}
	// Outbound bridge (/share + ACP) status.
	if addr := bridgeCurrentAddr(); addr != "" {
		lines = append(lines, "bridge: running on http://"+addr)
	} else {
		lines = append(lines, "bridge: off — start with /share to expose a localhost SSE endpoint")
	}
	return strings.Join(lines, "\n")
}

// buildReviewPrompt is the shared prompt body used by /security-review.
// /review itself moved to internal/slash/review.go on 2026-07-28 —
// the legacy cmdReview REPL handler that used this for the default
// "staged changes" target has been removed so the slash path can
// drive a richer, model-directed review (CC-style LOCAL_REVIEW_PROMPT).
// Pulled out so the prompt itself can be unit-tested without driving
// the TUI input path.
func buildReviewPrompt(target string, security bool) string {
	var b strings.Builder
	if security {
		fmt.Fprintf(&b, "Perform an OWASP-flavored security review of %s.\n\n", target)
		b.WriteString("Look for:\n")
		b.WriteString("- Input validation / injection (SQL, command, path, XSS)\n")
		b.WriteString("- AuthN/AuthZ gaps (missing checks, broken role enforcement)\n")
		b.WriteString("- Secret handling (logged credentials, hard-coded keys)\n")
		b.WriteString("- Race conditions / TOCTOU on auth + permission paths\n")
		b.WriteString("- Crypto misuse (weak random, IV reuse, broken cipher modes)\n")
		b.WriteString("- Dependency vulns (look at go.mod / package.json for known-bad versions)\n\n")
	} else {
		fmt.Fprintf(&b, "Review %s.\n\n", target)
		b.WriteString("Look for:\n")
		b.WriteString("- Bugs / incorrect behavior\n")
		b.WriteString("- Style + idiom mismatches with surrounding code\n")
		b.WriteString("- Missed edge cases or error paths\n")
		b.WriteString("- Performance footguns (allocation in hot paths, N+1, …)\n\n")
	}
	b.WriteString("Use the existing tools (Bash for git/grep, Read for code) to gather context. ")
	b.WriteString("Report VERDICT (PASS / NEEDS WORK / FAIL) + a bulleted list of findings with file:line refs.")
	return b.String()
}

// cmdBug composes a GitHub-issue-ready bug report template — version,
// active model, mode, OS/arch, plus the last few user/assistant turns —
// and copies it to the system clipboard (OSC 52 + ~/.metis/clipboard.txt
// fallback). Used to just print a URL hint; now you can paste straight
// into a new issue.
//
// Free-form complaint can be supplied as args: `/bug agent freezes
// after long Edit`. Body gets prepended to the report.
func cmdBug(r *REPL, args string) string {
	var b strings.Builder
	b.WriteString("## Description\n\n")
	if args = strings.TrimSpace(args); args != "" {
		b.WriteString(args + "\n")
	} else {
		b.WriteString("<replace with what went wrong + steps to reproduce>\n")
	}
	b.WriteString("\n## Environment\n\n")
	fmt.Fprintf(&b, "- metis version: %s\n", version.Short())
	if r != nil && r.Loop != nil {
		fmt.Fprintf(&b, "- model: %s\n", r.Loop.Model)
		if r.Loop.Provider != nil {
			fmt.Fprintf(&b, "- provider model id (wire): %s\n", r.Loop.Provider.ModelID())
			fmt.Fprintf(&b, "- context window: %d tokens\n", r.Loop.Provider.MaxContextTokens())
		}
	}
	if r != nil && r.Gate != nil {
		fmt.Fprintf(&b, "- permission mode: %s\n", r.Gate.Mode())
	}
	fmt.Fprintf(&b, "- platform: %s/%s\n", goruntime.GOOS, goruntime.GOARCH)
	fmt.Fprintf(&b, "- go: %s\n", goruntime.Version())

	if r != nil && r.Loop != nil {
		hist := r.Loop.History()
		// Last 4 user/assistant turns (in pairs) — gives the model
		// just enough context to repro without flooding the issue with
		// the entire session.
		const maxTurns = 4
		picked := pickTrailingTurns(hist, maxTurns)
		if len(picked) > 0 {
			b.WriteString("\n## Recent transcript\n\n")
			for _, m := range picked {
				role := string(m.Role)
				body := llmMessageText(m)
				if len(body) > 600 {
					body = body[:600] + "…"
				}
				fmt.Fprintf(&b, "**%s**: %s\n\n", role, body)
			}
		}
	}
	b.WriteString("\n---\n_Generated by /bug — review before submitting._\n")

	report := b.String()
	writeClipboard(report)
	return fmt.Sprintf("bug: report copied to clipboard (%d chars · %s)\n"+
		"  paste into: https://github.com/Ricardo-M-L/metis/issues/new",
		len(report), osc52Status())
}

// pickTrailingTurns returns the last maxTurns user/assistant messages
// from hist in original order. Skips tool_result-only messages (they
// don't carry user-readable narrative; the surrounding assistant
// text already mentions the tool name).
func pickTrailingTurns(hist []llm.Message, maxTurns int) []llm.Message {
	picked := make([]llm.Message, 0, maxTurns)
	for i := len(hist) - 1; i >= 0 && len(picked) < maxTurns; i-- {
		if hist[i].Role != llm.RoleUser && hist[i].Role != llm.RoleAssistant {
			continue
		}
		if llmMessageText(hist[i]) == "" {
			continue
		}
		picked = append([]llm.Message{hist[i]}, picked...)
	}
	return picked
}

// llmMessageText extracts a plain text representation of a Message
// regardless of whether the Content is a string (plain) or a slice of
// ContentBlock (assistant turns w/ tool_use). Tool blocks are skipped.
func llmMessageText(m llm.Message) string {
	if len(m.Content) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// cmdRename writes a new title onto the current session. Persists
// via session.Store.SetTitle which append-only rewrites the header
// row in the JSONL file. /sessions and /session-info both reflect the
// new title on the next read.
func cmdRename(r *REPL, args string) string {
	title := strings.TrimSpace(args)
	if title == "" {
		return "rename: usage `/rename <new title>` (current title shown via /session-info)"
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
			return "share: already running read-only on http://" + cur
		}
		// The Bubble Tea surface does not currently consume bridge input. Start
		// explicitly in read-only mode instead of advertising a /message queue
		// that accepts prompts but never executes them.
		addr, err := startBridge(nil)
		if err != nil {
			return "share: " + err.Error()
		}
		return fmt.Sprintf("share: http://%s — read-only endpoints: /transcript /events /health", addr)
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
		names := themes.ThemeNames()
		return fmt.Sprintf("theme: %s — available: %s, auto:<provider>",
			themes.Current().Name, strings.Join(names, ", "))
	}
	// `/theme auto` — retint the active palette using the current
	// provider id (anthropic / openai / kimi / ...). Falls back to
	// the active theme unchanged when the provider is unknown.
	if arg == "auto" {
		provider := ""
		if r != nil && r.Loop != nil && r.Loop.Provider != nil {
			provider = r.Loop.Provider.Name()
		}
		themes.ApplyProviderTint(provider)
		return "theme: " + themes.Current().Name
	}
	if strings.HasPrefix(arg, "auto:") {
		provider := strings.TrimPrefix(arg, "auto:")
		themes.ApplyProviderTint(provider)
		return "theme: " + themes.Current().Name
	}
	if name := themes.SwitchTheme(arg); name != "" {
		return "theme: " + name
	}
	return fmt.Sprintf("unknown theme %q — available: %s, auto, auto:<provider>",
		arg, strings.Join(themes.ThemeNames(), ", "))
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
		cur := string(r.Loop.EffortValue())
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
		r.Loop.SetEffort(llm.EffortDefault)
		return "effort: cleared (using provider default)"
	}
	e, ok := llm.ParseEffort(arg)
	if !ok {
		return "(unknown effort: " + args + " — use: effort low|medium|high|off)"
	}
	r.Loop.SetEffort(e)
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
		if r.Loop.FastEnabled() {
			state = "on"
		}
		return "fast mode: " + state + " — use: fast on | fast off | fast toggle"
	case "on", "true", "1", "yes":
		r.Loop.SetFast(true)
		return "fast mode: on (effort=low, max_tokens halved for next turn)"
	case "off", "false", "0", "no":
		r.Loop.SetFast(false)
		return "fast mode: off"
	case "toggle", "t":
		if r.Loop.ToggleFast() {
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
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 || fields[0] == "status" {
		return cmdGitStatus(r, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), "status")))
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
	switch fields[0] {
	case "diff":
		return cmdGitDiff(r, rest)
	case "log":
		return cmdGitLog(r, rest)
	case "branch":
		return cmdGitBranch(r, rest)
	case "checkout":
		return cmdGitCheckout(r, rest)
	case "stash":
		return cmdGitStash(r, rest)
	case "fetch":
		return cmdGitFetch(r, rest)
	case "commit":
		return cmdGitCommit(r, rest)
	default:
		return "git: unsupported shortcut " + fields[0] + " (use status|diff|log|branch|checkout|stash|fetch|commit)"
	}
}

func cmdGitStatus(r *REPL, args string) string {
	return runGitCmd(r, args, "status")
}

func cmdGitDiff(r *REPL, args string) string {
	staged := strings.Contains(args, "--cached") || strings.Contains(args, "-c")
	gitArgs := []string{"diff"}
	if staged {
		gitArgs = []string{"diff", "--cached"}
	}
	return runGitCmd(r, strings.TrimSpace(args), gitArgs...)
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
		return runGitCmd(r, args, "log", "--stat", "-n", n)
	}
	return runGitCmd(r, args, "log", "-n", n, "--oneline")
}

func cmdGitBranch(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if strings.Contains(args, "-a") {
		return runGitCmd(r, args, "branch", "-a")
	}
	if strings.HasPrefix(args, "-c ") {
		name := strings.TrimSpace(strings.TrimPrefix(args, "-c"))
		if name == "" {
			return "(branch name required: branch -c <name>)"
		}
		return runGitCmd(r, args, "branch", name)
	}
	return runGitCmd(r, args, "branch")
}

func cmdGitCheckout(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "(branch name required: checkout <branch>)"
	}
	return runGitCmd(r, args, "checkout", args)
}

func cmdGitStash(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "push" {
		msg := "metis stash " + time.Now().Format(time.RFC3339)
		return runGitCmd(r, args, "stash", "push", "-m", msg)
	}
	if args == "pop" {
		return runGitCmd(r, args, "stash", "pop")
	}
	if args == "list" {
		return runGitCmd(r, args, "stash", "list")
	}
	return "(stash: use: stash push | stash pop | stash list)"
}

func cmdGitFetch(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "--all" || args == "-a" {
		return runGitCmd(r, args, "fetch", "--all")
	}
	if args != "" {
		return runGitCmd(r, args, "fetch", args)
	}
	return runGitCmd(r, args, "fetch")
}

func cmdGitCommit(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	msg := extractFlag(args, "-m")
	if msg == "" {
		diff, _ := runGitCmdOut(r, "diff", "--staged")
		if len(diff) == 0 {
			return "(nothing staged to commit)"
		}
		return "(no commit message; use: commit -m 'your message')"
	}
	return runGitCmd(r, args, "commit", "-m", msg)
}

func runGitCmd(r *REPL, rest string, gitArgs ...string) string {
	out, err := runGitCmdOut(r, gitArgs...)
	if err != nil {
		return "git " + strings.Join(gitArgs, " ") + ": " + err.Error()
	}
	if len(out) == 0 {
		return "(ok)"
	}
	return string(out)
}

func runGitCmdOut(r *REPL, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if r != nil && r.sandbox != nil {
		cmd.Env = r.sandbox.FilterEnv(os.Environ(), false)
		if tempDir := r.sandbox.TempDir(); tempDir != "" {
			cmd.Env = append(cmd.Env, "TMPDIR="+tempDir, "TMP="+tempDir, "TEMP="+tempDir)
		}
		wrapped, err := r.sandbox.Wrap(cmd, sandbox.Request{})
		if err != nil {
			return nil, fmt.Errorf("sandbox failed closed: %w", err)
		}
		cmd = wrapped
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		return nil, fmt.Errorf("%s: %s", err, stderr.String())
	}
	return out, err
}

func extractFlag(args, flag string) string {
	idx := strings.Index(args, flag)
	if idx < 0 {
		return ""
	}
	tail := strings.TrimSpace(args[idx+len(flag):])
	if tail == "" {
		return ""
	}
	if tail[0] == '\'' || tail[0] == '"' {
		quote := tail[0]
		if end := strings.IndexByte(tail[1:], quote); end >= 0 {
			return tail[1 : end+1]
		}
	}
	return tail
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
// cmdSkill remains only as a compatibility implementation for embedders; the
// registered canonical surface is /skills and both paths use SKILL.md.

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

// ensureMCPToken is a narrow test seam around the credential side effect. The
// only production call that passes interactive=true is handleMCPLogin, reached
// when the user explicitly enters `/mcp login <name>`. Autonomous startup and
// lazy tool execution remain in runtime/mcp and always pass interactive=false.
var ensureMCPToken = func(ctx context.Context, serverKey, serverURL string, interactive bool) (string, error) {
	store := mcpoauth.NewTokenStore()
	if interactive {
		return store.Login(ctx, serverKey, serverURL)
	}
	return store.EnsureToken(ctx, serverKey, serverURL, false)
}

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
		// Usage: mcp add [--env KEY=VAL ...] <name> <command> [args...]
		// --env may repeat. Stops parsing flags at the first non-flag arg.
		env, rest, perr := parseMCPAddFlags(parts[1:])
		if perr != nil {
			return "mcp: " + perr.Error()
		}
		if len(rest) < 2 {
			return "usage: mcp add [--env KEY=VAL ...] <name> <command> [args...]"
		}
		return r.handleMCPAdd(rest[0], rest[1], rest[2:], env)
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
	case "login":
		if len(parts) < 2 {
			return "usage: mcp login <name>"
		}
		return r.handleMCPLogin(parts[1])
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
		"  start <name> | login <name> | enable <name> | disable <name> | edit [<name>] | test <name> | logs <name> | reload"
}

// handleMCPLogin is the sole interactive OAuth entry point for MCP servers.
// Loading and validating the on-disk entry before starting the browser flow
// keeps `/mcp login` explicit and prevents stdio/static-header entries from
// accidentally entering OAuth.
func (r *REPL) handleMCPLogin(name string) string {
	base := context.Background()
	if r != nil && r.ctx != nil {
		base = r.ctx
	}
	ctx, cancel := context.WithTimeout(base, mcpLoginTimeout)
	defer cancel()
	return r.handleMCPLoginContext(ctx, base, name)
}

func (r *REPL) handleMCPLoginContext(ctx, lifecycleCtx context.Context, name string) string {
	ticket := r.beginMCPLaunchTicket(lifecycleCtx)
	defer ticket.Finish()
	operationCtx, cancelOperation := mcpLaunchOperationContext(ctx, ticket.Context())
	defer cancelOperation()
	target, err := resolveMCPLoginTarget(name)
	if err != nil {
		return redactMCPLoginError(err)
	}
	var registry *tools.Registry
	if r != nil && r.Loop != nil {
		registry = r.Loop.Registry
	}
	launch, err := runMCPLogin(operationCtx, ticket.Context(), target, registry)
	if err != nil {
		return "mcp login " + name + ": " + redactMCPLoginError(err)
	}
	if err := operationCtx.Err(); err != nil {
		closeMCPLoginLaunch(launch)
		return "mcp login " + name + ": " + redactMCPLoginError(err)
	}
	toolCount, ownsServer := adoptOrPublishMCPLoginLaunch(registry, name, launch, ticket.Adopt)
	if r != nil && ownsServer {
		r.mcpLoginServers = append(r.mcpLoginServers, launch.server)
	}
	if registry != nil {
		return fmt.Sprintf("(MCP server %q OAuth login complete · %d tools available now)", name, toolCount)
	}
	return fmt.Sprintf("(MCP server %q OAuth login complete)", name)
}

func resolveMCPLoginTarget(name string) (mcpLoginTarget, error) {
	reg, err := mcp.Load()
	if err != nil {
		return mcpLoginTarget{}, fmt.Errorf("mcp login: %w", err)
	}
	entry := mcp.FindServer(reg, name)
	if entry == nil {
		return mcpLoginTarget{}, fmt.Errorf("no MCP server named: %s", name)
	}
	expanded, err := mcp.ExpandServerEntry(*entry)
	if err != nil {
		return mcpLoginTarget{}, fmt.Errorf("mcp login %s: %w", name, err)
	}
	if expanded.URL == "" {
		return mcpLoginTarget{}, fmt.Errorf("mcp login %s: server is not an HTTP server", name)
	}
	if !strings.EqualFold(strings.TrimSpace(expanded.Auth), "oauth") {
		return mcpLoginTarget{}, fmt.Errorf("mcp login %s: server does not use OAuth (set auth = \"oauth\" in mcp.toml)", name)
	}
	return mcpLoginTarget{name: expanded.Name, url: expanded.URL}, nil
}

func (r *REPL) handleMCPList() string {
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	if len(reg.Servers) == 0 {
		return "(no MCP servers — try `mcp add <name> <command>`)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d MCP server(s) registered (in %s):\n", len(reg.Servers), mcp.Path())
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

// parseMCPAddFlags consumes leading `--env KEY=VAL` flags (repeatable)
// and returns the parsed env map plus the remaining positional args.
// `--env=KEY=VAL` is accepted as the equals-glued shorthand. An empty
// value is allowed (drops the var); an empty KEY is rejected.
func parseMCPAddFlags(tokens []string) (map[string]string, []string, error) {
	env := map[string]string{}
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		switch {
		case t == "--env":
			if i+1 >= len(tokens) {
				return nil, nil, fmt.Errorf("--env needs a KEY=VAL argument")
			}
			kv := tokens[i+1]
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, nil, fmt.Errorf("--env value must be KEY=VAL, got %q", kv)
			}
			env[k] = v
			i += 2
		case strings.HasPrefix(t, "--env="):
			kv := strings.TrimPrefix(t, "--env=")
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, nil, fmt.Errorf("--env value must be KEY=VAL, got %q", kv)
			}
			env[k] = v
			i++
		default:
			return env, tokens[i:], nil
		}
	}
	return env, nil, nil
}

func (r *REPL) handleMCPAdd(name, command string, args []string, env map[string]string) string {
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	existed := mcp.FindServer(reg, name) != nil
	if err := mcp.AddServerWithEnv(reg, name, command, args, env); err != nil {
		return "mcp: " + err.Error()
	}
	if err := mcp.Save(reg); err != nil {
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
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	if !mcp.RemoveServer(reg, name) {
		return "(no MCP server named: " + name + ")"
	}
	if err := mcp.Save(reg); err != nil {
		return "mcp: save: " + err.Error()
	}
	return "(removed MCP server: " + name + ")"
}

// handleMCPStart launches a registered MCP server and grafts its tools onto
// the live registry. Returned tools are visible to the LLM on the next turn.
func (r *REPL) handleMCPStart(name string) string {
	base := context.Background()
	if r != nil && r.ctx != nil {
		base = r.ctx
	}
	ticket := r.beginMCPLaunchTicket(base)
	defer ticket.Finish()
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(ticket.Context(), 30*time.Second)
	defer cancel()
	staged := tools.NewRegistry()
	srv, err := launchMCPServerWithLifecycle(ctx, ticket.Context(), func(liveCtx context.Context) (*mcptools.Server, error) {
		return launchConfiguredMCPServer(liveCtx, reg, name, staged, r.sandbox)
	})
	if err != nil {
		return "mcp start: " + err.Error()
	}
	discovered := staged.All()
	toolCount, ownsServer := adoptOrPublishMCPLoginLaunch(
		r.Loop.Registry, name,
		mcpLoginLaunch{server: srv, tools: discovered}, ticket.Adopt,
	)
	if ownsServer {
		r.mcpLoginServers = append(r.mcpLoginServers, srv)
	}
	return fmt.Sprintf("(MCP server %q started · %d tools registered)", name, toolCount)
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
	if r == nil || r.Loop == nil {
		return exportFailure(fmt.Errorf("no active conversation"))
	}
	history := r.Loop.History()
	if len(history) == 0 {
		return exportFailure(fmt.Errorf("no conversation to export"))
	}
	path, err := exportConversationToFile(history, args, time.Now())
	if err != nil {
		return exportFailure(err)
	}
	return exportSuccess(path)
}

// =============================================================================
// PERMISSIONS
// =============================================================================

func cmdMode(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "mode: " + string(r.Gate.Mode())
	}
	mode, ok := permission.ParseMode(args)
	if !ok {
		return "unknown mode: " + args + " (want default|acceptEdits|plan|dontAsk|bypassPermissions|fullAccess)"
	}
	if err := applyREPLPermissionMode(r, mode); err != nil {
		return "mode unchanged: " + err.Error()
	}
	return "mode set to: " + string(mode)
}

func cmdAllow(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "usage: allow <tool-name> (e.g. allow Bash)"
	}
	if r == nil || r.Gate == nil {
		return "allow: permission gate unavailable"
	}
	store := permission.Default(config.Home())
	if err := r.Gate.RememberPersistent(store, args, ""); err != nil {
		return "allowed for this session, but persistence failed: " + err.Error()
	}
	return "allowed permanently: " + args
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
	beforeTokens := r.Loop.EstimateContextTokens()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	instructions := strings.TrimSpace(args)
	result, err := r.Loop.CompactNow(ctx, agent.CompactOptions{
		Trigger:      "manual",
		Force:        true,
		Instructions: instructions,
		Persist:      r.replaceHistory,
	})
	if err != nil {
		return "compact failed: " + err.Error()
	}
	if !result.Applied {
		return fmt.Sprintf("compact: no reduction (%d messages · ~%d tokens)", before, beforeTokens)
	}
	r.resetTokenUsageAfterCompaction()
	message := fmt.Sprintf("compact: %d → %d messages · ~%d → ~%d tokens",
		result.BeforeMessages, result.AfterMessages, result.BeforeTokens, result.AfterTokens)
	if instructions != "" {
		message += " · applied custom summary instructions"
	}
	return message
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
	if err := cmd.Run(); err != nil {
		return "config editor failed: " + err.Error()
	}
	return "(config saved — restart metis for changes to take effect)"
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
			prefix := v
			if len(prefix) > 4 {
				prefix = prefix[:4]
			}
			rows = append(rows, infoRow{Key: k, Value: "set", Hint: prefix + "…"})
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

// cmdLogin / cmdLogout expose the top-level auth store without pretending a
// nested Bubble Tea wizard can run inside the active chat program. Login stays
// a precise cross-terminal handoff; logout is safe to perform directly.
func cmdLogin(r *REPL, args string) string {
	provider := strings.TrimSpace(args)
	if provider == "" {
		return "login: keep this chat open and run `metis auth login` in another terminal\n" +
			"  · the interactive wizard selects a provider and stores its credential in " + auth.Path() + "\n" +
			"  · browser OAuth: `metis auth oauth <provider>`"
	}
	return "login: inline provider selection is not supported while chat is active\n" +
		"  · keep this chat open and run `metis auth login` in another terminal, then choose " + provider + "\n" +
		"  · for OAuth providers, run `metis auth oauth " + provider + "`"
}

func cmdLogout(r *REPL, args string) string {
	providers := strings.Fields(args)
	stored, err := auth.List()
	if err != nil {
		return "logout: could not read " + auth.Path() + ": " + err.Error()
	}
	if len(providers) == 0 {
		if len(stored) == 0 {
			return "logout: no stored provider credentials in " + auth.Path()
		}
		return "logout: provider required — stored: " + strings.Join(stored, ", ") + "\n" +
			"  usage: /logout <provider> [provider...]"
	}

	known := make(map[string]bool, len(stored))
	for _, provider := range stored {
		known[provider] = true
	}
	removed := make([]string, 0, len(providers))
	missing := make([]string, 0)
	for _, provider := range providers {
		if !known[provider] {
			missing = append(missing, provider)
			continue
		}
		if err := auth.Remove(provider); err != nil {
			return "logout: remove " + provider + ": " + err.Error()
		}
		removed = append(removed, provider)
	}
	parts := make([]string, 0, 3)
	if len(removed) > 0 {
		parts = append(parts, "logout: removed stored credentials for "+strings.Join(removed, ", "))
	}
	if len(missing) > 0 {
		parts = append(parts, "no stored credential for "+strings.Join(missing, ", "))
	}
	parts = append(parts, "environment variables are unchanged and may still authenticate the provider")
	return strings.Join(parts, "\n")
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

// cmdStatusLine reports the actual customization contract implemented by
// statusline_script.go: an executable statusline script under METIS_HOME.
func cmdStatusLine(r *REPL, args string) string {
	home := config.Home()
	path := filepath.Join(home, "statusline.sh")
	state := "not installed"
	if active := statusLineScriptPath(); active != "" {
		path = active
		state = "active"
	} else {
		for _, candidate := range []string{path, filepath.Join(home, "statusline")} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				path = candidate
				state = "present but not executable (run chmod +x)"
				break
			}
		}
	}
	return renderInfoBox("Status Line", []infoRow{
		{Key: "current layout", Value: "elapsed · effort · mode · subagents · tokens"},
		{Key: "custom script", Value: path},
		{Key: "script status", Value: state},
		{Key: "refresh", Value: statusLineRefresh.String()},
		{Key: "", Value: ""},
		{Key: "Customize", Value: "create the script above, make it executable, and print one line to stdout"},
		{Key: "environment", Value: "METIS_MODEL, METIS_MODE, METIS_SESSION, METIS_TOKENS_TOTAL, METIS_CWD"},
		{Key: "", Value: ""},
		{Key: "Built-in pieces:", Value: ""},
		{Key: "  elapsed", Value: "wall-clock for the current turn"},
		{Key: "  effort", Value: "reasoning effort glyph (low/med/high/off)"},
		{Key: "  mode", Value: "permission mode (auto/ask/bypass/plan/deny)"},
		{Key: "  subagents", Value: "active Agent tool sub-agent pills"},
		{Key: "  tokens", Value: "context-window load + percent (right-aligned)"},
		{Key: "  spinner", Value: "↑in / ↓out per-turn breakdown"},
	})
}

func cmdCost(r *REPL, args string) string {
	// Mirror renderCost (render_info.go) so the REPL fast-path and the
	// slash.Registry path produce identical output.
	in := r.totalTokens.in
	out := r.totalTokens.out
	cacheCreate := r.totalTokens.CacheCreate()
	cacheRead := r.totalTokens.CacheRead()
	total := r.totalTokens.PromptTokens() + out
	priceIn, priceOut := guessPriceUSDPerM(r.Loop.Model)
	// Cache reads bill at 10% of fresh input on Anthropic; cache_create
	// at 125%. Estimate the savings: (read × 0.9 × priceIn) is how much
	// cheaper this session was versus paying full input rate. Useful
	// to show users "your /memory + addendum sectioning earned you $X".
	costUSD := (float64(in)+float64(cacheCreate)*1.25+float64(cacheRead)*0.10)*priceIn/1_000_000 +
		float64(out)*priceOut/1_000_000
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

// cmdUsage surfaces what we can know without provider-specific quota
// endpoints: current session totals (already tracked in r.totalTokens)
// and the dashboard URL for the active provider so the user can jump
// to authoritative numbers in one click.
//
// Dashboards are hand-mapped by provider name — there is no portable
// "rate limit endpoint" across Anthropic/DeepSeek/Kimi/MiniMax/GLM
// today, so this is the honest answer.
func cmdUsage(r *REPL, args string) string {
	dashboards := map[string]string{
		"anthropic": "https://console.anthropic.com/settings/usage",
		"openai":    "https://platform.openai.com/usage",
		"deepseek":  "https://platform.deepseek.com/usage",
		"kimi":      "https://platform.moonshot.cn/console/info",
		"minimax":   "https://platform.minimaxi.com/user-center/basic-information/interface-key",
		"glm":       "https://open.bigmodel.cn/usercenter/resourcepack",
		"zhipu":     "https://open.bigmodel.cn/usercenter/resourcepack",
	}
	t := &r.totalTokens
	rows := []infoRow{
		{Key: "provider", Value: r.providerName},
		{Key: "model", Value: r.model},
	}
	if url, ok := dashboards[strings.ToLower(r.providerName)]; ok {
		rows = append(rows, infoRow{Key: "dashboard", Value: url, Hint: "open for live rate-limit + quota"})
	} else {
		rows = append(rows, infoRow{Key: "dashboard", Value: "(unknown — check your provider's console)"})
	}
	rows = append(rows,
		infoRow{Key: "", Value: "── session totals ──"},
		infoRow{Key: "input", Value: fmtThousands(t.Input())},
		infoRow{Key: "output", Value: fmtThousands(t.Output())},
		infoRow{Key: "cache_create", Value: fmtThousands(t.CacheCreate())},
		infoRow{Key: "cache_read", Value: fmtThousands(t.CacheRead())},
		infoRow{Key: "cache hit rate", Value: fmt.Sprintf("%.1f%%", t.CacheHitRate()*100)},
	)
	return renderInfoBox("Usage", rows)
}

func cmdStack(r *REPL, args string) string {
	return string(debug.Stack())
}

// =============================================================================
// SKILL HELPERS
// =============================================================================

func (r *REPL) handleSkillList() string {
	return r.renderManagedSkillsList()
}

func (r *REPL) handleSkillInstall(name string) string {
	return r.skillInstallPlan(name)
}

func (r *REPL) handleSkillUninstall(name string) string {
	return r.removeManagedSkill(name)
}

// handleSkillSearch is the compatibility wrapper for the canonical guarded
// local-search + install-planner path. It performs no network request itself.
func (r *REPL) handleSkillSearch(query string) string {
	return r.handleSkillSearchLocal(query)
}

// tokenTracker tracks token usage with two distinct concepts:
//
//   - Session cumulative (fresh in / cache / out) — for `/cost` billing
//     summaries.
//     Every API call adds to these; they only grow.
//
//   - Most-recent API call (lastIn / lastOut / lastCacheCreate /
//     lastCacheRead) — overwritten on every API call. Two distinct
//     status displays read from these:
//
//     (a) Spinner row "↓ 38123 tokens"  →  LastTotal() == full prompt + lastOut
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
// The two numbers diverge only because LastTotal adds output while
// ContextUsage is input-side context pressure.
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

// LastCacheHitRate is the most-recent-turn cache hit fraction.
// Same formula as CacheHitRate but anchored on lastIn / lastCacheCreate
// / lastCacheRead. Used by /context to display the current turn's
// hit rate alongside the session average so the user can see when a
// big tool_result just busted their cache prefix.
func (t *tokenTracker) LastCacheHitRate() float64 {
	denom := t.lastCacheRead + t.lastCacheCreate + t.lastIn
	if denom == 0 {
		return 0
	}
	return float64(t.lastCacheRead) / float64(denom)
}

// LastTotal is the most recent API call's full prompt+output combined — the
// per-turn cost. Spinner row uses this to surface what the just-finished
// round trip consumed.
func (t *tokenTracker) LastTotal() int { return t.LastPromptTokens() + t.lastOut }

// PromptTokens and LastPromptTokens add the mutually-exclusive fresh,
// cache-create and cache-read input buckets exactly once. OpenAI-compatible
// adapters normalize cached input out of `in` before events reach this type.
func (t *tokenTracker) PromptTokens() int {
	return t.in + t.cacheCreate + t.cacheRead
}

func (t *tokenTracker) LastPromptTokens() int {
	return t.lastIn + t.lastCacheCreate + t.lastCacheRead
}

// ContextUsage is the most recent API call's input-side total including
// prompt-cache tokens. Bottom-right status bar uses this to show
// context-window load — distinct from per-turn cost (which still
// includes output).
//
// Provider adapters normalize wire usage before it reaches this tracker:
// OpenAI/DeepSeek prompt_tokens includes cached input, so that adapter emits
// `input = prompt - cached`; Anthropic already reports the buckets separately.
// Summing the three normalized buckets here therefore counts each prompt token
// exactly once. This fixes the real >100% cause instead of hiding it by
// discarding cache_read or only clamping the rendered percentage.
func (t *tokenTracker) ContextUsage() int {
	return t.LastPromptTokens()
}

// ResetLast invalidates only the latest-call/context display fields. A
// compaction rewrites active history but does not refund tokens already spent,
// so cumulative /cost counters and their smoothed display values remain.
func (t *tokenTracker) ResetLast() {
	t.lastIn = 0
	t.lastOut = 0
	t.lastCacheCreate = 0
	t.lastCacheRead = 0
}

// Reset zeroes both raw and displayed counters. Called by /clear, /new and
// session switches; successful compaction uses ResetLast so historical spend
// remains visible.
func (t *tokenTracker) Reset() {
	t.in = 0
	t.out = 0
	t.cacheCreate = 0
	t.cacheRead = 0
	t.ResetLast()
	t.dispIn = 0
	t.dispOut = 0
}

// Snapshot returns the cumulative session totals — for persisting cost
// across a resume.
func (t *tokenTracker) Snapshot() (in, out, cacheCreate, cacheRead int) {
	return t.in, t.out, t.cacheCreate, t.cacheRead
}

// Restore seeds the cumulative totals from a persisted snapshot so a
// resumed session's /cost reflects pre-resume spend (claude-code parity —
// CC stores/restores session cost via project config). The display values
// are set to match so the restored totals show immediately rather than
// animating up from zero. last* (per-turn) stay zero — there's no "most
// recent call" until the next turn runs.
func (t *tokenTracker) Restore(in, out, cacheCreate, cacheRead int) {
	t.in, t.out = in, out
	t.cacheCreate, t.cacheRead = cacheCreate, cacheRead
	t.dispIn, t.dispOut = in, out
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

// =============================================================================
// DESKTOP FEATURES (Codex parity)
// =============================================================================

// cmdResume opens the enhanced resume/fork session picker.
func cmdResume(r *REPL, args string) string {
	if r.Session == nil {
		return "session store not available"
	}
	cwd, _ := os.Getwd()
	sessions, err := r.Session.ListResumable(session.ResumeListOptions{Limit: 50, WorkDir: cwd})
	if err != nil {
		return "failed to list sessions: " + err.Error()
	}
	// This is a placeholder — the actual screen is opened via the TUI's
	// applyScreenResult path. The REPL fallback just lists sessions.
	var b strings.Builder
	b.WriteString("Recent sessions:\n")
	for i, s := range sessions {
		if i >= 20 {
			break
		}
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		b.WriteString(fmt.Sprintf("  %s  %s\n", shortID(s.ID), title))
	}
	b.WriteString("\nUse /resume in the TUI for the full picker with search and fork.")
	return b.String()
}

// cmdDiffView opens the full-screen git diff viewer.
func cmdDiffView(r *REPL, args string) string {
	return "opening diff viewer..."
}

// cmdAgentsView is the headless fallback for the TUI-owned /agents screen.
func cmdAgentsView(r *REPL, args string) string {
	return "opening agents view..."
}

// cmdDesktop launches the native desktop app.
func cmdDesktop(r *REPL, args string) string {
	cwd, _ := os.Getwd()
	return fmt.Sprintf("desktop app launch: run 'metis desktop' from terminal to launch the native app for workspace %s", cwd)
}
