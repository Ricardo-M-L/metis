// Package config loads metis's TOML configuration with cascading precedence:
//  1. CLI flags (highest)
//  2. Project-local: $PWD/.metis/config.toml
//  3. User: $METIS_HOME/config.toml or ~/.metis/config.toml
//  4. Legacy: ~/.config/metis/config.toml (auto-migrated on first run)
//  5. Defaults (lowest)
//
// Inspired by Claude Code's settings precedence and Hermes' provider overlays.
// Layout under ~/.metis/ deliberately matches Claude Code's ~/.claude/ —
// one home directory holds config + sessions + skills + memory + cron + history,
// instead of the older XDG-style split between ~/.config and ~/.local/share.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

type Config struct {
	Provider      ProviderSet   `toml:"provider"`
	Permission    Permission    `toml:"permission"`
	UI            UI            `toml:"ui"`
	Session       Session       `toml:"session"`
	Tools         Tools         `toml:"tools"`
	LoopDetection LoopDetection `toml:"loop_detection"`
	Agents        Agents        `toml:"agents"`
	MCP           MCP           `toml:"mcp"`
	Channels      Channels      `toml:"channels"`
	Hooks         HooksConfig   `toml:"hooks"`
}

// Agents governs sub-agent (Agent / Fork tool) execution limits. Added
// 2026-05-12 alongside the multi-agent expansion (Phase G of the
// claude-code-parity plan). The fields are the cross-cutting safety
// net: concurrency cap, wall-clock timeout, and worktree GC policy.
// All three apply to every sub-agent invocation regardless of
// `run_in_background`, `isolation`, or `name`.
type Agents struct {
	// MaxConcurrentSubAgents is the LEGACY combined cap. Kept for
	// backward compat with existing config.toml files. When set and
	// the new MaxConcurrentNamed / MaxConcurrentAnon are zero, the
	// total is split 1/3 named + 2/3 anonymous (rounded). When the
	// new fields are explicitly set, this field is ignored. 0 →
	// fall back to the new defaults.
	MaxConcurrentSubAgents int `toml:"max_concurrent_subagents"`

	// MaxConcurrentNamed caps named teammates in the Roster ("organ
	// chart" agents — addressable by name via MessageTeammate). 0 →
	// default 20. The named pool is intentionally smaller than anon
	// because each named slot represents a persistent "role" that
	// stays in /agents list; the anon pool is the spawn-and-forget
	// research helper pool.
	MaxConcurrentNamed int `toml:"max_concurrent_named"`

	// MaxConcurrentAnon caps anonymous sub-agents (one-shot
	// `Agent({prompt})` invocations without a name). 0 → default 40.
	MaxConcurrentAnon int `toml:"max_concurrent_anon"`

	// MaxAgentDepth caps how deep Agent() spawns can nest. 0 →
	// default 1 (sub-agents may NOT spawn further sub-agents;
	// the main agent can spawn workers but they stay flat).
	//
	// 2026-05-16: lowered default 3 → 1 to align with claude-code's
	// architectural constraint ("子智能体不能再生成其他子智能体").
	// Users who want recursive decomposition (main → plan → explore
	// → 子探索) raise this explicitly.
	MaxAgentDepth int `toml:"max_agent_depth"`

	// MaxForkDepth caps how deep Fork() spawns can nest. Lower than
	// MaxAgentDepth because Fork carries the parent's conversation
	// history forward, so each level doubles context size AND
	// breaks prompt-cache reuse (level-2 fork's prefix differs from
	// the cached main prefix). 0 → default 1.
	//
	// 2026-05-15: lowered default 2 → 1 to match claude-code's
	// stricter rule (CC rejects Fork-in-Fork outright). Users who
	// genuinely need a fork tree set this to 2+ explicitly.
	MaxForkDepth int `toml:"max_fork_depth"`

	// DefaultTimeoutSeconds bounds a sub-agent's wall-clock duration
	// when its tool call did not pass `timeout_seconds`. 0 falls back
	// to the default (600 = 10 min).
	DefaultTimeoutSeconds int `toml:"default_timeout_seconds"`

	// CleanupOrphanWorktrees governs whether a worktree spawned via
	// `isolation:"worktree"` is force-removed when its parent context
	// is cancelled. Default true.
	CleanupOrphanWorktrees bool `toml:"cleanup_orphan_worktrees"`
}

// Channels groups the chat-platform adapters wired up by SendMessage.
// Each subsection only has *_env fields — the actual tokens / webhook URLs
// stay in environment variables to keep config.toml diffable.
type Channels struct {
	DefaultPlatform string          `toml:"default_platform"` // used when channel arg has no "<platform>:" prefix
	Slack           SlackChannel    `toml:"slack"`
	Telegram        TelegramChannel `toml:"telegram"`
	Discord         DiscordChannel  `toml:"discord"`
	Dingtalk        DingtalkChannel `toml:"dingtalk"`
	Feishu          FeishuChannel   `toml:"feishu"`
	Wechat          WechatChannel   `toml:"wechat"`
}

type SlackChannel struct {
	BotTokenEnv    string `toml:"bot_token_env"`
	DefaultChannel string `toml:"default_channel"`
}

type TelegramChannel struct {
	BotTokenEnv      string `toml:"bot_token_env"`
	DefaultChatIDEnv string `toml:"default_chat_id_env"`
}

type DiscordChannel struct {
	WebhookURLEnv string `toml:"webhook_url_env"`
}

type DingtalkChannel struct {
	WebhookURLEnv string `toml:"webhook_url_env"`
	SecretEnv     string `toml:"secret_env"`
}

type FeishuChannel struct {
	WebhookURLEnv string `toml:"webhook_url_env"`
	SecretEnv     string `toml:"secret_env"`
}

type WechatChannel struct {
	Backend       string `toml:"backend"` // "work" (default; 企业微信群机器人); "public" reserved
	WebhookURLEnv string `toml:"webhook_url_env"`
}

// HooksConfig — user-defined lifecycle hooks loaded from config.toml.
// Mirrors claude-code's settings.json `hooks` model: each lifecycle event
// gets a list of HookSpec, fired in order at the matching point. The
// first MVP supports `type = "command"` only — a shell command that
// receives the event JSON on stdin and (for PreToolUse) can return a
// modified payload on stdout to short-circuit / rewrite the tool call.
type HooksConfig struct {
	PreToolUse        []HookSpec `toml:"pre_tool_use"`
	PostToolUse       []HookSpec `toml:"post_tool_use"`
	SessionStart      []HookSpec `toml:"session_start"`
	SessionEnd        []HookSpec `toml:"session_end"`
	UserPromptSubmit  []HookSpec `toml:"user_prompt_submit"`
	Notification      []HookSpec `toml:"notification"`
	PermissionRequest []HookSpec `toml:"permission_request"`
	PermissionDenied  []HookSpec `toml:"permission_denied"`
	CwdChanged        []HookSpec `toml:"cwd_changed"`
	PreCompact        []HookSpec `toml:"pre_compact"`
}

// HookSpec is one entry in HooksConfig. Type defaults to "command".
// Command is the shell command (interpreted by the system shell). If
// matches the spec only fires when the event's tool name matches `If`
// (glob, e.g. `Bash` or `Bash(git *)`). Empty If = always.
type HookSpec struct {
	Type    string `toml:"type"` // "command"; future: "http" / "agent"
	Command string `toml:"command"`
	If      string `toml:"if"`              // optional matcher, e.g. "Bash" or "Bash(git *)"
	Timeout int    `toml:"timeout_seconds"` // 0 = 30s default
}

// MCP groups configuration for external Model Context Protocol servers.
type MCP struct {
	Servers []MCPServer `toml:"servers"`
}

// MCPServer is a single stdio MCP server metis launches as a subprocess.
// Tools exposed by the server are registered as `mcp__<name>__<tool>`.
type MCPServer struct {
	Name     string   `toml:"name"`
	Command  string   `toml:"command"`
	Args     []string `toml:"args"`
	Disabled bool     `toml:"disabled"` // omit/false = active
}

// LoopDetection configures the agent loop detector.
// Defaults are tuned for typical coding sessions; raise thresholds for
// long-running jobs. The detector is on by default (post-2026-05-08);
// set `disabled = true` in `[loop_detection]` to turn it off entirely.
//
// `Enabled` is retained as a no-op alias for backward compatibility —
// older configs that explicitly set `enabled = true` keep working
// without surprise behavior changes; setting it to false does NOT
// disable the detector (use `disabled = true` for that).
type LoopDetection struct {
	Enabled             bool `toml:"enabled"`               // legacy / no-op (always-on by default now)
	Disabled            bool `toml:"disabled"`              // set true to turn the detector off
	Warning             int  `toml:"warning"`               // per-tool consecutive call count
	Critical            int  `toml:"critical"`              // per-tool consecutive call count
	Global              int  `toml:"global"`                // total tool calls in a single Run before abort
	SignatureWindow     int  `toml:"signature_window"`      // sliding-window size in steps; default 10
	SignatureMaxRepeats int  `toml:"signature_max_repeats"` // same-signature count to trip; default 5
}

type ProviderSet struct {
	Default   string                 `toml:"default"`
	Anthropic ProviderAnthropic      `toml:"anthropic"`
	OpenAI    ProviderOpenAI         `toml:"openai"`
	Gemini    ProviderGemini         `toml:"gemini"`
	Custom    map[string]ProviderRaw `toml:"custom"`
}

// ProviderGemini configures the Google Generative Language API (v1beta).
// Endpoint is hard-coded but BaseURL is configurable for VPC-routed
// deployments. Auth uses the x-goog-api-key header; the legacy
// `?key=...` query param is NOT supported because our retries would
// log the key on a 4xx.
type ProviderGemini struct {
	APIKeyEnv string `toml:"api_key_env"`
	APIKey    string `toml:"api_key"`
	BaseURL   string `toml:"base_url"`
	Model     string `toml:"model"`
	MaxTokens int    `toml:"max_tokens"`
	// ContextWindow overrides the model-prefix lookup used for
	// auto-compaction. Set when the served model's name doesn't match
	// a known prefix or its window differs from the upstream default.
	ContextWindow int     `toml:"context_window"`
	TimeoutSecs   int     `toml:"timeout_seconds"`
	Temperature   float64 `toml:"temperature"`
}

type ProviderAnthropic struct {
	APIKeyEnv string `toml:"api_key_env"`
	APIKey    string `toml:"api_key"` // direct (discouraged; prefer env)
	BaseURL   string `toml:"base_url"`
	Model     string `toml:"model"`
	MaxTokens int    `toml:"max_tokens"`
	// ContextWindow overrides the model-prefix lookup used for
	// auto-compaction. Required for Anthropic-compatible third-party
	// gateways (MiniMax, OpenRouter, ...) where the served model
	// doesn't match `claude-*` and the real window may be smaller
	// than 200k.
	ContextWindow int     `toml:"context_window"`
	TimeoutSecs   int     `toml:"timeout_seconds"`
	AnthropicBeta string  `toml:"anthropic_beta"`
	Temperature   float64 `toml:"temperature"`
	// AntiDistillation sends the top-level opt-in field
	// `anti_distillation: ["fake_tools"]` on every request — exact
	// wire-format parity with claude-code's services/api/claude.ts:312.
	//
	// IMPORTANT — this flag is a SERVER-SIDE opt-in. The Anthropic
	// backend reads it and applies its own response-stream
	// countermeasures. The metis client does NOT inject anything
	// into the tools[] array (that would just give the model
	// fake tools to misuse, which is the opposite of what we want).
	//
	// Practical implication: only meaningful when base_url points at
	// the REAL Anthropic API (api.anthropic.com or an Anthropic-
	// operated alias). Third-party gateways (yunwu.ai, OpenRouter,
	// MiniMax, Together, Groq, ...) silently ignore the unknown
	// field — enabling this against them is a no-op. The runtime
	// emits a stderr warning at startup if the flag is set and
	// base_url isn't a recognized Anthropic origin, so the user
	// isn't quietly fooled into thinking it's protecting them.
	//
	// Default off. Preset for users who DO talk to real Anthropic
	// and want the wire-format opt-in available without code changes.
	AntiDistillation bool `toml:"anti_distillation"`

	// ClientSideDecoys is a separate, third-party-gateway-friendly
	// anti-distillation mechanism. When true, every request body
	// gets a non-standard top-level field (`_decoy_tools_v2_archive`)
	// containing several plausible-looking fake tool definitions.
	//
	// Why this differs from AntiDistillation:
	//   - AntiDistillation is the CC opt-in that asks the Anthropic
	//     SERVER to inject countermeasures. Useless against MiniMax /
	//     yunwu / OpenRouter etc. because their backends don't
	//     implement the server-side mechanism.
	//   - ClientSideDecoys is pure-client. Adversaries who record
	//     HTTP traffic to train a competing model see the decoys in
	//     the captured bytes; the model itself NEVER sees them
	//     because the field is at the wire level, not in tools[],
	//     system, or messages.
	//
	// Key correctness property: the decoys cannot affect model output
	// — that's the "non-standard field" trick. Any well-behaved API
	// (and every Anthropic-compat gateway tested so far) silently
	// ignores unknown top-level fields. A schema-strict gateway
	// could reject the request; in that rare case, turn this off.
	//
	// Default off. Independent of AntiDistillation; both can be on.
	ClientSideDecoys bool `toml:"client_side_decoys"`
}

type ProviderOpenAI struct {
	APIKeyEnv string `toml:"api_key_env"`
	APIKey    string `toml:"api_key"`
	BaseURL   string `toml:"base_url"`
	Model     string `toml:"model"`
	MaxTokens int    `toml:"max_tokens"`
	// ContextWindow overrides the model-prefix lookup. See ProviderAnthropic.
	ContextWindow int     `toml:"context_window"`
	TimeoutSecs   int     `toml:"timeout_seconds"`
	Temperature   float64 `toml:"temperature"`
}

type ProviderRaw struct {
	// Transport picks the wire format. Recognized values:
	//   anthropic_messages | openai_chat | gemini_native     (HTTP+API key)
	//   azure_openai      | vertex_anthropic | bedrock_anthropic  (cloud auth)
	Transport string `toml:"transport"`
	APIKeyEnv string `toml:"api_key_env"`
	// APIKey — inline credential, lowest-priority fallback (after
	// env + auth.json). Discouraged for shared / committed configs
	// since it puts the secret in plaintext in config.toml. Useful
	// for one-off / personal setups where rotating an env var or
	// running `metis auth login` is friction. Mirrors the inline
	// path the built-in providers (anthropic / openai / gemini) have
	// always had — added 2026-05-09 to close the gap that made
	// `[provider.custom.<id>] api_key = "..."` silently drop the
	// secret in TOML parsing.
	APIKey      string `toml:"api_key"`
	BaseURL     string `toml:"base_url"`
	Model       string `toml:"model"`
	MaxTokens   int    `toml:"max_tokens"`
	TimeoutSecs int    `toml:"timeout_seconds"`

	// ContextWindow override. Required for cloud-auth providers
	// (Azure / Vertex / Bedrock) where the metis side can't infer
	// from the model name (Azure uses deployment names; Bedrock model
	// ids look like "anthropic.claude-…-v1:0").
	ContextWindow int `toml:"context_window"`

	// Cloud-auth profile fields. Only consumed by the matching
	// transport — leaving any of these empty for non-cloud profiles
	// is fine.

	// Azure (azure_openai): the API version query string Azure
	// requires on every request (e.g. "2024-08-01-preview"). The
	// resource subdomain goes in BaseURL; the deployment name goes
	// in Model.
	APIVersion string `toml:"api_version"`

	// AWS Bedrock (bedrock_anthropic): the region and the
	// secret-half-credentials. Access key id flows through APIKeyEnv;
	// secret access key flows through SecretKeyEnv; optional STS
	// session token flows through SessionTokenEnv. Region is in
	// BaseURL because Bedrock has no customer-controllable URL.
	SecretKeyEnv    string `toml:"secret_key_env"`
	SessionTokenEnv string `toml:"session_token_env"`

	// GCP Vertex (vertex_anthropic): the path to the service-account
	// JSON key file and the GCP project + region. Region goes in
	// BaseURL; project is here.
	ServiceAccountFile string `toml:"service_account_file"`
	Project            string `toml:"project"`
	Region             string `toml:"region"`
}

type Permission struct {
	Mode  string `toml:"mode"`
	Allow []Rule `toml:"allow"`
	Deny  []Rule `toml:"deny"`
}

type Rule struct {
	Tool  string `toml:"tool"`
	Match string `toml:"match"`
}

type UI struct {
	Theme        string        `toml:"theme"`
	Markdown     bool          `toml:"markdown"`
	ShowTokens   bool          `toml:"show_tokens"`
	ShowToolJSON bool          `toml:"show_tool_json"`
	Performance  UIPerformance `toml:"performance"`
	// StreamlinedOutput enables the CC-style "distillation-resistant"
	// output mode for `metis run` (non-interactive). Mirrors
	// claude-code's utils/streamlinedTransform.ts behavior:
	//
	//   - thinking content omitted entirely
	//   - tool calls collapsed into cumulative summaries between text
	//     messages ("searched 3 patterns, read 2 files, ran 1 command")
	//   - per-tool detailed input/output suppressed
	//
	// The point: protect against someone batch-running metis to
	// harvest rich agent traces (full reasoning + tool args/outputs)
	// for training a competing model. Keeps the user's actual answer
	// (text content) intact so the output is still usable.
	//
	// Default off — most users want the full trace for debugging /
	// observability. Turn on for scripted / CI / batch jobs whose
	// output stream might be observed. Per-invocation override via
	// `--streamlined` CLI flag.
	StreamlinedOutput bool `toml:"streamlined_output"`

	// PermissionTimeoutSeconds is how long an interactive permission
	// prompt waits for the user to answer before defaulting to deny.
	// 0 (default) → 60 seconds, matching the historical hardcoded
	// constant. Set higher if you frequently AFK during agent runs
	// and don't want denied permission to abort a long task; set
	// lower for tighter security review windows.
	PermissionTimeoutSeconds int `toml:"permission_timeout_seconds"`

	// VoiceMaxRecordSeconds caps how long /voice will record before
	// auto-stopping and transcribing. 0 (default) → 30 seconds,
	// matching the historical hardcoded constant. Bump to 120+ if
	// you dictate longer instructions; lower for quick voice-memo
	// style usage.
	VoiceMaxRecordSeconds int `toml:"voice_max_record_seconds"`

	// StatusLineRefreshSeconds is how often the bottom status line
	// re-runs its script / refreshes its data. 0 (default) → 5
	// seconds. Lower for snappier git-branch / cron-chip updates;
	// higher if your status-line script is expensive (CI status
	// poll, etc).
	StatusLineRefreshSeconds int `toml:"status_line_refresh_seconds"`
}

// UIPerformance gathers the tunables that decide how snappy / how
// CPU-hungry the chat surface feels. Each field has a sensible default
// in defaults() so first-run users get a working tuning out of the
// box; power users can override per-key without re-stating the rest.
//
// Precedence (highest first):
//
//  1. METIS_TICK_MS / METIS_EVENT_BUFFER / METIS_MOUSE_WHEEL_LINES env
//     vars — for ad-hoc experimentation without editing the file.
//  2. Values set here in [ui.performance] in config.toml.
//  3. Built-in defaults from defaults() below.
//
// Env wins over config so a `METIS_TICK_MS=16 metis chat` invocation
// for one debugging session doesn't require a TOML edit + revert.
type UIPerformance struct {
	// TickMs drives the TUI redraw + event-drain cadence. 40ms ≈ 25fps,
	// right at the human "feels continuous" threshold. Lower = smoother
	// animation but more CPU; 16 = 60fps. Bounds 1..1000.
	TickMs int `toml:"tick_ms"`

	// EventBufferSize is the depth of the agent.Event channels between
	// the LLM stream consumer and the TUI renderer. Default 256 handles
	// 200 events/sec bursts comfortably. Above ~1024 there's no win
	// because the per-tick drain catches up regardless.
	EventBufferSize int `toml:"event_buffer_size"`

	// MouseWheelLines is how many transcript lines one wheel "click"
	// scrolls. Default 1 = pixel-precise; bump to 3 if you prefer the
	// browser-like jumpy feel.
	MouseWheelLines int `toml:"mouse_wheel_lines"`

	// ReducedMotion calms all animations: spinner ticks at 500ms,
	// shimmer is disabled. Set true for accessibility (vestibular
	// disorders, screen-reader workflows) or just to save battery.
	ReducedMotion bool `toml:"reduced_motion"`

	// SlowRenderMs — any single renderMessage / renderToolEvent call
	// taking longer than this emits a "slow render" Debug log. Mirrors
	// claude-code's render-to-screen.ts "Slow render: …ms" line. Only
	// observed when METIS_LOG_LEVEL=debug; default 8 (~half a 25fps
	// frame budget).
	SlowRenderMs int `toml:"slow_render_ms"`

	// StatsLogEvery — every Nth View() invocation, the render cache
	// dumps cumulative hits/miss/hit_rate/avg_render_ms at Debug level.
	// claude-code uses LOG_EVERY=20 in render-to-screen.ts; we default
	// to 100 since metis ticks per spinner frame regardless of activity.
	StatsLogEvery int `toml:"stats_log_every"`

	// MaxMountedItems caps how many chat items the virtualized list
	// physically holds. claude-code's MAX_MOUNTED_ITEMS=300 hard limit:
	// regardless of how many messages a session has, fiber alloc /
	// per-frame work stays bounded. 0 = unbounded (default,
	// preserves existing behavior). Set to 300 for claude-code parity
	// when long sessions start eating memory.
	MaxMountedItems int `toml:"max_mounted_items"`

	// ScrollQuantum quantizes mouse-wheel events: only every N lines'
	// worth of accumulated wheel delta triggers a list.ScrollBy call.
	// claude-code uses SCROLL_QUANTUM=40. 0 = no quantization
	// (every wheel event scrolls; existing behavior). Helpful for
	// trackpad users who emit dozens of events per gesture.
	ScrollQuantum int `toml:"scroll_quantum"`
}

type Session struct {
	Dir                  string  `toml:"dir"`
	SkillDir             string  `toml:"skill_dir"`
	AutoCompactThreshold float64 `toml:"auto_compact_threshold"`
	// AutoCompactMinimumTokens is the DeepSeek-TUI-style absolute floor:
	// full Compact will NOT fire when estimated context is below this
	// many tokens, even if AutoCompactThreshold is crossed. Protects
	// prefix-cache anchors on small/fresh sessions from churn. Default
	// 50_000 — wired into Compactor.MinimumTokens at session start.
	// Set 0 to opt out (legacy percent-only triggering).
	AutoCompactMinimumTokens int `toml:"auto_compact_minimum_tokens"`
	MaxIterations            int `toml:"max_iterations"`
}

type Tools struct {
	// Disabled is the legacy early-stage blocklist applied before any
	// tool gets registered into the per-session Registry. Affects
	// `metis tools` listing and MetisInfo introspection output.
	// Kept for backward compat with config files predating Allowed /
	// Disallowed.
	Disabled []string `toml:"disabled"`

	// Allowed is the post-registration allowlist applied after MCP +
	// plugin tools load. Empty = inherit (no filter). When non-empty,
	// only tool names matching one of these patterns survive in the
	// pool the model sees. Supports the MCP server-prefix grammar
	// described on Disallowed.
	Allowed []string `toml:"allowed"`

	// Disallowed is the post-registration blocklist. Applied after
	// Allowed. Pattern grammar (handled by
	// internal/tools.ExpandToolPatterns):
	//
	//   "Bash"             — exact tool name
	//   "mcp__office-word" — every tool whose name starts with
	//                        "mcp__office-word__" (the whole server)
	//   "mcp__" / "mcp__*" — every MCP tool, all servers
	//
	// Mirrors claude-code's filterToolsByDenyRules
	// (restored-src/src/tools.ts:262) prefix semantics so users moving
	// off claude-code can copy their deny rules verbatim.
	Disallowed []string `toml:"disallowed"`

	Bash ToolBashSettings `toml:"bash"`

	// Lazy MCP tool schemas (ToolSearch) are controlled exclusively
	// by the ENABLE_TOOL_SEARCH environment variable. See
	// internal/agent/lazy_tools.go for the parse table:
	//
	//   (unset)     → auto, fires at 10% of context window
	//   "auto:N"    → auto, fires at N%
	//   "true"      → always lazy
	//   "false"     → never lazy
	//
	// We deliberately don't expose this in TOML — the env-var path
	// matches openclaude's `tst-auto` knob and keeps the matrix of
	// "lazy yes/no, threshold, override" in one place per process.
}

type ToolBashSettings struct {
	TimeoutSeconds int      `toml:"timeout_seconds"`
	MaxOutputBytes int      `toml:"max_output_bytes"`
	Shell          string   `toml:"shell"`
	Allowlist      []string `toml:"allowlist"`
	Denylist       []string `toml:"denylist"`

	// Soft sandbox — defaults are safe-by-default. Anything that loosens
	// safety must be named with a `dangerously_` prefix so an audit can
	// grep for "what's been opted out of".
	Sandbox SandboxBashSettings `toml:"sandbox"`
}

// SandboxBashSettings controls the soft sandbox applied to Bash tool calls.
//
//   - Allow / Deny operate on the canonical command name (first token of cmd,
//     after stripping a `cmd.subcmd` suffix).
//   - Network=block injects HTTP_PROXY=http://localhost:0 into the child env so
//     curl/wget/etc fail to connect; it's not a kernel-level network namespace
//     (we deliberately don't pull in Docker/unshare for this).
//   - DangerouslyInheritEnv disables the API-key/credential blocklist.
//   - DangerouslyAllowNetwork is a hard override that skips the proxy injection.
type SandboxBashSettings struct {
	Allow                   []string `toml:"allow"`
	Deny                    []string `toml:"deny"`
	Network                 string   `toml:"network"` // "default" | "block"
	DangerouslyInheritEnv   bool     `toml:"dangerously_inherit_env"`
	DangerouslyAllowNetwork bool     `toml:"dangerously_allow_network"`

	// Mode selects the macOS Seatbelt (sandbox-exec) wrapper applied
	// to bash subprocesses. One of "off" (default — direct spawn,
	// backwards compatible) / "permissions" (Seatbelt profile with
	// global read + cwd/temp/~/.metis write) / "auto-allow" (same
	// profile + permission gate auto-approves). See
	// internal/tools/builtin/bash/sandbox_darwin.go for the profile
	// content. Non-macOS platforms reject anything other than "off"
	// with a clear error rather than silently running unsandboxed.
	Mode string `toml:"mode"`
}

// Home returns the directory metis treats as its single source of truth.
// Override with $METIS_HOME for tests or sandboxed installs; default is
// ~/.metis (claude-code style).
func Home() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".metis")
}

func defaults() *Config {
	dh := Home()
	return &Config{
		Provider: ProviderSet{
			Default: "anthropic",
			Anthropic: ProviderAnthropic{
				APIKeyEnv: "ANTHROPIC_API_KEY",
				BaseURL:   "https://api.anthropic.com",
				Model:     "claude-opus-4-7",
				// 64k is the *output* cap for current Claude models
				// (Opus 4.7 / Sonnet 4.6) — the 200k figure people quote
				// is the context window (input + output combined).
				// Asking for 200000 here would 422 against Anthropic's
				// real cap. 64k is the most you can request today.
				MaxTokens:   64000,
				TimeoutSecs: 120,
				Temperature: 1.0,
			},
			OpenAI: ProviderOpenAI{
				APIKeyEnv: "OPENAI_API_KEY",
				BaseURL:   "https://api.openai.com/v1",
				Model:     "gpt-4o",
				// gpt-4o tops out at 16k completion tokens. Above this
				// the API rejects the request — there is no "200k mode"
				// for OpenAI today; 128k is the *context*, not output.
				MaxTokens:   16000,
				TimeoutSecs: 120,
				Temperature: 1.0,
			},
			Gemini: ProviderGemini{
				APIKeyEnv: "GEMINI_API_KEY",
				BaseURL:   "https://generativelanguage.googleapis.com",
				// 2.5 family ships with a 1M-token context window;
				// max output is 65,536. We pick 64000 to leave a tiny
				// margin for the model's own bookkeeping tokens.
				Model:       "gemini-2.5-pro",
				MaxTokens:   64000,
				TimeoutSecs: 120,
				Temperature: 1.0,
			},
			Custom: map[string]ProviderRaw{},
		},
		Permission: Permission{
			Mode: "ask",
		},
		UI: UI{
			Theme:        "auto",
			Markdown:     true,
			ShowTokens:   true,
			ShowToolJSON: false,
			Performance: UIPerformance{
				TickMs:          40,
				EventBufferSize: 256,
				MouseWheelLines: 1,
				ReducedMotion:   false,
				SlowRenderMs:    8,
				StatsLogEvery:   100,
			},
		},
		Session: Session{
			Dir:      filepath.Join(dh, "sessions"),
			SkillDir: filepath.Join(dh, "skills"),
			// 0.95 (up from 0.85 on 2026-05-16): the per-turn peak
			// against a 1M-context provider sat under cap*0.85 in the
			// longrun stress test, so compaction never fired. Pushing
			// the gate later means the prompt cache survives much
			// longer between rewrites; the LLM transport overflow
			// auto-retry catches the rare overshoot.
			AutoCompactThreshold: 0.95,
			// 50K absolute floor (DeepSeek-TUI MINIMUM_AUTO_COMPACTION_TOKENS
			// idea, scaled down from their 500K to fit metis's broader
			// provider mix). On 1M-context this barely matters; on
			// 128K it prevents Compact from triggering on a fresh
			// session just because max_tokens is configured large.
			AutoCompactMinimumTokens: 50_000,
			MaxIterations:            100,
		},
		Tools: Tools{
			Bash: ToolBashSettings{
				TimeoutSeconds: 120,
				// 32 KiB — matches claude-code's BASH_MAX_OUTPUT_DEFAULT
				// (30k chars, see opensource-contributions/
				// claude-code-sourcemap utils/shell/outputLimits.ts).
				// The previous 1 MiB cap let a single `make build` /
				// `git log` / verbose Cargo output drop ~250k tokens
				// into history forever; users hit "second turn already
				// 50k tokens" as a result. The cap can be raised per-
				// project via `[tools.bash] max_output_bytes = N` in
				// config.toml — the data we DO want to keep is usually
				// near the start (compile errors) or end (final
				// status), and Bash's truncation marker tells the model
				// when output got clipped so it can re-run with a
				// narrower filter (head, tail, grep) instead of
				// silently losing context.
				MaxOutputBytes: 32 * 1024,
				Shell:          shellDefault(),
				// Baseline floor — three classic destructive idioms that
				// the security audit (internal/security) flags as required.
				// Users can extend this list in config.toml; settings there
				// replace the slice rather than merging, so anyone overriding
				// should re-include these.
				Denylist: []string{"rm -rf /", "dd of=/dev", ":(){:|:&};:"},
			},
		},
		LoopDetection: LoopDetection{
			// Enabled is no-op now (always-on by default); kept true so
			// the field reads truthy in `metis config show`.
			Enabled: true,
			Warning: 10,
			// 2026-05-15 refactor (option C): Global default = 0 means
			// "disabled" — no hard cap on total tool calls per session.
			// The signature-window detector (5 reps / 10 steps) catches
			// genuine wedge loops; the diminishing-returns detector in
			// progress_detector.go catches silent no-progress runs.
			// Counting raw tool invocations was a misleading proxy:
			// SubAgentList/SubAgentOutput polling in multi-agent workflows
			// trips count-based caps even when every iter brings new info.
			// claude-code itself has no equivalent count cap (it relies on
			// tokenBudget.ts diminishing-returns logic that we mirror).
			// Set [loop_detection].global = N to opt into a runaway cap.
			Critical:            20,
			Global:              0,
			SignatureWindow:     10,
			SignatureMaxRepeats: 5,
		},
		Agents: Agents{
			// MaxConcurrentSubAgents kept for backward compat. New
			// callers should read MaxConcurrentNamed + MaxConcurrentAnon.
			// 0 here means "use the new split fields"; we explicitly
			// keep it 0 so a freshly-defaulted Config doesn't accidentally
			// override the more generous split caps.
			MaxConcurrentSubAgents: 0,
			MaxConcurrentNamed:     20,
			MaxConcurrentAnon:      40,
			MaxAgentDepth:          1, // CC-aligned: sub-agents may not spawn sub-agents
			MaxForkDepth:           1,
			DefaultTimeoutSeconds:  600,
			CleanupOrphanWorktrees: true,
		},
	}
}

func shellDefault() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/bash"
}

// Load merges defaults + user file + project file. On first run after the
// home-dir consolidation it also auto-migrates legacy paths
// (~/.config/metis + ~/.local/share/metis) into ~/.metis.
func Load() (*Config, []string, error) {
	migrateLegacyHome()

	cfg := defaults()
	var loaded []string

	for _, path := range searchPaths() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, loaded, fmt.Errorf("load %s: %w", path, err)
		}
		loaded = append(loaded, path)
	}

	// `metis config init` from the XDG era wrote the old data paths into
	// config.toml verbatim — so even after migrateLegacyHome() moves the
	// files, the toml still points at the old location. Rewrite in-memory
	// when (and only when) the value matches a known legacy default
	// exactly; users who pointed Session.Dir at a custom location keep
	// theirs.
	//
	// Two legacy defaults are recognised:
	//
	//   1. ~/.local/share/metis/   — XDG-style under the new project name
	//   2. ~/.local/share/delphi/  — XDG-style under the OLD project name
	//                                (Metis was called "Delphi" before the
	//                                2026-04-29 rename; users who ran
	//                                `delphi config init` back then have
	//                                this hardcoded in their config.toml)
	//
	// Without case (2) a long-time user's session/skill data ended up
	// living at a delphi-named path forever — the rename migration never
	// triggered because ~/.metis/ was non-empty (post-rename installs
	// populate it). Bug audit 2026-05-09.
	if home, err := os.UserHomeDir(); err == nil {
		legacyDirs := []struct {
			oldSessions string
			oldSkills   string
		}{
			{
				oldSessions: filepath.Join(home, ".local", "share", "metis", "sessions"),
				oldSkills:   filepath.Join(home, ".local", "share", "metis", "skills"),
			},
			{
				oldSessions: filepath.Join(home, ".local", "share", "delphi", "sessions"),
				oldSkills:   filepath.Join(home, ".local", "share", "delphi", "skills"),
			},
		}
		for _, ld := range legacyDirs {
			if cfg.Session.Dir == ld.oldSessions {
				cfg.Session.Dir = filepath.Join(Home(), "sessions")
			}
			if cfg.Session.SkillDir == ld.oldSkills {
				cfg.Session.SkillDir = filepath.Join(Home(), "skills")
			}
		}
	}
	return cfg, loaded, nil
}

func searchPaths() []string {
	var out []string
	out = append(out, filepath.Join(Home(), "config.toml"))
	if cwd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(cwd, ".metis", "config.toml"))
	}
	return out
}

// migrateLegacyHome moves config + data from previous home-dir locations
// into the unified ~/.metis/. Two legacy sources are checked, in order:
//
//  1. ~/.delphi/      — Metis was previously named "Delphi"; users who
//     installed before the 2026-04-29 rename keep their data there.
//  2. ~/.config/metis + ~/.local/share/metis — even older XDG-style
//     layout from before the home-dir consolidation.
//
// Runs at most once: as soon as ~/.metis exists with content, it's a no-op.
//
// We use os.Rename, which on Unix is atomic within the same filesystem
// and fast for whole-directory moves. Across filesystems it falls back to
// ENXIO — in that rare case we leave the legacy data alone.
func migrateLegacyHome() {
	dh := Home()
	if entries, err := os.ReadDir(dh); err == nil && len(entries) > 0 {
		return // already populated, nothing to do
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	// Step 1: Delphi-era home (~/.delphi → ~/.metis). Most common path
	// for users upgrading across the rename. We rename the whole dir at
	// once when possible — atomic + cheap.
	legacyDelphi := filepath.Join(home, ".delphi")
	if dirHasFiles(legacyDelphi) {
		// If ~/.metis doesn't exist, a single rename moves everything.
		if _, err := os.Stat(dh); os.IsNotExist(err) {
			if err := os.Rename(legacyDelphi, dh); err == nil {
				fmt.Fprintf(os.Stderr, "metis: migrated %s → %s (project renamed Delphi → Metis 2026-04-29)\n", legacyDelphi, dh)
				return
			}
			// Rename failed (cross-fs?) — fall through to per-file copy.
		}
		// ~/.metis exists but is empty (or rename failed) — move entries.
		if err := os.MkdirAll(dh, 0o755); err == nil {
			moved := 0
			if entries, err := os.ReadDir(legacyDelphi); err == nil {
				for _, e := range entries {
					src := filepath.Join(legacyDelphi, e.Name())
					dst := filepath.Join(dh, e.Name())
					if _, err := os.Stat(dst); err == nil {
						continue // never overwrite
					}
					if err := os.Rename(src, dst); err == nil {
						moved++
					}
				}
			}
			if moved > 0 {
				fmt.Fprintf(os.Stderr, "metis: migrated %d entries from ~/.delphi → ~/.metis\n", moved)
			}
		}
		// Whether the inner copy succeeded or not, don't fall through to
		// the older XDG step — the Delphi-era migration path is the one
		// that matters for the typical user.
		return
	}

	// Step 2: pre-Delphi XDG-style layout. Rare but kept for completeness.
	legacyConfig := filepath.Join(home, ".config", "metis")
	legacyData := filepath.Join(home, ".local", "share", "metis")

	cfgExists := dirHasFiles(legacyConfig)
	dataExists := dirHasFiles(legacyData)
	if !cfgExists && !dataExists {
		return
	}

	if err := os.MkdirAll(dh, 0o755); err != nil {
		return
	}

	moved := 0
	if cfgExists {
		// Move config.toml + mcp.toml + any other files from ~/.config/metis
		if entries, err := os.ReadDir(legacyConfig); err == nil {
			for _, e := range entries {
				src := filepath.Join(legacyConfig, e.Name())
				dst := filepath.Join(dh, e.Name())
				if _, err := os.Stat(dst); err == nil {
					continue // never overwrite
				}
				if err := os.Rename(src, dst); err == nil {
					moved++
				}
			}
		}
	}
	if dataExists {
		for _, sub := range []string{"sessions", "skills", "memory", "cron", "tasks", "history"} {
			src := filepath.Join(legacyData, sub)
			dst := filepath.Join(dh, sub)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.Rename(src, dst); err == nil {
				moved++
			}
		}
	}

	if moved > 0 {
		fmt.Fprintf(os.Stderr, "metis: migrated %d legacy entries → %s\n", moved, dh)
	}
}

func dirHasFiles(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

// ResolveAPIKey returns the API key for the given provider using a 3-tier chain:
//
//  1. Environment variable (api_key_env from config.toml)
//  2. ~/.metis/auth.json   (written by `metis auth login`)
//  3. config.toml api_key   (legacy / explicit)
//
// Env wins so existing CI / shell-export flows keep working without users having
// to re-run the wizard. auth.json is the preferred place for hand-managed keys
// (0o600, opencode-style). config.toml api_key is kept for backward compat — new
// users should never need to write it.
//
// Applies uniformly to anthropic / openai / gemini built-ins AND to
// [provider.custom.<id>] profiles. Pre-2026-05-09 the custom branch only
// honored env + auth.json — the inline `api_key` TOML field parsed but was
// dead code; users who wrote it got "missing API key" anyway.
func (c *Config) ResolveAPIKey(provider string) (string, error) {
	switch provider {
	case "anthropic":
		if c.Provider.Anthropic.APIKeyEnv != "" {
			if v := os.Getenv(c.Provider.Anthropic.APIKeyEnv); v != "" {
				return v, nil
			}
		}
		if k, _ := auth.Get(provider); k != "" {
			return k, nil
		}
		if c.Provider.Anthropic.APIKey != "" {
			return c.Provider.Anthropic.APIKey, nil
		}
	case "openai":
		if c.Provider.OpenAI.APIKeyEnv != "" {
			if v := os.Getenv(c.Provider.OpenAI.APIKeyEnv); v != "" {
				return v, nil
			}
		}
		if k, _ := auth.Get(provider); k != "" {
			return k, nil
		}
		if c.Provider.OpenAI.APIKey != "" {
			return c.Provider.OpenAI.APIKey, nil
		}
	case "gemini":
		// Both GEMINI_API_KEY (newer official name) and GOOGLE_API_KEY
		// (legacy) are recognized — Google's own SDKs accept either, so
		// users with one already exported don't have to re-set it.
		if c.Provider.Gemini.APIKeyEnv != "" {
			if v := os.Getenv(c.Provider.Gemini.APIKeyEnv); v != "" {
				return v, nil
			}
		}
		if v := os.Getenv("GOOGLE_API_KEY"); v != "" {
			return v, nil
		}
		if k, _ := auth.Get(provider); k != "" {
			return k, nil
		}
		if c.Provider.Gemini.APIKey != "" {
			return c.Provider.Gemini.APIKey, nil
		}
	default:
		raw, ok := c.Provider.Custom[provider]
		if !ok {
			// auth.json may carry a credential for a custom provider that
			// isn't yet listed under [provider.custom] — let it through.
			if k, _ := auth.Get(provider); k != "" {
				return k, nil
			}
			return "", fmt.Errorf("unknown provider %q", provider)
		}
		if raw.APIKeyEnv != "" {
			if v := os.Getenv(raw.APIKeyEnv); v != "" {
				return v, nil
			}
		}
		if k, _ := auth.Get(provider); k != "" {
			return k, nil
		}
		// Inline `api_key = "..."` in [provider.custom.<id>]. Same final
		// fallback as the anthropic / openai / gemini cases above. Pre
		// 2026-05-09 the field didn't exist on ProviderRaw at all —
		// users who wrote the key in their custom block had it silently
		// dropped at TOML parse time and got "missing API key" later.
		if raw.APIKey != "" {
			return raw.APIKey, nil
		}
	}
	return "", fmt.Errorf("missing API key for provider %q", provider)
}

// PermissionTimeout returns the duration (with default fallback) to
// wait for an interactive permission prompt. See UI.PermissionTimeoutSeconds.
func (u *UI) PermissionTimeout() time.Duration {
	if u != nil && u.PermissionTimeoutSeconds > 0 {
		return time.Duration(u.PermissionTimeoutSeconds) * time.Second
	}
	return 60 * time.Second
}

// VoiceMaxRecord returns the duration (with default fallback) for the
// /voice auto-stop. See UI.VoiceMaxRecordSeconds.
func (u *UI) VoiceMaxRecord() time.Duration {
	if u != nil && u.VoiceMaxRecordSeconds > 0 {
		return time.Duration(u.VoiceMaxRecordSeconds) * time.Second
	}
	return 30 * time.Second
}

// StatusLineRefresh returns the duration (with default fallback) for
// the status-line refresh tick. See UI.StatusLineRefreshSeconds.
func (u *UI) StatusLineRefresh() time.Duration {
	if u != nil && u.StatusLineRefreshSeconds > 0 {
		return time.Duration(u.StatusLineRefreshSeconds) * time.Second
	}
	return 5 * time.Second
}
