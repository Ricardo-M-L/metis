// Package config loads metis's TOML configuration with cascading precedence:
//   1. CLI flags (highest)
//   2. Project-local: $PWD/.metis/config.toml
//   3. User: $METIS_HOME/config.toml or ~/.metis/config.toml
//   4. Legacy: ~/.config/metis/config.toml (auto-migrated on first run)
//   5. Defaults (lowest)
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
	MCP           MCP           `toml:"mcp"`
	Channels      Channels      `toml:"channels"`
}

// Channels groups the chat-platform adapters wired up by SendMessage.
// Each subsection only has *_env fields — the actual tokens / webhook URLs
// stay in environment variables to keep config.toml diffable.
type Channels struct {
	DefaultPlatform string             `toml:"default_platform"` // used when channel arg has no "<platform>:" prefix
	Slack           SlackChannel       `toml:"slack"`
	Telegram        TelegramChannel    `toml:"telegram"`
	Discord         DiscordChannel     `toml:"discord"`
	Dingtalk        DingtalkChannel    `toml:"dingtalk"`
	Feishu          FeishuChannel      `toml:"feishu"`
	Wechat          WechatChannel      `toml:"wechat"`
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
// Defaults are tuned for typical coding sessions; raise thresholds for long-running jobs.
type LoopDetection struct {
	Enabled  bool `toml:"enabled"`
	Warning  int  `toml:"warning"`  // per-tool consecutive call count
	Critical int  `toml:"critical"` // per-tool consecutive call count
	Global   int  `toml:"global"`   // total tool calls in a single Run before abort
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
	APIKeyEnv   string  `toml:"api_key_env"`
	APIKey      string  `toml:"api_key"`
	BaseURL     string  `toml:"base_url"`
	Model       string  `toml:"model"`
	MaxTokens   int     `toml:"max_tokens"`
	// ContextWindow overrides the model-prefix lookup used for
	// auto-compaction. Set when the served model's name doesn't match
	// a known prefix or its window differs from the upstream default.
	ContextWindow int     `toml:"context_window"`
	TimeoutSecs   int     `toml:"timeout_seconds"`
	Temperature   float64 `toml:"temperature"`
}

type ProviderAnthropic struct {
	APIKeyEnv     string  `toml:"api_key_env"`
	APIKey        string  `toml:"api_key"` // direct (discouraged; prefer env)
	BaseURL       string  `toml:"base_url"`
	Model         string  `toml:"model"`
	MaxTokens     int     `toml:"max_tokens"`
	// ContextWindow overrides the model-prefix lookup used for
	// auto-compaction. Required for Anthropic-compatible third-party
	// gateways (MiniMax, OpenRouter, ...) where the served model
	// doesn't match `claude-*` and the real window may be smaller
	// than 200k.
	ContextWindow int     `toml:"context_window"`
	TimeoutSecs   int     `toml:"timeout_seconds"`
	AnthropicBeta string  `toml:"anthropic_beta"`
	Temperature   float64 `toml:"temperature"`
}

type ProviderOpenAI struct {
	APIKeyEnv   string  `toml:"api_key_env"`
	APIKey      string  `toml:"api_key"`
	BaseURL     string  `toml:"base_url"`
	Model       string  `toml:"model"`
	MaxTokens   int     `toml:"max_tokens"`
	// ContextWindow overrides the model-prefix lookup. See ProviderAnthropic.
	ContextWindow int     `toml:"context_window"`
	TimeoutSecs   int     `toml:"timeout_seconds"`
	Temperature   float64 `toml:"temperature"`
}

type ProviderRaw struct {
	Transport   string  `toml:"transport"` // anthropic_messages | openai_chat
	APIKeyEnv   string  `toml:"api_key_env"`
	BaseURL     string  `toml:"base_url"`
	Model       string  `toml:"model"`
	MaxTokens   int     `toml:"max_tokens"`
	TimeoutSecs int     `toml:"timeout_seconds"`
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
	Theme        string         `toml:"theme"`
	Markdown     bool           `toml:"markdown"`
	ShowTokens   bool           `toml:"show_tokens"`
	ShowToolJSON bool           `toml:"show_tool_json"`
	Performance  UIPerformance  `toml:"performance"`
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
}

type Session struct {
	Dir                  string  `toml:"dir"`
	SkillDir             string  `toml:"skill_dir"`
	AutoCompactThreshold float64 `toml:"auto_compact_threshold"`
	MaxIterations        int     `toml:"max_iterations"`
}

type Tools struct {
	Disabled []string         `toml:"disabled"`
	Bash     ToolBashSettings `toml:"bash"`
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
			},
		},
		Session: Session{
			Dir:                  filepath.Join(dh, "sessions"),
			SkillDir:             filepath.Join(dh, "skills"),
			AutoCompactThreshold: 0.85,
			MaxIterations:        100,
		},
		Tools: Tools{
			Bash: ToolBashSettings{
				TimeoutSeconds: 120,
				MaxOutputBytes: 1 << 20, // 1 MiB
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
			Enabled:  true,
			Warning:  10,
			Critical: 20,
			Global:   60,
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
	// when (and only when) the value matches the legacy default exactly;
	// users who pointed Session.Dir at a custom location keep theirs.
	if home, err := os.UserHomeDir(); err == nil {
		oldSessions := filepath.Join(home, ".local", "share", "metis", "sessions")
		oldSkills := filepath.Join(home, ".local", "share", "metis", "skills")
		if cfg.Session.Dir == oldSessions {
			cfg.Session.Dir = filepath.Join(Home(), "sessions")
		}
		if cfg.Session.SkillDir == oldSkills {
			cfg.Session.SkillDir = filepath.Join(Home(), "skills")
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
	}
	return "", fmt.Errorf("missing API key for provider %q", provider)
}
