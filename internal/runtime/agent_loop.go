package runtime

import (
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
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
	Model    string
	MaxIter  int
	// MemoryManager is optional. When nil, BuildAgentLoop constructs
	// one at the default location (`<sessionDir>/memory`) — keeps
	// existing call-sites working. main.go now constructs it earlier
	// so the same instance can also be threaded into BuildToolRegistry,
	// letting the Memory tool write through the same store the Loop
	// reads via BuildContext (fixes the 2026-04-30 disconnect bug).
	MemoryManager *memory.MemoryManager
}

// BuildMemoryManager constructs a MemoryManager rooted under the
// session dir. Returns nil on error so callers don't have to thread
// errors through the bootstrap (memory failure is non-fatal — chat
// works without persistent recall).
//
// Idempotent: calling twice with the same cfg returns two managers
// pointing at the same on-disk store, both operating on the same
// files via their internal mutexes. main.go uses one instance only;
// the second-use case is just `metis tools` listing where we want
// the Memory capability to advertise even without state.
func BuildMemoryManager(cfg *config.Config) *memory.MemoryManager {
	memRoot := filepath.Join(cfg.Session.Dir, "memory")
	mm, err := memory.NewMemoryManager(memRoot)
	if err != nil {
		return nil
	}
	return mm
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

	// Loop detector — only wire when explicitly enabled. Without a
	// detector the loop runs unbounded; the global iteration cap on
	// agent.Loop is the only safety net.
	if cfg.LoopDetection.Enabled {
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
