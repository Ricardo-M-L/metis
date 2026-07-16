package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/Ricardo-M-L/metis/acp"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/exitcode"
	"github.com/Ricardo-M-L/metis/internal/helpdocs"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/notify"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	taskstore "github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/telemetry"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
	"github.com/Ricardo-M-L/metis/internal/tui"
	"github.com/Ricardo-M-L/metis/internal/version"
	worktreepkg "github.com/Ricardo-M-L/metis/internal/worktree"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

// defaultSystem is the embedded base system prompt. The actual text
// lives in internal/runtime/prompts/base.md so it diffs as plain
// markdown and can be edited without recompiling string literals.
// This package-level var is kept for backward-compat with the
// pre-embed call sites; new code should call rtpkg.DefaultBasePrompt()
// directly.
var defaultSystem = rtpkg.DefaultBasePrompt()

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Wait for in-flight LLM transport dump-prompts goroutines before
	// process exit so the JSONL file isn't missing the response side
	// of fast `metis run` invocations. METIS_DUMP_PROMPTS off → no-op
	// (no goroutines to wait on). 2026-05-09 fix.
	defer transport.FlushDumps()
	if err := dispatch(ctx, os.Args[1:]); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "metis:", err)
		}
		// FlushDumps must run before os.Exit (deferred funcs are
		// skipped on os.Exit). Call directly here too.
		transport.FlushDumps()
		os.Exit(exitcode.Classify(err))
	}
}

func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return cmdChat(ctx, args)
	}
	// Subcommand-can-be-anywhere: 2026-05-18 fix for the
	// `metis -p X -m Y run --mode ask 'prompt'` failure mode. Users
	// (and Anthropic's docs) sometimes put global flags before the
	// subcommand verb. Before this hoist, the switch below saw
	// args[0]="-p" → fell through to `default:` → cmdRun(args) →
	// parseFlags() consumed -p/-m, then stopped at "run" (a
	// non-flag), so `run --mode ask 'prompt'` got joined back into
	// the model's prompt and the `--mode` override was silently
	// ignored (gate stayed in config-default mode). Now we sniff for
	// a known verb anywhere in args[0..2] and hoist it forward so the
	// switch dispatches correctly.
	if idx, found := findEarlySubcommand(args, 16); found {
		hoisted := make([]string, 0, len(args))
		hoisted = append(hoisted, args[idx])
		hoisted = append(hoisted, args[:idx]...)
		hoisted = append(hoisted, args[idx+1:]...)
		args = hoisted
	}
	switch args[0] {
	case "chat":
		return cmdChat(ctx, args[1:])
	case "run":
		return cmdRun(ctx, args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "dirs":
		return cmdDirs(args[1:])
	case "projects":
		return cmdProjects(args[1:])
	case "tools":
		return cmdTools(args[1:])
	case "schema":
		return cmdSchema(args[1:])
	case "models":
		return cmdModels(ctx, args[1:])
	case "sessions":
		return cmdSessions(args[1:])
	case "stats":
		return cmdStats(ctx, args[1:])
	case "skills":
		return cmdSkills(args[1:])
	case "desktop":
		return cmdDesktop(ctx, args[1:])
	case "acp":
		return cmdACP(ctx, args[1:])
	case "mcp-serve":
		return cmdMCPServe(ctx, args[1:])
	case "daemon":
		return cmdDaemon(ctx, args[1:])
	case "ps":
		return cmdPs(ctx, args[1:])
	case "logs":
		return cmdLogs(ctx, args[1:])
	case "kill":
		return cmdKill(ctx, args[1:])
	case "attach":
		return cmdAttach(ctx, args[1:])
	case "coordinator":
		return cmdCoordinator(ctx, args[1:])
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
	case "eval":
		return cmdEval(ctx, args[1:])
	case "update":
		return cmdUpdate(ctx, args[1:])
	case "version", "-v", "--version":
		return cmdVersion(args[1:])
	case "env":
		// 2026-05-22: print the curated env-var reference
		// (internal/helpdocs/env.md, embedded). Surfaces what
		// otherwise lived only as `os.Getenv("METIS_...")` calls
		// scattered across the code — users were missing knobs.
		fmt.Print(helpdocs.Env())
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		// No subcommand. The default fallback is to treat args as an
		// inline prompt for `run` (so `metis "explain X"` works as a
		// one-shot). But the claude-code "open chat with intent"
		// gestures — bare `-r` / `--resume` / `-c` / `--continue`,
		// optionally followed by a session id — must route to chat
		// instead, otherwise a user typing `metis -r` to open the
		// session picker hits "run: prompt is required" because
		// cmdRun's parseFlags strips the flags and finds no prompt.
		// This was the 2026-05-08 user video bug.
		if hasInteractiveIntentFlag(args) {
			return cmdChat(ctx, args)
		}
		return cmdRun(ctx, args)
	}
}

// findEarlySubcommand scans the first `lookahead` args for a known
// metis verb (chat / run / config / etc.) preceded only by global
// flags + their values. Returns its index if found so the dispatcher
// can hoist it to args[0]. Limited window so a stray "run" inside a
// long inline prompt doesn't get hoisted by accident.
//
// The set must stay in sync with the switch in dispatch(); keep it
// small (only the verbs users actually combine with leading globals).
func findEarlySubcommand(args []string, lookahead int) (int, bool) {
	verbs := map[string]bool{
		"chat": true, "run": true, "config": true, "tools": true,
		"models": true, "sessions": true, "stats": true, "skills": true,
		"acp": true, "daemon": true, "ps": true, "logs": true,
		"kill": true, "attach": true, "coordinator": true, "cron": true,
		"auth": true, "plugin": true, "plugins": true, "audit": true,
		"desktop": true, "diag": true, "eval": true, "update": true, "version": true,
		"dirs": true, "projects": true, "help": true,
	}
	if lookahead > len(args) {
		lookahead = len(args)
	}
	// args[0] is already handled by the switch; we look at args[1..lookahead).
	for i := 1; i < lookahead; i++ {
		if verbs[args[i]] {
			// Sanity: prior tokens must look like flags or flag values
			// (no positional arg that would be a prompt fragment).
			ok := true
			for j := 0; j < i; j++ {
				if !looksLikeFlagOrValue(args, j) {
					ok = false
					break
				}
			}
			if ok {
				return i, true
			}
		}
	}
	return 0, false
}

// looksLikeFlagOrValue reports whether args[j] is plausibly a flag
// or the value of a flag (i.e., not the start of a prompt fragment).
// "-p" / "--mode" / "minimax" (when preceded by "-p") qualify; bare
// strings like "explain this code" do not.
func looksLikeFlagOrValue(args []string, j int) bool {
	tok := args[j]
	if strings.HasPrefix(tok, "-") {
		return true
	}
	// Value of a flag — previous token must be a flag.
	if j == 0 {
		return false
	}
	prev := args[j-1]
	return strings.HasPrefix(prev, "-")
}

// hasInteractiveIntentFlag reports whether args contain any of the
// flags that mean "open the chat TUI with this state primed":
// -r / --resume (with or without a value), -c / --continue. A user
// typing one of these without an explicit `chat` subcommand wants
// chat, not run.
//
// We keep this narrow on purpose. A general "all flags, no prompt =
// chat" heuristic would mis-route `metis -m gpt-4 "what is 2+2"` —
// scanning for flag-vs-value boundaries to disambiguate is fragile.
// The specific pain point the user hit is the resume gesture, so we
// only special-case that family.
func hasInteractiveIntentFlag(args []string) bool {
	for _, a := range args {
		switch a {
		case "-r", "--resume", "-c", "--continue":
			return true
		}
		// `--resume=xyz` / `--continue=true` forms (uncommon but legal
		// for Go's flag package) — match by prefix.
		if strings.HasPrefix(a, "--resume=") || strings.HasPrefix(a, "--continue=") {
			return true
		}
	}
	return false
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
  metis schema          Print the tool contract (JSON Schema) — the SDK's typed surface
  metis models          List LLM providers + models from models.dev catalog
  metis models <p>      Show one provider's models + capabilities + cost
  metis models <p> <m>  Deep-dive on a model + generate config.toml snippet
  metis sessions list   List recent saved sessions
  metis sessions timing <id>      Per-step timing breakdown of a past session
  metis sessions export <id>      Print a session's JSONL to stdout
  metis sessions import [--id ID] Read JSONL from stdin and create a new session
  metis skills list     List built-in skills library
  metis skills install <name>  Install a built-in skill
  metis skills curator <status|run|list-archived|restore|pin|unpin>  Manage agent-created skills
  metis desktop [--web] [--port PORT]  Launch native desktop (or legacy browser UI)
  metis acp [--addr ADDR]  Run as Agent Client Protocol server (default: stdio)
  metis mcp-serve [--mode MODE]  Run as MCP server (stdio); register with: claude mcp add metis -- metis mcp-serve
  metis cron <list|add|rm|pause|resume|run|start|audit>  Manage scheduled prompts
  metis auth <login|logout|list>  Manage provider credentials (~/.metis/auth.json)
  metis audit           Print a security audit of the current configuration
  metis diag [--llm] [--tool-smoke] [--json]  Run a non-interactive health check
  metis eval [--dir DIR] [--tag T] [--out P]  Run the markdown scenario pack
  metis update [--check]  Self-update from the private release (needs METIS_GITHUB_TOKEN)
  metis version [-V]    Print version (-V for build fingerprint)
  metis env             Print the full METIS_* env-var reference
  metis help            This help

Flags (chat / run):
  -m, --model <id>      Override model
  -p, --provider <id>   Override provider (anthropic | openai | gemini | <custom>)
      --mode <id>       Permission mode: ask | auto | bypass | plan | deny
      --no-markdown     Disable markdown rendering of assistant output
      --no-stream       Don't stream (assemble then print)
      --max-iter <n>    Iteration cap per turn (default 150; overrides [session] max_iterations in config.toml)
      --max-budget-usd <x>  Stop the session once cumulative LLM spend reaches x USD (0 = unlimited; sub-agents draw from the same pool)
      --output-schema <file>  (run) Constrain the final reply to a JSON Schema — validated locally, 2 correction retries, then exit 11
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
	cfg               *config.Config
	provider          llm.Provider
	registry          *tools.Registry
	gate              *permission.Gate
	store             *session.Store
	sessionID         string
	sessionPointerCwd string // cwd used to key the crash-recovery pointer; empty if write failed at boot
	loop              *agent.Loop
	// cronSvc is the per-session cron service shared with the CronCreate/
	// List/Delete + ScheduleWakeup tools. The TUI mounts an in-session
	// scheduler on this same instance so session-only (ephemeral) jobs the
	// model schedules mid-chat actually fire. nil in headless paths.
	cronSvc               *agent.CronService
	useMD                 bool
	showTok               bool
	model                 string
	providerName          string          // resolved provider profile name (cfg.Provider.Default OR --provider). Threaded to the TUI so mid-session /model switches know which profile to rebuild against.
	defaultPermissionMode permission.Mode // invocation-resolved baseline for in-process /new and /branch
	// mcpServers collects handles to live MCP server subprocesses for
	// Cleanup. Written from the background-launch goroutine kicked off
	// in setupRuntime when phase-2 async MCP is enabled, hence the
	// mutex. The slice is also read by /mcp prompts (CollectMCPPrompts).
	mcpServers   []*mcptools.Server
	mcpServersMu sync.Mutex
	// mcpLauncherDone closes when the background MCP launcher finishes
	// (or right away when --bare). Cleanup waits on it so we never tear
	// down the parent process while a handshake goroutine is still
	// mid-spawn — the resulting "broken pipe / killed" warnings to
	// stderr are noisy and obscure the real shutdown cause.
	mcpLauncherDone <-chan struct{}
	plugins         *rtpkg.PluginRegistry // nil when no plugins installed
	allowedDirs     *rtpkg.AllowedDirs    // --add-dir state, persisted to ~/.metis/additional-dirs.json
	// autoMemExtractor is the live G.5 DreamTask handle. Surfaced
	// to the slash registry so /dream status can read phase + last-
	// run stats. nil when --auto-memory isn't set.
	autoMemExtractor *agent.AutoMemoryExtractor

	// subAgentRoster is the cross-session Roster every spawned
	// sub-agent registers in (G.0/G.3). Stashed on the runtime so
	// the /agents slash command can render the live state instead
	// of the placeholder hint (G.17, 2026-05-12).
	subAgentRoster *agent.Roster
}

// WaitForMCP blocks until the background MCP launcher finishes spawning
// + handshaking with every configured server, or until `timeout` elapses
// — whichever comes first. Safe to call from cmdRun before the first
// LLM round-trip so tool registration is complete when the model sees
// the catalog.
//
// Pre-2026-05-22 `metis run` raced the launcher: the LLM frequently
// got an empty `mcp__*` table because the goroutine in setupRuntime
// hadn't connected yet (remote-server cu test, 2026-05-22). cmdRun
// now calls WaitForMCP before AppendUser; TUI / chat paths skip the
// wait because they're long-running and the launcher settles on its
// own before the first user keystroke.
//
// Returns true if the launcher finished within `timeout`, false on
// timeout. Caller decides whether to surface the failure (run mode
// prints a warning to stderr but continues so simple no-MCP-needed
// prompts still complete).
func (r *runtime) WaitForMCP(timeout time.Duration) bool {
	if r.mcpLauncherDone == nil {
		return true
	}
	select {
	case <-r.mcpLauncherDone:
		return true
	case <-time.After(timeout):
		return false
	}
}

// rebindSession updates every process-owned router used by a long-lived TUI
// after /resume, /branch or /new. Keeping this in the composition layer avoids
// making internal/tui depend on transport/task/checkpoint implementations.
func (r *runtime) rebindSession(sessionID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.sessionID = sessionID
	rtpkg.SetCurrentSessionID(sessionID)
	transport.SetSessionID(sessionID)
	taskstore.SetCurrentTaskStore(sessionID)
	if r.loop != nil {
		if cwd, err := os.Getwd(); err == nil {
			r.loop.SetCheckpointer(checkpoint.NewManager(sessionID, cwd, ""))
		}
	}
	if r.sessionPointerCwd != "" {
		_ = session.WritePointer(sessionID, r.sessionPointerCwd)
	}
}

// releaseSessionWork stops process-owned work that must not cross a top-level
// /new, /branch or /resume boundary. Each component is optional because tests,
// embedders and reduced runtimes do not necessarily wire all three.
func (r *runtime) releaseSessionWork() {
	if r == nil {
		return
	}
	if r.cronSvc != nil {
		r.cronSvc.ClearEphemeral()
	}
	if r.subAgentRoster != nil {
		r.subAgentRoster.Reset()
	}
	if r.loop == nil {
		return
	}
	if r.loop.Monitors != nil {
		r.loop.Monitors.StopAll()
	}
	if r.loop.Jobs != nil {
		r.loop.Jobs.Reset(0)
	}
}

// Cleanup closes any subprocesses or connections owned by the runtime.
// Safe to call multiple times.
func (r *runtime) Cleanup() {
	// Stop session-owned work before closing the provider/MCP dependencies it
	// may still be using. This is idempotent, so it is safe after an in-process
	// session boundary already released the prior session's workers.
	r.releaseSessionWork()

	// Wait briefly for the background MCP launcher so we close handles
	// that DID come online. After the grace cap we move on — orphan
	// handshake goroutines get reaped on process exit anyway, and a
	// non-interactive `metis run` shouldn't pay a 10 s tail just to
	// wait for an MCP server we never used.
	//
	// Grace cap: 1 s. The MCP handshake itself has its own 10 s timeout
	// (MCP_CONNECT_TIMEOUT, defaultConnectTimeout in internal/mcp), so
	// in practice goroutines have already unwound by the time we get
	// here on a normal exit; the 1 s mostly covers a launch that's
	// still in the npm-resolve phase.
	if r.mcpLauncherDone != nil {
		select {
		case <-r.mcpLauncherDone:
		case <-time.After(1 * time.Second):
		}
		r.mcpLauncherDone = nil
	}
	r.mcpServersMu.Lock()
	for _, s := range r.mcpServers {
		_ = s.Close()
	}
	r.mcpServers = nil
	r.mcpServersMu.Unlock()
	if r.plugins != nil {
		_ = r.plugins.Close()
		r.plugins = nil
	}
	// Clear the crash-recovery pointer on clean shutdown — its
	// continued presence would re-prompt "found a recent session" on
	// next startup. Only crashes / kill -9 leave the pointer behind.
	if r.sessionPointerCwd != "" {
		_ = session.ClearPointer(r.sessionPointerCwd)
		r.sessionPointerCwd = ""
	}
}

type cliFlags struct {
	model        string
	provider     string
	modelSet     bool // true when --model/-m was present, even with an empty value
	providerSet  bool // true when --provider/-p was present, even with an empty value
	mode         string
	noMarkdown   bool
	noStream     bool
	streamlined  bool // --streamlined: distillation-resistant output (thinking stripped, tools summarized)
	maxIter      int
	maxBudgetUSD float64 // --max-budget-usd: session USD spend cap (0 = unlimited)
	system       string
	systemSet    bool // true when --system was present, even with an empty value
	resumeID     string
	newSessionID string // internal/native-client hook: choose ID for a fresh run
	cont         bool   // -c / --continue: pick up the most recently modified session
	useTUI       bool
	noAuthWizard bool   // skip the first-run wizard (CI / scripted use)
	effort       string // "low" | "medium" | "high" — Anthropic thinking budget / OpenAI reasoning_effort
	fast         bool   // collapse one turn: effort=low + halved max_tokens
	addDirs      stringList
	agentProfile string // --agent=NAME — load .metis/agents/<name>.md
	worktree     string // --worktree=slug — spin up git worktree with explicit slug
	worktreeOn   bool   // -W — spin up worktree with auto slug

	// --debug / -d: write a verbose trace to ~/.metis/debug.log alongside
	// METIS_DEBUG=1's stderr output. Doesn't change visible behavior; just
	// captures more for post-mortem.
	debug bool

	// --bare: skip side-effectful loaders (MCP / plugins / skills / hook
	// subprocesses). Designed for fastest cold start when the user wants
	// the chat surface but knows they don't need any of those today —
	// dropping them shaves ~600ms off boot on a populated config.
	bare bool

	// --dangerously-skip-permissions: alias of `--mode bypass`. Named to
	// match Claude Code so an existing user's muscle memory works. Wins
	// over --mode if both are set (the explicit --mode loses to the
	// "yes I really mean it" wrapper).
	dangerouslySkipPerms bool

	// --scope / -s: where mcp / skill / auth subcommands write. Three
	// values (parity with Claude Code: local | user | project). Today
	// metis only has one scope (`user` == ~/.metis/), so the flag is
	// parsed for forward-compat but only `user` is honored — anything
	// else logs a warning and falls back to user. Will become real in
	// the Phase E daemon work where per-scope mcp.toml lookup matters.
	scope string

	// --input-format / --output-format: I/O modes for `metis run`.
	//   * input-format=json: read JSONL from stdin, one prompt per line
	//   * output-format=json | stream-json: emit structured events instead
	//     of the human-readable transcript (json = single object at end;
	//     stream-json = NDJSON during the turn)
	// Mirrors Claude Code's `--output-format json|stream-json`.
	inputFormat  string
	outputFormat string

	// --output-schema <file>: constrain the FINAL reply of `metis run`
	// to a JSON Schema. Validated locally after the loop; invalid
	// output buys the model up to 2 correction turns, then exit 11.
	// Mirrors claude-code's headless jsonSchema QueryParam.
	outputSchema string

	// --auto-memory: turn on extractMemories v2 (Claude Code parity).
	// Off by default — opt-in keeps the per-turn LLM-call cost
	// predictable. Also accept METIS_AUTO_MEMORY=1 as env fallback so
	// users can persist via shell init without typing the flag.
	autoMemory bool

	// Phase E #46-#48 — small ergonomic flags. None of these change
	// runtime behaviour drastically; they're either UI hints (a session
	// name shown in /sessions) or sugar over existing surfaces (a /batch
	// shortcut, a tmux launcher).
	sessionName string // --name <text>: human-friendly session label
	agentTeams  bool   // --agent-teams: alias for /batch entry path
	tmuxOn      bool   // --tmux: when starting a worktree, wrap the session in tmux

	// coordinator — Phase G.8 (2026-05-12). Flips the main loop into
	// team-lead mode: tool palette narrows to orchestration tools and
	// the system prompt overlays an explicit "plan, dispatch,
	// synthesize" prompt. Equivalent to METIS_COORDINATOR_MODE=1.
	coordinator bool

	// --cache / --cache-ttl: enable the on-disk response cache for
	// `metis run` (CACHE-D, 2026-05-11). Hits skip the API entirely
	// when the same (model, provider, system, prompt) tuple was
	// previously answered without using any tools. Tool-use turns are
	// never cached (would lie about world state on replay).
	runCache    bool
	runCacheTTL string // duration string, e.g. "1h" / "24h" / "off"

	// pickResume is set when the user typed bare `-r` / `--resume` with
	// no UUID argument — claude-code parity: that gesture means "show
	// me the recent sessions and let me pick one". setupRuntime opens
	// the picker before chat starts and writes the chosen id back into
	// resumeID. Mutually exclusive with explicit `--resume <id>`.
	pickResume bool

	// --prompt-dump: render the assembled system prompt (with overlays,
	// project context, addendum, env, provider hint, all section flags
	// applied) and print to stdout, then exit. No LLM call is made.
	// Useful for measuring token cost, debugging prompt drift, and
	// verifying conditional sections fire in the right configuration.
	dumpPrompt bool

	// --simple / METIS_SIMPLE=1: replace the full base prompt + tool
	// description bundle with a one-sentence stub + cwd + date. Mirror
	// of claude-code's CLAUDE_CODE_SIMPLE. Ideal for short `metis run`
	// commands and CI scripts where the heavy guidance is wasted.
	simpleMode bool

	// --tools / --disallow-tools — session-scoped tool-pool override
	// (2026-05-14). Applied AFTER MCP + plugin tools register, so
	// dynamically-loaded MCP tools are also subject to the filter.
	// CLI allowlist OVERRIDES config Allowed (so `--tools Read` truly
	// gives only Read regardless of config). CLI disallowlist UNIONS
	// with config Disallowed (CLI cannot loosen config restrictions).
	// Patterns support MCP server-prefix matching — see
	// internal/tools.ExpandToolPatterns for the grammar.
	tools         string // --tools "Read,Edit,Bash" (allowlist, comma/space separated)
	disallowTools string // --disallow-tools "WebFetch,mcp__office-word"

	// --metrics-log <path>: append one JSONL line per turn to <path>.
	// Each line carries the turn number, per-turn token usage (split
	// in / out / cache_read / cache_create), tool call + error counts,
	// duration_ms, stop_reason, and how many nudges / rescues fired in
	// that turn. Designed for offline scrape — feeds the eval runner
	// and ad-hoc spend analysis without forcing the user to parse
	// stderr `[metrics]` lines or hook into the event stream.
	metricsLog string
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
	// Pre-scan: bare `-r` / `--resume` (no UUID) is claude-code's
	// "show the picker" gesture. Detect it BEFORE Go's flag parser
	// runs — otherwise StringVar treats the next token (a flag like
	// `-c` or end-of-args) as the value and we get a confusing
	// "missing value" error or the wrong id captured.
	args = liftBareResume(args, &out.pickResume)
	f.StringVar(&out.model, "model", "", "model id")
	f.StringVar(&out.model, "m", "", "model id (shorthand)")
	f.StringVar(&out.provider, "provider", "", "provider id")
	f.StringVar(&out.provider, "p", "", "provider id (shorthand)")
	f.StringVar(&out.mode, "mode", "", "permission mode")
	f.BoolVar(&out.noMarkdown, "no-markdown", false, "disable markdown rendering")
	f.BoolVar(&out.noStream, "no-stream", false, "disable streaming")
	f.BoolVar(&out.streamlined, "streamlined", false, "distillation-resistant output: drop thinking, collapse tool calls into cumulative summaries (per-call override of [ui] streamlined_output)")
	f.IntVar(&out.maxIter, "max-iter", 0, "max tool iterations per turn")
	f.Float64Var(&out.maxBudgetUSD, "max-budget-usd", 0, "stop the session once cumulative LLM spend reaches this many USD (0 = unlimited)")
	f.StringVar(&out.system, "system", "", "override system prompt")
	f.StringVar(&out.resumeID, "resume", "", "resume session id")
	f.StringVar(&out.newSessionID, "session-id", "", "use this id for a fresh session")
	// `-r` short alias for --resume. Claude Code accepts both; metis used
	// to error with `flag provided but not defined: -r` (user video bug
	// 2026-05-07 21:14). Mapping the short flag onto the same var means a
	// stale alias doesn't drift away from the long form.
	f.StringVar(&out.resumeID, "r", "", "resume session id (short for --resume)")
	// `-c` / --continue: pick the most recently modified session in the
	// store. Resolution happens in setupRuntime once we have a Store
	// handle; the flag here just records intent.
	f.BoolVar(&out.cont, "continue", false, "resume the most recently modified session")
	f.BoolVar(&out.cont, "c", false, "resume the most recently modified session (short for --continue)")
	f.BoolVar(&out.useTUI, "tui", false, "use the TUI (default when stdout is a TTY)")
	f.BoolVar(&out.noAuthWizard, "no-auth-wizard", false, "skip the first-run auth wizard if no API key is found")
	f.StringVar(&out.effort, "effort", "", "reasoning intensity: low | medium | high")
	f.BoolVar(&out.fast, "fast", false, "fast turn: effort=low + halved max_tokens")
	f.Var(&out.addDirs, "add-dir", "additional directory accessible to tools (repeatable)")
	f.StringVar(&out.agentProfile, "agent", "", "load agent profile from ~/.metis/agents/<name>.md")
	f.StringVar(&out.worktree, "worktree", "", "spawn a git worktree for this session (slug)")
	f.BoolVar(&out.worktreeOn, "W", false, "spawn a git worktree with an auto-generated slug")
	// Phase B parity flags. `--debug`/`-d` writes a verbose trace to
	// ~/.metis/debug.log on top of the existing METIS_DEBUG=1 stderr
	// path, so leaving the flag on doesn't pollute interactive output.
	f.BoolVar(&out.debug, "debug", false, "write verbose trace to ~/.metis/debug.log")
	f.BoolVar(&out.debug, "d", false, "write verbose trace to ~/.metis/debug.log (short for --debug)")
	// --bare: skip MCP / plugins / hooks / skills loading. Sometimes the
	// fastest cold start is the right answer (CI, throwaway smoke).
	f.BoolVar(&out.bare, "bare", false, "skip MCP and plugin loaders (fastest cold start)")
	// --dangerously-skip-permissions: friendly alias of `--mode bypass`.
	// Named to match Claude Code so muscle memory works.
	f.BoolVar(&out.dangerouslySkipPerms, "dangerously-skip-permissions", false,
		"equivalent to --mode bypass (no permission prompts; use with care)")
	// --scope / -s: forward-compat for the per-scope mcp.toml / skills/
	// layout that lands with the daemon work. Today only `user` is
	// honored; anything else logs a warning at startup. Plumbed here
	// (rather than scattered across mcp/skills/auth subcommands) so all
	// future scope-aware code reads from one cliFlags field.
	f.StringVar(&out.scope, "scope", "", "config scope: local | user | project (default user)")
	f.StringVar(&out.scope, "s", "", "config scope: local | user | project (short for --scope)")
	// --input-format / --output-format: machine-readable I/O for `metis
	// run`. Defaults stay human-readable for interactive runs.
	f.StringVar(&out.inputFormat, "input-format", "",
		"`metis run` input mode: json (NDJSON prompts on stdin)")
	f.StringVar(&out.outputFormat, "output-format", "",
		"`metis run` output mode: json | stream-json")
	f.StringVar(&out.outputSchema, "output-schema", "",
		"`metis run`: path to a JSON Schema the final reply must conform to (validated locally; invalid output retried up to 2x, then exit 11)")
	// Phase E #46-#48
	f.StringVar(&out.sessionName, "name", "",
		"human-friendly session label (shows in /sessions; persisted via SetTitle)")
	f.BoolVar(&out.coordinator, "coordinator", false,
		"team-lead mode: narrow tool palette to orchestration tools (Agent / Fork / SendMessage / SubAgent* / Read / Grep / ...) and overlay a coordinator system prompt. Equivalent to setting METIS_COORDINATOR_MODE=1.")
	f.BoolVar(&out.agentTeams, "agent-teams", false,
		"start in agent-teams mode (a /batch shortcut surface)")
	f.BoolVar(&out.tmuxOn, "tmux", false,
		"when starting in a worktree, also wrap the session in a tmux pane")
	// Auto-memory v2 — extractMemories on LoopEnd via forked agent.
	f.BoolVar(&out.autoMemory, "auto-memory", false,
		"enable auto-memory extraction on every turn boundary (writes to ~/.metis/memory/<topic>.md)")
	// CACHE-D: response cache for `metis run` (off by default).
	f.BoolVar(&out.runCache, "cache", false,
		"enable on-disk response cache for `metis run` (CI/cron use). Tool-use turns are never cached.")
	f.StringVar(&out.runCacheTTL, "cache-ttl", "",
		"response-cache TTL for `metis run` (e.g. 1h, 30m, 24h, or 'off'). Default 1h.")
	f.BoolVar(&out.dumpPrompt, "prompt-dump", false,
		"print the assembled system prompt to stdout and exit (no LLM call)")
	f.BoolVar(&out.simpleMode, "simple", false,
		"use a one-sentence system prompt (skip the heavy base prompt + redirects + skills + reversibility sections). Equivalent to METIS_SIMPLE=1.")
	// --tools / --disallow-tools: session-scoped tool-pool override.
	// Both accept comma- OR space-separated lists; disallow also accepts
	// MCP server prefix (e.g. "mcp__office-word" mutes that whole server).
	f.StringVar(&out.tools, "tools", "",
		"allowlist: only expose these tools to the model (e.g. \"Read,Edit,Bash\"). Empty = use config + all registered tools.")
	f.StringVar(&out.disallowTools, "disallow-tools", "",
		"blocklist: hide these tools from the model. Supports MCP server prefix: \"mcp__office-word\" mutes the whole server; \"mcp__\" mutes all MCP tools.")
	f.StringVar(&out.metricsLog, "metrics-log", "",
		"append per-turn JSONL metrics to <path> (turn_number, tokens.{in,out,cache_read,cache_create}, tool_calls, tool_errors, duration_ms, stop_reason, nudges_fired, rescue_fired)")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	f.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "model", "m":
			out.modelSet = true
		case "provider", "p":
			out.providerSet = true
		case "system":
			out.systemSet = true
		}
	})
	return out, f.Args(), nil
}

func setupRuntime(ctx context.Context, flags *cliFlags) (*runtime, error) {
	cfg, loaded, snap, err := config.LoadWithSnapshot()
	if err != nil {
		return nil, err
	}
	if os.Getenv("METIS_DEBUG") == "1" {
		fmt.Fprintln(os.Stderr, "metis: loaded config files:", loaded)
	}

	// Resolve and load a resumed session before provider/client and prompt
	// construction. Provider, model, and system are persisted in the header;
	// reading them after BuildProvider would restore the transcript while still
	// sending future turns through whatever happens to be configured today.
	store, err := session.NewStore(cfg.Session.Dir)
	if err != nil {
		return nil, err
	}
	resumeTarget, err := resolveResumeTarget(flags, store)
	if err != nil {
		return nil, err
	}
	var preparedResume *rtpkg.PreparedResume
	if resumeTarget != "" {
		preparedResume, err = rtpkg.PrepareResume(store, resumeTarget)
		if err != nil {
			return nil, err
		}
	}

	// Non-empty values also count as explicit for in-process callers that
	// construct cliFlags directly instead of going through parseFlags.
	providerSet := flags.providerSet || flags.provider != ""
	modelSet := flags.modelSet || flags.model != ""
	systemSet := flags.systemSet || flags.system != ""

	// Project registry: bump cwd's last-accessed entry. Idempotent;
	// failures are stderr-logged and swallowed (a broken registry
	// must not block a real session). `metis projects` reads this.
	projectsAutoRegisterFromCWD(config.Home())

	// Agent profile (--agent=NAME) loads early so its values participate
	// in flag merging and tool-registry filtering downstream. CLI overrides
	// always win — profile is "use this default unless I said otherwise".
	agentProf, err := rtpkg.LoadAgentProfile(flags.agentProfile)
	if err != nil {
		return nil, err
	}

	// Phase B: --debug opens a long-lived debug log. Done early so
	// every later step's stderr complaint is also captured. The log
	// duplicates METIS_DEBUG=1 stderr — we don't redirect, just tee.
	// Caller's responsibility to call Close() through rt.Cleanup.
	if flags.debug {
		if err := openDebugLog(); err != nil {
			fmt.Fprintf(os.Stderr, "metis: --debug: %v\n", err)
		}
	}

	// Phase B: --scope advisory. Today only `user` is real — warn the
	// user when they pass anything else so they know it didn't take.
	if flags.scope != "" && flags.scope != "user" {
		fmt.Fprintf(os.Stderr,
			"metis: --scope=%q ignored (only 'user' is implemented today; "+
				"per-scope mcp.toml + skills/ lands with the daemon work)\n",
			flags.scope)
	}

	// Apply flag overrides
	provName := cfg.Provider.Default
	if preparedResume != nil && !providerSet && preparedResume.Header.Provider != "" {
		provName = preparedResume.Header.Provider
	}
	if flags.provider != "" {
		provName = flags.provider
	}
	model := flags.model
	if preparedResume != nil && !modelSet && !providerSet && preparedResume.Header.Model != "" {
		model = preparedResume.Header.Model
	}
	mode := cfg.Permission.Mode
	if flags.mode != "" {
		mode = flags.mode
	}
	// --dangerously-skip-permissions overrides any other mode. Named to
	// match Claude Code; a deliberate "I really mean it" wrapper around
	// `--mode bypass`.
	if flags.dangerouslySkipPerms {
		mode = "bypass"
	}
	// 2026-05-11: ModeAuto removed. Reject both CLI flag and config
	// values to surface the change loud and clear — silent fallback
	// would just shift the user's confusion to "why is metis behaving
	// differently than before". Includes the migration hint inline so
	// the user knows what to switch to.
	if mode == "auto" {
		return nil, errors.New("permission mode \"auto\" has been removed.\n" +
			"It collided with claude-code's `auto` (LLM-classifier, ant-only)\n" +
			"and was confusing every user comparing the two.\n" +
			"\n" +
			"Pick one of:\n" +
			"  ask          — prompt for every non-allowlisted action (claude-code default)\n" +
			"  acceptEdits  — auto-allow Edit/Write/NotebookEdit, ask for Bash\n" +
			"  plan         — read-only mode, no writes or shell\n" +
			"  bypass       — approve everything (use --dangerously-skip-permissions)\n" +
			"  deny         — refuse everything\n" +
			"\n" +
			"Edit ~/.metis/config.toml `mode = ...` or run with `--mode acceptEdits`.")
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
	// Translate --simple → METIS_SIMPLE=1 so the single env-var probe
	// in runtime.IsSimpleMode() is the only signal downstream code
	// needs to read. Both flag and env paths converge to the same
	// behavior.
	if flags.simpleMode {
		os.Setenv("METIS_SIMPLE", "1")
	}

	// promptCtx captures the runtime signals each prompt section
	// inspects to decide whether to fire. Populated once here so the
	// dump-prompt path, the simple-mode path, and the regular path all
	// share the same source of truth.
	promptCtx := rtpkg.PromptCtx{
		Model:        model,
		ProviderName: provName,
		EnabledTools: nil, // filled in below once registry exists; nil = legacy "assume Bash"
		HasSkills:    rtpkg.HasInstalledSkills(),
		IsSubAgent:   false,
		Mode:         mode,
	}

	// Render the base prompt fresh per boot. Three paths:
	//   1. --system flag overrides entirely (user-supplied prompt).
	//   2. METIS_SIMPLE=1 / --simple → one-sentence stub for CI use.
	//   3. Default → section registry assembled with promptCtx.
	var resumedSystem string
	if preparedResume != nil && !systemSet {
		resumedSystem = preparedResume.Header.System
	}
	var system string
	switch {
	case systemSet:
		system = flags.system
	case resumedSystem != "":
		system = resumedSystem
	case rtpkg.IsSimpleMode():
		system = rtpkg.SimpleBasePrompt(model)
	default:
		system = rtpkg.AssembleBaseString(promptCtx)
		if hint := rtpkg.ProviderHintFor(provName, model); hint != "" {
			system = system + "\n\n" + hint
		}
	}
	// Agent profile body REPLACES the default system prompt — that's the
	// whole point of "I am a code reviewer". Skip when profile body is
	// empty (frontmatter-only profiles still customize tools/model).
	if agentProf != nil && agentProf.SystemPrompt != "" && !systemSet && resumedSystem == "" {
		system = agentProf.SystemPrompt
	}
	// Append user's optional ~/.metis/system.md addendum (claude-code-style
	// global system prompt). No-op when the file doesn't exist or the
	// profile asked us to skip it.
	// Build BOTH the string form (for legacy / non-Anthropic providers
	// that don't read SystemSections) AND the typed-section form (so
	// the Anthropic provider can emit per-section cache_control). The
	// two are kept in sync — the string is just the sectioned form
	// joined with boundary markers, so a provider walking either one
	// sees identical content.
	// G.8 (2026-05-12) — flip CoordinatorMode early so the assembled
	// system prompt picks up the coordinator overlay AND the tool
	// registry filter applies during BuildToolRegistry below. Env
	// `METIS_COORDINATOR_MODE=1` is checked by IsCoordinatorMode()
	// directly; the CLI flag routes through SetCoordinatorMode so
	// downstream callers see one consistent answer.
	if flags.coordinator {
		rtpkg.SetCoordinatorMode(true)
	}

	var systemSections []rtpkg.SystemPromptSection
	if resumedSystem == "" && (agentProf == nil || !agentProf.OmitClaudeMd) {
		assembleOpts := rtpkg.AssembleOptions{}
		// Inject the coordinator overlay between `base` and the
		// project-context sections so it shares the cached prefix.
		// CoordinatorOverlay() returns a zero-value section (Name=="")
		// when the mode is off — caller skips the append in that case.
		if ov := rtpkg.CoordinatorOverlay(); ov.Name != "" {
			assembleOpts.Overlays = append(assembleOpts.Overlays, ov)
		}
		// Plan-mode overlay: when `--mode plan` is active, append a
		// short fragment explaining the read-only workflow so the
		// model doesn't waste turns hitting permission denials. The
		// permission gate already enforces read-only at the tool
		// layer; this just tells the model NOT to try mutating tools.
		if ov := rtpkg.PlanOverlay(mode == string(permission.ModePlan)); ov.Name != "" {
			assembleOpts.Overlays = append(assembleOpts.Overlays, ov)
		}
		// Two assembly paths:
		//   - Default: per-section registry (identity/privacy/style/...).
		//     Each section is independently cacheable + sub-agents /
		//     Bash-less profiles skip irrelevant sections.
		//   - --simple / --system override / explicit user prompt:
		//     wrap `system` as one "base" section to preserve the
		//     caller's intent (don't fragment a user-supplied prompt).
		if systemSet || rtpkg.IsSimpleMode() {
			systemSections = rtpkg.AssembleSystemPromptSections(system, assembleOpts)
		} else {
			systemSections = rtpkg.AssembleSystemPromptSectionsCtx(promptCtx, assembleOpts)
		}
		system = rtpkg.RenderSections(systemSections)
	}

	// --add-dir / persisted additional dirs. Done before system prompt
	// finalization so the LLM sees the list at turn 0.
	allowedDirs := rtpkg.NewAllowedDirs(flags.addDirs)
	if extra := allowedDirs.SystemPromptAddendum(); extra != "" && resumedSystem == "" {
		system = system + extra
		// Allowed-dirs is stable per-session (set once at boot, doesn't
		// drift), so cache it as its own section. Volatile=false +
		// Cache=true → Anthropic gets a 4th breakpoint candidate; if
		// the budget runs out, BuildSystemBlocksFromSections silently
		// drops it from the cache list (still emitted, just uncached).
		systemSections = append(systemSections, rtpkg.SystemPromptSection{
			Name:  "allowed_dirs",
			Body:  extra,
			Cache: true,
		})
	}

	// --prompt-dump: short-circuit before any LLM / channel / cron
	// wiring. We've already done provider resolution, mode resolution,
	// overlay assembly, allowed_dirs — i.e. exactly what would be sent
	// to the model on turn 0. Print to stdout and exit 0; nothing
	// downstream gets initialised. Per-section markers (=== N: Name
	// [cache?] ===) so a token-counter can split + budget per section.
	if flags.dumpPrompt {
		printPromptDump(systemSections, system)
		os.Exit(0)
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
	// MinimalSystem is the trimmed prompt the Agent tool's sub-agents
	// run with — base + env, no <project_context>, no addendum. The
	// parent loop's full assembled `system` already saw those, so a
	// sub-agent inheriting them would just re-pay the tokens for
	// information it doesn't need at its narrower scope. Mirrors
	// openclaw's "minimal mode" sub-agent prompt.
	minimalSystem := rtpkg.AssembleSystemPromptWithOptions(
		rtpkg.DefaultBasePrompt(),
		rtpkg.AssembleOptions{Minimal: true},
	)
	// Background-bash job pool. One per process so all chat / run /
	// agent paths share the same `bg_<id>.out` filesystem layout +
	// `<job_notification>` sink. Created here so BuildToolRegistry
	// can wire it into Bash + register BashList/BashOutput/BashKill.
	jobsPool := jobs.NewRegistry("")
	// Monitor registry — per-line pattern watcher that complements the
	// jobs pool. Lives next to jobsPool because a Monitor watch is
	// always anchored to a job ID returned by Spawn. nil-safe across
	// the stack: tools with no Monitor wiring just skip the tool, the
	// loop's drain is a no-op.
	monitorReg := agent.NewMonitorRegistry(0)
	// Runtime cleanup and in-process session boundaries stop all tail watchers.
	// Do not defer that here: setupRuntime returns before the chat starts, so a
	// setup-local defer cannot own the live runtime's lifecycle.

	// Sub-agent Roster — process-wide registry that backs G.0's
	// concurrency cap and G.3 named teammates / G.16 UI observability.
	// Capacity reads from config.Agents (split named/anon since
	// 2026-05-14) with env override on top. Precedence:
	//   METIS_MAX_SUBAGENTS_NAMED / _ANON   (highest, per-kind)
	//   METIS_MAX_SUBAGENTS                 (combined, split 1:2)
	//   config.Agents.MaxConcurrentNamed / _Anon
	//   legacy config.Agents.MaxConcurrentSubAgents (split 1:2)
	//   defaults (20 named / 40 anon)
	//
	// Runtime cleanup and in-process session boundaries cancel everyone so
	// orphan sub-agents cannot keep burning tokens or report into a new session.
	capNamed, capAnon := resolveRosterCaps(cfg)
	subAgentRoster := agent.NewRoster(capNamed, capAnon)

	reg := rtpkg.BuildToolRegistry(rtpkg.ToolRegistryOptions{
		Cfg:             cfg,
		Gate:            gate,
		Provider:        prov,
		Model:           model,
		System:          system,
		MinimalSystem:   minimalSystem,
		ChannelRegistry: chReg,
		DefaultPlatform: cfg.Channels.DefaultPlatform,
		CronService:     cronSvc,
		MemoryManager:   memoryMgr,
		Jobs:            jobsPool,
		Monitors:        monitorReg,
		ConfigSnapshot:  snap,
		Roster:          subAgentRoster,
	})

	// G.8 — coordinator mode replaces every non-whitelisted tool
	// with a stub that errors when invoked. The user sees the names
	// in /tools but the model can't accidentally Edit/Write/Bash; the
	// system prompt overlay above tells it to dispatch sub-agents.
	rtpkg.FilterRegistryInPlace(reg)

	// Wire the gate's read-only resolver to the live registry now
	// that tool registration is finalised. Two-tier resolution:
	//
	//  1. Input-aware tools (Bash, Git) parse the stringInput. Bash
	//     auto-allows `git status / ls / cat`-style safe argv via
	//     permission.IsSafeReadOnlyBash (which is shell-meta aware,
	//     blocks sudo / pipes / cmd substitution). 2026-05-13 fix
	//     for the "acceptEdits asks even for `ls`" complaint.
	//  2. Metadata-only tools: query the registry's IsReadOnly. Auto-
	//     allows SubAgentOutput / BashOutput / TaskOutput / Skill /
	//     LSP / MetisInfo / ToolSearch / WebFetch / etc.
	//
	// nil hook → gate falls back to its hardcoded legacy allowlist
	// (Read / Edit / Write / NotebookEdit). Headless tests that
	// build a Gate without a registry stay on that path.
	gate.SetReadOnlyHook(func(name, stringInput string) bool {
		// Tier 1: input-aware short-circuits.
		switch name {
		case "Bash":
			return permission.IsSafeReadOnlyBash(stringInput)
		}
		// Tier 2: tool-declared metadata.
		t, ok := reg.Get(name)
		if !ok {
			return false
		}
		return tools.IsReadOnly(t, nil)
	})

	// MCP servers — launch each enabled stdio server, register its tools as
	// `mcp__<name>__<tool>`. Failures are non-fatal: warn and continue.
	//
	// Sources merge in this order: runtime entries from ~/.metis/mcp.toml
	// (mutated by /mcp add) win over [[mcp.servers]] declared in
	// config.toml. The merge happens via runtime.MergeWithConfig so legacy
	// config-only setups still work without any user action.
	//
	// Phase 2 (2026-05-18) — launch runs in a BACKGROUND GOROUTINE so the
	// chat UI starts immediately. Each server's tools register into the
	// shared (mutex-protected) registry as soon as its handshake completes.
	// claude-code uses the same shape — `getMcpToolsCommandsAndResources`
	// streams per-server `onConnectionAttempt` callbacks rather than
	// blocking the prompt session on a sum-of-handshakes wall clock.
	// Cleanup waits on `mcpLauncherDone` so we never tear down the
	// parent process while a goroutine is mid-handshake.
	var mcpReg *mcp.Registry
	mcpLauncherDoneCh := make(chan struct{})
	if flags.bare {
		// --bare skips the MCP launch dance entirely. The user gets
		// builtins-only — no `mcp__*` tools, no spawned subprocesses,
		// no per-server merge cost. Still safe to /mcp list / add later;
		// nothing in this path mutates mcp.toml.
		mcpReg = &mcp.Registry{}
		close(mcpLauncherDoneCh) // no work — Cleanup's <-recv is instant
	} else {
		var mcpLoadErr error
		mcpReg, mcpLoadErr = mcp.Load()
		if mcpLoadErr != nil {
			fmt.Fprintf(os.Stderr, "metis: mcp.toml: %v\n", mcpLoadErr)
			mcpReg = &mcp.Registry{}
		}
		mcpReg.MergeWithConfig(cfg.MCP.Servers)
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
	var pluginReg *rtpkg.PluginRegistry
	if !flags.bare {
		// EnsureBundledPlugins already ran inside setupRuntime so the
		// extracted plugin tree is on disk by the time we get here.
		var pluginErrs []error
		pluginReg, pluginErrs = rtpkg.LoadPlugins(ctx, reg)
		for _, e := range pluginErrs {
			fmt.Fprintf(os.Stderr, "metis: plugin load: %v\n", e)
		}
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

	// Global tool visibility filter (2026-05-14) — applied AFTER MCP +
	// plugins register so dynamically-loaded mcp__* / plugin tools are
	// also subject to it. Merge rules:
	//
	//   allow  = flags.tools  if non-empty, else cfg.Tools.Allowed
	//   deny   = cfg.Tools.Disallowed ∪ flags.disallowTools
	//
	// The CLI flag REPLACES the config allowlist (so `--tools Read`
	// truly limits to Read regardless of config), but UNIONS into the
	// config blocklist — CLI can tighten, never loosen.
	allowVis := tools.SplitCSV(flags.tools)
	if len(allowVis) == 0 {
		allowVis = cfg.Tools.Allowed
	}
	denyVis := append([]string(nil), cfg.Tools.Disallowed...)
	denyVis = append(denyVis, tools.SplitCSV(flags.disallowTools)...)
	tools.ApplyToolVisibility(reg, allowVis, denyVis)

	maxIter := flags.maxIter
	if maxIter == 0 {
		maxIter = cfg.Session.MaxIterations
	}

	loop := rtpkg.BuildAgentLoop(cfg, rtpkg.AgentLoopOptions{
		Provider: prov, Registry: reg, Gate: gate,
		System: system, SystemSections: systemSections,
		Model: model, MaxIter: maxIter,
		MemoryManager: memoryMgr,
		Jobs:          jobsPool,
		Monitors:      monitorReg,
		MaxBudgetUSD:  flags.maxBudgetUSD,
	})

	// METIS_SIMPLE / --simple → use the curated short tool descriptions
	// matched 1:1 with the simple-mode system prompt: short prompt +
	// short tool docs reach the LLM as one consistent "small surface"
	// boot.
	if rtpkg.IsSimpleMode() {
		loop.ShortToolDescriptions = true
	}

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

	// Gate→Loop mode bridge — 2026-05-18 fix for the plan-mode deny
	// storm. Two state variables (Gate.Mode and Loop.PlanMode) used
	// to be independently mutable, so flipping Gate.Mode=plan via
	// Shift+Tab or `/mode plan` left Loop.PlanMode=false and the loop
	// went on dispatching tools straight into a wall of denies. Now:
	// Gate is the single source of truth; SetMode fires this listener
	// which pushes the matching plan-mode flag onto Loop. Both
	// directions (model-initiated EnterPlanMode and user-initiated
	// Shift+Tab) converge on the same code path.
	gate.SetModeChangeListener(func(m permission.Mode) {
		loop.SetPlanMode(m == permission.ModePlan)
	})
	// Apply once at boot so a session started in plan mode (via
	// `metis --mode plan` or `[permissions] mode = "plan"` in config)
	// has Loop.PlanMode aligned before the first user turn.
	loop.SetPlanMode(gate.Mode() == permission.ModePlan)

	// G.5 — stash the auto-memory extractor on this var so the
	// `rt` struct below can pick it up for /dream status. Declared
	// outside the if-block so the unconditional rt assignment
	// compiles in either branch.
	var pendingExtractor *agent.AutoMemoryExtractor

	// Auto-memory v2: opt-in via --auto-memory or METIS_AUTO_MEMORY=1.
	// Wires the extractor to LoopEnd; extractor itself is best-effort
	// and never blocks the turn.
	if flags.autoMemory || os.Getenv("METIS_AUTO_MEMORY") == "1" {
		loop.AutoMemory = true
		// Pass the configured skill dir so SkillSynth + curator target the
		// same directory the live Skill tool / loader use (honors a custom
		// [session] skill_dir override).
		ext, err := agent.NewAutoMemoryExtractor(loop, "", cfg.Session.SkillDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metis: auto-memory init: %v (disabled)\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "metis: auto-memory enabled (memdir: %s)\n", ext.MemdirRoot())
			// G.5 (2026-05-12) — DreamTask completion channel.
			// Buffered so the extractor never blocks on its
			// notify-send if the model is still mid-turn; size 8
			// is generous (a 5-minute auto-memory session rarely
			// fires more than 1-2 forks).
			dreamCh := make(chan agent.DreamNotification, 8)
			loop.DreamNotify = dreamCh
			ext.SetDreamNotify(dreamCh)
			loop.Hooks.Register(pubhook.LoopEndHandler(func(ctx context.Context, _ pubhook.Context, stopReason string) {
				if os.Getenv("METIS_AUTO_MEMORY_DEBUG") == "1" {
					fmt.Fprintf(os.Stderr, "[auto-memory] LoopEnd stop=%s — invoking extractor\n", stopReason)
				}
				ext.OnLoopEnd(ctx, stopReason)
			}))
			// Stash on the runtime so the `/dream` slash command
			// can read live phase + last-run stats. Wired below
			// after the `rt` struct is constructed.
			pendingExtractor = ext
		}
	}

	rt := &runtime{
		cfg: cfg, provider: prov, registry: reg, gate: gate, store: store,
		loop: loop, cronSvc: cronSvc, useMD: cfg.UI.Markdown && !flags.noMarkdown,
		showTok: cfg.UI.ShowTokens, model: model, providerName: provName,
		defaultPermissionMode: permission.Mode(mode),
		mcpLauncherDone:       mcpLauncherDoneCh,
		plugins:               pluginReg,
		allowedDirs:           allowedDirs,
		autoMemExtractor:      pendingExtractor,
		subAgentRoster:        subAgentRoster,
	}

	// Phase 2 MCP launch — kicked off only after `rt` is fully built so
	// the goroutine can reach into rt.mcpServers / rt.mcpServersMu. Bare
	// mode already closed the done-channel above and starts NO launcher.
	// (The prior code let bare fall into a headless `else` arm whose
	// goroutine `defer close`d mcpLauncherDoneCh a SECOND time → "close
	// of closed channel" panic on startup — caught 2026-06-15 launching
	// `metis chat --bare`.)
	if !flags.bare {
		// Diagnostic sink for async MCP errors: a TUI must keep stderr
		// OFF the alt-screen (→ debug log, possibly nil = silent), since
		// stray stderr corrupts the chat; a headless run wants it on
		// stderr, its only output channel.
		diag := os.Stderr
		if flags.useTUI || term.IsTerminal(int(os.Stdout.Fd())) {
			diag = debugLogFile
		}
		lazyMode := mcp.ParseLazyMode(os.Getenv("METIS_LAZY_MCP"))
		go func(reg *tools.Registry, mcpReg *mcp.Registry, mode mcp.LazyMode) {
			defer close(mcpLauncherDoneCh)
			servers, errs := mcp.LaunchAllLazy(ctx, mcpReg, reg, mode)
			for _, e := range errs {
				if diag != nil {
					fmt.Fprintf(diag, "metis: MCP launch: %v\n", e)
				}
			}
			rt.mcpServersMu.Lock()
			rt.mcpServers = append(rt.mcpServers, servers...)
			rt.mcpServersMu.Unlock()
			// Attach to a running IDE's MCP server if one advertises a
			// matching workspace via ~/.metis/ide/*.lock (best-effort).
			if ide := connectIDE(ctx, reg, diag); ide != nil {
				rt.mcpServersMu.Lock()
				rt.mcpServers = append(rt.mcpServers, ide)
				rt.mcpServersMu.Unlock()
			}
		}(reg, mcpReg, lazyMode)
	}

	if preparedResume != nil {
		res, err := rtpkg.ApplyPreparedResume(preparedResume, loop, gate, os.Stderr)
		if err != nil {
			rt.Cleanup()
			return nil, err
		}
		rt.sessionID = res.SessionID
		// An explicit CLI mode is an invocation override. ApplyResume restores
		// the stored posture first; the command-line choice wins last.
		if flags.mode != "" || flags.dangerouslySkipPerms {
			gate.SetMode(permission.Mode(mode))
		}
	} else {
		if flags.newSessionID != "" {
			if !validExplicitSessionID(flags.newSessionID) {
				return nil, fmt.Errorf("invalid --session-id %q", flags.newSessionID)
			}
			if _, err := os.Stat(filepath.Join(store.Dir, flags.newSessionID+".jsonl")); err == nil {
				return nil, fmt.Errorf("session %s already exists", flags.newSessionID)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("check session %s: %w", flags.newSessionID, err)
			}
			rt.sessionID = flags.newSessionID
		} else {
			rt.sessionID = store.NewSessionID()
		}
		if err := persistFreshSessionHeader(store, rt.sessionID, provName, model, system, mode); err != nil {
			rt.Cleanup()
			return nil, err
		}
	}
	// --name <text> persists as the session title. Done after the
	// fresh-header write so resume → already-existing sessions also
	// get re-titled via the same path.
	if flags.sessionName != "" {
		if err := persistSessionTitle(store, rt.sessionID, flags.sessionName); err != nil {
			rt.Cleanup()
			return nil, err
		}
	}
	// Wire per-step tool timing into this session's sidecar so
	// `metis sessions timing <id>` can show where the run spent its
	// wall-clock time after the fact. Best-effort + cheap; appends one
	// JSONL line per tool call.
	loop.TimingSink = store.NewTimingRecorder(rt.sessionID).Record

	// Tell the tasks pkg what session id is current so TodoWrite /
	// TodoRead persist into the right per-session file. Done last so a
	// resume failure above doesn't leave a stale id in the singleton.
	rtpkg.SetCurrentSessionID(rt.sessionID)
	// Wire the same id into the LLM transport layer so dump-prompts
	// (METIS_DUMP_PROMPTS=1) lands in dump-prompts/<sid>.jsonl
	// instead of dump-prompts/default.jsonl.
	transport.SetSessionID(rt.sessionID)
	// Same dance for the structured Task* tools (TaskCreate / TaskList /
	// TaskUpdate / TaskOutput / TaskStop / TaskGet). Lives in the
	// internal/tasks package (not runtime) to break an otherwise
	// circular import (tools/builtin → runtime → tools/builtin).
	taskstore.SetCurrentTaskStore(rt.sessionID)
	// Crash-recovery pointer (#27 / claude-code-sourcemap bridgePointer):
	// write a per-cwd pointer file ~/.metis/session-pointers/<sha8(cwd)>.json
	// so a future startup can detect "you had a session live N minutes ago,
	// resume it?" Best-effort throughout — a write/heartbeat failure must
	// never crash the agent, so errors are intentionally swallowed.
	if cwd, err := os.Getwd(); err == nil {
		_ = session.WritePointer(rt.sessionID, cwd)
		// Heartbeat goroutine bumps mtime every HeartbeatInterval until
		// the parent ctx is cancelled (signal.NotifyContext catches
		// SIGINT/SIGTERM). Only crashes / kill -9 leave the pointer
		// behind for the next startup to detect — graceful shutdown
		// goes through runtime.Cleanup → session.ClearPointer.
		session.StartHeartbeat(ctx, rt.sessionID, cwd)
		rt.sessionPointerCwd = cwd
	}

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

	// 2026-05-22: apply UI tunables that used to be hardcoded
	// constants (permissionTimeout / voiceMaxRecord / statusLineRefresh).
	// All three are mutable package-vars with defaults — these calls
	// override them when the user set explicit values in [ui].
	tui.SetPermissionTimeout(cfg.UI.PermissionTimeout())
	tui.SetVoiceMaxRecord(cfg.UI.VoiceMaxRecord())
	tui.SetStatusLineRefresh(cfg.UI.StatusLineRefresh())

	return rt, nil
}

// resolveResumeTarget selects the session before provider and prompt
// construction so setupRuntime can restore header defaults in time. Order:
// explicit --resume, interactive picker, then --continue's newest session.
func resolveResumeTarget(flags *cliFlags, store *session.Store) (string, error) {
	resumeTarget := flags.resumeID
	if resumeTarget == "" && flags.pickResume {
		picked, err := runResumePicker(store)
		if err != nil {
			return "", err
		}
		resumeTarget = picked
	}
	if resumeTarget == "" && flags.cont {
		entries, listErr := store.List(1)
		if listErr != nil {
			fmt.Fprintf(os.Stderr, "metis: --continue: %v (starting fresh)\n", listErr)
		} else if len(entries) == 0 {
			fmt.Fprintln(os.Stderr, "metis: --continue: no prior sessions found (starting fresh)")
		} else {
			resumeTarget = entries[0].ID
		}
	}
	if resumeTarget != "" && flags.newSessionID != "" {
		return "", errors.New("--resume and --session-id are mutually exclusive")
	}
	return resumeTarget, nil
}

func validExplicitSessionID(id string) bool {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// NewRegistry helper because tools.NewRegistry doesn't exist; create one.
// We rely on the global registry pattern for built-ins, so make a fresh instance.

// --- subcommands ---

func cmdChat(ctx context.Context, args []string) error {
	// Early input capture (Task #76): start grabbing stdin RIGHT NOW so
	// keystrokes typed during the cold-start window (config load,
	// runtime build, slash registry, etc.) don't echo to a phantom
	// prompt before bubbletea takes over. Stop() is called just before
	// RunTUI, and the buffered bytes are forwarded to bubbletea via
	// SetEarlyInputReader. No-op when stdin isn't a TTY.
	earlyIn := rtpkg.NewEarlyInput()
	flags, _, err := parseFlags(args)
	if err != nil {
		if earlyIn != nil {
			earlyIn.Stop()
		}
		return err
	}
	// Trust-this-folder safety check — claude-code's first-run dance.
	// Skipped when stdin isn't a terminal (CI, expect-scripted) or
	// when METIS_NO_TRUST_PROMPT=1. Persists answer to
	// ~/.metis/trusted-dirs.json so it asks once per directory.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		// Trust prompt reads stdin too — restore terminal mode FIRST
		// so the bufio.Scanner inside ensureTrusted sees normal line
		// input, not raw bytes. The buffer (if any) is preserved for
		// the bubbletea hand-off below.
		if earlyIn != nil {
			earlyIn.Stop()
		}
		if err := ensureTrusted(); err != nil {
			return err
		}
		if notice := maybeNotifyUpdate(); notice != "" {
			tui.SetPendingUpdateNotice(notice)
		}
	}

	// --worktree: spawn a git worktree first so config/CLAUDE.md/etc.
	// load from the new cwd. The worktree info is stashed in a closure
	// for cleanup at the end of cmdChat.
	var worktreeInfo *worktreepkg.Info
	if flags.worktree != "" || flags.worktreeOn {
		info, err := worktreepkg.Spawn(flags.worktree)
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
	// Late-register the SlashCommand tool now that both the tool registry
	// (rt.registry) and the slash registry (sl) exist — the tool lets the
	// model invoke user-authored custom commands. Registered here rather
	// than in BuildToolRegistry because the slash registry is built after
	// the tool registry, and the registry accepts post-construction adds.
	if rt.registry != nil {
		rt.registry.Register(builtin.NewSlashCommand(rt.gate, slashModelRunner{reg: sl}))
		// MCP resource tools — read the async-populated server list live.
		resAdapter := mcpResourceAdapter{rt: rt}
		rt.registry.Register(builtin.NewListMcpResources(rt.gate, resAdapter))
		rt.registry.Register(builtin.NewReadMcpResource(rt.gate, resAdapter))
	}
	// Wire the working-tree checkpointer so /rewind can restore files +
	// conversation together. Best-effort: a shadow-repo init failure just
	// leaves /rewind reporting "nothing to rewind".
	if rt.loop != nil && rt.sessionID != "" {
		if cwd, err := os.Getwd(); err == nil {
			rt.loop.Checkpointer = checkpoint.NewManager(rt.sessionID, cwd, "")
		}
	}

	useTUI := flags.useTUI
	if !useTUI {
		// Auto-detect TTY: use TUI when stdout is a terminal
		useTUI = term.IsTerminal(int(os.Stdout.Fd()))
	}

	if useTUI {
		hooks := tui.ExternalHooks{
			DirAdd:              rt.allowedDirs.Add,
			DirRemove:           rt.allowedDirs.Remove,
			DirList:             rt.allowedDirs.All,
			BtwAsk:              rt.askSideQuestion,
			FreshPermissionMode: rt.defaultPermissionMode,
			SessionSwitch:       rt.rebindSession,
			SessionBoundary:     rt.releaseSessionWork,
		}
		// Hand the early-input buffer to bubbletea. If the trust prompt
		// already consumed it (Stop() was called above), Reader()
		// returns os.Stdin directly — no double-read of the same fd.
		if earlyIn != nil {
			earlyIn.Stop() // idempotent — safe even if trust prompt called it
			tui.SetEarlyInputReader(earlyIn.Reader())
		}
		return tui.RunTUI(ctx, rt.loop, rt.cronSvc, sl, rt.store, rt.sessionID, rt.gate, rt.model, rt.providerName, rt.cfg.Session.SkillDir, rt.cfg, true, hooks) // true = force new session banner
	}
	// Non-TUI path: just stop the capture so terminal mode is restored.
	if earlyIn != nil {
		earlyIn.Stop()
	}

	repl, err := tui.NewREPL(rt.loop, sl, rt.store, rt.sessionID, rt.useMD, rt.showTok, rt.gate, rt.model, rt.cfg.Session.SkillDir)
	if err != nil {
		return err
	}
	repl.ConfigureProviderSwitch(rt.cfg, rt.providerName)
	repl.SessionSwitch = rt.rebindSession
	repl.SessionBoundary = rt.releaseSessionWork
	repl.FreshPermissionMode = rt.defaultPermissionMode
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

	// CACHE-D: response cache lookup BEFORE we touch the API. Hit →
	// print cached text and return early; saves the full round-trip
	// (and the dollars). Tool-use turns are never cached so cache
	// hits are always safe to replay verbatim.
	var cacheKey string
	cacheTTL := rtpkg.ParseRunCacheTTL(flags.runCacheTTL)
	if flags.runCache || os.Getenv("METIS_RUN_CACHE") == "1" {
		cacheKey = rtpkg.RunCacheKey(rt.model, rt.cfg.Provider.Default, rt.loop.System, prompt)
		if hit, _ := rtpkg.LookupRunCache(cacheKey); hit != nil {
			fmt.Print(hit.Response)
			fmt.Println()
			fmt.Fprintf(os.Stderr, "[cache] hit — served from ~/.metis/run-cache (%s ago, saved API call)\n",
				time.Since(hit.CreatedAt).Round(time.Second))
			return nil
		}
	}

	// 2026-05-22: wait for MCP launcher to settle BEFORE we hand the
	// prompt to the loop. Without this, `metis run` raced the
	// async-spawn goroutine in setupRuntime — the LLM would frequently
	// see an empty mcp__* table and report "no such tool" even though
	// servers were correctly configured (remote tencent-cloud cu test,
	// 2026-05-22). 15 s covers a cold npx-resolve startup; if servers
	// are slower the run continues but warns to stderr so the user
	// knows tool calls will fail.
	if ok := rt.WaitForMCP(15 * time.Second); !ok {
		fmt.Fprintln(os.Stderr, "metis: MCP launcher still running after 15s — continuing without it (mcp__* tools may be unavailable)")
	}

	// Subdirectory hints (mirrors TUI submit path): when the prompt
	// @-mentions a path below cwd, gather per-dir CLAUDE.md /
	// AGENTS.md / METIS.md along the descent and prepend them to the
	// LLM-facing message. Preserve the transcript-stored prompt as the
	// raw text so session JSONL / history.jsonl stay clean.
	llmPrompt := prompt
	if cwd, err := os.Getwd(); err == nil {
		if hints := rtpkg.CollectSubdirHints(prompt, cwd, nil); hints != "" {
			llmPrompt = hints + "\n\n" + prompt
		}
	}
	// --output-schema: load the schema up front (a typo'd path errors
	// before any tokens burn) and append the output contract to the
	// prompt. Final-reply validation + correction retries happen after
	// the event loop below.
	schemaEnforcer, err := rtpkg.NewOutputSchemaEnforcer(flags.outputSchema)
	if err != nil {
		return err
	}
	if schemaEnforcer != nil {
		llmPrompt = llmPrompt + "\n\n" + schemaEnforcer.Instruction()
	}
	rt.loop.AppendUser(llmPrompt)
	if rt.store != nil && rt.sessionID != "" {
		_ = rt.store.AppendMessage(rt.sessionID, llm.Message{
			Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: prompt}},
		})
	}
	_ = rtpkg.AppendHistory(rtpkg.HistoryEntry{
		SessionID: rt.sessionID, Input: prompt, Source: "run",
	})

	// 2026-05-23: per-run wall-clock cap covers the WHOLE invocation
	// (parent loop + every sub-agent / fork spawned under it). The
	// existing per-turn wall-clock in loop.go (45 min default) only
	// bounds ONE turn — a fanned-out run with 18 sub-agents each
	// running 5-10 min easily blew past wall expectations even though
	// no single turn was abnormally long (see kimi-cli-go porting
	// test 2026-05-23 — 41 min on --max-iter 30, sub-agent budget
	// wasn't inherited so each agent ran its own 100-iter default).
	//
	// Default: 30 min. Override via METIS_RUN_MAX_SECONDS for
	// workflows that genuinely need longer.
	runWallCap := 30 * time.Minute
	if env := os.Getenv("METIS_RUN_MAX_SECONDS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			runWallCap = time.Duration(n) * time.Second
		}
	}
	ctx, cancelRunCap := context.WithTimeout(ctx, runWallCap)
	defer cancelRunCap()

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() { done <- rt.loop.Run(ctx, events); close(events) }()

	// Streamlined output mode (Task #86, CC parity mechanism 4).
	// Activated via [ui] streamlined_output toml OR --streamlined CLI flag.
	// When on:
	//   - thinking deltas → dropped entirely
	//   - per-tool [tool] X stderr lines → suppressed; counts accumulated
	//   - text deltas → flush accumulated tool summary to stderr first,
	//     then reset counter (CC pattern)
	//   - tool errors → still surfaced (errors are signal, not noise)
	//   - LoopDone → flush any remaining unflushed counts
	streamlined := flags.streamlined || rt.cfg.UI.StreamlinedOutput
	var accum rtpkg.StreamlinedAccumulator
	flushAccum := func() {
		if !streamlined || accum.Empty() {
			return
		}
		fmt.Fprintf(os.Stderr, "[%s]\n", accum.Summary())
		accum.Reset()
	}

	// CACHE-D: accumulate the final assistant text + track whether any
	// tool was used. Save to cache only when (a) cacheKey is set
	// (user opted in) AND (b) the turn ran without any tool_use
	// blocks (tool-using turns observe the world; cached replays
	// would lie about that observation).
	var cacheTextBuf strings.Builder
	usedToolsThisRun := false

	// --output-schema: accumulate assistant text instead of streaming
	// it to stdout — partial/invalid candidates must not pollute the
	// pipe; only the final validated JSON is printed (CC headless
	// contract). stderr traffic (tools, metrics) is unaffected.
	var schemaTextBuf strings.Builder

	// Token totals — summed across every API call this run made.
	// Emitted as a single `[metrics]` line at LoopDone so the eval
	// harness can scrape token spend per scenario without parsing
	// streaming token deltas. Mirrors claude-code's /cost output:
	// cache_read and cache_create are reported separately so a
	// 60k-token cache hit looks cheap, not the same line item as
	// 60k tokens of fresh input.
	var totIn, totOut, totCacheRead, totCacheCreate int

	// --metrics-log: open append-only JSONL sink for per-turn metrics.
	// One line per round-trip (EventTurnEnd or final EventLoopDone).
	// Created lazily so that a typo'd path errors loudly before the
	// LLM call burns any tokens.
	var metricsLogFile *os.File
	if flags.metricsLog != "" {
		f, ferr := os.OpenFile(flags.metricsLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if ferr != nil {
			return fmt.Errorf("--metrics-log: %w", ferr)
		}
		metricsLogFile = f
		defer metricsLogFile.Close()
	}

	// Per-turn metrics state. Reset on every EventTurnEnd; flushed
	// on EventTurnEnd (mid-loop) and EventLoopDone (final).
	turnNumber := 1
	turnStart := time.Now()
	var turnIn, turnOut, turnCacheRead, turnCacheCreate int
	var turnToolCalls, turnToolErrors int
	var turnNudges, turnRescues int

	// OTLP metrics exporter — active only when OTEL_EXPORTER_OTLP_ENDPOINT
	// is set; nil otherwise (all methods no-op). Records each turn's spend
	// and is flushed when cmdRun returns (best-effort).
	otlp := telemetry.New(rt.model, rt.sessionID)
	defer func() { _ = otlp.Export(context.Background()) }()

	emitMetrics := func(stopReason string) {
		// Fold the turn into the OTLP counters regardless of --metrics-log.
		otlp.RecordTurn(telemetry.TurnMetrics{
			InputTokens:  turnIn,
			OutputTokens: turnOut,
			CacheRead:    turnCacheRead,
			CacheCreate:  turnCacheCreate,
			ToolCalls:    turnToolCalls,
			ToolErrors:   turnToolErrors,
			DurationMS:   time.Since(turnStart).Milliseconds(),
		})
		if metricsLogFile == nil {
			return
		}
		rec := map[string]any{
			"turn_number": turnNumber,
			"tokens": map[string]any{
				"in":           turnIn,
				"out":          turnOut,
				"cache_read":   turnCacheRead,
				"cache_create": turnCacheCreate,
			},
			"tool_calls":   turnToolCalls,
			"tool_errors":  turnToolErrors,
			"duration_ms":  time.Since(turnStart).Milliseconds(),
			"stop_reason":  stopReason,
			"nudges_fired": turnNudges,
			"rescue_fired": turnRescues,
		}
		if b, jerr := json.Marshal(rec); jerr == nil {
			_, _ = metricsLogFile.Write(append(b, '\n'))
		}
	}
	resetTurn := func() {
		turnNumber++
		turnStart = time.Now()
		turnIn, turnOut, turnCacheRead, turnCacheCreate = 0, 0, 0, 0
		turnToolCalls, turnToolErrors = 0, 0
		turnNudges, turnRescues = 0, 0
	}

	runStart := time.Now()
	// Phase A — honest exit code: track loop-end aborts that the
	// previous `return err` branch was silently treating as success
	// because Loop.Run() returns nil on every non-error abort path
	// (diminishing_returns, max_iterations, loop_detected). Capture
	// the StopReason from the final EventLoopDone, plus the most
	// recent EventInfo text (which carries the abort's user-facing
	// detail — see e.g. iter_cap "budget exhausted (N iters...)" at
	// internal/agent/loop.go:1036 or diminishing-returns at :907).
	// At end of cmdRun, if no real error fired AND the stop reason
	// was one of the incomplete-triggers, return *IncompleteError so
	// shell wrappers see exit code 11 instead of 0. The 4-round
	// 2026-05-18 benchmark caught the bug: mini-interpreter task
	// failed `go test` but `metis run … && echo ok` printed "ok".
	var lastInfoText string
	var incompleteReason, incompleteDetail string
	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			if streamlined {
				flushAccum()
			}
			if schemaEnforcer != nil {
				schemaTextBuf.WriteString(ev.TextDelta)
			} else {
				fmt.Print(ev.TextDelta)
			}
			if cacheKey != "" {
				cacheTextBuf.WriteString(ev.TextDelta)
			}
		case agent.EventThinkingDelta:
			if streamlined {
				continue // distillation gold — drop entirely
			}
			// (Default path keeps no per-thinking-delta surfacing in
			// run mode either, but explicit no-op here documents the
			// streamlined contract.)
		case agent.EventToolStart:
			usedToolsThisRun = true
			turnToolCalls++
			if streamlined {
				accum.AccumulateTool(ev.ToolName)
				continue
			}
			fmt.Fprintf(os.Stderr, "\n[tool] %s\n", ev.ToolName)
		case agent.EventToolResult:
			if ev.ToolResult != nil && ev.ToolResult.IsError {
				turnToolErrors++
				// Errors always surface — both modes need them so
				// scripts can detect failure.
				fmt.Fprintf(os.Stderr, "[tool error] %s\n", truncStderr(ev.ToolResult.Output, 300))
			}
		case agent.EventPermissionRequest:
			ev.PermissionReply <- agent.PermissionDecisionDeny
			fmt.Fprintf(os.Stderr, "[permission] denied (non-interactive); use chat for interactive prompts\n")
		case agent.EventAskUser:
			// Headless / metis run path: there's no user to answer the
			// model's question. Send an empty string back to dismiss
			// the prompt — the AskUser tool surfaces that as an
			// IsError result so the model can fall back to a default
			// rather than hang waiting for input that will never come.
			if ev.AskUserReply != nil {
				ev.AskUserReply <- ""
			}
			fmt.Fprintf(os.Stderr, "[askuser] dismissed (non-interactive); use chat for interactive prompts\n")
		case agent.EventPlan:
			// Plan mode: archive + print a structured summary so the
			// non-interactive caller can pipe the result.
			_ = rtpkg.ArchivePlan(rtpkg.ArchivedPlan{
				SessionID: rt.sessionID,
				ToolCalls: ev.ToolCalls,
			})
			if streamlined {
				// Don't dump per-tool args in streamlined mode; just
				// the count — the archived file has the details.
				fmt.Fprintf(os.Stderr, "\n[plan] %d tool call(s) — archived to ~/.metis/plans/\n", len(ev.ToolCalls))
			} else {
				fmt.Fprintf(os.Stderr, "\n[plan] %d tool call(s) — archived to ~/.metis/plans/\n", len(ev.ToolCalls))
				for _, tc := range ev.ToolCalls {
					fmt.Fprintf(os.Stderr, "  → %s(%v)\n", tc.Name, tc.Input)
				}
			}
		case agent.EventCompactionStart:
			// Same purpose as the TUI's spinner-override: tell the
			// caller "we're entering a 5-30s LLM summarize" so a long
			// pause doesn't look like a hang in run/cron-run mode.
			tier := ev.Info
			if tier == "" {
				tier = "compact"
			}
			fmt.Fprintf(os.Stderr, "[compact] starting %s — summarizing history\n", tier)
			// OSC 9;4 indeterminate — picks up iTerm2 tab / Ghostty tab
			// progress indicator even when stderr scrolls past the
			// "[compact]" line. No-op when stdout isn't a TTY (piped
			// run output won't pollute downstream consumers).
			tui.SetTerminalProgress(tui.ProgressIndeterminate)
		case agent.EventContextCompacted:
			fmt.Fprintf(os.Stderr, "[compact] %s done\n", ev.Info)
			tui.SetTerminalProgress(tui.ProgressClear)
		case agent.EventInfo:
			// Surface auto-compaction notices ("context compacted: M → N
			// messages"), loop-detector aborts, and similar non-error
			// status. Goes to stderr so piping stdout (the assistant's
			// reply) into another tool stays clean. The TUI handles
			// EventInfo separately; this branch is the metis-run /
			// metis-cron-run / non-interactive path.
			if ev.Info != "" {
				// Per-turn nudge / rescue counters for --metrics-log.
				// Pattern-match the same strings the loop emits — see
				// internal/agent/iter_nudge.go and empty_stop_rescue.go.
				if strings.Contains(ev.Info, "iter nudge") {
					turnNudges++
				}
				if strings.Contains(ev.Info, "rescue") {
					turnRescues++
				}
				// Stash for incomplete-detail capture (see EventLoopDone
				// branch below). The loop's abort paths emit a verbose
				// EventInfo IMMEDIATELY before EventLoopDone, so the
				// final value of lastInfoText at EventLoopDone time is
				// the abort's user-facing explanation.
				lastInfoText = ev.Info
				fmt.Fprintf(os.Stderr, "[info] %s\n", ev.Info)
			}
		case agent.EventTokens:
			totIn += ev.InputTokens
			totOut += ev.OutputTokens
			totCacheRead += ev.CacheReadInputTokens
			totCacheCreate += ev.CacheCreationInputTokens
			turnIn += ev.InputTokens
			turnOut += ev.OutputTokens
			turnCacheRead += ev.CacheReadInputTokens
			turnCacheCreate += ev.CacheCreationInputTokens
		case agent.EventTurnEnd:
			// Mid-loop turn boundary: flush the current turn's metrics
			// line (stop_reason=tool_use is implicit since EventTurnEnd
			// only fires after a tool-use round) then reset.
			emitMetrics("tool_use")
			resetTurn()
			// Schema mode validates the FINAL turn's text only — interim
			// "let me check..." prose from tool-use turns is not the
			// answer and would confuse JSON extraction.
			schemaTextBuf.Reset()
		case agent.EventLoopDone:
			if streamlined {
				flushAccum() // emit any trailing tool counts
			}
			fmt.Println()
			// Per-turn JSONL flush for --metrics-log. StopReason on the
			// final turn comes from the loop ("end_turn", "no_tool_calls",
			// "diminishing_returns", "halted_by_hook", etc.); falls back
			// to "done" when the provider didn't supply one.
			finalStop := ev.StopReason
			if finalStop == "" {
				finalStop = "done"
			}
			// Phase A — incomplete classification. These three abort
			// reasons mean the loop stopped before the model confirmed
			// the task was done; pre-fix the wrapper saw exit 0 here.
			// "halted_by_hook" is NOT in this list because hooks are
			// user/operator-installed and a halt is intentional, not
			// a failure signal. "plan_mode" is also a deliberate stop,
			// not an abort. Keep this switch explicit (not a default
			// branch) — silently flagging unknown reasons as incomplete
			// would block legitimate provider-side stop reasons we
			// haven't enumerated yet.
			switch finalStop {
			case "diminishing_returns", "max_iterations", "loop_detected", "stuck_after_reset", "turn_wall_clock", "budget_usd":
				incompleteReason = finalStop
				incompleteDetail = lastInfoText
			}
			emitMetrics(finalStop)
			// Token + duration metrics on stderr — picked up by the
			// eval runner (internal/eval/runner.go::scrapeMetrics) and
			// useful for scripts that want to log spend per call. One
			// line, key=value, parseable without JSON.
			// cache_hit reports the share of total INPUT tokens served from
			// the provider's prompt cache. Total input = tokens.in (the
			// non-cache portion the model actually re-encoded) + cache_read
			// + cache_create. Without this derived field, the raw line
			// invites the trap of computing `cache_read / tokens.in` and
			// getting >100% — the numerator and denominator don't share
			// a base in Anthropic's usage semantics (`input_tokens` already
			// excludes the cached portion). See 2026-05-15 user report.
			totalInput := totIn + totCacheRead + totCacheCreate
			hitPct := 0.0
			if totalInput > 0 {
				hitPct = float64(totCacheRead) / float64(totalInput) * 100
			}
			// 2026-05-23: added stop_reason so users can tell at a glance
			// WHY the loop ended — previously they had to read the JSONL
			// metrics-log to distinguish "done" / "max_iterations" /
			// "diminishing_returns" / "stuck_after_reset" / "loop_detected"
			// / "halted_by_hook" / "plan_mode" / provider-supplied stops.
			// Tail of the [metrics] line so existing parsers that ignore
			// trailing fields still work.
			fmt.Fprintf(os.Stderr,
				"[metrics] tokens.in=%d tokens.out=%d tokens.cache_read=%d tokens.cache_create=%d cache_hit=%.1f%% duration_ms=%d stop_reason=%s\n",
				totIn, totOut, totCacheRead, totCacheCreate, hitPct, time.Since(runStart).Milliseconds(), finalStop)

			// 2026-05-24: persist the assistant tail to the session
			// JSONL. Pre-fix, cmdRun only wrote the user prompt
			// (line 1502) — the assistant message + every tool_use +
			// tool_result that the loop produced lived only in
			// memory, gone the moment the process exited. Resume of
			// a `metis run` session saw only the user prompt and
			// nothing else (user test 2026-05-24, session
			// 8b6ddd95). The TUI's persistTail (tui_update.go:738)
			// already handles this correctly for chat mode; this
			// applies the same pattern to one-shot run mode so
			// session files stay symmetric across modes.
			//
			// Walk back through History from the end to find the
			// last user message; append every block after it. Empty
			// no-op when there's nothing to flush (loop hadn't
			// recorded the prompt yet — shouldn't happen post-
			// LoopDone but safer to be defensive).
			if rt.store != nil && rt.sessionID != "" {
				hist := rt.loop.History()
				for i := len(hist) - 1; i >= 0; i-- {
					if hist[i].Role == llm.RoleUser && len(hist[i].Content) > 0 && hist[i].Content[0].Type == "text" {
						for j := i + 1; j < len(hist); j++ {
							_ = rt.store.AppendMessage(rt.sessionID, hist[j])
						}
						break
					}
				}
			}
		case agent.EventError:
			return ev.Err
		}
	}
	err = <-done

	// --output-schema: validate the final reply; invalid output buys
	// the model up to MaxSchemaRetries correction turns carrying the
	// exact validation error, then fails with exit 11 so wrappers see
	// parseable-or-failed semantics. Only the validated JSON reaches
	// stdout (text deltas were suppressed above).
	if schemaEnforcer != nil && err == nil {
		finalText := schemaTextBuf.String()
		validated, verr := schemaEnforcer.Validate(finalText)
		for attempt := 1; verr != nil && attempt <= rtpkg.MaxSchemaRetries; attempt++ {
			fmt.Fprintf(os.Stderr, "[schema] output invalid (%v) — correction %d/%d\n", verr, attempt, rtpkg.MaxSchemaRetries)
			rt.loop.AppendUser(schemaEnforcer.RetryMessage(verr))
			finalText, err = rtpkg.RunLoopCollectText(ctx, rt.loop)
			if err != nil {
				return err
			}
			validated, verr = schemaEnforcer.Validate(finalText)
		}
		if verr != nil {
			fmt.Println(finalText) // last candidate — caller may still salvage it
			fmt.Fprintf(os.Stderr, "\n[metis] TASK INCOMPLETE — reason: output_schema\n[metis] detail: %v\n", verr)
			return &exitcode.IncompleteError{Reason: "output_schema", Detail: verr.Error()}
		}
		fmt.Println(validated)
	}

	// CACHE-D: save the response IFF user opted in AND no tools ran.
	// Tool-using turns are excluded by design — replaying them from
	// cache would re-emit observations of a world that may have
	// changed since. Errors during save go to stderr but don't break
	// the run (cache is best-effort).
	if cacheKey != "" && err == nil && !usedToolsThisRun && cacheTextBuf.Len() > 0 {
		ttlSecs := int(cacheTTL.Seconds())
		entry := &rtpkg.RunCacheEntry{
			PromptHash: cacheKey,
			Model:      rt.model,
			Prompt:     prompt,
			Response:   cacheTextBuf.String(),
			TTLSeconds: ttlSecs,
			UsedTools:  false,
		}
		if saveErr := rtpkg.SaveRunCache(entry); saveErr != nil {
			fmt.Fprintf(os.Stderr, "[cache] save warning: %v\n", saveErr)
		}
	} else if cacheKey != "" && usedToolsThisRun {
		// Surface why we didn't cache — saves the user from wondering
		// "I asked for --cache, why no cache file?"
		fmt.Fprintf(os.Stderr, "[cache] not cached (turn used tools — replay would lie about world state)\n")
	}

	// Wait for any in-flight auto-memory forks (extractMemories) to
	// finish before exiting — without this, `metis run` returns the
	// instant EventLoopDone fires, but the LoopEnd hook spawned a
	// goroutine that would die mid-Complete. 120s cap covers the long
	// tail: Phase B added SkillSynth + 4-phase prompt, pushing dream
	// duration from ~10s to 30-60s on slow providers; the prior 45s
	// cap was truncating mid-fork and leaving stale PID bodies in the
	// dream lock (which then blocked future dreams via the time gate).
	if flags.autoMemory || os.Getenv("METIS_AUTO_MEMORY") == "1" {
		waitForkInflight(120 * time.Second)
	}
	// Phase A — surface incomplete aborts to wrapper scripts via the
	// process exit code. Only overrides the success path: if a real
	// error already fired (err != nil from the goroutine), keep that
	// one — it carries more diagnostic info than the bare incomplete
	// reason. Stderr lines use a stable "[metis] TASK INCOMPLETE"
	// marker so CI greps can match without parsing the detail.
	if err == nil && incompleteReason != "" {
		fmt.Fprintf(os.Stderr, "\n[metis] TASK INCOMPLETE — reason: %s\n", incompleteReason)
		if incompleteDetail != "" {
			fmt.Fprintf(os.Stderr, "[metis] detail: %s\n", incompleteDetail)
		}
		return &exitcode.IncompleteError{Reason: incompleteReason, Detail: incompleteDetail}
	}
	return err
}

// waitForkInflight blocks until agent.ForkInflight() returns 0 or the
// timeout elapses. Polled with a tight cadence — extractor forks are
// I/O-bound (LLM round-trip), so we don't burn CPU here.
func waitForkInflight(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if agent.ForkInflight() == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// acpAuthRequiredProvider keeps the ACP control plane available before the
// user configures credentials. Initialize remains a credential-free
// capability handshake; the first prompt still fails with the exact auth
// error setupRuntime produced, so starting ACP never weakens model access.
type acpAuthRequiredProvider struct {
	name  string
	model string
	err   error
}

func (p *acpAuthRequiredProvider) Name() string { return p.name }
func (p *acpAuthRequiredProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, p.err
}
func (p *acpAuthRequiredProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, p.err
}
func (p *acpAuthRequiredProvider) MaxContextTokens() int { return 200_000 }
func (p *acpAuthRequiredProvider) ModelID() string       { return p.model }

func prepareACPLoop(ctx context.Context, flags *cliFlags) (*agent.Loop, func(), error) {
	rt, err := setupRuntime(ctx, flags)
	if err == nil {
		return rt.loop, rt.Cleanup, nil
	}
	if !errors.Is(err, config.ErrMissingAPIKey) {
		return nil, nil, err
	}
	providerName := flags.provider
	if providerName == "" {
		providerName = "unconfigured"
	}
	provider := &acpAuthRequiredProvider{name: providerName, model: flags.model, err: err}
	gate := permission.New(permission.ModeDeny)
	loop := agent.NewLoop(provider, tools.NewRegistry(), gate, nil, "", 1)
	return loop, func() {}, nil
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
	// ACP is a protocol service, not an interactive auth surface. It must be
	// ready to answer initialize even when the client has not configured a key.
	flags.noAuthWizard = true
	loop, cleanup, err := prepareACPLoop(ctx, flags)
	if err != nil {
		return err
	}
	defer cleanup()
	// Tell the ACP layer what version to advertise in InitializeResult
	// so clients see the same version as `metis --version`.
	acp.SetServerVersion(version.Version)
	srv := acp.NewServer(loop, addr)
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
		// Default form mirrors `claude --version` — short semver only.
		// Full git-describe fingerprint stays available via `-V`. Without
		// this, `metis version` printed `v0.1.3-21-gab7a825-dirty` while
		// the bottom status bar correctly showed `current: v0.1.3` —
		// inconsistency caught by cmp_drive.sh's ui__version_string.
		short := "v" + version.Short()
		if hasCommit {
			fmt.Printf("%s (Metis · %s)\n", short, version.Commit)
		} else {
			fmt.Printf("%s (Metis)\n", short)
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
		return errors.New("usage: metis cron <list|add|rm|pause|resume|run|start|audit|allow|denied>")
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
		opts, err := parseCronListOptions(rest)
		if err != nil {
			return err
		}
		return writeCronList(os.Stdout, svc.List(), opts)
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
	case "audit":
		return cmdCronAudit(svc, rest)
	case "allow":
		return cmdCronAllow(svc, cronRoot, rest)
	case "denied":
		return cmdCronDenied(cronRoot, rest)
	}
	return fmt.Errorf("cron: unknown subcommand %q", sub)
}

// cmdCronAudit lists or prints transcripts for a silent cron job's
// fires. Two forms:
//
//	metis cron audit <id>            → list every fire (newest first)
//	metis cron audit <id> latest     → print the latest fire's transcript
//	metis cron audit <id> <file>     → print one specific fire by name
//
// `latest` is the common interactive case ("what did my silent
// health-check find on its last run?"); the explicit filename form is
// for scripts that want to walk a particular range.
func cmdCronAudit(svc *agent.CronService, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis cron audit <id> [latest|<filename>]")
	}
	jobID := args[0]
	dir, ok := svc.AuditPath(jobID)
	if !ok {
		return fmt.Errorf("cron audit: no audit log for %q (job may not be silent, or it hasn't fired yet)", jobID)
	}
	names, err := svc.ListAuditFires(jobID)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("(no fires recorded yet)")
		return nil
	}
	// Plain list mode.
	if len(args) == 1 {
		fmt.Printf("Audit fires for %s (newest first):\n", jobID)
		for _, n := range names {
			fmt.Printf("  %s\n", n)
		}
		fmt.Printf("\nView one: metis cron audit %s latest    OR    metis cron audit %s <filename>\n", jobID, jobID)
		return nil
	}
	target := args[1]
	if target == "latest" {
		target = names[0]
	}
	path := filepath.Join(dir, target)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	fmt.Print(string(data))
	return nil
}

// cmdCronDenied shows the tool calls an unattended job tried to make but
// wasn't pre-authorized for. This is the "review before you approve"
// surface: each line carries a ready-to-paste rule so the user can run
// `cron allow <id> <rule>` to authorize it for next time.
func cmdCronDenied(cronRoot string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis cron denied <id>")
	}
	jobID := args[0]
	denials, err := agent.ListCronDenials(cronRoot, jobID)
	if err != nil {
		return err
	}
	if len(denials) == 0 {
		fmt.Printf("(no blocked tool calls for %s — nothing to approve)\n", jobID)
		return nil
	}
	// Collapse duplicate suggestions: a job firing every minute blocks the
	// same call repeatedly, but the user only needs to approve the rule once.
	seen := map[string]bool{}
	fmt.Printf("Blocked tool calls for %s (not pre-authorized):\n\n", jobID)
	for _, d := range denials {
		if seen[d.Suggest] {
			continue
		}
		seen[d.Suggest] = true
		fmt.Printf("  %s  %s\n", d.Tool, d.Input)
		if strings.HasPrefix(d.Reason, "dangerous_pattern:") {
			fmt.Printf("    ⚠ dangerous (%s) — denied even if allow-listed; cannot be approved\n",
				strings.TrimPrefix(d.Reason, "dangerous_pattern:"))
			continue
		}
		fmt.Printf("    approve: metis cron allow %s '%s'\n", jobID, d.Suggest)
	}
	return nil
}

// cmdCronAllow appends a pre-authorization rule to a job's allow-list and
// clears its denied log (the user has acted on it). Subsequent fires run
// any tool call matching the rule without prompting.
func cmdCronAllow(svc *agent.CronService, cronRoot string, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: metis cron allow <id> <rule>   e.g. metis cron allow <id> 'Bash(git pull:*)'")
	}
	jobID := args[0]
	rule := strings.TrimSpace(args[1])
	if rule == "" {
		return errors.New("cron allow: empty rule")
	}
	if _, ok := svc.Get(jobID); !ok {
		return fmt.Errorf("cron allow: job not found: %s", jobID)
	}
	if err := svc.Update(jobID, func(j *agent.CronJob) {
		for _, existing := range j.AllowTools {
			if existing == rule {
				return // already authorized — no-op
			}
		}
		j.AllowTools = append(j.AllowTools, rule)
	}); err != nil {
		return err
	}
	// Best-effort: clear the denied log now that the user has acted. A
	// failure here is cosmetic (stale entries reappear once re-denied).
	_ = agent.ClearCronDenials(cronRoot, jobID)
	fmt.Printf("authorized %q for cron job %s\n", rule, jobID)
	return nil
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
	var allow stringList
	fs.Var(&allow, "allow", "pre-authorize a tool for unattended fires, repeatable. `Tool(content)` form: --allow 'Bash(git pull:*)' --allow Write. Unlisted tool calls are denied + recorded for `cron denied`.")
	var silent bool
	fs.BoolVar(&silent, "silent", false, "fire without printing to chat — transcripts land in ~/.metis/cron/audit/<id>/ for `cron audit <id>` to inspect")
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
		Silent:      silent,
	}
	if disabled != "" {
		for _, t := range strings.Split(disabled, ",") {
			if t = strings.TrimSpace(t); t != "" {
				job.DisabledTools = append(job.DisabledTools, t)
			}
		}
	}
	job.AllowTools = append(job.AllowTools, allow...)
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
	if len(job.AllowTools) > 0 {
		fmt.Printf("  pre-authorized: %s\n", strings.Join(job.AllowTools, ", "))
	} else {
		fmt.Printf("  no tools pre-authorized — write/exec/network calls will be blocked.\n")
		fmt.Printf("  authorize with: --allow 'Bash(<cmd>:*)' / --allow Write, or `metis cron denied %s` after a fire.\n", job.ID)
	}
	return nil
}

func cmdCronRun(ctx context.Context, svc *agent.CronService, args []string) error {
	if len(args) == 0 {
		return errors.New("cron run: missing id")
	}
	job, err := advanceManualCronRun(svc, args[0])
	if err != nil {
		return fmt.Errorf("cron run %s: %w", args[0], err)
	}
	// Force ModeAsk regardless of cfg.Permission.Mode — see cmdCronStart
	// for the full rationale: a cron fire is governed by its allow-list,
	// not the operator's ambient mode, and only ModeAsk routes ask-tier
	// tool calls through executeCronJob's EvaluateCronPermission handler.
	rt, err := setupRuntime(ctx, &cliFlags{mode: string(permission.ModeAsk)})
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
	// Force ModeAsk regardless of cfg.Permission.Mode. A durable cron
	// fire's permission model is its pre-authorization allow-list
	// (enforced in executeCronJob's EventPermissionRequest handler via
	// EvaluateCronPermission), NOT the operator's ambient interactive
	// mode. If the daemon host's config is bypass/acceptEdits, the gate
	// would auto-allow state-changing tools WITHOUT ever emitting
	// EventPermissionRequest, so the allow-list is silently never
	// consulted and the job runs every (non-dangerous-pattern) tool the
	// model emits. ModeAsk routes every ask-tier call through the handler
	// so the allow-list — and its dangerous-pattern floor — always govern.
	rt, err := setupRuntime(ctx, &cliFlags{mode: string(permission.ModeAsk)})
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
		err := executeCronJob(ctx, rt, j, persistentHist, mainHist)
		return reportCronFireError(os.Stderr, j, err)
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

	// Silent-fire audit channel — opened once per fire when Silent is
	// set. All event handling below mirrors a copy to this writer; the
	// stdout/stderr renderers also branch on `silent` so the user's
	// chat surface doesn't fill with cron noise. Mirrors hermes
	// SILENT_MARKER but with a per-fire transcript instead of a single
	// rolling log.
	// cronRoot must match NewCronService's root (runtime.cronRoot ==
	// filepath.Join(cfg.Session.Dir, "cron")) so audit transcripts and the
	// denied store land where the `cron audit` / `cron denied` readers look.
	// The old derivation used filepath.Dir(SessionDir)/cron, one level too
	// high (~/.metis/cron vs ~/.metis/sessions/cron) — `cron audit` then
	// reported "no audit log" while the files sat in the wrong tree.
	sessionDir := rt.cfg.Session.Dir
	if sessionDir == "" {
		sessionDir = filepath.Join(os.Getenv("HOME"), ".metis", "sessions")
	}
	cronRoot := filepath.Join(sessionDir, "cron")

	var auditW *agent.AuditWriter
	if job.Silent {
		if w, err := agent.OpenAuditLog(cronRoot, job.ID); err == nil {
			auditW = w
			auditW.Append(agent.AuditEntry{Kind: "start", Text: job.Name})
			defer func() {
				_ = auditW.Close()
			}()
		} else {
			// Audit failure shouldn't block the fire — log to stderr
			// once and continue.
			fmt.Fprintf(os.Stderr, "[cron %s] audit open failed: %v (continuing without)\n", job.ID, err)
		}
	}

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

	// Counts tool calls this fire that were denied for lack of pre-
	// authorization, so we can nudge the user once at the end ("N blocked,
	// run `cron denied`") instead of staying silent like the old auto-deny.
	cronDeniedCount := 0

	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			if !job.Silent {
				fmt.Print(ev.TextDelta)
			}
			if auditW != nil {
				auditW.Append(agent.AuditEntry{Kind: "text", Text: ev.TextDelta})
			}
		case agent.EventToolStart:
			if !job.Silent {
				fmt.Fprintf(os.Stderr, "\n[cron %s] [tool] %s\n", job.ID, ev.ToolName)
			}
			if auditW != nil {
				auditW.Append(agent.AuditEntry{Kind: "tool_start", Tool: ev.ToolName})
			}
		case agent.EventToolResult:
			if auditW != nil && ev.ToolResult != nil {
				auditW.Append(agent.AuditEntry{
					Kind: "tool_result", Text: ev.ToolResult.Output, IsError: ev.ToolResult.IsError,
				})
			}
		case agent.EventCompactionStart:
			tier := ev.Info
			if tier == "" {
				tier = "compact"
			}
			if !job.Silent {
				fmt.Fprintf(os.Stderr, "[cron %s] [compact] starting %s\n", job.ID, tier)
			}
			if auditW != nil {
				auditW.Append(agent.AuditEntry{Kind: "info", Text: "compact start: " + tier})
			}
		case agent.EventContextCompacted:
			if !job.Silent {
				fmt.Fprintf(os.Stderr, "[cron %s] [compact] %s done\n", job.ID, ev.Info)
			}
			if auditW != nil {
				auditW.Append(agent.AuditEntry{Kind: "info", Text: "compact done: " + ev.Info})
			}
		case agent.EventInfo:
			if ev.Info == "" {
				continue
			}
			if !job.Silent {
				fmt.Fprintf(os.Stderr, "[cron %s] [info] %s\n", job.ID, ev.Info)
			}
			if auditW != nil {
				auditW.Append(agent.AuditEntry{Kind: "info", Text: ev.Info})
			}
		case agent.EventPermissionRequest:
			// Cron fires are unattended: there's no operator to answer a
			// mid-fire confirmation. The original code auto-DENIED every
			// ask-tier request, which silently broke any job whose prompt
			// needed a write/exec/network tool (observed 2026-06-15: a
			// Bash-write job fired on schedule but produced nothing, the
			// transcript showed "权限门拒绝"). Now the decision comes from the
			// job's pre-authorization allow-list (claude-code's background-
			// agent model): the user grants specific tools ahead of time via
			// `cron add --allow` / `cron allow`, those run; everything else is
			// denied and recorded so the user can review and extend the list.
			// Dangerous-pattern commands stay denied even if allow-listed.
			if allow, reason := agent.EvaluateCronPermission(job, ev.PermissionTool, ev.PermissionInput); allow {
				ev.PermissionReply <- agent.PermissionDecisionAllow
			} else {
				ev.PermissionReply <- agent.PermissionDecisionDeny
				cronDeniedCount++
				_ = agent.RecordCronDenial(cronRoot, job.ID, agent.CronDenial{
					Tool:    ev.PermissionTool,
					Input:   agent.FlattenToolInput(ev.PermissionInput),
					Reason:  reason,
					Suggest: agent.SuggestCronRule(ev.PermissionTool, ev.PermissionInput),
				})
				if auditW != nil {
					auditW.Append(agent.AuditEntry{
						Kind:    "denied",
						Tool:    ev.PermissionTool,
						Text:    reason,
						IsError: true,
					})
				}
			}
		case agent.EventAskUser:
			// Cron-run path: no operator on the other end. Drain with
			// empty answer so the tool surfaces a structured error and
			// the loop keeps moving instead of hanging on input.
			if ev.AskUserReply != nil {
				ev.AskUserReply <- ""
			}
		case agent.EventLoopDone:
			if !job.Silent {
				fmt.Println()
			}
			if auditW != nil {
				auditW.Append(agent.AuditEntry{Kind: "loop_done"})
			}
		case agent.EventError:
			if auditW != nil {
				auditW.Append(agent.AuditEntry{Kind: "error", Text: ev.Err.Error(), IsError: true})
			}
			return ev.Err
		}
	}
	runErr := <-done

	// Nudge the user once if this fire wanted tools it wasn't pre-authorized
	// for. Without this the denials are invisible until they go looking —
	// the same silent-failure trap the old blanket auto-deny created. Both
	// surfaces (stderr for an attached daemon, notify for backgrounded) so
	// the hint lands wherever the user is.
	if cronDeniedCount > 0 {
		msg := fmt.Sprintf("cron %q: %d tool call(s) blocked (not pre-authorized) — run `metis cron denied %s` to review/allow", job.Name, cronDeniedCount, job.ID)
		fmt.Fprintf(os.Stderr, "[cron %s] %s\n", job.ID, msg)
		notify.SendNotification("metis cron", msg)
	}

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
		jsonOutput := false
		for _, arg := range args[1:] {
			if arg == "--json" {
				jsonOutput = true
				continue
			}
			return fmt.Errorf("config show: unknown option %q", arg)
		}
		cfg, loaded, err := config.Load()
		if err != nil {
			return err
		}
		if jsonOutput {
			_, model := providerKeyAndModel(cfg, cfg.Provider.Default)
			return json.NewEncoder(os.Stdout).Encode(map[string]any{
				"provider":       cfg.Provider.Default,
				"model":          model,
				"permissionMode": cfg.Permission.Mode,
				"sessionDir":     cfg.Session.Dir,
				"files":          loaded,
			})
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
		fmt.Printf("loop_detection.signature_window = %d\n", cfg.LoopDetection.SignatureWindow)
		fmt.Printf("loop_detection.signature_max_repeats = %d\n", cfg.LoopDetection.SignatureMaxRepeats)
		fmt.Printf("agents.max_concurrent_subagents = %d (legacy combined; 0 = use split fields below)\n", cfg.Agents.MaxConcurrentSubAgents)
		fmt.Printf("agents.max_concurrent_named = %d\n", cfg.Agents.MaxConcurrentNamed)
		fmt.Printf("agents.max_concurrent_anon = %d\n", cfg.Agents.MaxConcurrentAnon)
		fmt.Printf("agents.max_agent_depth = %d\n", cfg.Agents.MaxAgentDepth)
		fmt.Printf("agents.max_fork_depth = %d\n", cfg.Agents.MaxForkDepth)
		// Show the effective resolved caps so users see what the
		// Roster actually got (env + legacy + split fields applied).
		if effNamed, effAnon := resolveRosterCaps(cfg); effNamed != cfg.Agents.MaxConcurrentNamed || effAnon != cfg.Agents.MaxConcurrentAnon {
			fmt.Printf("agents.effective_caps = named=%d anon=%d (after env override / legacy split)\n", effNamed, effAnon)
		}
		fmt.Printf("agents.default_timeout_seconds = %d\n", cfg.Agents.DefaultTimeoutSeconds)
		fmt.Printf("agents.cleanup_orphan_worktrees = %v\n", cfg.Agents.CleanupOrphanWorktrees)
		fmt.Printf("tools.enable_tool_search = %q (env)\n", os.Getenv("ENABLE_TOOL_SEARCH"))
		return nil
	case "init":
		return writeStarterConfig()
	case "schema":
		return cmdConfigSchema(args[1:])
	}
	return fmt.Errorf("config: unknown subcommand %q", args[0])
}

// cmdConfigSchema writes the JSON Schema for ~/.metis/config.toml so
// IDE language servers (VSCode `tamasfe.even-better-toml`, Zed's
// TOML LSP, JetBrains TOML plugin) can offer autocomplete and
// validation while the user edits config.toml.
//
// Usage:
//
//	metis config schema                # write to ~/.metis/schema/config.schema.json
//	metis config schema --stdout       # print to stdout
//	metis config schema --output PATH  # write to a custom path
//
// Mirrors crush's `crush config schema` subcommand. Schema is
// generated by reflecting on internal/config.Config (see
// internal/config/schema.go for the WeakMap-style cache).
func cmdConfigSchema(args []string) error {
	stdout := false
	outputPath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--stdout":
			stdout = true
		case "--output", "-o":
			if i+1 >= len(args) {
				return errors.New("config schema: --output requires a path")
			}
			outputPath = args[i+1]
			i++
		case "--help", "-h":
			fmt.Println("metis config schema — write JSON Schema for config.toml")
			fmt.Println()
			fmt.Println("Usage: metis config schema [flags]")
			fmt.Println()
			fmt.Println("Generates a JSON Schema (draft-07) describing every field of")
			fmt.Println("~/.metis/config.toml. Wire your IDE's TOML LSP to this file")
			fmt.Println("for autocomplete + validation while editing config.")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  --output PATH  Write to PATH instead of ~/.metis/schema/config.schema.json")
			fmt.Println("  --stdout       Print to stdout instead of writing a file")
			return nil
		default:
			return fmt.Errorf("config schema: unknown flag %q (try --help)", args[i])
		}
	}

	body, err := config.GenerateConfigSchema()
	if err != nil {
		return fmt.Errorf("config schema: %w", err)
	}
	if stdout {
		fmt.Println(string(body))
		return nil
	}
	if outputPath == "" {
		dir := filepath.Join(config.Home(), "schema")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("config schema: mkdir: %w", err)
		}
		outputPath = filepath.Join(dir, "config.schema.json")
	}
	if err := os.WriteFile(outputPath, body, 0o644); err != nil {
		return fmt.Errorf("config schema: write: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"config · schema written to %s (%d bytes)\n  Wire your IDE's TOML LSP to this path for autocomplete.\n",
		outputPath, len(body))
	return nil
}

func cmdTools(args []string) error {
	// Honor --tools / --disallow-tools on the listing path too so the
	// user can preview exactly what the model would see under those
	// filters without launching a full chat session.
	fs := flag.NewFlagSet("metis tools", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var allowFlag, denyFlag string
	fs.StringVar(&allowFlag, "tools", "", "allowlist (comma/space-separated) — same grammar as `metis chat --tools`")
	fs.StringVar(&denyFlag, "disallow-tools", "", "blocklist (supports MCP server prefix) — same grammar as `metis chat --disallow-tools`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	reg := buildListingRegistry(cfg, allowFlag, denyFlag)
	for _, t := range reg.All() {
		fmt.Printf("%-15s  %s\n", t.Name(), t.Description())
	}
	return nil
}

// buildListingRegistry assembles the full informational tool registry the
// user would see in a chat — every builtin + sub-agent + memory/cron/etc.
// tool wired against stubs — with the same visibility filter the chat REPL
// applies. Shared by `metis tools` (human listing) and `metis schema`
// (machine-readable contract) so both advertise the identical tool surface.
func buildListingRegistry(cfg *config.Config, allowFlag, denyFlag string) *tools.Registry {
	gate := permission.New("ask")
	reg := tools.NewRegistry()
	builtin.Register(reg, cfg, gate)
	reg.Register(builtin.NewAgent(gate, nil, reg, "", ""))
	reg.Register(builtin.NewFork(gate, nil, reg))
	tmpRoster := agent.NewRoster(0, 0)
	builtin.AttachSubAgentTools(reg, gate, tmpRoster)
	reg.Register(builtin.NewMessageTeammate(gate, tmpRoster))
	reg.Register(builtin.NewSendMessage(gate, channels.NewRegistry(), cfg.Channels.DefaultPlatform))
	if cronSvc, err := agent.NewCronService(filepath.Join(cfg.Session.Dir, "cron")); err == nil {
		reg.Register(builtin.NewScheduleWakeup(gate, cronSvc))
	}
	reg.Register(builtin.NewMemory(gate, rtpkg.BuildMemoryManager(cfg)))
	reg.Register(builtin.NewMetisInfo(gate, cfg, nil, nil, reg))
	reg.Register(builtin.NewEnterPlanModeWithGate(gate))
	reg.Register(builtin.NewExitPlanModeWithGate(gate))
	rtpkg.FilterRegistryInPlace(reg)

	// Mirror setupRuntime's visibility merge so a `--tools X --disallow-tools
	// Y` preview matches the exact pool a chat would expose.
	allowVis := tools.SplitCSV(allowFlag)
	if len(allowVis) == 0 {
		allowVis = cfg.Tools.Allowed
	}
	denyVis := append([]string(nil), cfg.Tools.Disallowed...)
	denyVis = append(denyVis, tools.SplitCSV(denyFlag)...)
	tools.ApplyToolVisibility(reg, allowVis, denyVis)
	return reg
}

// cmdSchema prints the machine-readable tool contract — every tool's name,
// description, and JSON-Schema input — as one JSON document. This is metis'
// analogue of Claude Code's generated sdk-tools.d.ts: the stable, typed
// surface a client SDK (any language) consumes so it and the agent never
// drift on tool shapes. Generated from the live tool registry, so it's
// always in sync with what `metis acp` / `metis chat` actually expose.
func cmdSchema(args []string) error {
	fs := flag.NewFlagSet("metis schema", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var allowFlag, denyFlag string
	fs.StringVar(&allowFlag, "tools", "", "allowlist (same grammar as `metis chat --tools`)")
	fs.StringVar(&denyFlag, "disallow-tools", "", "blocklist (same grammar as `metis chat --disallow-tools`)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	reg := buildListingRegistry(cfg, allowFlag, denyFlag)

	type toolContract struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	out := make([]toolContract, 0, len(reg.All()))
	for _, t := range reg.All() {
		out = append(out, toolContract{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"metis_version": version.Version,
		"protocol":      "acp",
		"tools":         out,
	})
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
		opts, err := parseSessionListOptions(args[1:])
		if err != nil {
			return err
		}
		es, err := listSessionEntries(store, opts)
		if err != nil {
			return err
		}
		if opts.jsonOutput {
			records := make([]sessionListRecord, 0, len(es))
			for _, e := range es {
				hdr, _, _ := store.LoadHeader(e.ID)
				record := sessionListRecord{
					ID: e.ID, Title: e.Title, Model: e.Model,
					CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
				}
				if hdr != nil {
					record.Provider = hdr.Provider
					record.WorkDir = hdr.WorkDir
				}
				records = append(records, record)
			}
			return json.NewEncoder(os.Stdout).Encode(records)
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
	case "branch":
		// `metis sessions branch <id> [--keep N]` — fork an existing
		// session at message N (default: copy entire history). Mirrors
		// claude-code's /branch but exposed as a CLI subcommand for
		// scripting. New session id printed to stdout.
		if len(args) < 2 {
			return errors.New("usage: metis sessions branch <id> [--keep N]")
		}
		srcID := args[1]
		keepN := 0 // 0 = full clone
		for i := 2; i < len(args); i++ {
			if args[i] == "--keep" && i+1 < len(args) {
				if n, ok := atoiSafe(args[i+1]); ok {
					keepN = n
				}
				i++
			}
		}
		_, msgs, err := store.Load(srcID)
		if err != nil {
			return fmt.Errorf("branch: load %s: %w", srcID, err)
		}
		clone := msgs
		if keepN > 0 && keepN < len(msgs) {
			clone = msgs[:keepN]
		}
		newID, err := store.Branch(srcID, clone)
		if err != nil {
			return err
		}
		fmt.Println(newID)
		return nil
	case "timing":
		if len(args) < 2 {
			return errors.New("usage: metis sessions timing <id>")
		}
		return printSessionTiming(store, args[1])
	}
	return fmt.Errorf("sessions: unknown subcommand %q", sub)
}

type sessionListRecord struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model"`
	WorkDir   string `json:"workDir,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// printSessionTiming renders a past session's per-step timing: a timeline
// of every tool call with its duration (slowest flagged) plus a /cost-style
// totals footer (wall clock, total tool time, per-tool breakdown). The data
// comes from the session's timing sidecar, written live by the Loop's
// TimingSink.
func printSessionTiming(store *session.Store, id string) error {
	steps, err := store.ReadTiming(id)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		fmt.Printf("no timing recorded for session %s\n", id)
		fmt.Println("(timing is captured for runs after this feature shipped; older sessions have none)")
		return nil
	}

	// Header (created_at) for wall-clock context, best-effort.
	var created time.Time
	if hdr, _, lerr := store.Load(id); lerr == nil && hdr != nil {
		created = hdr.CreatedAt
	}

	fmt.Printf("session %s — %d tool steps\n", id, len(steps))
	fmt.Println("────────────────────────────────────────────────────────")

	var total time.Duration
	perTool := map[string]time.Duration{}
	perToolN := map[string]int{}
	var slowest session.TimingStep
	for _, s := range steps {
		d := time.Duration(s.ElapsedMS) * time.Millisecond
		total += d
		perTool[s.Tool] += d
		perToolN[s.Tool]++
		if s.ElapsedMS > slowest.ElapsedMS {
			slowest = s
		}
	}
	for i, s := range steps {
		mark := "  "
		if s.Tool == slowest.Tool && s.ElapsedMS == slowest.ElapsedMS {
			mark = "▲ " // slowest step
		}
		errMark := ""
		if s.IsError {
			errMark = " [error]"
		}
		fmt.Printf("%s%3d. %-16s %8s%s\n", mark, i+1, s.Tool,
			formatDur(time.Duration(s.ElapsedMS)*time.Millisecond), errMark)
	}

	fmt.Println("────────────────────────────────────────────────────────")
	// Per-tool breakdown, sorted by total time descending.
	type row struct {
		tool string
		d    time.Duration
		n    int
	}
	rows := make([]row, 0, len(perTool))
	for t, d := range perTool {
		rows = append(rows, row{t, d, perToolN[t]})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].d > rows[b].d })
	fmt.Println("by tool (total time):")
	for _, r := range rows {
		fmt.Printf("  %-16s %8s  ×%d\n", r.tool, formatDur(r.d), r.n)
	}

	fmt.Println("────────────────────────────────────────────────────────")
	fmt.Printf("total tool time:  %s across %d calls\n", formatDur(total), len(steps))
	fmt.Printf("slowest step:     %s (%s)\n", slowest.Tool, formatDur(time.Duration(slowest.ElapsedMS)*time.Millisecond))
	if !created.IsZero() {
		fmt.Printf("session started:  %s\n", created.Format("2006-01-02 15:04:05"))
	}
	fmt.Println("(LLM round timing + token cost for the LIVE session: use /cost in chat)")
	return nil
}

// formatDur renders a duration compactly: 12ms / 1.4s / 2m3s.
func formatDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// atoiSafe parses an int without pulling strconv across the file's
// existing imports; returns (0, false) on any error.
func atoiSafe(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
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
		// `metis skills install [--optional] <name>` — when --optional
		// is set the manifest lands in ~/.metis/optional-skills/
		// instead of ~/.metis/skills/, marking it as TrustTrusted in
		// the loader (between bundled and user). Mirrors hermes'
		// `hermes skills install --source official`.
		optional := false
		ref := ""
		for _, a := range args[1:] {
			switch a {
			case "--optional", "-o":
				optional = true
			default:
				if ref == "" {
					ref = a
				}
			}
		}
		if ref == "" {
			return fmt.Errorf("usage: metis skills install [--optional] <name> | <owner>/<repo>:<name>")
		}
		dir := cfg.Session.SkillDir
		if optional {
			dir = filepath.Join(filepath.Dir(cfg.Session.SkillDir), "optional-skills")
		}
		return installSkillUnified(ref, dir)
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
	case "curator":
		// cfg.Session.SkillDir: the authoritative user-skills dir the
		// loader, installer, and live Skill tool all use (honors a custom
		// [session] skill_dir). The dream-cycle auto-sweep is wired to the
		// same dir (cmd/metis passes cfg.Session.SkillDir into the
		// extractor), so the manual CLI and the automatic path agree.
		return cmdSkillsCurator(args[1:], cfg.Session.SkillDir)
	}
	return fmt.Errorf("skills: unknown subcommand %q (use: list | install | info | uninstall | curator)", sub)
}

// cmdSkillsCurator drives the agent-skill curator from the CLI — the
// manual counterpart to the automatic sweep that runs at the end of each
// dream cycle. The curator only ever touches agent-created (flat .md) user
// skills; installed, bundled, project, and pinned skills are never
// archived, and archiving is always recoverable.
func cmdSkillsCurator(args []string, skillDir string) error {
	c := skills.NewCurator(skillDir)
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status":
		active, idle, cands, err := c.LifecycleStates(time.Now())
		if err != nil {
			return err
		}
		pins, err := c.Pins()
		if err != nil {
			return err
		}
		archived, err := c.ListArchived()
		if err != nil {
			return err
		}
		fmt.Printf("skill curator (idle after %d days, archive after %d days)\n",
			skills.CuratorIdleStateDaysDefault, skills.CuratorIdleDaysDefault)
		fmt.Printf("  active:             %d%s\n", len(active), formatNameList(active))
		fmt.Printf("  idle:               %d%s\n", len(idle), formatNameList(idle))
		fmt.Printf("  archive candidates: %d%s\n", len(cands), formatNameList(cands))
		fmt.Printf("  pinned:             %d%s\n", len(pins), formatNameList(pins))
		fmt.Printf("  archived:           %d%s\n", len(archived), formatNameList(archived))
		if len(cands) > 0 {
			fmt.Println("  run `metis skills curator run` to archive idle skills (recoverable).")
		}
		return nil
	case "run":
		res, err := c.Sweep(time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("curator: scanned %d, archived %d, kept-fresh %d, pinned %d, failed %d\n",
			res.Scanned, len(res.Archived), res.Skipped, res.Pinned, res.Failed)
		for _, n := range res.Archived {
			fmt.Println("  archived:", n)
		}
		return nil
	case "list-archived":
		archived, err := c.ListArchived()
		if err != nil {
			return err
		}
		if len(archived) == 0 {
			fmt.Println("no archived skills")
			return nil
		}
		for _, n := range archived {
			fmt.Println(n)
		}
		return nil
	case "restore":
		if len(args) < 2 {
			return fmt.Errorf("usage: metis skills curator restore <name>")
		}
		if err := c.Restore(args[1]); err != nil {
			return err
		}
		fmt.Println("restored:", args[1])
		return nil
	case "pin":
		if len(args) < 2 {
			return fmt.Errorf("usage: metis skills curator pin <name>")
		}
		if err := c.Pin(args[1]); err != nil {
			return err
		}
		fmt.Println("pinned:", args[1])
		return nil
	case "unpin":
		if len(args) < 2 {
			return fmt.Errorf("usage: metis skills curator unpin <name>")
		}
		if err := c.Unpin(args[1]); err != nil {
			return err
		}
		fmt.Println("unpinned:", args[1])
		return nil
	}
	return fmt.Errorf("skills curator: unknown subcommand %q (use: status | run | list-archived | restore <name> | pin <name> | unpin <name>)", sub)
}

// formatNameList renders a " (a, b, c)" suffix for a count line, or "" when
// the list is empty.
func formatNameList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " (" + strings.Join(names, ", ") + ")"
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
	// /dream — DreamTask (auto-memory) status (G.5, 2026-05-12).
	// Reads the live extractor's phase + last-run stats. Off-mode
	// when --auto-memory wasn't set: returns a hint about enabling
	// it instead of failing silently.
	r.Register(slash.Cmd{Name: "dream", Description: "DreamTask status — current phase, last run files & duration", Handler: func(arg string) (string, slash.Signal) {
		return formatDreamStatus(rt.autoMemExtractor, arg), slash.SignalNone
	}})
	// /agents — multi-agent roster surface (G.11 + G.17, 2026-05-12).
	// Replaces the placeholder cmd registered in RegisterAll above
	// (Registry.Register's last-write-wins on the index map). Reads
	// the live Roster on rt, supports `list / kill / status`
	// subcommands.
	r.Register(slash.Cmd{Name: "agents", Description: "list / kill / status sub-agents (Agent tool teammates)", Handler: func(arg string) (string, slash.Signal) {
		return formatAgentsCommand(rt.subAgentRoster, arg), slash.SignalNone
	}})
	// /teammate — direct user → named teammate channel (2026-05-16).
	// claude-code 架构图 image 5: "你也可以直接与单个团队成员交互,
	// 不必事事都通过主 Agent 进行沟通". metis pre-fix had no way for
	// the user to talk to a specific teammate — every input went to
	// the main agent. This command pushes a PeerMessage with From="user"
	// directly into the named teammate's Mailbox so it sees the request
	// on its next turn boundary as a <peer_message> reminder.
	r.Register(slash.Cmd{
		Name:        "teammate",
		Description: "send a message directly to a named teammate: /teammate <name> <message>",
		Handler: func(arg string) (string, slash.Signal) {
			return runTeammateMessage(rt.subAgentRoster, arg), slash.SignalNone
		},
	})
	// User-authored commands from ~/.metis/commands/*.md and
	// <cwd>/.metis/commands/*.md. Loaded LAST so a user .md can't
	// shadow a built-in (LoadCustomCommands refuses to register on
	// name collision). Errors are silent — a missing dir is the
	// common case. The returned name list is logged for
	// observability (`metis doctor` surfaces it).
	_ = slash.LoadCustomCommands(r, config.Home())

	// MCP prompts (Phase D #40). Each launched server is asked for its
	// prompts/list; capable servers contribute `mcp__<server>__<prompt>`
	// slash commands here. Done after LoadCustomCommands so a user
	// .md command sharing the same name still wins (Registry.Register
	// updates index map, last-write-wins). Probe failures are silent.
	if rt != nil {
		// Snapshot under the mutex — the background launcher may still be
		// appending. Captures whatever's online at this moment; servers
		// still launching just miss this round's prompt registration.
		rt.mcpServersMu.Lock()
		snapshot := append([]*mcptools.Server(nil), rt.mcpServers...)
		rt.mcpServersMu.Unlock()
		if len(snapshot) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			handles := mcp.CollectPrompts(ctx, snapshot)
			cancel()
			_ = registerMCPPromptsAsSlash(r, handles)
		}
	}
	return r
}

// formatAgentsCommand renders the /agents slash output (G.11 + G.17,
// 2026-05-12). Supports:
//
//	/agents              → equivalent to /agents list
//	/agents list         → roster snapshot grouped by named vs anon
//	/agents list all     → include anonymous teammates too
//	/agents status <name|id>  → detail for one teammate
//	/agents kill <name|id>    → terminate one teammate
//
// `resume <id>` is reserved for the Agent tool's resume_from path —
// at slash-command time we don't have provider/registry handles to
// spawn a fresh sub-loop, so /agents resume returns a hint pointing
// the user to `Agent({ resume_from: "agt-..." })` in chat. Future
// G.x can wire the full inline spawn.
// runTeammateMessage parses `/teammate <name> <body>` and pushes a
// PeerMessage to the recipient's Mailbox. Returns user-facing status
// (success or failure reason) for the slash output. Mirrors the
// MessageTeammate tool's logic but addressed from the user (not
// another sub-agent), so the From field is "user" and the recipient
// renders the message as a <peer_message from="user"> reminder on its
// next turn — the model can then choose to reply via MessageTeammate
// or just act.
func runTeammateMessage(roster *agent.Roster, arg string) string {
	if roster == nil {
		return "no roster wired (running outside a chat session?)"
	}
	parts := strings.SplitN(strings.TrimSpace(arg), " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "usage: /teammate <name> <message>\nexample: /teammate alice can you check the auth flow once you finish the schema work?"
	}
	to, body := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	t, ok := roster.Lookup(to)
	if !ok {
		t, ok = roster.LookupByAgentID(to)
	}
	if !ok {
		return fmt.Sprintf("no teammate with name=%s — use /agents list to see active named teammates", to)
	}
	if t.Mailbox == nil {
		return fmt.Sprintf("%q is an anonymous sub-agent and cannot receive peer messages (Sub-Agent paradigm: isolation only)", t.Name)
	}
	select {
	case t.Mailbox <- agent.PeerMessage{From: "user", Body: body, Sent: time.Now()}:
		return fmt.Sprintf("message delivered to %s (agent_id=%s) — it will see this on its next turn boundary", t.Name, t.AgentID)
	default:
		return fmt.Sprintf("teammate mailbox full (%s has 16 messages queued) — wait for its next turn and retry", t.Name)
	}
}

func formatAgentsCommand(roster *agent.Roster, arg string) string {
	if roster == nil {
		return "(agents: no Roster wired — sub-agent registry unavailable in this build)"
	}
	parts := strings.Fields(arg)
	sub := ""
	if len(parts) > 0 {
		sub = strings.ToLower(parts[0])
	}
	switch sub {
	case "", "list", "ls":
		showAll := len(parts) > 1 && strings.EqualFold(parts[1], "all")
		return renderAgentsList(roster, showAll)
	case "status", "show":
		if len(parts) < 2 {
			return "(agents status: usage: /agents status <name|agent_id>)"
		}
		return renderAgentsStatus(roster, parts[1])
	case "kill", "stop":
		if len(parts) < 2 {
			return "(agents kill: usage: /agents kill <name|agent_id>)"
		}
		return renderAgentsKill(roster, parts[1])
	case "resume":
		if len(parts) < 2 {
			return "(agents resume: usage: /agents resume <agent_id>)"
		}
		return fmt.Sprintf(
			"(agents resume: type into chat — `Agent({ resume_from: %q, prompt: \"continue\" })`. The resume_from path needs a live provider, which the slash handler doesn't hold.)",
			parts[1],
		)
	default:
		return fmt.Sprintf("(agents: unknown subcommand %q — try: list | status <id> | kill <id> | resume <id>)", sub)
	}
}

// renderAgentsList shows the roster table. `showAll` includes the
// anonymous spawns; default hides them (claude-code parity — anons
// clutter the view, named teammates are the user-relevant rows).
func renderAgentsList(roster *agent.Roster, showAll bool) string {
	snaps := roster.List()
	if len(snaps) == 0 {
		return "(agents: roster empty — spawn one with Agent({prompt: ...}))"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "agents (in flight + recent) — %d total\n", len(snaps))
	hiddenAnon := 0
	for _, s := range snaps {
		if !s.IsNamed() && !showAll {
			hiddenAnon++
			continue
		}
		tag := s.Name
		if !s.IsNamed() {
			tag = "(anon)"
		}
		bg := ""
		if s.Background {
			bg = " bg"
		}
		fmt.Fprintf(&b, "  %s  %-20s  %-9s  %s%s\n",
			s.AgentID, tag, s.Status, time.Since(s.Started).Truncate(time.Second), bg)
	}
	if hiddenAnon > 0 && !showAll {
		fmt.Fprintf(&b, "  ... %d anonymous teammate(s) hidden — use `/agents list all` to show\n", hiddenAnon)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderAgentsStatus shows one teammate's full state. `id` matches
// by name first, then by AgentID — same resolution rule
// MessageTeammate uses, so the user can paste either handle.
func renderAgentsStatus(roster *agent.Roster, id string) string {
	tm, ok := roster.Lookup(id)
	if !ok {
		tm, ok = roster.LookupByAgentID(id)
	}
	if !ok {
		return fmt.Sprintf("(agents status: no teammate with name=%q or agent_id=%q)", id, id)
	}
	s := tm.Snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "agent_id     : %s\n", s.AgentID)
	fmt.Fprintf(&b, "name         : %s (named=%t)\n", s.Name, s.IsNamed())
	fmt.Fprintf(&b, "status       : %s\n", s.Status)
	fmt.Fprintf(&b, "background   : %t\n", s.Background)
	fmt.Fprintf(&b, "started      : %s ago\n", time.Since(s.Started).Truncate(time.Second))
	if !s.EndTime.IsZero() {
		fmt.Fprintf(&b, "ended        : %s ago (ran %s)\n",
			time.Since(s.EndTime).Truncate(time.Second),
			s.EndTime.Sub(s.Started).Truncate(time.Second),
		)
	}
	if s.StopHint != "" {
		fmt.Fprintf(&b, "stop_hint    : %s\n", s.StopHint)
	}
	if s.ExitErr != nil {
		fmt.Fprintf(&b, "exit_err     : %v\n", s.ExitErr)
	}
	if s.Output != "" {
		preview := s.Output
		if len(preview) > 600 {
			preview = preview[:600] + "..."
		}
		fmt.Fprintf(&b, "output (head):\n%s\n", preview)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderAgentsKill resolves the target and calls its Cancel func.
// Returns a user-readable confirmation; the actual sub-loop cleanup
// happens in the goroutine watching ctx.Done().
func renderAgentsKill(roster *agent.Roster, id string) string {
	tm, ok := roster.Lookup(id)
	if !ok {
		tm, ok = roster.LookupByAgentID(id)
	}
	if !ok {
		return fmt.Sprintf("(agents kill: no teammate with name=%q or agent_id=%q)", id, id)
	}
	if tm.Cancel == nil {
		return fmt.Sprintf("(agents kill: teammate %s has no cancel func — already finished)", tm.AgentID)
	}
	tm.Cancel()
	return fmt.Sprintf("(agents: cancelled %s — will transition to killed shortly)", tm.AgentID)
}

// formatDreamStatus renders /dream output (G.5, 2026-05-12). When
// the extractor is nil, returns a hint about enabling auto-memory.
// `arg` is the slash command's trailing text — only "status" (or
// empty) is supported today; other tokens get a usage hint so
// future subcommands have an obvious extension point.
func formatDreamStatus(ext *agent.AutoMemoryExtractor, arg string) string {
	sub := strings.TrimSpace(strings.ToLower(arg))
	if sub != "" && sub != "status" {
		return "(dream: unknown subcommand — usage: /dream [status])"
	}
	if ext == nil {
		return "(dream: auto-memory not enabled — run metis with --auto-memory or set METIS_AUTO_MEMORY=1)"
	}
	st := ext.Stats()
	var b strings.Builder
	fmt.Fprintf(&b, "DreamTask status\n")
	fmt.Fprintf(&b, "  phase            : %s\n", st.Phase)
	fmt.Fprintf(&b, "  total runs       : %d\n", st.TotalExtractions)
	fmt.Fprintf(&b, "  in flight        : %t\n", st.InProgress)
	fmt.Fprintf(&b, "  trailing pending : %t\n", st.Pending)
	if !st.LastFiredAt.IsZero() {
		fmt.Fprintf(&b, "  last fired       : %s ago\n", time.Since(st.LastFiredAt).Truncate(time.Second))
	}
	if st.LastDuration > 0 {
		fmt.Fprintf(&b, "  last duration    : %s\n", st.LastDuration.Truncate(time.Millisecond))
	}
	if len(st.LastFilesTouched) > 0 {
		fmt.Fprintf(&b, "  last files       : %s\n", strings.Join(st.LastFilesTouched, ", "))
	} else if st.TotalExtractions > 0 {
		fmt.Fprintf(&b, "  last files       : (none — scan complete, nothing new)\n")
	}
	fmt.Fprintf(&b, "  memdir           : %s\n", ext.MemdirRoot())
	return strings.TrimRight(b.String(), "\n")
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

// printPromptDump writes the assembled system prompt to stdout with
// per-section markers + a footer summary. Used by --prompt-dump for
// token accounting and prompt regressions. Each section header includes
// (cache=Y/N volatile=Y/N) so the user can see what's cacheable across
// turns vs what re-renders.
func printPromptDump(sections []rtpkg.SystemPromptSection, rendered string) {
	if len(sections) == 0 {
		fmt.Println("# Assembled system prompt (no typed sections — legacy string path)")
		fmt.Println()
		fmt.Println(rendered)
		fmt.Printf("\n--- summary ---\nrendered: %d chars\n", len(rendered))
		return
	}
	fmt.Println("# Assembled system prompt")
	fmt.Println()
	var totalChars, cacheableChars int
	for i, s := range sections {
		cacheTag := "no-cache"
		if s.Cache {
			cacheTag = "cache"
		}
		volTag := ""
		if s.Volatile {
			volTag = " volatile"
		}
		fmt.Printf("=== %d. %s (%s%s · %d chars) ===\n", i+1, s.Name, cacheTag, volTag, len(s.Body))
		fmt.Println(s.Body)
		fmt.Println()
		totalChars += len(s.Body)
		if s.Cache {
			cacheableChars += len(s.Body)
		}
	}
	fmt.Println("--- summary ---")
	fmt.Printf("sections:  %d\n", len(sections))
	fmt.Printf("chars:     %d total, %d cacheable (%d non-cacheable)\n",
		totalChars, cacheableChars, totalChars-cacheableChars)
	fmt.Printf("rendered:  %d chars (joined)\n", len(rendered))
	fmt.Printf("est tokens: ~%d (rough chars/4)\n", len(rendered)/4)
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
