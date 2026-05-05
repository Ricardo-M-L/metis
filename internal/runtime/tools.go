package runtime

import (
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/config"
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
	// Agent tool: needs the provider + registry references that
	// builtin.Register doesn't see.
	reg.Register(builtin.NewAgentWithMinimal(opts.Gate, opts.Provider, reg, opts.Model, opts.System, opts.MinimalSystem))
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
	RegisterSkillTool(reg, opts)
	// Memory tool: writes flow through MemoryManager.Core() so they
	// land in the same store BuildContext reads. Without a manager
	// (e.g. `metis tools` informational listing), the tool registers
	// with nil and Execute returns a clear error so the capability
	// shows up in /tools but isn't usable.
	reg.Register(builtin.NewMemory(opts.Gate, opts.MemoryManager))
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
func RegisterSkillTool(reg *tools.Registry, opts ToolRegistryOptions) {
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
}
