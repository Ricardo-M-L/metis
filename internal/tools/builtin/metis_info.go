package builtin

// metis_info.go — INI-style introspection tool the LLM calls when it
// suspects "wait, why is feature X not behaving as I expect?".
//
// Mirrors crush's crush_info / opencode's opencode_info: a single
// flat dump of config sources, registered providers, MCP states,
// installed skills, configured hooks, the current permission mode,
// and the live bg-jobs pool. Distinct from /doctor (which is a
// HUMAN-facing health check) — the LLM sees a structured INI it can
// parse and reason over without scraping the user's screen.
//
// Wiring: constructed by runtime/tools.go alongside Memory / Skill /
// Bash etc. Takes a snapshot of the references it needs at
// construction time; the tool's Execute then reads through them so
// late-mutated state (e.g. a /reload-toggled MCP server) shows up
// without a tool re-register.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// MetisInfo is the introspection tool. All five reference fields are
// optional — Execute renders sections only for the data it actually
// has. That way the bare `metis tools` listing path can register the
// capability without wiring every dependency, and the chat REPL gets
// the full report.
//
// Pointer-typed fields (Cfg / Gate / Pool / SkillsLoader / Reg) are
// READ-ONLY from this tool's perspective: Execute snapshots state
// per-call, never mutates.
type MetisInfo struct {
	gate   *permission.Gate
	cfg    *config.Config
	pool   *jobs.Registry
	skills *skills.Loader
	reg    *tools.Registry
	// snap holds the config.Snapshot taken at session bootstrap. nil
	// disables the [config_staleness] section gracefully (the older
	// 4-arg constructor still works). 2026-05-09 addition mirrors
	// crush's ConfigStore.ConfigStaleness().
	snap *config.Snapshot
}

// NewMetisInfo wires up the introspection tool. Pass nil for any
// reference you don't have — Execute renders that section as
// "(unavailable)" rather than crashing.
func NewMetisInfo(gate *permission.Gate, cfg *config.Config, pool *jobs.Registry, sk *skills.Loader, reg *tools.Registry) MetisInfo {
	return MetisInfo{gate: gate, cfg: cfg, pool: pool, skills: sk, reg: reg}
}

// WithSnapshot returns a copy of MetisInfo carrying the given config
// staleness snapshot. Builder-style so callers don't need to thread
// the snapshot through every constructor in the chain.
func (m MetisInfo) WithSnapshot(snap *config.Snapshot) MetisInfo {
	m.snap = snap
	return m
}

func (MetisInfo) Name() string { return "MetisInfo" }
func (MetisInfo) Description() string {
	return "Introspection: dump effective config, providers, MCP servers, skills, hooks, permission mode, registered tools, and live background jobs as INI sections. Call when debugging \"why isn't tool X / skill Y / MCP Z working?\"."
}

func (MetisInfo) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"section": map[string]any{
				"type":        "string",
				"description": "Optional section filter — one of: providers, mcp, skills, tools, hooks, permission, jobs, options. Omit to dump everything.",
			},
		},
	}
}

func (m MetisInfo) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (MetisInfo) IsReadOnly(map[string]any) bool                 { return true }
func (MetisInfo) IsDestructive(map[string]any) bool              { return false }

func (m MetisInfo) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "metis-info: read-only introspection, always allowed"
}

func (m MetisInfo) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	section, _ := in["section"].(string)
	section = strings.ToLower(strings.TrimSpace(section))

	var b strings.Builder
	want := func(name string) bool { return section == "" || section == name }

	if want("config") {
		m.writeConfigStaleness(&b)
	}
	if want("providers") {
		m.writeProviders(&b)
	}
	if want("mcp") {
		m.writeMCP(&b)
	}
	if want("skills") {
		m.writeSkills(&b)
	}
	if want("tools") {
		m.writeTools(&b)
	}
	if want("hooks") {
		m.writeHooks(&b)
	}
	if want("permission") {
		m.writePermission(&b)
	}
	if want("jobs") {
		m.writeJobs(&b)
	}
	if want("options") {
		m.writeOptions(&b)
	}

	if b.Len() == 0 {
		return &tools.Result{Output: "(no sections matched filter " + section + ")"}, nil
	}
	return &tools.Result{Output: b.String()}, nil
}

// writeConfigStaleness diffs the current filesystem state against the
// snapshot taken at session bootstrap. dirty=true here means the user
// (or an external tool) edited config.toml since metis launched —
// exposing it lets the LLM suggest `/reload` instead of dispensing
// stale advice.
func (m MetisInfo) writeConfigStaleness(b *strings.Builder) {
	if m.snap == nil {
		return
	}
	st := m.snap.Diff()
	fmt.Fprintln(b, "[config_staleness]")
	fmt.Fprintf(b, "dirty = %v\n", st.Dirty)
	if len(st.Changed) > 0 {
		fmt.Fprintf(b, "changed = %s\n", strings.Join(st.Changed, ", "))
	}
	if len(st.Missing) > 0 {
		fmt.Fprintf(b, "missing = %s\n", strings.Join(st.Missing, ", "))
	}
	if len(st.NewlyAppeared) > 0 {
		fmt.Fprintf(b, "newly_appeared = %s\n", strings.Join(st.NewlyAppeared, ", "))
	}
	if len(st.Errors) > 0 {
		paths := make([]string, 0, len(st.Errors))
		for p := range st.Errors {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		fmt.Fprintf(b, "errors = %s\n", strings.Join(paths, ", "))
	}
	fmt.Fprintln(b)
}

// writeProviders enumerates the configured provider blocks. The
// presence of an APIKey or APIKeyEnv pair flips a row into "configured"
// (we don't deref the env var to check it's set — that would let the
// LLM probe for credentials, which is more harm than help).
func (m MetisInfo) writeProviders(b *strings.Builder) {
	if m.cfg == nil {
		fmt.Fprintln(b, "[providers]")
		fmt.Fprintln(b, "(unavailable)")
		fmt.Fprintln(b)
		return
	}
	c := m.cfg
	type row struct{ name, status string }
	var rows []row
	add := func(name string, configured bool) {
		st := "missing"
		if configured {
			st = "configured"
		}
		rows = append(rows, row{name, st})
	}
	add("anthropic", c.Provider.Anthropic.APIKeyEnv != "" || c.Provider.Anthropic.APIKey != "" || c.Provider.Anthropic.BaseURL != "")
	add("openai", c.Provider.OpenAI.APIKeyEnv != "" || c.Provider.OpenAI.APIKey != "" || c.Provider.OpenAI.BaseURL != "")
	add("gemini", c.Provider.Gemini.APIKeyEnv != "" || c.Provider.Gemini.APIKey != "" || c.Provider.Gemini.BaseURL != "")
	for name := range c.Provider.Custom {
		rows = append(rows, row{name, "configured (custom)"})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	fmt.Fprintln(b, "[providers]")
	fmt.Fprintf(b, "default = %s\n", c.Provider.Default)
	for _, r := range rows {
		fmt.Fprintf(b, "%s = %s\n", r.name, r.status)
	}
	fmt.Fprintln(b)
}

func (m MetisInfo) writeMCP(b *strings.Builder) {
	if m.cfg == nil {
		return
	}
	if len(m.cfg.MCP.Servers) == 0 {
		return
	}
	fmt.Fprintln(b, "[mcp]")
	servers := append([]config.MCPServer(nil), m.cfg.MCP.Servers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	for _, s := range servers {
		state := "configured"
		if s.Disabled {
			state = "disabled"
		}
		fmt.Fprintf(b, "%s = %s (transport=%s)\n", s.Name, state, mcpTransportLabel(s))
	}
	fmt.Fprintln(b)
}

// mcpTransportLabel labels the MCP server's transport. metis currently
// only models stdio (Command + Args), but we keep this isolated so a
// future http/sse server config doesn't ripple through every caller.
func mcpTransportLabel(s config.MCPServer) string {
	if s.Command != "" {
		return "stdio"
	}
	return "unknown"
}

func (m MetisInfo) writeSkills(b *strings.Builder) {
	if m.skills == nil {
		return
	}
	all, err := m.skills.List()
	if err != nil || len(all) == 0 {
		return
	}
	type row struct {
		name, trust, state string
	}
	rows := make([]row, 0, len(all))
	for _, sk := range all {
		state := "active"
		if sk.Disabled {
			state = "disabled"
		}
		rows = append(rows, row{sk.Name, sk.TrustLevel, state})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	fmt.Fprintln(b, "[skills]")
	fmt.Fprintf(b, "count = %d\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(b, "%s = %s, %s\n", r.name, r.trust, r.state)
	}
	fmt.Fprintln(b)
}

func (m MetisInfo) writeTools(b *strings.Builder) {
	if m.reg == nil {
		return
	}
	all := m.reg.All()
	if len(all) == 0 {
		return
	}
	names := make([]string, 0, len(all))
	for _, t := range all {
		names = append(names, t.Name())
	}
	sort.Strings(names)
	fmt.Fprintln(b, "[tools]")
	fmt.Fprintf(b, "registered = %d\n", len(names))
	for _, n := range names {
		fmt.Fprintf(b, "tool = %s\n", n)
	}
	if m.cfg != nil && len(m.cfg.Tools.Disabled) > 0 {
		disabled := append([]string(nil), m.cfg.Tools.Disabled...)
		sort.Strings(disabled)
		fmt.Fprintf(b, "disabled = %s\n", strings.Join(disabled, ", "))
	}
	fmt.Fprintln(b)
}

func (m MetisInfo) writeHooks(b *strings.Builder) {
	if m.cfg == nil {
		return
	}
	type row struct {
		event, command, matcher string
	}
	var rows []row
	add := func(event string, hooks []config.HookSpec) {
		for _, h := range hooks {
			rows = append(rows, row{event, h.Command, h.If})
		}
	}
	add("pre_tool_use", m.cfg.Hooks.PreToolUse)
	add("post_tool_use", m.cfg.Hooks.PostToolUse)
	add("session_start", m.cfg.Hooks.SessionStart)
	add("session_end", m.cfg.Hooks.SessionEnd)
	add("user_prompt_submit", m.cfg.Hooks.UserPromptSubmit)
	add("notification", m.cfg.Hooks.Notification)
	add("permission_request", m.cfg.Hooks.PermissionRequest)
	add("permission_denied", m.cfg.Hooks.PermissionDenied)
	add("cwd_changed", m.cfg.Hooks.CwdChanged)
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].event != rows[j].event {
			return rows[i].event < rows[j].event
		}
		return rows[i].command < rows[j].command
	})
	fmt.Fprintln(b, "[hooks]")
	for _, r := range rows {
		if r.matcher != "" {
			fmt.Fprintf(b, "%s (if=%s) = %s\n", r.event, r.matcher, r.command)
		} else {
			fmt.Fprintf(b, "%s = %s\n", r.event, r.command)
		}
	}
	fmt.Fprintln(b)
}

func (m MetisInfo) writePermission(b *strings.Builder) {
	if m.gate == nil {
		return
	}
	fmt.Fprintln(b, "[permission]")
	fmt.Fprintf(b, "mode = %s\n", m.gate.Mode())
	if m.cfg != nil && len(m.cfg.Tools.Bash.Allowlist) > 0 {
		all := append([]string(nil), m.cfg.Tools.Bash.Allowlist...)
		sort.Strings(all)
		fmt.Fprintf(b, "bash_allowlist = %s\n", strings.Join(all, ", "))
	}
	fmt.Fprintln(b)
}

func (m MetisInfo) writeJobs(b *strings.Builder) {
	if m.pool == nil {
		return
	}
	all := m.pool.List()
	fmt.Fprintln(b, "[jobs]")
	fmt.Fprintf(b, "count = %d\n", len(all))
	running := 0
	for _, j := range all {
		if j.Status == jobs.StatusRunning {
			running++
		}
	}
	fmt.Fprintf(b, "running = %d\n", running)
	for _, j := range all {
		fmt.Fprintf(b, "%s = %s, %s\n", j.ID, j.Status, j.Description)
	}
	fmt.Fprintln(b)
}

func (m MetisInfo) writeOptions(b *strings.Builder) {
	if m.cfg == nil {
		return
	}
	fmt.Fprintln(b, "[options]")
	fmt.Fprintf(b, "ui_theme = %s\n", m.cfg.UI.Theme)
	fmt.Fprintf(b, "session_skill_dir = %s\n", m.cfg.Session.SkillDir)
	fmt.Fprintf(b, "default_provider = %s\n", m.cfg.Provider.Default)
	fmt.Fprintln(b)
}
