# internal/runtime

The wiring layer between `cmd/metis` (entry point) and the agent /
tools / TUI packages. Handles: provider construction from config,
tool slice assembly, MCP client/server lifecycle, plan-mode setup,
plugin loading, prompt assembly, history persistence, scheduler /
daemon scaffolding. If a `metis <subcommand>` needs to build out the
agent and its dependencies, the wiring lives here.

## File-naming convention (browse by family)

| Family | Files | What it owns | Entry file |
|---|---|---|---|
| `mcp*` | 3 | MCP client setup + env-var expansion in MCP server configs | `mcp.go`, `mcp_env.go` |
| `plan*` | 2 | Plan-mode entry + flag handling | `plan_mode.go` |
| `agent*` | 2 | Agent profile loader + per-profile wiring | `agent_profile.go` |
| `provider*` | 2 | LLM provider construction from `[providers.*]` toml | `provider.go` |
| singletons | ~30 | Each a single wiring concern: `tools.go`, `tasks.go`, `system_prompt.go`, `sections.go`, `permission.go`, `skills.go`, `schedule.go`, `streamlined.go`, `snapshot.go`, `run.go`, `resume.go`, `history.go`, `early_init.go`, `dirs.go`, `daemon.go`, `coordinator.go`, `preconnect.go`, `plugin.go`, `learning.go`, `subdir.go` |

## Sub-packages

| Path | Purpose |
|---|---|
| `prompts/` | The system-prompt template assembly (base sections + addenda) |
| `prompts/base/` | The 8-file ordered base prompt (`01_identity.md` through `08_interaction_modes.md`) |
| `builtin_profiles/` | Built-in subagent profiles (`verify.md`, `plan.md`, `general.md` ...) used by `Agent({subagent_type: ...})` |

## Where do I find...

- **System prompt assembly** → `system_prompt.go` + `sections.go`; the
  base text lives under `prompts/base/`
- **Provider construction** (from `~/.metis/config.toml`) → `provider.go`
- **Tool slice the runtime exports to the loop** → `tools.go`
- **MCP wiring** (clients, env-var expansion, server lifecycle) →
  `mcp.go`, `mcp_env.go`
- **Plan-mode setup** → `plan_mode.go`
- **Subagent profile definitions** → `builtin_profiles/<name>.md`
- **Daemon / coordinator wiring** (for background mode) → `daemon.go`,
  `coordinator.go`
- **Plugin loading** → `plugin.go`
- **History entries** (`AppendHistory` used by `metis run` / TUI) →
  `history.go`

## Design invariants

- `runtime` depends on nothing in `internal/agent` for types other
  than `agent.Loop` itself — the dependency arrow is `runtime → agent`,
  never the reverse.
- Provider construction reads `~/.metis/config.toml` AND honors env
  overrides (e.g., `METIS_MAX_SUBAGENTS`). Any new env var goes here
  + documented in the config schema.
- The base prompt files in `prompts/base/` are ordered: earlier files
  carry more weight in the model's attention. Don't reorder without
  benchmarking.
- Subagent profiles in `builtin_profiles/*.md` use YAML frontmatter
  (`name`, `description`, `tools`, `permission_mode`, `effort`,
  `max_turns`) + Markdown body. The `verify` profile's mandatory
  `VERDICT: PASS/FAIL/PARTIAL` trailing line is the contract gate's
  hook — see `internal/agent/contract.go::extractVerdict`.
