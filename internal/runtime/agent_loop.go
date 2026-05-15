package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// AgentLoopOptions bundles the inputs BuildAgentLoop needs. The struct
// shape lets future refactors add fields (e.g. a custom hook registry,
// a pre-built compactor) without breaking call sites.
type AgentLoopOptions struct {
	Provider llm.Provider
	Registry *tools.Registry
	Gate     *permission.Gate
	System   string
	// SystemSections is the typed-section form of System. When
	// non-empty, BuildAgentLoop attaches it to the Loop so per-iteration
	// requests can carry per-section cache_control hints to the
	// Anthropic provider. Memory becomes its own Volatile section at
	// request-build time (see Loop.buildRequest), so memory updates
	// don't invalidate the addendum cache. nil → legacy boundary-marker
	// path on the System string.
	SystemSections []SystemPromptSection
	Model          string
	MaxIter        int
	// MemoryManager is optional. When nil, BuildAgentLoop constructs
	// one at the default location (`<sessionDir>/memory`) — keeps
	// existing call-sites working. main.go now constructs it earlier
	// so the same instance can also be threaded into BuildToolRegistry,
	// letting the Memory tool write through the same store the Loop
	// reads via BuildContext (fixes the 2026-04-30 disconnect bug).
	MemoryManager *memory.MemoryManager

	// Jobs is the background-bash pool shared between Bash auto-bg
	// promotion and the Loop.injectJobNotifications drainer. nil
	// disables the entire job-pool feature on this Loop (sub-agents,
	// headless tests). Same instance flows into BuildToolRegistry so
	// BashList / BashOutput / BashKill see the same jobs the Loop's
	// notification path drains.
	Jobs *jobs.Registry

	// Monitors is the per-line pattern-match registry shared with the
	// Monitor tool. nil disables Monitor on this Loop. When non-nil,
	// Loop.injectMonitorEvents drains it at every iteration boundary
	// and pushes <monitor_event> system-reminder messages.
	Monitors *agent.MonitorRegistry
}

// BuildMemoryManager constructs a MemoryManager. Returns nil on error
// so callers don't have to thread errors through the bootstrap (memory
// failure is non-fatal — chat works without persistent recall).
//
// Scope resolution (2026-05-15):
//
//  1. If `./.metis/memory/` exists in the process cwd, that directory
//     is the root. This is "project-scoped" memory — running metis
//     inside a project keeps its notes local instead of polluting the
//     user-global store. Mirrors the existing precedent for project
//     agent profiles (./.metis/agents/ wins over ~/.metis/agents/).
//  2. Otherwise fall back to `<cfg.Session.Dir>/memory`, the
//     user-global root used historically.
//
// Opt-in by design: users have to `mkdir -p .metis/memory` themselves
// to switch a project to local memory. The detection is presence-only,
// not "walk up looking for a .metis/ marker" — keeps the rule simple
// and predictable (no surprise inheritance from a parent directory).
//
// Idempotent: calling twice with the same cfg returns two managers
// pointing at the same on-disk store, both operating on the same
// files via their internal mutexes.
func BuildMemoryManager(cfg *config.Config) *memory.MemoryManager {
	memRoot := resolveMemoryRoot(cfg)
	mm, err := memory.NewMemoryManager(memRoot)
	if err != nil {
		return nil
	}
	return mm
}

// resolveMemoryRoot picks the memory root path per the scope rules
// documented on BuildMemoryManager. Exported only via that wrapper;
// callers shouldn't need to know which root won.
//
// Logs the chosen root to stderr when METIS_DEBUG=1 so users can
// verify cwd-scoped memory took effect without grepping the file
// system.
func resolveMemoryRoot(cfg *config.Config) string {
	const projectDir = ".metis/memory"
	if st, err := os.Stat(projectDir); err == nil && st.IsDir() {
		abs, err := filepath.Abs(projectDir)
		if err == nil {
			if os.Getenv("METIS_DEBUG") == "1" {
				fmt.Fprintf(os.Stderr, "metis: memory root = %s (project-scoped)\n", abs)
			}
			return abs
		}
	}
	root := filepath.Join(cfg.Session.Dir, "memory")
	if os.Getenv("METIS_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "metis: memory root = %s (user-global)\n", root)
	}
	return root
}

// BuildAgentLoop constructs the agent.Loop with memory, compactor, and
// loop-detector wired up according to cfg. Only knobs the user actually
// configured override defaults — caller doesn't need to know which fields
// have sensible defaults vs which require explicit values.
//
// Memory is best-effort: if the memory dir can't be created we silently
// skip it (the loop runs fine without persistent recall, and noisy
// startup errors here would be confusing).
func BuildAgentLoop(cfg *config.Config, opts AgentLoopOptions) *agent.Loop {
	maxIter := opts.MaxIter
	if maxIter <= 0 {
		maxIter = cfg.Session.MaxIterations
	}

	hookReg := agent.NewHookRegistry()
	// User-defined lifecycle hooks from config.toml [hooks.*]. Registers
	// shell-command hooks for PreToolUse / PostToolUse / SessionStart /
	// SessionEnd. Errors are logged, never fatal.
	LoadConfigHooks(hookReg, &cfg.Hooks)

	loop := agent.NewLoop(opts.Provider, opts.Registry, opts.Gate,
		hookReg, opts.System, maxIter)
	loop.Model = opts.Model
	if len(opts.SystemSections) > 0 {
		loop.SystemSections = toLLMSections(opts.SystemSections)
	}
	// "plan" permission mode and Loop.PlanMode are two separate flags
	// today — gate gates tools to read-only, Loop.PlanMode makes the
	// loop emit EventPlan (collect tool calls) instead of executing.
	// Bind them here so `--mode plan` triggers both: the user's mental
	// model is "I asked for plan mode, I want to see the plan, don't
	// run anything."
	if opts.Gate != nil && string(opts.Gate.Mode()) == string(permission.ModePlan) {
		loop.PlanMode = true
	}

	// Memory manager — persistent recall across sessions. Caller can
	// pass a pre-built one (so the same instance gets handed to the
	// Memory tool) or leave it nil to fall back to the default store.
	if opts.MemoryManager != nil {
		loop.Memory = opts.MemoryManager
	} else if mm := BuildMemoryManager(cfg); mm != nil {
		loop.Memory = mm
	}

	// Per-turn auto retrieval (METIS_AUTO_RETRIEVE=K). Off by default;
	// users opt in via env. Bound the value: invalid / negative / >50
	// silently clamp to a safe range so a typo doesn't pull 1000
	// passages into every system prompt.
	if v := os.Getenv("METIS_AUTO_RETRIEVE"); v != "" {
		if k, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && k > 0 {
			if k > 50 {
				k = 50
			}
			loop.AutoRetrieveK = k
			if os.Getenv("METIS_DEBUG") == "1" {
				fmt.Fprintf(os.Stderr, "metis: auto-retrieve enabled (top-%d archival passages per turn)\n", k)
			}
		}
	}


	// Lazy MCP tool schemas (ToolSearch). Mode is read from the
	// ENABLE_TOOL_SEARCH env var inside agent/dispatch.go on every
	// call — we just need to feed Loop.ContextWindow so the auto
	// mode has a budget to compare against. 0 here means the
	// provider didn't publish a window; auto mode silently no-ops
	// in that case.
	loop.ContextWindow = opts.Provider.MaxContextTokens()

	// Wire jobs.Registry → Loop.JobNotify so the Run loop drains
	// completed bash jobs and injects <job_notification> messages.
	// Loop.Jobs gets the same pointer so the TUI status bar can
	// render a "⚙ N jobs" chip without crossing the runtime layer.
	if opts.Jobs != nil {
		loop.JobNotify = opts.Jobs.Notify()
		loop.Jobs = opts.Jobs
	}
	// Monitor pattern-watch registry — same lifecycle as Jobs (one per
	// process, drained at iteration boundaries). Wired here so a
	// caller that opted out of Jobs but somehow set Monitors gets a
	// silent no-op rather than a confusing dangling registry.
	if opts.Monitors != nil && opts.Jobs != nil {
		loop.Monitors = opts.Monitors
	}

	// Auto-compaction. Threshold from cfg, fallback to package default.
	compactCfg := agent.DefaultCompactionConfig()
	if cfg.Session.AutoCompactThreshold > 0 {
		compactCfg.Threshold = cfg.Session.AutoCompactThreshold
	}
	loop.Compactor = agent.NewCompactor(compactCfg, opts.Model,
		opts.Provider.MaxContextTokens(), opts.Provider)
	// max_tokens is per-provider; ShouldCompact subtracts this from the
	// effective input cap so compaction fires before
	// `input + max_tokens > context_window` triggers a 4xx server-side.
	// Without this, max_tokens=64k + context_window=192k threshold=0.85
	// only triggers at 163k input — but the API rejects at 128k.
	loop.Compactor.MaxOutputTokens = providerMaxTokens(cfg)

	// Tier the compactor's per-block thresholds to the active provider's
	// effective input cap. openclaude's `compressToolHistory.ts` insight:
	// a 16k window can't afford the same "keep 800 chars per old
	// tool_result" budget that a 200k window happily absorbs. metis's
	// default config used to apply the 800-char cap regardless of
	// window, which made small-window OpenAI-compat providers
	// (DeepSeek-V2 16k, Ollama defaults) thrash on long sessions.
	// Apply the tier AFTER MaxOutputTokens is set so the effective cap
	// is correct.
	loop.Compactor.ApplyWindowTier(opts.Provider.MaxContextTokens() - providerMaxTokens(cfg))

	// Microcompact cache dir (2026-05-15 wire-up). The Compactor.Microcompact
	// path was implemented + tested but never reachable in production
	// because MicrocompactDir defaulted to "" (which ShouldMicrocompact
	// uses as the kill switch). Now: default-on, lands under
	// <session.dir>/microcompact-cache. env METIS_MICROCOMPACT=0 disables
	// it (useful if the user is on a network-mount Session.Dir where
	// per-iteration disk writes are expensive).
	//
	// Why default-on: the path is lossless from the model's POV
	// (cached content recoverable via Read on the stub-printed path),
	// and only fires for genuinely large tool_results past the snip
	// threshold. The cost is small — a few KB writes per turn at most.
	if os.Getenv("METIS_MICROCOMPACT") != "0" && cfg.Session.Dir != "" {
		loop.Compactor.MicrocompactDir = filepath.Join(cfg.Session.Dir, "microcompact-cache")
		if os.Getenv("METIS_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "metis: microcompact cache dir = %s\n", loop.Compactor.MicrocompactDir)
		}
	}

	// Loop detector — wired by default (post-2026-05-08). The earlier
	// opt-in design left the user's only safety net at MaxIters=50,
	// which a runaway turn could plausibly reach in well under an hour
	// while looking like normal work; the live video showed 1h 18m of
	// repeated `cd … && git rebase --continue` retries with no halt.
	// Now the detector is always on with crush-parity signature
	// thresholds (10-step window, ≥5 same-signature steps trip).
	// `[loop_detection].disabled = true` in config.toml turns it off
	// for users who explicitly want unbounded loops.
	if !cfg.LoopDetection.Disabled {
		det := agent.NewLoopDetector()
		if cfg.LoopDetection.Warning > 0 {
			det.WarningThreshold = cfg.LoopDetection.Warning
		}
		if cfg.LoopDetection.Critical > 0 {
			det.CriticalThreshold = cfg.LoopDetection.Critical
		}
		if cfg.LoopDetection.Global > 0 {
			det.GlobalThreshold = cfg.LoopDetection.Global
		}
		if cfg.LoopDetection.SignatureWindow > 0 {
			det.SignatureWindowSize = cfg.LoopDetection.SignatureWindow
		}
		if cfg.LoopDetection.SignatureMaxRepeats > 0 {
			det.SignatureMaxRepeats = cfg.LoopDetection.SignatureMaxRepeats
		}
		loop.Detector = det
	}

	return loop
}

// providerMaxTokens returns the configured per-request output budget for
// whichever provider is currently active. Returns 0 when nothing is set
// (Compactor falls back to using the full context window for the
// threshold calculation in that case).
//
// We can't ask the Provider interface directly — `MaxTokens()` was never
// exposed, partly because per-call overrides (Loop.Fast halves it on
// the wire) make a fixed accessor misleading. Reading from cfg here is
// the next-best thing: same source the Provider constructor used at
// toLLMSections converts the runtime-side SystemPromptSection slice
// to the llm package's wire-shaped SystemSection slice. The two types
// are intentionally separate — runtime knows about scanning/loading
// (project_context paths, addendum file), llm knows about wire-format
// (cache_control, anthropic block shape) — so they don't import each
// other. This shim is the boundary translator.
func toLLMSections(in []SystemPromptSection) []llm.SystemSection {
	out := make([]llm.SystemSection, 0, len(in))
	for _, s := range in {
		out = append(out, llm.SystemSection{
			Name: s.Name, Body: s.Body, Cache: s.Cache, Volatile: s.Volatile,
		})
	}
	return out
}

// boot time.
func providerMaxTokens(cfg *config.Config) int {
	switch cfg.Provider.Default {
	case "anthropic":
		return cfg.Provider.Anthropic.MaxTokens
	case "openai":
		return cfg.Provider.OpenAI.MaxTokens
	case "gemini", "google":
		return cfg.Provider.Gemini.MaxTokens
	}
	return 0
}
