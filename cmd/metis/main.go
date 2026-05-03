package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/Ricardo-M-L/metis/acp"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	taskstore "github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
	"github.com/Ricardo-M-L/metis/internal/tui"
	"github.com/Ricardo-M-L/metis/internal/version"
)

const defaultSystem = `You are metis, a fast, local-first agent CLI.
You assist with software engineering tasks. You have access to tools that
let you read/write files, search the codebase, run shell commands, and
fetch URLs. Prefer concrete actions over speculation. Be terse. When you
finish a task, summarize in one sentence.`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := dispatch(ctx, os.Args[1:]); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "metis:", err)
		}
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return cmdChat(ctx, args)
	}
	switch args[0] {
	case "chat":
		return cmdChat(ctx, args[1:])
	case "run":
		return cmdRun(ctx, args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "tools":
		return cmdTools()
	case "sessions":
		return cmdSessions(args[1:])
	case "skills":
		return cmdSkills(args[1:])
	case "acp":
		return cmdACP(ctx, args[1:])
	case "cron":
		return cmdCron(ctx, args[1:])
	case "auth":
		return cmdAuth(ctx, args[1:])
	case "plugin", "plugins":
		return cmdPlugin(ctx, args[1:])
	case "audit":
		return cmdAudit()
	case "diag":
		return cmdDiag(ctx, args[1:])
	case "update":
		return cmdUpdate(ctx, args[1:])
	case "version", "-v", "--version":
		return cmdVersion(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		// No subcommand: treat as inline prompt for `run`
		return cmdRun(ctx, args)
	}
}

func printUsage() {
	fmt.Println(`metis — fast, local-first agent CLI

Usage:
  metis                 Start an interactive chat
  metis chat            Start an interactive chat
  metis run <prompt>    Non-interactive: one prompt, print result, exit
  metis config show     Print effective config + which files were read
  metis config init     Write a starter config to ~/.metis/config.toml
  metis tools           List available tools
  metis sessions list   List recent saved sessions
  metis sessions export <id>      Print a session's JSONL to stdout
  metis sessions import [--id ID] Read JSONL from stdin and create a new session
  metis skills list     List built-in skills library
  metis skills install <name>  Install a built-in skill
  metis acp [--addr ADDR]  Run as Agent Client Protocol server (default: stdio)
  metis cron <list|add|rm|pause|resume|run|start>  Manage scheduled prompts
  metis auth <login|logout|list>  Manage provider credentials (~/.metis/auth.json)
  metis audit           Print a security audit of the current configuration
  metis diag [--llm] [--tool-smoke] [--json]  Run a non-interactive health check
  metis update [--check]  Self-update from the private release (needs METIS_GITHUB_TOKEN)
  metis version [-V]    Print version (-V for build fingerprint)
  metis help            This help

Flags (chat / run):
  -m, --model <id>      Override model
  -p, --provider <id>   Override provider (anthropic | openai | gemini | <custom>)
      --mode <id>       Permission mode: ask | auto | bypass | plan | deny
      --no-markdown     Disable markdown rendering of assistant output
      --no-stream       Don't stream (assemble then print)
      --max-iter <n>    Iteration cap per turn (default 50)
      --add-dir <path>  Add a directory to the agent's accessible scope (repeatable)
      --agent <name>    Load an agent profile from ~/.metis/agents/<name>.md
  -W, --worktree [slug] Spawn in a fresh git worktree (slug optional)

Env:
  ANTHROPIC_API_KEY     Required for Anthropic provider
  OPENAI_API_KEY        Required for OpenAI provider
  GEMINI_API_KEY        Required for Gemini provider (GOOGLE_API_KEY also accepted)
  METIS_DEBUG=1         Verbose error output
  METIS_TICK_MS=N       Override TUI tick interval (default 40ms; 1-1000)`)
}

// --- runtime wiring shared between chat and run ---

type runtime struct {
	cfg         *config.Config
	provider    llm.Provider
	registry    *tools.Registry
	gate        *permission.Gate
	store       *session.Store
	sessionID   string
	loop        *agent.Loop
	useMD       bool
	showTok     bool
	model       string
	mcpServers  []*mcptools.Server
	plugins     *rtpkg.PluginRegistry // nil when no plugins installed
	allowedDirs *rtpkg.AllowedDirs    // --add-dir state, persisted to ~/.metis/additional-dirs.json
}

// Cleanup closes any subprocesses or connections owned by the runtime.
// Safe to call multiple times.
func (r *runtime) Cleanup() {
	for _, s := range r.mcpServers {
		_ = s.Close()
	}
	r.mcpServers = nil
	if r.plugins != nil {
		_ = r.plugins.Close()
		r.plugins = nil
	}
}

type cliFlags struct {
	model        string
	provider     string
	mode         string
	noMarkdown   bool
	noStream     bool
	maxIter      int
	system       string
	resumeID     string
	useTUI       bool
	noAuthWizard bool   // skip the first-run wizard (CI / scripted use)
	effort       string // "low" | "medium" | "high" — Anthropic thinking budget / OpenAI reasoning_effort
	fast         bool   // collapse one turn: effort=low + halved max_tokens
	addDirs      stringList
	agentProfile string // --agent=NAME — load .metis/agents/<name>.md
	worktree     string // --worktree=slug — spin up git worktree with explicit slug
	worktreeOn   bool   // -W — spin up worktree with auto slug
}

// stringList accumulates repeated flag values: --add-dir A --add-dir B → [A,B].
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	if v == "" {
		return nil
	}
	*s = append(*s, v)
	return nil
}

func parseFlags(args []string) (*cliFlags, []string, error) {
	f := flag.NewFlagSet("metis", flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	out := &cliFlags{}
	f.StringVar(&out.model, "model", "", "model id")
	f.StringVar(&out.model, "m", "", "model id (shorthand)")
	f.StringVar(&out.provider, "provider", "", "provider id")
	f.StringVar(&out.provider, "p", "", "provider id (shorthand)")
	f.StringVar(&out.mode, "mode", "", "permission mode")
	f.BoolVar(&out.noMarkdown, "no-markdown", false, "disable markdown rendering")
	f.BoolVar(&out.noStream, "no-stream", false, "disable streaming")
	f.IntVar(&out.maxIter, "max-iter", 0, "max tool iterations per turn")
	f.StringVar(&out.system, "system", "", "override system prompt")
	f.StringVar(&out.resumeID, "resume", "", "resume session id")
	f.BoolVar(&out.useTUI, "tui", false, "use the TUI (default when stdout is a TTY)")
	f.BoolVar(&out.noAuthWizard, "no-auth-wizard", false, "skip the first-run auth wizard if no API key is found")
	f.StringVar(&out.effort, "effort", "", "reasoning intensity: low | medium | high")
	f.BoolVar(&out.fast, "fast", false, "fast turn: effort=low + halved max_tokens")
	f.Var(&out.addDirs, "add-dir", "additional directory accessible to tools (repeatable)")
	f.StringVar(&out.agentProfile, "agent", "", "load agent profile from ~/.metis/agents/<name>.md")
	f.StringVar(&out.worktree, "worktree", "", "spawn a git worktree for this session (slug)")
	f.BoolVar(&out.worktreeOn, "W", false, "spawn a git worktree with an auto-generated slug")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	return out, f.Args(), nil
}

func setupRuntime(ctx context.Context, flags *cliFlags) (*runtime, error) {
	cfg, loaded, err := config.Load()
	if err != nil {
		return nil, err
	}
	if os.Getenv("METIS_DEBUG") == "1" {
		fmt.Fprintln(os.Stderr, "metis: loaded config files:", loaded)
	}

	// Agent profile (--agent=NAME) loads early so its values participate
	// in flag merging and tool-registry filtering downstream. CLI overrides
	// always win — profile is "use this default unless I said otherwise".
	agentProf, err := rtpkg.LoadAgentProfile(flags.agentProfile)
	if err != nil {
		return nil, err
	}

	// Apply flag overrides
	provName := cfg.Provider.Default
	if flags.provider != "" {
		provName = flags.provider
	}
	model := flags.model
	mode := cfg.Permission.Mode
	if flags.mode != "" {
		mode = flags.mode
	}
	// Profile-on-CLI merge.
	mergedModel, mergedMode, mergedEffort, mergedMaxIter :=
		agentProf.MergeOnto(model, mode, flags.effort, flags.maxIter)
	model = mergedModel
	mode = mergedMode
	if flags.effort == "" {
		flags.effort = mergedEffort
	}
	if flags.maxIter == 0 {
		flags.maxIter = mergedMaxIter
	}

	// First-run auth gate: launches the wizard when no key is resolvable
	// AND stderr is interactive AND the user hasn't passed --no-auth-wizard.
	cfg, provName, err = rtpkg.EnsureAPIKey(cfg, provName, rtpkg.AuthGateOptions{
		NoWizard:  flags.noAuthWizard,
		IsTTY:     func() bool { return term.IsTerminal(int(os.Stderr.Fd())) },
		RunWizard: wizardAdapter,
		Stderr:    os.Stderr,
	})
	if err != nil {
		return nil, err
	}

	// Build LLM provider client + resolve final model id. Bootstrap logic
	// lives in internal/runtime so main.go stays composer-only.
	pbuild, err := rtpkg.BuildProvider(cfg, provName, model)
	if err != nil {
		return nil, err
	}
	prov := pbuild.Provider
	model = pbuild.Model

	// Permission gate (allow/deny rules from config.toml).
	gate := rtpkg.BuildPermissionGate(cfg, mode)

	// Tool registry: built-ins + Agent + SendMessage in one go. Channel
	// adapter assembly happens inside BuildToolRegistry's caller; we pass
	// it the configured registry so SendMessage can advertise the right
	// platforms.
	system := defaultSystem
	if flags.system != "" {
		system = flags.system
	}
	// Agent profile body REPLACES the default system prompt — that's the
	// whole point of "I am a code reviewer". Skip when profile body is
	// empty (frontmatter-only profiles still customize tools/model).
	if agentProf != nil && agentProf.SystemPrompt != "" {
		system = agentProf.SystemPrompt
	}
	// Append user's optional ~/.metis/system.md addendum (claude-code-style
	// global system prompt). No-op when the file doesn't exist or the
	// profile asked us to skip it.
	if agentProf == nil || !agentProf.OmitClaudeMd {
		system = rtpkg.AssembleSystemPrompt(system)
	}

	// --add-dir / persisted additional dirs. Done before system prompt
	// finalization so the LLM sees the list at turn 0.
	allowedDirs := rtpkg.NewAllowedDirs(flags.addDirs)
	if extra := allowedDirs.SystemPromptAddendum(); extra != "" {
		system = system + extra
	}
	chReg := rtpkg.BuildChannelRegistry(&cfg.Channels)
	// Cron service is shared between the scheduler goroutine (started
	// elsewhere) and the ScheduleWakeup tool we register below — they
	// both need the same on-disk job set, so we construct one instance
	// here and thread it through.
	cronSvc, err := agent.NewCronService(filepath.Join(cfg.Session.Dir, "cron"))
	if err != nil {
		// Non-fatal: chat still works, just without ScheduleWakeup /
		// /cron commands. Surface the error so the user can fix
		// permission issues on ~/.metis/sessions/cron/.
		fmt.Fprintf(os.Stderr, "warning: cron service init failed: %v\n", err)
	}
	// MemoryManager built up-front so the same instance is shared by:
	//   1. The Memory tool (LLM writes go through Core.UpdateBlock here)
	//   2. The agent.Loop (BuildContext renders the same store at request
	//      time, so writes from #1 land in the next turn's system prompt)
	// Pre-fix this was constructed inside BuildAgentLoop and the Memory
	// tool maintained its own ~/.metis/memories/*.md fork, never reaching
	// BuildContext (the 2026-04-30 audit disconnect bug).
	memoryMgr := rtpkg.BuildMemoryManager(cfg)
	reg := rtpkg.BuildToolRegistry(rtpkg.ToolRegistryOptions{
		Cfg:             cfg,
		Gate:            gate,
		Provider:        prov,
		Model:           model,
		System:          system,
		ChannelRegistry: chReg,
		DefaultPlatform: cfg.Channels.DefaultPlatform,
		CronService:     cronSvc,
		MemoryManager:   memoryMgr,
	})

	// MCP servers — launch each enabled stdio server, register its tools as
	// `mcp__<name>__<tool>`. Failures are non-fatal: warn and continue.
	//
	// Sources merge in this order: runtime entries from ~/.metis/mcp.toml
	// (mutated by /mcp add) win over [[mcp.servers]] declared in
	// config.toml. The merge happens via runtime.MergeWithConfig so legacy
	// config-only setups still work without any user action.
	mcpReg, mcpLoadErr := rtpkg.LoadMCP()
	if mcpLoadErr != nil {
		fmt.Fprintf(os.Stderr, "metis: mcp.toml: %v\n", mcpLoadErr)
		mcpReg = &rtpkg.MCPRegistry{}
	}
	mcpReg.MergeWithConfig(cfg.MCP.Servers)
	mcpServers, mcpErrs := rtpkg.LaunchAllMCP(ctx, mcpReg, reg)
	for _, e := range mcpErrs {
		fmt.Fprintf(os.Stderr, "metis: MCP launch: %v\n", e)
	}

	// Agent profile tool filter — applied after MCP tools register so the
	// profile's allowlist / disallowed_tools cover dynamically-loaded MCP
	// tools too. No-op when no profile is loaded.
	if agentProf != nil && (len(agentProf.Tools) > 0 || len(agentProf.DisallowedTools) > 0) {
		all := make([]string, 0, len(reg.All()))
		for _, t := range reg.All() {
			all = append(all, t.Name())
		}
		reg.Restrict(agentProf.FilterToolNames(all))
	}

	// Plugins (MCP-bundle style): each ~/.metis/plugins/<name>/plugin.toml
	// can declare an MCP server + skill files + (advanced) hook subprocesses.
	// Load failures are surfaced on stderr but don't block startup.
	pluginReg, pluginErrs := rtpkg.LoadPlugins(ctx, reg)
	for _, e := range pluginErrs {
		fmt.Fprintf(os.Stderr, "metis: plugin load: %v\n", e)
	}
	// Re-register the Skill tool now that we know the plugin sources.
	// First phase happened inside BuildToolRegistry with PluginSources=nil
	// so the bundled+user+project layers were already exposed to the LLM;
	// this second phase folds in plugin-contributed skills. Without this
	// step the LLM couldn't see anything plugins ship (E2E test bug #3).
	if pluginReg != nil {
		rtpkg.RegisterSkillTool(reg, rtpkg.ToolRegistryOptions{
			Cfg:           cfg,
			Gate:          gate,
			PluginSources: pluginReg.SkillSources(),
		})
	}

	// Session store
	store, err := session.NewStore(cfg.Session.Dir)
	if err != nil {
		return nil, err
	}

	maxIter := flags.maxIter
	if maxIter == 0 {
		maxIter = cfg.Session.MaxIterations
	}

	loop := rtpkg.BuildAgentLoop(cfg, rtpkg.AgentLoopOptions{
		Provider: prov, Registry: reg, Gate: gate,
		System: system, Model: model, MaxIter: maxIter,
		MemoryManager: memoryMgr,
	})

	// Apply --effort / --fast flag overrides. Effort goes through the
	// canonical pkg/llm parser so "low"/"l"/"fast" all normalize to the
	// same EffortLow value; an unknown string falls back to default and
	// emits a one-line warning rather than silently dropping the user's
	// intent. --fast is a separate boolean handled by Loop.buildRequest.
	if flags.effort != "" {
		if e, ok := llm.ParseEffort(flags.effort); ok {
			loop.Effort = e
		} else {
			fmt.Fprintf(os.Stderr, "metis: --effort %q ignored (must be low|medium|high)\n", flags.effort)
		}
	}
	if flags.fast {
		loop.Fast = true
	}

	rt := &runtime{
		cfg: cfg, provider: prov, registry: reg, gate: gate, store: store,
		loop: loop, useMD: cfg.UI.Markdown && !flags.noMarkdown,
		showTok: cfg.UI.ShowTokens, model: model,
		mcpServers:  mcpServers,
		plugins:     pluginReg,
		allowedDirs: allowedDirs,
	}

	if flags.resumeID != "" {
		res, err := rtpkg.ApplyResume(store, flags.resumeID, loop, gate, os.Stderr)
		if err != nil {
			return nil, err
		}
		rt.sessionID = res.SessionID
	} else {
		rt.sessionID = store.NewSessionID()
		_ = rtpkg.WriteFreshHeader(store, rt.sessionID, model, system, cfg.Permission.Mode)
	}
	// Tell the tasks pkg what session id is current so TodoWrite /
	// TodoRead persist into the right per-session file. Done last so a
	// resume failure above doesn't leave a stale id in the singleton.
	rtpkg.SetCurrentSessionID(rt.sessionID)
	// Same dance for the structured Task* tools (TaskCreate / TaskList /
	// TaskUpdate / TaskOutput / TaskStop / TaskGet). Lives in the
	// internal/tasks package (not runtime) to break an otherwise
	// circular import (tools/builtin → runtime → tools/builtin).
	taskstore.SetCurrentTaskStore(rt.sessionID)

	// Push UI performance tunables into the TUI package so its per-tick
	// helpers (tickInterval / eventBufferSize / mouseWheelLines) read
	// the user's preferences instead of the built-in defaults. Env vars
	// still override per call — this is the persistent layer.
	tui.SetPerfConfig(tui.PerfConfig{
		TickMs:          cfg.UI.Performance.TickMs,
		EventBufferSize: cfg.UI.Performance.EventBufferSize,
		MouseWheelLines: cfg.UI.Performance.MouseWheelLines,
		ReducedMotion:   cfg.UI.Performance.ReducedMotion,
		SlowRenderMs:    cfg.UI.Performance.SlowRenderMs,
		StatsLogEvery:   cfg.UI.Performance.StatsLogEvery,
		MaxMountedItems: cfg.UI.Performance.MaxMountedItems,
		ScrollQuantum:   cfg.UI.Performance.ScrollQuantum,
	})
	return rt, nil
}

// NewRegistry helper because tools.NewRegistry doesn't exist; create one.
// We rely on the global registry pattern for built-ins, so make a fresh instance.

// --- subcommands ---

func cmdChat(ctx context.Context, args []string) error {
	flags, _, err := parseFlags(args)
	if err != nil {
		return err
	}
	// Trust-this-folder safety check — claude-code's first-run dance.
	// Skipped when stdin isn't a terminal (CI, expect-scripted) or
	// when METIS_NO_TRUST_PROMPT=1. Persists answer to
	// ~/.metis/trusted-dirs.json so it asks once per directory.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if err := ensureTrusted(); err != nil {
			return err
		}
		maybeNotifyUpdate()
	}

	// --worktree: spawn a git worktree first so config/CLAUDE.md/etc.
	// load from the new cwd. The worktree info is stashed in a closure
	// for cleanup at the end of cmdChat.
	var worktreeInfo *rtpkg.WorktreeInfo
	if flags.worktree != "" || flags.worktreeOn {
		info, err := rtpkg.SpawnWorktree(flags.worktree)
		if err != nil {
			return err
		}
		if err := os.Chdir(info.Path); err != nil {
			return fmt.Errorf("chdir to worktree %s: %w", info.Path, err)
		}
		worktreeInfo = info
		fmt.Fprintf(os.Stderr, "(worktree: %s on branch %s)\n", info.Path, info.Branch)
	}

	rt, err := setupRuntime(ctx, flags)
	if err != nil {
		return err
	}
	defer rt.Cleanup()
	defer func() {
		if worktreeInfo != nil && worktreeInfo.Created {
			// Default policy: KEEP the worktree on disk so the user can
			// inspect commits / cherry-pick. Sweep policy in worktree.go
			// reaps after 30 days. Future: prompt the user; for now,
			// no-op cleanup mirrors claude-code's "keep" default.
			_ = worktreeInfo
		}
	}()
	sl := buildSlash(rt)

	useTUI := flags.useTUI
	if !useTUI {
		// Auto-detect TTY: use TUI when stdout is a terminal
		useTUI = term.IsTerminal(int(os.Stdout.Fd()))
	}

	if useTUI {
		hooks := tui.ExternalHooks{
			DirAdd:    rt.allowedDirs.Add,
			DirRemove: rt.allowedDirs.Remove,
			DirList:   rt.allowedDirs.All,
			BtwAsk:    rt.askSideQuestion,
		}
		return tui.RunTUI(ctx, rt.loop, sl, rt.store, rt.sessionID, rt.gate, rt.model, rt.cfg.Session.SkillDir, rt.cfg, true, hooks) // true = force new session banner
	}

	repl, err := tui.NewREPL(rt.loop, sl, rt.store, rt.sessionID, rt.useMD, rt.showTok, rt.gate, rt.model, rt.cfg.Session.SkillDir)
	if err != nil {
		return err
	}
	return repl.Run(ctx)
}

func cmdRun(ctx context.Context, args []string) error {
	flags, rest, err := parseFlags(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("run: prompt is required")
	}
	prompt := joinSpaces(rest)
	rt, err := setupRuntime(ctx, flags)
	if err != nil {
		return err
	}
	defer rt.Cleanup()
	rt.loop.AppendUser(prompt)
	if rt.store != nil && rt.sessionID != "" {
		_ = rt.store.AppendMessage(rt.sessionID, llm.Message{
			Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: prompt}},
		})
	}
	_ = rtpkg.AppendHistory(rtpkg.HistoryEntry{
		SessionID: rt.sessionID, Input: prompt, Source: "run",
	})

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() { done <- rt.loop.Run(ctx, events); close(events) }()

	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			fmt.Print(ev.TextDelta)
		case agent.EventToolStart:
			fmt.Fprintf(os.Stderr, "\n[tool] %s\n", ev.ToolName)
		case agent.EventToolResult:
			if ev.ToolResult != nil && ev.ToolResult.IsError {
				fmt.Fprintf(os.Stderr, "[tool error] %s\n", truncStderr(ev.ToolResult.Output, 300))
			}
		case agent.EventPermissionRequest:
			ev.PermissionReply <- agent.PermissionDecisionDeny
			fmt.Fprintf(os.Stderr, "[permission] denied (non-interactive); use chat for interactive prompts\n")
		case agent.EventPlan:
			// Plan mode: archive + print a structured summary so the
			// non-interactive caller can pipe the result.
			_ = rtpkg.ArchivePlan(rtpkg.ArchivedPlan{
				SessionID: rt.sessionID,
				ToolCalls: ev.ToolCalls,
			})
			fmt.Fprintf(os.Stderr, "\n[plan] %d tool call(s) — archived to ~/.metis/plans/\n", len(ev.ToolCalls))
			for _, tc := range ev.ToolCalls {
				fmt.Fprintf(os.Stderr, "  → %s(%v)\n", tc.Name, tc.Input)
			}
		case agent.EventInfo:
			// Surface auto-compaction notices ("context compacted: M → N
			// messages"), loop-detector aborts, and similar non-error
			// status. Goes to stderr so piping stdout (the assistant's
			// reply) into another tool stays clean. The TUI handles
			// EventInfo separately; this branch is the metis-run /
			// metis-cron-run / non-interactive path.
			if ev.Info != "" {
				fmt.Fprintf(os.Stderr, "[info] %s\n", ev.Info)
			}
		case agent.EventLoopDone:
			fmt.Println()
		case agent.EventError:
			return ev.Err
		}
	}
	return <-done
}

func cmdACP(ctx context.Context, args []string) error {
	addr := "stdio"
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--addr" && i+1 < len(args) {
			addr = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	flags, _, err := parseFlags(rest)
	if err != nil {
		return err
	}
	rt, err := setupRuntime(ctx, flags)
	if err != nil {
		return err
	}
	defer rt.Cleanup()
	srv := acp.NewServer(rt.loop, addr)
	if err := srv.Listen(); err != nil {
		return err
	}
	if addr == "stdio" || addr == "" {
		srv.Wait()
		return nil
	}
	fmt.Fprintf(os.Stderr, "metis acp listening on %s (Ctrl-C to stop)\n", addr)
	<-ctx.Done()
	return srv.Close()
}

// cmdVersion handles `metis version` (and the `--version` / `-v` aliases).
// Default form mirrors `claude --version`: just the number and product name.
// Pass `--verbose` (or `-V`) for the full build fingerprint — useful when
// you're trying to figure out which build is actually on your $PATH.
func cmdVersion(args []string) error {
	verbose := false
	for _, a := range args {
		if a == "--verbose" || a == "-V" {
			verbose = true
		}
	}

	hasCommit := version.Commit != "" && version.Commit != "unknown"
	if !verbose {
		if hasCommit {
			fmt.Printf("%s (Metis · %s)\n", version.Version, version.Commit)
		} else {
			fmt.Printf("%s (Metis)\n", version.Version)
		}
		return nil
	}

	fmt.Printf("%s (Metis)\n", version.Version)
	if hasCommit {
		fmt.Printf("  commit: %s\n", version.Commit)
	}
	if version.Date != "" && version.Date != "unknown" {
		fmt.Printf("  built:  %s\n", version.Date)
	}
	fmt.Printf("  go:     %s\n", goruntime.Version())
	fmt.Printf("  os:     %s/%s\n", goruntime.GOOS, goruntime.GOARCH)
	exe, _ := os.Executable()
	if exe != "" {
		fmt.Printf("  binary: %s\n", exe)
	}
	return nil
}

func cmdAudit() error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	reg := tools.NewRegistry()
	gate := permission.New(permission.Mode(cfg.Permission.Mode))
	builtin.Register(reg, cfg, gate)

	rep := security.Run(cfg, reg)
	crit, warn, info := rep.Counts()

	if len(rep.Findings) == 0 {
		fmt.Println("audit: no issues found")
		return nil
	}
	for _, f := range rep.Findings {
		fmt.Printf("[%s] %s — %s (%s)\n", f.Severity, f.Code, f.Message, f.Resource)
	}
	fmt.Printf("\nsummary: %d critical, %d warn, %d info\n", crit, warn, info)
	if rep.HasCritical() {
		return fmt.Errorf("audit: %d critical finding(s)", crit)
	}
	return nil
}

func cmdCron(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis cron <list|add|rm|pause|resume|run|start>")
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	cronRoot := filepath.Join(cfg.Session.Dir, "cron")
	svc, err := agent.NewCronService(cronRoot)
	if err != nil {
		return err
	}

	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		jobs := svc.List()
		if len(jobs) == 0 {
			fmt.Println("(no cron jobs)")
			return nil
		}
		for _, j := range jobs {
			next := "—"
			if !j.NextRun.IsZero() {
				next = j.NextRun.Format(time.RFC3339)
			}
			fmt.Printf("%s  enabled=%v paused=%v next=%s name=%q\n",
				j.ID, j.Enabled, j.Paused, next, j.Name)
		}
		return nil
	case "rm":
		if len(rest) == 0 {
			return errors.New("rm: missing id")
		}
		return svc.Remove(rest[0])
	case "pause":
		if len(rest) == 0 {
			return errors.New("pause: missing id")
		}
		return svc.Pause(rest[0])
	case "resume":
		if len(rest) == 0 {
			return errors.New("resume: missing id")
		}
		return svc.Resume(rest[0])
	case "add":
		return cmdCronAdd(svc, rest)
	case "run":
		return cmdCronRun(ctx, svc, rest)
	case "start":
		return cmdCronStart(ctx, svc)
	}
	return fmt.Errorf("cron: unknown subcommand %q", sub)
}

func cmdCronAdd(svc *agent.CronService, args []string) error {
	fs := flag.NewFlagSet("cron add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var name, prompt, every, at, cronExpr, tz, mode, sessionRef, disabled string
	var jitter time.Duration
	var repeat int
	fs.StringVar(&name, "name", "", "job name")
	fs.StringVar(&prompt, "prompt", "", "prompt the agent should run")
	fs.StringVar(&every, "every", "", "interval, e.g. 5m, 1h")
	fs.StringVar(&at, "at", "", "RFC3339 time, e.g. 2026-05-01T09:00:00Z")
	fs.StringVar(&cronExpr, "cron", "", "cron expression, e.g. \"0 9 * * *\" or @daily")
	fs.StringVar(&tz, "tz", "", "IANA timezone for cron / at (default local)")
	fs.DurationVar(&jitter, "jitter", 0, "random offset added to NextRun (e.g. 30s) — spreads :00-aligned jobs")
	fs.IntVar(&repeat, "repeat", 0, "stop after N firings (0 = infinite)")
	fs.StringVar(&mode, "mode", "isolated", "session mode: isolated | persistent | main")
	fs.StringVar(&sessionRef, "session", "", "session ref for mode=main (default \"main\")")
	fs.StringVar(&disabled, "disable-tools", "", "comma-separated tools to deny while this job runs (e.g. WebFetch,Agent)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if prompt == "" {
		return errors.New("cron add: --prompt required")
	}

	job := &agent.CronJob{
		Name:        name,
		Prompt:      prompt,
		Enabled:     true,
		Repeat:      repeat,
		SessionMode: mode,
		SessionRef:  sessionRef,
	}
	if disabled != "" {
		for _, t := range strings.Split(disabled, ",") {
			if t = strings.TrimSpace(t); t != "" {
				job.DisabledTools = append(job.DisabledTools, t)
			}
		}
	}
	switch {
	case every != "":
		d, err := time.ParseDuration(every)
		if err != nil {
			return fmt.Errorf("invalid --every: %w", err)
		}
		job.Schedule = agent.CronSchedule{
			Kind:     "every",
			EveryMs:  d.Milliseconds(),
			JitterMs: jitter.Milliseconds(),
			TZ:       tz,
		}
	case at != "":
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			return fmt.Errorf("invalid --at: %w", err)
		}
		job.Schedule = agent.CronSchedule{Kind: "at", At: at, JitterMs: jitter.Milliseconds(), TZ: tz}
	case cronExpr != "":
		job.Schedule = agent.CronSchedule{
			Kind:     "cron",
			CronExpr: cronExpr,
			JitterMs: jitter.Milliseconds(),
			TZ:       tz,
		}
	default:
		return errors.New("cron add: need --every, --at, or --cron")
	}
	if err := svc.Create(job); err != nil {
		return err
	}
	fmt.Printf("created cron job: %s (next: %s)\n", job.ID, job.NextRun.Format(time.RFC3339))
	return nil
}

func cmdCronRun(ctx context.Context, svc *agent.CronService, args []string) error {
	if len(args) == 0 {
		return errors.New("cron run: missing id")
	}
	job, ok := svc.Get(args[0])
	if !ok {
		return fmt.Errorf("cron job not found: %s", args[0])
	}
	rt, err := setupRuntime(ctx, &cliFlags{})
	if err != nil {
		return err
	}
	defer rt.Cleanup()
	// Manual `metis cron run <id>` is one-shot — there's no daemon
	// holding cross-fire history, so we hand it empty maps and the
	// per-mode logic falls through to "isolated"-equivalent for any
	// SessionMode value (no prior history to load).
	return executeCronJob(ctx, rt, job,
		map[string][]llm.Message{}, map[string][]llm.Message{})
}

func cmdCronStart(ctx context.Context, svc *agent.CronService) error {
	rt, err := setupRuntime(ctx, &cliFlags{})
	if err != nil {
		return err
	}
	defer rt.Cleanup()
	// Per-mode in-memory history stores. Cron jobs with SessionMode set
	// to "persistent" or "main" need their conversation to survive
	// across firings of the same job, so we cache message slices keyed
	// by job ID (persistent) or shared session name (main).
	//
	// Note: these are PROCESS-LIFETIME — restarting `metis cron start`
	// resets all histories. Persisting to disk would mean wiring through
	// session.Store; left as TODO since cron daemons typically run
	// continuously.
	persistentHist := map[string][]llm.Message{}
	mainHist := map[string][]llm.Message{}
	onFire := func(j *agent.CronJob) error {
		fmt.Fprintf(os.Stderr, "[cron] firing %s (%s, mode=%s)\n", j.ID, j.Name, sessionModeOrDefault(j))
		return executeCronJob(ctx, rt, j, persistentHist, mainHist)
	}
	svc.Start(ctx, onFire)
	fmt.Fprintln(os.Stderr, "metis cron scheduler running (Ctrl-C to stop)")
	<-ctx.Done()
	svc.Stop()
	return nil
}

// sessionModeOrDefault collapses an empty SessionMode (legacy jobs +
// `metis cron add` without --mode) to "isolated" so log output reads
// consistently regardless of how the job was created.
func sessionModeOrDefault(j *agent.CronJob) string {
	if j.SessionMode == "" {
		return agent.SessionModeIsolated
	}
	return j.SessionMode
}

func executeCronJob(ctx context.Context, rt *runtime, job *agent.CronJob,
	persistentHist, mainHist map[string][]llm.Message) error {

	// 1. Pick the right starting history per SessionMode.
	switch sessionModeOrDefault(job) {
	case agent.SessionModePersistent:
		rt.loop.Restore(persistentHist[job.ID])
	case agent.SessionModeMain:
		key := job.SessionRef
		if key == "" {
			key = "main"
		}
		rt.loop.Restore(mainHist[key])
	default: // isolated
		rt.loop.Reset()
	}
	rt.loop.AppendUser(job.Prompt)

	// 2. Apply per-job tool blacklist (Hermes pattern). Each name in
	//    DisabledTools gets a temporary deny rule appended to the gate;
	//    we capture the rule count so we can pop them after the fire.
	denyCount := 0
	for _, t := range job.DisabledTools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		rt.gate.AppendRules(permission.Rule{
			Tool:   t,
			Verb:   permission.DecisionDeny,
			Source: "cron-job:" + job.ID,
		})
		denyCount++
	}
	defer func() {
		// Best-effort restore: drop the trailing N rules we just
		// appended. If something else touched the rule stack mid-fire
		// we accept the residue (rare, and a bypass mode safety net).
		if denyCount > 0 {
			rt.gate.PopRules(denyCount)
		}
	}()

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() { done <- rt.loop.Run(ctx, events); close(events) }()

	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			fmt.Print(ev.TextDelta)
		case agent.EventToolStart:
			fmt.Fprintf(os.Stderr, "\n[cron %s] [tool] %s\n", job.ID, ev.ToolName)
		case agent.EventInfo:
			if ev.Info != "" {
				fmt.Fprintf(os.Stderr, "[cron %s] [info] %s\n", job.ID, ev.Info)
			}
		case agent.EventPermissionRequest:
			ev.PermissionReply <- agent.PermissionDecisionDeny
		case agent.EventLoopDone:
			fmt.Println()
		case agent.EventError:
			return ev.Err
		}
	}
	runErr := <-done

	// 3. Save history back to the right cache for next fire. Skip on
	//    error so a partial run doesn't poison the persistent thread —
	//    the next fire restarts from where we last had a clean turn.
	if runErr == nil {
		switch sessionModeOrDefault(job) {
		case agent.SessionModePersistent:
			persistentHist[job.ID] = rt.loop.History()
		case agent.SessionModeMain:
			key := job.SessionRef
			if key == "" {
				key = "main"
			}
			mainHist[key] = rt.loop.History()
		}
	}
	return runErr
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "show":
		cfg, loaded, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Println("# files read:")
		for _, p := range loaded {
			fmt.Println("#  ", tildify(p))
		}
		fmt.Println()
		fmt.Printf("provider.default = %q\n", cfg.Provider.Default)
		fmt.Printf("anthropic.model = %q\n", cfg.Provider.Anthropic.Model)
		fmt.Printf("anthropic.base_url = %q\n", cfg.Provider.Anthropic.BaseURL)
		fmt.Printf("permission.mode = %q\n", cfg.Permission.Mode)
		fmt.Printf("session.dir = %q\n", tildify(cfg.Session.Dir))
		fmt.Printf("session.skill_dir = %q\n", tildify(cfg.Session.SkillDir))
		fmt.Printf("ui.markdown = %v\n", cfg.UI.Markdown)
		fmt.Printf("loop_detection.enabled = %v\n", cfg.LoopDetection.Enabled)
		fmt.Printf("loop_detection.global = %d\n", cfg.LoopDetection.Global)
		return nil
	case "init":
		return writeStarterConfig()
	}
	return fmt.Errorf("config: unknown subcommand %q", args[0])
}

func cmdTools() error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	gate := permission.New("ask")
	reg := tools.NewRegistry()
	builtin.Register(reg, cfg, gate)
	// Show the Agent tool for completeness even though Execute will error
	// without a real provider — this is just an informational listing.
	reg.Register(builtin.NewAgent(gate, nil, reg, "", ""))
	// Same for SendMessage — its real wiring lives in setupRuntime, but
	// we want it visible in `metis tools` so users know the capability
	// exists without firing up a chat session.
	reg.Register(builtin.NewSendMessage(gate, channels.NewRegistry(), cfg.Channels.DefaultPlatform))
	// ScheduleWakeup also needs a CronService reference at chat time;
	// in this informational listing we point it at a stub so the tool
	// shows up in /tools output. Calling Execute would error, but the
	// listing should advertise the capability.
	if cronSvc, err := agent.NewCronService(filepath.Join(cfg.Session.Dir, "cron")); err == nil {
		reg.Register(builtin.NewScheduleWakeup(gate, cronSvc))
	}
	// Memory tool — same pattern, point at the real on-disk store so
	// `/tools` listing reflects it.
	reg.Register(builtin.NewMemory(gate, rtpkg.BuildMemoryManager(cfg)))
	for _, t := range reg.All() {
		fmt.Printf("%-15s  %s\n", t.Name(), t.Description())
	}
	return nil
}

func cmdSessions(args []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	store, err := session.NewStore(cfg.Session.Dir)
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		es, err := store.List(20)
		if err != nil {
			return err
		}
		if len(es) == 0 {
			fmt.Println("(no sessions)")
			return nil
		}
		for _, e := range es {
			if e.Title != "" {
				fmt.Printf("%s  %s  model=%s  title=%q\n", e.ID, e.CreatedAt.Format(time.RFC3339), e.Model, e.Title)
			} else {
				fmt.Printf("%s  %s  model=%s\n", e.ID, e.CreatedAt.Format(time.RFC3339), e.Model)
			}
		}
		return nil
	case "export":
		if len(args) < 2 {
			return errors.New("usage: metis sessions export <id>")
		}
		return store.Export(args[1], os.Stdout)
	case "import":
		preferred := ""
		for i := 1; i < len(args); i++ {
			if args[i] == "--id" && i+1 < len(args) {
				preferred = args[i+1]
				i++
			}
		}
		newID, err := store.Import(os.Stdin, preferred)
		if err != nil {
			return err
		}
		fmt.Println(newID)
		return nil
	}
	return fmt.Errorf("sessions: unknown subcommand %q", sub)
}

func cmdSkills(args []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		return listBuiltInSkills()
	case "install":
		if len(args) < 2 {
			return fmt.Errorf("usage: metis skills install <name> | <owner>/<repo>:<name>")
		}
		return installSkillUnified(args[1], cfg.Session.SkillDir)
	case "info":
		if len(args) < 2 {
			return fmt.Errorf("usage: metis skills info <name>")
		}
		return showSkillInfo(args[1], cfg.Session.SkillDir)
	case "uninstall":
		if len(args) < 2 {
			return fmt.Errorf("usage: metis skills uninstall <name>")
		}
		store := skills.NewStore(cfg.Session.SkillDir)
		if err := store.Delete(args[1]); err != nil {
			return err
		}
		fmt.Println("uninstalled:", args[1])
		return nil
	}
	return fmt.Errorf("skills: unknown subcommand %q (use: list | install | info | uninstall)", sub)
}

// installSkillUnified picks between local-bundled install and GitHub fetch
// based on the ref shape: "<owner>/<repo>:<name>" goes to GitHub, anything
// else falls back to the legacy installSkill (copies from build-time skills/).
func installSkillUnified(ref, skillDir string) error {
	if strings.Contains(ref, "/") && strings.Contains(ref, ":") {
		store := skills.NewStore(skillDir)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sk, err := store.Install(ctx, skills.NewGitHubSource(), ref)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s from %s\n", sk.Name, sk.Source)
		return nil
	}
	return installSkill(ref, skillDir)
}

func showSkillInfo(name, skillDir string) error {
	store := skills.NewStore(skillDir)
	sk, err := store.Get(name)
	if err != nil {
		return err
	}
	fmt.Printf("name:        %s\n", sk.Name)
	fmt.Printf("description: %s\n", sk.Description)
	if sk.Category != "" {
		fmt.Printf("category:    %s\n", sk.Category)
	}
	if len(sk.Tags) > 0 {
		fmt.Printf("tags:        %s\n", strings.Join(sk.Tags, ", "))
	}
	if sk.Source != "" {
		fmt.Printf("source:      %s\n", sk.Source)
	}
	if sk.ContentHash != "" {
		fmt.Printf("hash:        %s\n", sk.ContentHash)
	}
	fmt.Printf("uses:        %d\n", sk.Uses)
	return nil
}

func listBuiltInSkills() error {
	// Read every layer of the multi-source loader (bundled SKILL.md +
	// user dir + project dir) so `metis skills list` shows everything
	// the agent can actually invoke. Previously this read only the old
	// JSON files at the repo-root /skills/ — once the format moved to
	// SKILL.md under internal/agent/skills/builtin/ the listing
	// returned empty.
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	loader := skills.NewLoader(cfg.Session.SkillDir, "", nil)
	all, err := loader.List()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("(no skills found)")
		return nil
	}
	for _, s := range all {
		fmt.Printf("- %s: %s\n", s.Name, s.Description)
		if len(s.Tags) > 0 {
			fmt.Printf("  tags: %s\n", strings.Join(s.Tags, ", "))
		}
	}
	return nil
}

func installSkill(name, destDir string) error {
	// Find the skill in built-in library
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find executable: %w", err)
	}
	skillDir := filepath.Join(filepath.Dir(exe), "..", "share", "metis", "skills")
	if _, err := os.Stat(skillDir); err != nil {
		skillDir = filepath.Join(".", "skills")
	}
	srcPath := filepath.Join(skillDir, name+".json")
	b, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("skill %q not found in built-in library", name)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	destPath := filepath.Join(destDir, name+".json")
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("skill %q already installed (override by deleting %s)", name, destPath)
	}
	if err := os.WriteFile(destPath, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("installed %q to %s\n", name, destPath)
	return nil
}

// --- helpers ---

func buildSlash(rt *runtime) *slash.Registry {
	r := slash.NewRegistry()
	// Use the canonical command set defined in internal/slash/commands.go.
	// Previously buildSlash hand-registered a 7-cmd stub which was a long-
	// lived bug: 47 of the 54 commands the TUI submit handler dispatches
	// off of (cost / doctor / save / btw / batch / diff / stats / vim
	// / keybindings / permissions / hooks / cron / loop / agents / …)
	// were silently invisible to the slash registry, making them
	// unreachable through the slash path and producing the
	// "unknown: /save — try /help" surface bug seen in VNC v3-08.
	slash.RegisterAll(r, rt.cfg)

	// /mode — unique to the CLI build, lets the user inspect or set the
	// permission mode by string. Not in RegisterAll because the signal
	// path already exposes individual /auto, /bypass, /plan, /ask
	// togglers; this is the catch-all "show me / set me" form.
	r.Register(slash.Cmd{Name: "mode", Description: "show or set permission mode (ask|auto|bypass|plan|deny)", Handler: func(arg string) (string, slash.Signal) {
		if arg == "" {
			return "mode: " + string(rt.gate.Mode()), slash.SignalNone
		}
		rt.gate.SetMode(permission.Mode(arg))
		return "mode set to " + arg, slash.SignalNone
	}})
	return r
}

func writeStarterConfig() error {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = home + "/.config"
	}
	dir := xdg + "/metis"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := dir + "/config.toml"
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; not overwriting", path)
	}
	starter := `# metis config — full reference in docs/CONFIG.md

[provider]
default = "anthropic"

[provider.anthropic]
api_key_env = "ANTHROPIC_API_KEY"
model = "claude-opus-4-7"
max_tokens = 8192
timeout_seconds = 120
temperature = 1.0

[permission]
# ask | auto | bypass | plan | deny
mode = "ask"

# Always allow these without prompting:
[[permission.allow]]
tool = "Read"
[[permission.allow]]
tool = "LS"
[[permission.allow]]
tool = "Glob"
[[permission.allow]]
tool = "Grep"

[ui]
theme = "auto"
markdown = true
show_tokens = true

[tools.bash]
timeout_seconds = 120
max_output_bytes = 1048576
`
	if err := os.WriteFile(path, []byte(starter), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func joinSpaces(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func truncStderr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// tildify shortens paths under the user's home dir to "~/..." form so
// `metis config show` reads more naturally — `/Users/ricardo/.metis/x`
// becomes `~/.metis/x`. Falls back to the absolute path if HOME isn't
// resolvable or if `p` doesn't sit inside HOME.
func tildify(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}

// wizardAdapter bridges tui.RunAuthWizard's return type into the
// runtime.WizardFn shape (which is UI-agnostic). main.go owns the
// binding so internal/runtime never needs to import internal/tui.
func wizardAdapter() (*rtpkg.WizardResult, error) {
	res, err := tui.RunAuthWizard()
	if err != nil {
		if errors.Is(err, tui.ErrAuthCancelled) {
			return nil, rtpkg.ErrWizardCancelled
		}
		return nil, err
	}
	return &rtpkg.WizardResult{Provider: res.Provider, Key: res.Key}, nil
}
