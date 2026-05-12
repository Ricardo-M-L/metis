package runtime

import (
	"os"
	"path/filepath"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// ToolRegistryOptions bundles inputs BuildToolRegistry needs from the
// surrounding bootstrap. All fields are required — there's no fallback
// because the agent loop is unusable without a Provider / Gate / etc.
//
// CronService is optional: when nil, the ScheduleWakeup tool is not
// registered. metis run / metis cron CLI flows don't need it; the chat
// REPL does.
//
// PluginSources is optional: when set, the LLM-facing Skill tool's
// loader includes the plugin layer so plugin-contributed skills show up
// in `Skill list` / `Skill invoke`. Pass nil to skip the plugin layer
// (bundled + user + project still register).
//
// MemoryManager is required when the Memory tool should be exposed.
// Pass nil and the Memory tool falls back to a stub that errors clearly
// — useful for `metis tools` listing where we just want to advertise
// the capability without running it. The chat REPL always sets it.
type ToolRegistryOptions struct {
	Cfg             *config.Config
	Gate            *permission.Gate
	Provider        llm.Provider
	Model           string
	System          string
	MinimalSystem   string // optional; sub-agents use this to skip parent's project_context + addendum
	ChannelRegistry *channels.Registry
	DefaultPlatform string
	CronService     *agent.CronService
	PluginSources   []skills.PluginSkillSource
	MemoryManager   *memory.MemoryManager

	// Jobs is the process-wide background-bash pool shared with
	// agent.Loop. When non-nil:
	//   - Bash auto-promotes long-running commands to the pool at 60s
	//   - new BashList / BashOutput / BashKill tools become callable
	//   - completed jobs publish notifications the agent loop drains
	//
	// nil disables the entire job-pool feature gracefully (Bash falls
	// back to its pre-2026-05-09 foreground-only behavior).
	Jobs *jobs.Registry

	// Monitors is the per-line pattern-match registry shared with
	// agent.Loop. When non-nil AND Jobs is non-nil, the Monitor tool
	// becomes callable. nil disables Monitor; the model still has
	// Bash run_in_background as the polling fallback.
	Monitors *agent.MonitorRegistry

	// ConfigSnapshot is the {path,mtime,size} fingerprint captured at
	// session bootstrap so MetisInfo's [config_staleness] section can
	// detect "the user edited config.toml since metis launched".
	// Optional: nil hides the section without breaking other
	// introspection sections.
	ConfigSnapshot *config.Snapshot

	// Roster is the cross-session sub-agent registry that backs the
	// G.0 concurrency cap and (later) G.3 named teammates / G.16 UI
	// observability. Threaded into Agent + Fork tools at registry
	// build time. When nil, Agent/Fork run uncapped and untracked —
	// fine for tests but not production.
	Roster *agent.Roster
}

// BuildToolRegistry constructs the per-session tools.Registry, registers
// every built-in tool, and wires up the two tools that need extra runtime
// references (Agent → provider+registry; SendMessage → channel registry).
//
// Extracted from cmd/metis/main.go so main.go stays composer-only.
// Future tool families (memory tools, observability tools) plug in here.
func BuildToolRegistry(opts ToolRegistryOptions) *tools.Registry {
	reg := tools.NewRegistry()
	builtin.Register(reg, opts.Cfg, opts.Gate)
	// Wire jobs.Registry into Bash + register the three job-pool
	// tools. Done as a post-step so builtin.Register can stay
	// dependency-free (jobs is a leaf package the runtime owns).
	if opts.Jobs != nil {
		builtin.AttachJobsRegistry(reg, opts.Jobs, opts.Gate)
		if opts.Monitors != nil {
			builtin.AttachMonitorRegistry(reg, opts.Jobs, opts.Monitors, opts.Gate, opts.Cfg.Tools.Bash)
		}
	}
	// Agent tool: needs the provider + registry references that
	// builtin.Register doesn't see. Roster + DefaultTimeout wire in
	// G.0's concurrency cap + wall-clock budget. JobsPool wires
	// G.1's run_in_background path through the bash job pool's
	// notification channel.
	agentTool := builtin.NewAgentWithMinimal(opts.Gate, opts.Provider, reg, opts.Model, opts.System, opts.MinimalSystem)
	if opts.Roster != nil {
		agentTool = agentTool.WithRoster(opts.Roster)
	}
	if opts.Cfg != nil && opts.Cfg.Agents.DefaultTimeoutSeconds > 0 {
		agentTool = agentTool.WithDefaultTimeout(time.Duration(opts.Cfg.Agents.DefaultTimeoutSeconds) * time.Second)
	}
	if opts.Jobs != nil {
		agentTool = agentTool.WithJobsPool(opts.Jobs)
	}
	reg.Register(agentTool)
	// SubAgent reader tools (G.1, 2026-05-12) — SubAgentList /
	// SubAgentOutput / SubAgentStop. Mirrors AttachJobsRegistry for
	// bash. Roster nil → graceful no-op (tools register but report
	// "no sub-agents" instead of panicking).
	if opts.Roster != nil {
		builtin.AttachSubAgentTools(reg, opts.Gate, opts.Roster)
	}
	// Fork tool: same wiring as Agent, different semantics — child
	// inherits parent history+system instead of starting cold.
	reg.Register(builtin.NewFork(opts.Gate, opts.Provider, reg))
	// SendMessage tool: lit only when at least one channel adapter is
	// configured. We always register it though — its description will
	// just say "no channels available" until one is wired.
	reg.Register(builtin.NewSendMessage(opts.Gate, opts.ChannelRegistry, opts.DefaultPlatform))
	// ScheduleWakeup: lets the LLM self-pace via one-shot cron. Only
	// makes sense when a CronService is alive (chat REPL); skipped for
	// `metis run` one-shots where there's no scheduler ticking.
	if opts.CronService != nil {
		reg.Register(builtin.NewScheduleWakeup(opts.Gate, opts.CronService))
	}
	// Skill tool: register the bundled+user+project layers up-front. If
	// the caller has plugins to add, they'll call RegisterSkillTool
	// again after LoadPlugins. We register early (with PluginSources
	// nil) so even calls that skip RegisterSkillTool still get the 22
	// bundled skills exposed to the LLM.
	loader := RegisterSkillTool(reg, opts)
	// Memory tool: writes flow through MemoryManager.Core() so they
	// land in the same store BuildContext reads. Without a manager
	// (e.g. `metis tools` informational listing), the tool registers
	// with nil and Execute returns a clear error so the capability
	// shows up in /tools but isn't usable.
	reg.Register(builtin.NewMemory(opts.Gate, opts.MemoryManager))
	// MetisInfo tool: LLM-facing introspection. Reads the same
	// references this function already has, so it sees changes that
	// happen later (e.g. /reload toggling an MCP server's Disabled
	// bit). Mirrors crush's crush_info — distinct from /doctor.
	reg.Register(builtin.NewMetisInfo(opts.Gate, opts.Cfg, opts.Jobs, loader, reg).WithSnapshot(opts.ConfigSnapshot))
	return reg
}

// RegisterSkillTool installs (or replaces) the LLM-facing Skill tool on
// reg with a fresh multi-source loader. Call once with PluginSources=nil
// (BuildToolRegistry already does this) and then again after LoadPlugins
// returns the plugin registry so plugin-contributed skills become
// visible. Two-phase wiring exists because plugin loading needs reg to
// already exist (MCP-server plugins register their tools into it),
// chicken-and-egg with making the Skill tool's loader plugin-aware.
//
// Layers wired (in priority order):
//
//	bundled (in-binary)        — TrustBuiltin
//	optional (~/.metis/optional-skills)  — TrustTrusted, official-but-not-default
//	user (~/.metis/skills)     — TrustUser
//	project (./.metis/skills)  — TrustProject
//	plugin (LoadPlugins)       — TrustCommunity
//
// The optional dir is always passed (sibling to userDir); the loader
// silently skips it when the directory is missing.
//
// Returns the constructed loader so callers (BuildToolRegistry) can
// hand it to other tools that need to read the live skill list
// (MetisInfo). The loader is hot — Invalidate-aware — so passing the
// same instance everywhere keeps the introspection view honest.
func RegisterSkillTool(reg *tools.Registry, opts ToolRegistryOptions) *skills.Loader {
	userDir := opts.Cfg.Session.SkillDir
	// Project layer is the first existing `.metis/skills/` walking up
	// from CWD. We resolve at construct time; the loader doesn't watch
	// for new projects mid-session (chat scope = single project).
	var projectDir string
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, ".metis", "skills")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			projectDir = candidate
		}
	}
	// Optional dir is sibling to userDir — by convention
	// ~/.metis/optional-skills. `metis skills install --optional <name>`
	// drops manifests here.
	optionalDir := filepath.Join(filepath.Dir(userDir), "optional-skills")
	loader := skills.NewLoaderWithOptional(userDir, projectDir, optionalDir, opts.PluginSources)
	// Replace, not Register — the second phase (after LoadPlugins) needs
	// to overwrite the first phase's plugin-less Skill tool without
	// panicking on duplicate registration.
	reg.Replace(builtin.NewSkill(opts.Gate, loader, userDir))
	return loader
}
