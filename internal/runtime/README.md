# internal/runtime

The composition layer between `cmd/metis` and the agent, provider, tool,
plugin, MCP, persistence, and prompt packages. It builds runnable object
graphs from configuration and current process state. Policy implementations
generally live in their owning packages; the code here decides which pieces a
particular command or session receives.

## Browse by concern

| Concern | Main files |
|---|---|
| Provider construction and hints | `provider.go`, `provider_hints.go`, `auth_gate.go` |
| Loop and tool graph assembly | `agent_loop.go`, `tools.go`, `runtime_rebind.go`, `channels.go` |
| System prompt composition | `assembler.go`, `sections.go`, `system_prompt.go`, `skills_probe.go`, `git_context.go`, `subdir_hints.go` |
| Agent profiles | `agent_profile.go`, `builtin_profiles.go`, `builtin_profiles/` |
| MCP lifecycle and prompt/resource support | `mcp/` |
| Plugins | `plugin.go`, `bundled_plugins.go` |
| Live plan state | `plan_overlay.go`, `plan_archive.go` |
| Sessions and machine-readable output | `resume.go`, `snapshot.go`, `run_cache.go`, `history_jsonl.go`, `output_schema.go` |
| Coordinator, daemon, and scheduling | `coordinator.go`, `coordinator_mode.go`, `daemon.go`, `cron.go`, `schedule_installer.go` |
| Permissions and startup context | `permission.go`, `config_hooks.go`, `early_input.go`, `dirs.go`, `ide.go`, `preconnect.go` |

## Sub-packages and embedded data

| Path | Purpose |
|---|---|
| `prompts/` | Prompt source and the legacy aggregate base template |
| `prompts/base/` | Numbered base-prompt sections, including the optional computer-use section |
| `builtin_profiles/` | Bundled sub-agent profiles such as coordinator, explore, plan, teammate, and verify |
| `mcp/` | MCP configuration expansion, client/server lifecycle, prompt discovery, and cache support |

## Provider construction

Provider configuration is rooted at singular TOML sections such as
`[provider]`, `[provider.anthropic]`, and `[provider.custom.<id>]`.
`provider.go` has two construction paths:

- Built-in provider families are constructed directly because they need
  provider-specific configuration and authentication plumbing.
- Entries under `[provider.custom.<id>]` resolve their selected transport and
  are built through `internal/llm/transport`'s constructor registry.

Do not describe the transport registry as the universal provider factory; it
is the custom-profile extension path. Provider capability normalization and
user-facing hints live beside construction in `provider.go` and
`provider_hints.go`.

Environment overrides are owned by the concern that consumes them, not all by
`provider.go`. For example, provider credentials belong to provider/config
resolution, prompt toggles belong to prompt assembly, and sub-agent roster
caps are resolved by `cmd/metis`. Document a new public environment variable
centrally, but keep its implementation with its actual owner.

## Prompt and plan assembly

`assembler.go` executes the ordered getters from `sections.go`; source text is
split across the numbered files in `prompts/base/`. Callers may interleave
project context, provider hints, permission-aware sections, and volatile
overlays before rendering. The numbering is semantic ordering, so reorder or
recache sections only with prompt/cache behavior in mind.

Plan mode is live state, not a startup-only prompt flag. `plan_overlay.go`
adds a volatile per-request section while the permission mode is plan;
`EnterPlanMode`/`ExitPlanMode` tools and the permission gate enforce the
transition and read-only boundary. `plan_archive.go` handles persisted plan
artifacts. The permission gate remains authoritative if prompt text and live
mode ever disagree.

## Agent profiles and dependency direction

Agent profiles are Markdown with frontmatter parsed by `agent_profile.go`.
The schema includes model/tool filters, `permission_mode`, `effort`,
`max_turns`, initial prompt, skills, memory snapshot, and related options; the
Markdown body becomes the profile system prompt. Project profiles override
user profiles, which override bundled profiles.

The package intentionally imports multiple `internal/agent` types, including
`Loop`, events, hooks, monitors, cron services/jobs, tool calls, and the
sub-agent roster. The dependency direction is still `runtime → agent` (never
`agent → runtime`), but `Loop` is not the only shared type.

The bundled `verify` profile requires a trailing
`VERDICT: PASS|FAIL|PARTIAL`; `internal/agent/contract.go` parses the last such
marker for the large-run verification gate.
